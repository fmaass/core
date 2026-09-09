package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"windshift/internal/authz"
	"windshift/internal/database"
	"windshift/internal/logger"
	"windshift/internal/models"
	"windshift/internal/repository"
	"windshift/internal/restapi"
	"windshift/internal/sanitize"
	"windshift/internal/services"
	"windshift/internal/utils"
	"windshift/internal/validation"
	"windshift/internal/webhook"
)

type ItemHandler struct {
	db                  database.Database
	itemRepo            *repository.ItemRepository
	hierarchyService    *services.HierarchyService
	permissionService   *services.PermissionService
	authz               *authz.Authz
	itemCache           *services.ItemCacheService
	activityTracker     *services.ActivityTracker
	idResolver          *services.IDResolverService
	itemCRUD            *services.ItemCRUDService
	itemCreation        *services.ItemCreationService
	itemUpdate          *services.ItemUpdateApplicationService
	itemDeletion        *services.ItemDeletionApplicationService
	itemWorkspaceMove   *services.ItemWorkspaceMoveService
	mentionService      *services.MentionService // Mention service for processing @mentions (optional, can be nil)
	notificationService interface {
		EmitEvent(event *services.NotificationEvent)
	} // Notification service for async notification processing (optional, can be nil)
	actionService interface {
		EmitActionEvent(event *models.ActionEvent)
	} // Action service for automation workflows (optional, can be nil)
	webhookSender    *webhook.WebhookSender     // Webhook sender for dispatching webhook events (optional, can be nil)
	eventCoordinator *services.EventCoordinator // Centralized event coordinator for side effects (optional, can be nil)
	issueSyncService interface {
		PushStatusToGitHub(ctx context.Context, itemID int, newStatusID int)
	} // Issue sync service for pushing status changes to GitHub (optional, can be nil)
	conditionService  *services.ConditionService // Condition service for workflow transition conditions (optional, can be nil)
	approvalService   *services.ApprovalService  // Approval service for status-bound approvals (optional, can be nil)
	sseHub            *services.SSEHub           // Hub for the item event stream (optional, can be nil → /events returns 503)
	transitionMatrix  *services.TransitionMatrixService
	bulkUpdate        *services.ItemUpdateService
	iterationComplete *services.IterationCompletionService
	bulkEmitter       *services.BulkUpdateEmitter
	bulkMetrics       *services.BulkOperationMetrics
	dbRequestTimeout  time.Duration
}

const defaultDBRequestTimeout = 12 * time.Second

func NewItemHandler(db database.Database, permissionService *services.PermissionService, activityTracker *services.ActivityTracker, notificationService interface {
	EmitEvent(event *services.NotificationEvent)
}, cacheSizeMB ...int) *ItemHandler {
	// Initialize item cache service
	cacheConfig := services.DefaultItemCacheConfig()
	if len(cacheSizeMB) > 0 && cacheSizeMB[0] > 0 {
		cacheConfig.MaxCacheSize = cacheSizeMB[0]
	}
	itemCache, err := services.NewItemCacheService(db, cacheConfig)
	if err != nil {
		slog.Warn("failed to initialize item cache, continuing without cache", slog.Any("error", err))
		// Continue without cache, will fall back to direct queries
		itemCache = nil
	}

	hierarchyService := services.NewHierarchyService(db)
	itemUpdate := services.NewItemUpdateApplicationService(db, permissionService)
	itemUpdate.SetActivityTracker(activityTracker)
	itemUpdate.SetCache(itemCache, hierarchyService)
	// Embeddings that skip the EventCoordinator keep the original per-service
	// notification pipeline (actions/webhooks attach through later setters).
	var notify func(*services.NotificationEvent)
	if notificationService != nil {
		notify = notificationService.EmitEvent
	}
	itemUpdate.SetFallbackEmitter(services.NewLegacyItemUpdatedEmitter(db, notify, nil, nil))
	itemDeletion := services.NewItemDeletionApplicationService(db, permissionService)
	itemDeletion.SetCache(itemCache, hierarchyService)

	h := &ItemHandler{
		db:                  db,
		itemRepo:            repository.NewItemRepository(db),
		hierarchyService:    hierarchyService,
		permissionService:   permissionService,
		authz:               authz.New(db, permissionService),
		itemCache:           itemCache,
		activityTracker:     activityTracker,
		idResolver:          services.NewIDResolverService(db),
		itemCRUD:            services.NewItemCRUDService(db),
		itemCreation:        services.NewItemCreationService(db, permissionService),
		itemUpdate:          itemUpdate,
		itemDeletion:        itemDeletion,
		itemWorkspaceMove:   services.NewItemWorkspaceMoveService(db),
		notificationService: notificationService,
		transitionMatrix:    services.NewTransitionMatrixService(db),
		bulkUpdate:          services.NewItemUpdateService(db).WithPermissionService(permissionService),
		iterationComplete:   services.NewIterationCompletionService(db),
		bulkMetrics:         services.NewBulkOperationMetrics(),
		dbRequestTimeout:    defaultDBRequestTimeout,
	}
	// The hooks read h's fields at emit time, not now: the webhook sender, the
	// mention service and the event coordinator are wired by setters after this
	// constructor returns, so an emitter that captured them here would be a
	// permanently unwired one.
	h.bulkEmitter = services.NewBulkUpdateEmitter(services.BulkUpdateSideEffects{
		TrackEdit: func(userID, itemID int) error {
			if h.activityTracker == nil {
				return nil
			}
			return h.activityTracker.TrackItemActivity(userID, itemID, services.ActivityEdit)
		},
		InvalidateProjectSubtree: func(itemID int) {
			if h.itemCache == nil {
				return
			}
			h.invalidateEffectiveProjectSubtree(itemID)
		},
		EmitItemUpdated: func(original, updated *models.Item, statusChanged, assigneeChanged bool, userID int, fieldChanges []services.HistoryEntry, username string) {
			if h.eventCoordinator != nil {
				h.eventCoordinator.EmitItemUpdated(original, updated, statusChanged, assigneeChanged, userID, fieldChanges, username)
				return
			}
			if h.webhookSender != nil {
				h.webhookSender.DispatchEvent("item.updated", updated)
			}
		},
		ProcessMentions: func(params services.ProcessMentionsParams) error {
			if h.mentionService == nil {
				return nil
			}
			return h.mentionService.ProcessMentions(params)
		},
		MaskProjectNames: h.maskInaccessibleProjectNames,
	})
	return h
}

// BulkUpdateEmitter exposes the shared side-effect fan-out so other surfaces —
// REST v1's iteration completion above all — emit exactly what this one does
// instead of reimplementing it (INFRA-295).
func (h *ItemHandler) BulkUpdateEmitter() *services.BulkUpdateEmitter {
	return h.bulkEmitter
}

func (h *ItemHandler) SetBulkOperationMetrics(metrics *services.BulkOperationMetrics) {
	if metrics != nil {
		h.bulkMetrics = metrics
	}
}

// SetTransitionMatrixService replaces the default process-local service. This
// is primarily useful for sharing telemetry or injecting a test service.
func (h *ItemHandler) SetTransitionMatrixService(service *services.TransitionMatrixService) {
	if service != nil {
		h.transitionMatrix = service
	}
}

// SetDBRequestTimeout configures the database-work deadline used by measured
// item read endpoints. Non-positive values retain the safe default.
func (h *ItemHandler) SetDBRequestTimeout(timeout time.Duration) {
	if timeout > 0 {
		h.dbRequestTimeout = timeout
	}
}

func (h *ItemHandler) requestDBContext(r *http.Request) (context.Context, context.CancelFunc) {
	timeout := h.dbRequestTimeout
	if timeout <= 0 {
		timeout = defaultDBRequestTimeout
	}
	return context.WithTimeout(r.Context(), timeout)
}

// respondItemReadError records cancellations separately and never attempts a
// response write after the client-owned request context has been canceled.
func (h *ItemHandler) respondItemReadError(w http.ResponseWriter, r *http.Request, err error) {
	database.ObserveRequestQueryError(err)
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("item read database deadline exceeded", "path", r.URL.Path, "timeout", h.dbRequestTimeout)
		respondError(w, r, restapi.NewAPIError(http.StatusGatewayTimeout, "DATABASE_DEADLINE_EXCEEDED", "The database request timed out."))
		return
	}
	respondInternalError(w, r, err)
}

// respondItemReadErrorContext preserves the local database deadline when a
// driver (notably lib/pq) reports cancellation as a driver-specific error
// instead of wrapping context.DeadlineExceeded. Without this check a timed-out
// list query is incorrectly exposed as HTTP 500.
func (h *ItemHandler) respondItemReadErrorContext(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	if ctx != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	h.respondItemReadError(w, r, err)
}

// SetWebhookSender sets the webhook sender for dispatching webhook events
func (h *ItemHandler) SetWebhookSender(sender *webhook.WebhookSender) {
	h.webhookSender = sender
	h.itemUpdate.SetFallbackWebhook(sender)
}

// SetMentionService sets the mention service for processing @mentions
func (h *ItemHandler) SetMentionService(mentionService *services.MentionService) {
	h.mentionService = mentionService
	h.itemUpdate.SetMentionService(mentionService)
}

// SetActionService sets the action service for automation workflows
func (h *ItemHandler) SetActionService(actionService interface {
	EmitActionEvent(event *models.ActionEvent)
}) {
	h.actionService = actionService
	h.itemUpdate.SetFallbackAction(actionService)
}

// SetEventCoordinator sets the event coordinator for centralized side effects
func (h *ItemHandler) SetEventCoordinator(ec *services.EventCoordinator) {
	h.eventCoordinator = ec
	h.itemCreation.SetEmitter(ec)
	h.itemUpdate.SetEmitter(ec)
	h.itemDeletion.SetEmitter(ec)
}

// ItemCreationService exposes the fully wired user-facing creation pipeline so
// REST v1 can use the same validation, persistence, and side effects.
func (h *ItemHandler) ItemCreationService() *services.ItemCreationService {
	return h.itemCreation
}

// ItemUpdateApplicationService exposes the fully wired user-facing update
// pipeline so REST v1 gets the same committed-item side effects as the cookie
// surface.
func (h *ItemHandler) ItemUpdateApplicationService() *services.ItemUpdateApplicationService {
	return h.itemUpdate
}

// ItemDeletionApplicationService exposes the fully wired user-facing deletion
// pipeline for REST v1 and MCP.
func (h *ItemHandler) ItemDeletionApplicationService() *services.ItemDeletionApplicationService {
	return h.itemDeletion
}

// SetIssueSyncService sets the issue sync service for pushing status changes to GitHub
func (h *ItemHandler) SetIssueSyncService(svc interface {
	PushStatusToGitHub(ctx context.Context, itemID int, newStatusID int)
}) {
	h.issueSyncService = svc
}

// SetConditionService sets the condition service for workflow transition conditions
func (h *ItemHandler) SetConditionService(cs *services.ConditionService) {
	h.conditionService = cs
}

// parseIDListParam parses a comma-separated list of integer IDs from a
// query parameter. Empty/non-numeric tokens are silently dropped — a
// zero-length result means "no usable filter values supplied".
func parseIDListParam(raw string) []int {
	parts := strings.Split(raw, ",")
	ids := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if id, err := strconv.Atoi(p); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// itemCreateRequest is the legacy web/API create payload. Keep request-only
// fields here instead of adding them to models.Item, whose milestone
// representation on reads is the joined Milestones slice.
type itemCreateRequest struct {
	WorkspaceID       int            `json:"workspace_id"`
	Title             string         `json:"title"`
	Description       string         `json:"description"`
	StatusID          *int           `json:"status_id,omitempty"`
	PriorityID        *int           `json:"priority_id,omitempty"`
	ItemTypeID        *int           `json:"item_type_id,omitempty"`
	DueDate           *time.Time     `json:"due_date,omitempty"`
	StartDate         *time.Time     `json:"start_date,omitempty"`
	EndDate           *time.Time     `json:"end_date,omitempty"`
	IsTask            bool           `json:"is_task"`
	IterationID       *int           `json:"iteration_id,omitempty"`
	ProjectID         *int           `json:"project_id,omitempty"`
	InheritProject    bool           `json:"inherit_project"`
	TimeProjectID     *int           `json:"time_project_id,omitempty"`
	AssigneeID        *int           `json:"assignee_id,omitempty"`
	ParentID          *int           `json:"parent_id"`
	RelatedWorkItemID *int           `json:"related_work_item_id,omitempty"`
	StoryPoints       *float64       `json:"story_points,omitempty"`
	EstimateMinutes   *int           `json:"estimate_minutes,omitempty"`
	CustomFieldValues map[string]any `json:"custom_field_values,omitempty"`
	MilestoneIDs      []int          `json:"milestone_ids,omitempty"`
}

// SetApprovalService wires the approval service so status-bound approvals gate
// transitions through this handler.
func (h *ItemHandler) SetApprovalService(ap *services.ApprovalService) {
	h.approvalService = ap
}

func (h *ItemHandler) GetAll(w http.ResponseWriter, r *http.Request) {
	// Get user from context
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.requestDBContext(r)
	defer cancel()

	// Get accessible workspace IDs (includes active workspaces and inactive ones where user has admin access)
	accessibleWorkspaceIDs, err := h.getAccessibleWorkspaceIDs(user)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}

	// If user has no accessible workspaces, return empty list
	if len(accessibleWorkspaceIDs) == 0 {
		respondJSONOK(w, map[string]any{
			"items":       []models.Item{},
			"total_count": 0,
			"page":        1,
			"limit":       50,
		})
		return
	}

	// Parse pagination parameters
	page := 1
	limit := 50
	maxLimit := 1000

	if pageStr := r.URL.Query().Get("page"); pageStr != "" {
		var p int
		if p, err = strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		var l int
		if l, err = strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
			if limit > maxLimit {
				limit = maxLimit
			}
		}
	}

	offset := (page - 1) * limit

	// Build filters from query parameters
	var filters services.ItemFilters
	qlQuery := r.URL.Query().Get("ql")
	var collectionID int
	var workspaceID int

	// Resolve collection_id
	if qlQuery == "" {
		if collectionParam := r.URL.Query().Get("collection_id"); collectionParam != "" {
			cid, err := strconv.Atoi(collectionParam)
			if err != nil {
				respondValidationError(w, r, "Invalid collection_id parameter")
				return
			}
			collectionID = cid
		}
	}

	// Apply workspace_id only when no collection_id was provided
	if collectionID == 0 {
		if wsParam := r.URL.Query().Get("workspace_id"); wsParam != "" {
			wsID, err := strconv.Atoi(wsParam)
			if err != nil {
				respondValidationError(w, r, "Invalid workspace_id parameter")
				return
			}
			workspaceID = wsID
		}
	}

	// When no QL query, apply individual filters
	if qlQuery == "" && collectionID == 0 {
		if status := r.URL.Query().Get("status"); status != "" {
			statusID, err := strconv.Atoi(status)
			if err == nil {
				filters.StatusID = &statusID
			}
		}

		if priorityParam := r.URL.Query().Get("priority_id"); priorityParam != "" {
			priorityID, err := strconv.Atoi(priorityParam)
			if err == nil {
				filters.PriorityID = &priorityID
			}
		}

		if assigneeParam := r.URL.Query().Get("assignee_id"); assigneeParam != "" {
			assigneeID, err := strconv.Atoi(assigneeParam)
			if err == nil {
				filters.AssigneeID = &assigneeID
			}
		}

		// Hierarchy filters
		if parentID := r.URL.Query().Get("parent_id"); parentID != "" {
			if parentID == "null" || parentID == "0" {
				zero := 0
				filters.ParentID = &zero
				filters.ParentIDIsSet = true
			} else {
				pid, err := strconv.Atoi(parentID)
				if err == nil {
					filters.ParentID = &pid
					filters.ParentIDIsSet = true
				}
			}
		}

		if level := r.URL.Query().Get("level"); level != "" {
			levelInt, err := strconv.Atoi(level)
			if err != nil {
				respondValidationError(w, r, "Invalid level parameter: must be an integer")
				return
			}
			filters.Level = &levelInt
		}

		if maxLevel := r.URL.Query().Get("max_level"); maxLevel != "" {
			maxLevelInt, err := strconv.Atoi(maxLevel)
			if err != nil {
				respondValidationError(w, r, "Invalid max_level parameter: must be an integer")
				return
			}
			filters.MaxLevel = &maxLevelInt
		}

		if createdSince := r.URL.Query().Get("created_since"); createdSince != "" {
			filters.CreatedSince = &createdSince
		}
	}

	// Cap completed items to those finished since this date (ISO YYYY-MM-DD or
	// RFC3339). Only items in a completed status are constrained; everything
	// else passes. Applies to both QL and non-QL queries.
	if completedSince := r.URL.Query().Get("completed_since"); completedSince != "" {
		filters.CompletedSince = &completedSince
	}

	// ID filter (applies to both QL and non-QL queries)
	if idParam := r.URL.Query().Get("id"); idParam != "" {
		itemID, err := strconv.Atoi(idParam)
		if err == nil {
			filters.ItemID = &itemID
		}
	}

	// Multi-status include/exclude filters (apply to both QL and non-QL
	// queries — the board uses these to page non-completed columns
	// separately from the capped rightmost column).
	if raw := r.URL.Query().Get("status_id"); raw != "" {
		filters.StatusIDs = parseIDListParam(raw)
	}
	if raw := r.URL.Query().Get("status_id_not"); raw != "" {
		filters.StatusIDsNot = parseIDListParam(raw)
	}

	// Sub-filter QL (ANDed with collection/direct QL)
	subQLQuery := r.URL.Query().Get("sub_ql")

	// Determine sort order
	sortBy := r.URL.Query().Get("order_by")
	sortAsc := strings.EqualFold(r.URL.Query().Get("sort_direction"), "asc")
	cursor := r.URL.Query().Get("cursor")
	// Cursor mode is explicit for the first request; a continuation token
	// implicitly enables it. Collection/QL requests remain offset-paginated.
	cursorMode := strings.EqualFold(r.URL.Query().Get("cursor_mode"), "true") || cursor != ""

	// Collection cards/lists do not render item descriptions; allow callers to
	// trim that often-large column from list responses while keeping detail
	// endpoints unchanged.
	omitDescriptions := strings.EqualFold(r.URL.Query().Get("omit_descriptions"), "true") ||
		strings.EqualFold(r.URL.Query().Get("fields"), "summary")
	var watermark int64
	if strings.EqualFold(r.URL.Query().Get("include_watermark"), "true") {
		watermark, err = repository.NewItemChangeRepository(h.db).CurrentWatermark(accessibleWorkspaceIDs, workspaceID)
		if err != nil {
			respondInternalError(w, r, err)
			return
		}
	}

	// Call service
	pageResult, err := h.itemCRUD.ListWithQLPageContext(ctx, services.ListWithQLParams{
		WorkspaceID:  workspaceID,
		CollectionID: collectionID,
		QLQuery:      qlQuery,
		SubQLQuery:   subQLQuery,
		WorkspaceIDs: accessibleWorkspaceIDs,
		UserID:       user.ID,
		Filters:      filters,
		Pagination: services.PaginationParams{
			Limit:      limit,
			Offset:     offset,
			Cursor:     cursor,
			CursorMode: cursorMode,
		},
		SortBy:           sortBy,
		SortAsc:          sortAsc,
		OmitDescriptions: omitDescriptions,
	})
	if err != nil {
		// Check for QL-specific errors to return as validation errors
		if errors.Is(err, services.ErrQLQuery) {
			respondValidationError(w, r, err.Error())
			return
		}
		if errors.Is(err, services.ErrCollectionNotFound) {
			respondNotFound(w, r, "collection")
			return
		}
		if errors.Is(err, repository.ErrInvalidItemListCursor) {
			respondValidationError(w, r, "Invalid cursor parameter")
			return
		}
		h.respondItemReadErrorContext(ctx, w, r, err)
		return
	}
	items := pageResult.Items
	totalCount := pageResult.Total

	// Filter items based on user permissions
	filteredItems, err := h.filterItemsByPermissions(user.ID, items)
	if err != nil {
		slog.Error("permission check failed", slog.Int("user_id", user.ID), slog.String("operation", "GetAll"), slog.Any("error", err))
		respondInternalError(w, r, err)
		return
	}
	if ctx.Err() != nil {
		h.respondItemReadErrorContext(ctx, w, r, ctx.Err())
		return
	}
	items = filteredItems

	// Strip names of time projects the viewer can't access (keeps the IDs).
	h.maskInaccessibleProjectNamesContext(ctx, user.ID, items)
	if ctx.Err() != nil {
		h.respondItemReadErrorContext(ctx, w, r, ctx.Err())
		return
	}

	// Load labels for items
	if err := repository.NewLabelRepository(h.db).LoadForItemsContext(ctx, items); err != nil {
		h.respondItemReadErrorContext(ctx, w, r, err)
		return
	}
	if err := LoadPersonalLabelsForItemsContext(ctx, h.db, items, user.ID); err != nil {
		h.respondItemReadErrorContext(ctx, w, r, err)
		return
	}
	if err := repository.NewMilestoneAttachRepository(h.db).LoadForItemsContext(ctx, items); err != nil {
		h.respondItemReadErrorContext(ctx, w, r, err)
		return
	}

	// Compute sortable fields: system fields for the workspace
	sortableFields := repository.SystemSortableFieldKeys()

	// Create paginated response
	response := models.PaginatedItemsResponse{
		Items: items,
		Pagination: models.PaginationMeta{
			Page:       page,
			Limit:      limit,
			Total:      totalCount,
			TotalPages: (totalCount + limit - 1) / limit,
		},
		SortableFields: sortableFields,
		Watermark:      watermark,
		NextCursor:     pageResult.NextCursor,
	}

	respondJSONOK(w, response)
}

func (h *ItemHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}

	// Require authentication
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}

	h.respondItemByID(w, r, user, id)
}

// GetByKeyAndNumber resolves a stable display key reference, e.g.
// /api/workspaces/WI/items/123, then returns the same item shape as Get.
// This lets SPA deep links support both numeric IDs and workspace-key/item-number keys.
func (h *ItemHandler) GetByKeyAndNumber(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		respondValidationError(w, r, "workspace key is required")
		return
	}
	itemRef := strings.TrimSpace(r.PathValue("number"))
	if itemRef == "" {
		respondValidationError(w, r, "item number is required")
		return
	}

	lookupKey := key
	lookupNumber := 0
	if parts := strings.SplitN(itemRef, "-", 2); len(parts) == 2 {
		embeddedKey := strings.TrimSpace(parts[0])
		if embeddedKey == "" {
			respondValidationError(w, r, "invalid item key")
			return
		}
		// Allow /workspaces/1/items/WI-123 and /workspaces/WI/items/WI-123.
		// If the path workspace is itself a key and disagrees with the embedded
		// key, treat the item as not found rather than silently crossing workspaces.
		if _, numericPathWorkspace := strconv.Atoi(key); numericPathWorkspace != nil && !strings.EqualFold(key, embeddedKey) {
			respondNotFound(w, r, "item")
			return
		}
		lookupKey = embeddedKey
		var err error
		lookupNumber, err = strconv.Atoi(parts[1])
		if err != nil || lookupNumber <= 0 {
			respondValidationError(w, r, "invalid item key")
			return
		}
	} else {
		var err error
		lookupNumber, err = strconv.Atoi(itemRef)
		if err != nil || lookupNumber <= 0 {
			respondValidationError(w, r, "invalid item number")
			return
		}
	}

	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}

	id, err := repository.NewItemRepository(h.db).FindIDByKeyAndNumber(lookupKey, lookupNumber)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			respondNotFound(w, r, "item")
			return
		}
		respondInternalError(w, r, err)
		return
	}

	h.respondItemByID(w, r, user, id)
}

func (h *ItemHandler) respondItemByID(w http.ResponseWriter, r *http.Request, user *models.User, id int) {
	item, err := h.loadItemForUser(r.Context(), user, id, true)
	if errors.Is(err, repository.ErrNotFound) {
		respondNotFound(w, r, "item")
		return
	}
	if err != nil {
		respondInternalError(w, r, err)
		return
	}
	respondJSONOK(w, item)
}

// loadItemForUser returns the same enriched, permission-filtered item used by
// the standalone item endpoint. Keeping this orchestration below the HTTP
// layer lets aggregate endpoints reuse it without issuing internal requests.
func (h *ItemHandler) loadItemForUser(ctx context.Context, user *models.User, id int, trackView bool) (*models.Item, error) {
	// Get item with all details using service
	crudService := services.NewItemCRUDService(h.db)
	result, err := crudService.GetByIDWithWorkspaceStatus(id)
	if err != nil {
		return nil, err
	}
	item := result.Item

	// Check if user has permission to view this item. Active approvers without
	// workspace item.view are allowed through here so the approval inbox →
	// item navigation works; see CheckItemPermissionAsActor for the model.
	canView, err := h.canViewItemAsActor(ctx, user.ID, item.ID, item.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !canView {
		return nil, repository.ErrNotFound
	}

	// Check if workspace is inactive and user has permission to access it
	if !result.WorkspaceActive {
		canAccess, err := h.canAccessInactiveWorkspace(user, item.WorkspaceID)
		if err != nil {
			return nil, err
		}
		if !canAccess {
			return nil, repository.ErrNotFound
		}
	}

	// Get effective project from cache
	if h.itemCache != nil {
		effectiveProjectID, projectInheritanceMode, err := h.itemCache.GetEffectiveProjectForItem(id)
		if err == nil && effectiveProjectID != nil {
			item.EffectiveProjectID = effectiveProjectID
			item.ProjectInheritanceMode = projectInheritanceMode
			var epName sql.NullString
			if err := h.db.QueryRow("SELECT name FROM time_projects WHERE id = ?", *effectiveProjectID).Scan(&epName); err != nil && !errors.Is(err, sql.ErrNoRows) {
				slog.Warn("failed to load effective project name", slog.Int("project_id", *effectiveProjectID), slog.Any("error", err))
			}
			item.EffectiveProjectName = epName.String
		}
	}

	// Load labels for item
	singleItems := []models.Item{*item}
	if err := repository.NewLabelRepository(h.db).LoadForItems(singleItems); err != nil {
		slog.Warn("failed to load labels for item", slog.Any("error", err))
	}
	if err := LoadPersonalLabelsForItems(h.db, singleItems, user.ID); err != nil {
		slog.Warn("failed to load personal labels for item", slog.Any("error", err))
	}
	if err := repository.NewMilestoneAttachRepository(h.db).LoadForItems(singleItems); err != nil {
		slog.Warn("failed to load milestones for item", slog.Any("error", err))
	}
	*item = singleItems[0]

	// Track item view activity
	if trackView && h.activityTracker != nil {
		if err := h.activityTracker.TrackItemActivity(user.ID, item.ID, services.ActivityView); err != nil {
			slog.Warn("failed to track item view activity", slog.Int("user_id", user.ID), slog.Int("item_id", item.ID), slog.Any("error", err))
		}
	}

	// Strip names of time projects the viewer has no access to (incl. the
	// cross-workspace inherited effective project), keeping the IDs.
	masked := []models.Item{*item}
	h.maskInaccessibleProjectNames(user.ID, masked)
	*item = masked[0]

	return item, nil
}

// maxBatchItems caps how many item ids GetBatch accepts in one request,
// bounding the IN-clause size. The frontend chunks larger sets across multiple
// requests. Mirrors maxBatchLinkItems on the links batch endpoint.
const maxBatchItems = 500

// GetBatch returns full item-detail objects for a set of ids in a single
// request. It backs api.items.getMany(), which would otherwise fire one
// GET /items/{id} per id — a fan-out that under HTTP/2 grabbed a DB connection
// per item and could exhaust the pool during a collection delta refresh.
// Items the caller cannot view (or that no longer exist) are silently omitted;
// the consumer patches loaded rows by id and no-ops on the rest, so the
// 404-no-leak contract is preserved without per-id error signaling.
func (h *ItemHandler) GetBatch(w http.ResponseWriter, r *http.Request) {
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}

	ids := parseIDListParam(r.URL.Query().Get("ids"))
	if len(ids) == 0 {
		respondJSONOK(w, []models.Item{})
		return
	}
	if len(ids) > maxBatchItems {
		respondBadRequest(w, r, fmt.Sprintf("too many ids (max %d per request)", maxBatchItems))
		return
	}

	loaded, err := repository.NewItemRepository(h.db).FindByIDsWithDetails(ids)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}

	items := make([]models.Item, len(loaded))
	for i, it := range loaded {
		items[i] = *it
	}

	// Drop items the user can't view (workspace-memoized), matching GetAll.
	items, err = h.filterItemsByPermissions(user.ID, items)
	if err != nil {
		slog.Error("permission check failed", slog.Int("user_id", user.ID), slog.String("operation", "GetBatch"), slog.Any("error", err))
		respondInternalError(w, r, err)
		return
	}

	// Strip names of time projects the viewer can't access (keeps the IDs).
	h.maskInaccessibleProjectNames(user.ID, items)

	// Enrich with labels / personal labels (milestones are already attached by
	// the repository's batched loader).
	if err := repository.NewLabelRepository(h.db).LoadForItems(items); err != nil {
		slog.Warn("failed to load labels for batch items", slog.Any("error", err))
	}
	if err := LoadPersonalLabelsForItems(h.db, items, user.ID); err != nil {
		slog.Warn("failed to load personal labels for batch items", slog.Any("error", err))
	}

	respondJSONOK(w, items)
}

func (h *ItemHandler) Create(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeJSON[itemCreateRequest](w, r)
	if !ok {
		return
	}
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	canEdit, err := h.canEditItem(user.ID, input.WorkspaceID)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}
	if !canEdit {
		respondNotFound(w, r, "Item")
		return
	}

	result, err := h.itemCreation.Create(user.ID, user.Username, services.ItemCreateInput{
		WorkspaceID:       input.WorkspaceID,
		Title:             input.Title,
		Description:       input.Description,
		StatusID:          input.StatusID,
		PriorityID:        input.PriorityID,
		ItemTypeID:        input.ItemTypeID,
		DueDate:           input.DueDate,
		StartDate:         input.StartDate,
		EndDate:           input.EndDate,
		IsTask:            input.IsTask,
		IterationID:       input.IterationID,
		ProjectID:         input.ProjectID,
		InheritProject:    input.InheritProject,
		TimeProjectID:     input.TimeProjectID,
		AssigneeID:        input.AssigneeID,
		ParentID:          input.ParentID,
		RelatedWorkItemID: input.RelatedWorkItemID,
		StoryPoints:       input.StoryPoints,
		EstimateMinutes:   input.EstimateMinutes,
		CustomFieldValues: input.CustomFieldValues,
		MilestoneIDs:      input.MilestoneIDs,
	})
	if err != nil {
		var creationErr *services.ItemCreationValidationError
		var transitionErr *services.TransitionRejection
		var validationErr *validation.ValidationError
		if errors.Is(err, services.ErrMissingItemType) ||
			errors.Is(err, services.ErrInvalidItemType) ||
			errors.Is(err, services.ErrProjectNotFound) ||
			errors.As(err, &creationErr) ||
			errors.As(err, &transitionErr) ||
			errors.As(err, &validationErr) {
			respondValidationError(w, r, err.Error())
			return
		}
		respondInternalError(w, r, err)
		return
	}

	// Preserve the nil-coordinator compatibility path used by lightweight
	// embedders. Production and REST v1 share the configured coordinator
	// through ItemCreationService and therefore emit exactly once there.
	if h.eventCoordinator == nil {
		h.emitItemCreatedFallback(result.Item, user)
	}

	maskedCreated := []models.Item{*result.Item}
	h.maskInaccessibleProjectNames(user.ID, maskedCreated)
	respondJSONCreated(w, maskedCreated[0])
}

func (h *ItemHandler) emitItemCreatedFallback(item *models.Item, user *models.User) {
	if h.notificationService != nil {
		itemKey := fmt.Sprintf("%s-%d", item.WorkspaceKey, item.WorkspaceItemNumber)
		h.notificationService.EmitEvent(&services.NotificationEvent{
			EventType:   models.EventItemCreated,
			WorkspaceID: item.WorkspaceID,
			ActorUserID: user.ID,
			ItemID:      item.ID,
			AssigneeID:  item.AssigneeID,
			CreatorID:   &user.ID,
			Title:       "New Item Created",
			TemplateData: map[string]any{
				"item.title":     item.Title,
				"item.key":       itemKey,
				"item.id":        item.ID,
				"user.name":      user.Username,
				"workspace.name": item.WorkspaceName,
				"workspace.key":  item.WorkspaceKey,
			},
		})
	}
	if h.actionService != nil {
		h.actionService.EmitActionEvent(&models.ActionEvent{
			EventType:   models.ActionTriggerItemCreated,
			WorkspaceID: item.WorkspaceID,
			ItemID:      item.ID,
			ActorUserID: user.ID,
			NewValues: map[string]any{
				"title":        item.Title,
				"status_id":    item.StatusID,
				"item_type_id": item.ItemTypeID,
				"assignee_id":  item.AssigneeID,
				"creator_id":   item.CreatorID,
				"priority_id":  item.PriorityID,
			},
		})
	}
	if h.webhookSender != nil {
		h.webhookSender.DispatchEvent("item.created", item)
	}
}

// itemUpdateValidationMessage preserves the legacy transport wording for the
// workflow-protected fields. The shared update pipeline rejects both for every
// transport with a field-scoped ValidationError; only the transport-specific
// pointer to the transition/change-type endpoints differs.
func itemUpdateValidationMessage(valErr *validation.ValidationError) string {
	switch valErr.Field {
	case "status_id":
		return "status_id may not be set via item update; use POST /items/{id}/transition"
	case "item_type_id":
		return "item_type_id may not be set via item update; use POST /items/{id}/change-type"
	default:
		return valErr.Error()
	}
}

func (h *ItemHandler) Update(w http.ResponseWriter, r *http.Request) {
	// Parse request and validate item ID
	id, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}

	// Parse update data from request body
	var updateData map[string]any
	if err := newJSONDecoder(w, r).Decode(&updateData); err != nil {
		respondValidationError(w, r, err.Error())
		return
	}

	// Require authentication
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}

	// The application service loads the item and enforces workspace edit
	// permission, so this handler owns no persistence or authorization path.
	canEdit, err := h.itemUpdate.CanUserEditItem(user.ID, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			respondNotFound(w, r, "item")
			return
		}
		respondInternalError(w, r, err)
		return
	}
	if !canEdit {
		respondNotFound(w, r, "Item")
		return
	}

	// Run the shared user-facing update pipeline. REST v1 uses this same
	// service instance so committed-item events, mentions, activity, and cache
	// invalidation do not depend on the transport. The pipeline also rejects
	// status_id and item_type_id so workflow and condition rules are always
	// enforced front of the dedicated transition/change-type flows.
	result, err := h.itemUpdate.Update(user.ID, user.Username, id, updateData)

	if err != nil {
		// Check if it's a validation error (anywhere in the wrap chain — the
		// update service wraps with `fmt.Errorf("validation failed: %w", err)`
		// so a bare type assertion would miss wrapped ValidationErrors and
		// surface them as 500s. Specifically affects parent_id moves between
		// hierarchy levels.)
		var valErr *validation.ValidationError
		if errors.As(err, &valErr) {
			respondValidationError(w, r, itemUpdateValidationMessage(valErr))
			return
		}
		// Generic error
		respondInternalError(w, r, err)
		return
	}

	// The application service returns the committed item for the response.
	updatedItem := result.Item

	w.Header().Set("Content-Type", "application/json")

	// Push status change to GitHub if issue sync is configured
	if h.issueSyncService != nil && result.StatusChanged && updatedItem.StatusID != nil {
		go func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			h.issueSyncService.PushStatusToGitHub(ctx, updatedItem.ID, *updatedItem.StatusID)
		}(r.Context())
	}

	// Strip names of time projects the editor has no access to (incl. the
	// inherited effective project), matching the masked read paths. Mask a
	// copy so async consumers of updatedItem aren't mutated.
	maskedUpdated := []models.Item{*updatedItem}
	h.maskInaccessibleProjectNames(user.ID, maskedUpdated)

	respondJSONOK(w, maskedUpdated[0])
}

// maskInaccessibleProjectNames blanks the human-readable project name fields on
// items whose project / time-project / effective-project the user is not allowed
// to view. See TimePermissionService.MaskInaccessibleProjectNames.
func (h *ItemHandler) maskInaccessibleProjectNames(userID int, items []models.Item) {
	services.NewTimePermissionService(h.db, h.permissionService).MaskInaccessibleProjectNames(userID, items)
}

func (h *ItemHandler) maskInaccessibleProjectNamesContext(ctx context.Context, userID int, items []models.Item) {
	services.NewTimePermissionService(h.db, h.permissionService).MaskInaccessibleProjectNamesContext(ctx, userID, items)
}

// invalidateEffectiveProjectSubtree drops the cached hierarchy entry for an item
// and all descendants. Bulk updates use this handler-level helper; single-item
// user-facing updates run the same invalidation inside their application service.
func (h *ItemHandler) invalidateEffectiveProjectSubtree(itemID int) {
	_ = h.itemCache.InvalidateItemHierarchy(itemID, nil)
	if h.hierarchyService == nil {
		return
	}
	descendants, err := h.hierarchyService.GetDescendants(itemID, 0)
	if err != nil {
		slog.Warn("failed to load descendants for cache invalidation", slog.Int("item_id", itemID), slog.Any("error", err))
		return
	}
	for i := range descendants {
		_ = h.itemCache.InvalidateItemHierarchy(descendants[i].ID, nil)
	}
}

func (h *ItemHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}

	result, err := h.itemDeletion.Delete(services.ItemDeletionRequest{
		ItemID:        id,
		ActorUserID:   user.ID,
		ActorUsername: user.Username,
		Mode:          services.ItemDeletionSingle,
	})
	if h.respondItemDeletionError(w, r, err) {
		return
	}

	logAuditWithDetails(h.db, r, user, logger.ActionItemDelete, logger.ResourceItem, &id, result.Item.Title, map[string]any{
		"workspace_id": result.Item.WorkspaceID,
		"item_type_id": result.Item.ItemTypeID,
		"parent_id":    result.Item.ParentID,
		"status_id":    result.Item.StatusID,
		"assignee_id":  result.Item.AssigneeID,
		"creator_id":   result.Item.CreatorID,
	})
	if h.eventCoordinator == nil {
		h.emitItemDeletedFallback(result.Item, user, result.DescendantCount)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *ItemHandler) respondItemDeletionError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, repository.ErrNotFound) || errors.Is(err, services.ErrItemDeletionForbidden) {
		respondNotFound(w, r, "item")
		return true
	}
	respondInternalError(w, r, err)
	return true
}

func (h *ItemHandler) emitItemDeletedFallback(item *models.Item, user *models.User, descendantCount int) {
	if h.notificationService != nil {
		h.notificationService.EmitEvent(&services.NotificationEvent{
			EventType:   models.EventItemDeleted,
			WorkspaceID: item.WorkspaceID,
			ActorUserID: user.ID,
			ItemID:      item.ID,
			AssigneeID:  item.AssigneeID,
			CreatorID:   item.CreatorID,
			Title:       "Item Deleted",
			TemplateData: map[string]any{
				"item.title":  item.Title,
				"item.id":     item.ID,
				"user.name":   user.Username,
				"descendants": descendantCount,
			},
		})
	}
	if h.webhookSender != nil {
		h.webhookSender.DispatchEvent("item.deleted", item)
	}
}

// GetDeleteInfo returns information needed before deleting an item (descendant count, parent info)
func (h *ItemHandler) GetDeleteInfo(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}

	// Require authentication
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}

	repo := repository.NewItemRepository(h.db)
	item, err := repo.FindByID(id)
	if err != nil {
		if err == repository.ErrNotFound {
			respondNotFound(w, r, "item")
			return
		}
		respondInternalError(w, r, err)
		return
	}

	// Check permission - need at least view access
	canEdit, err := h.canEditItem(user.ID, item.WorkspaceID)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}
	if !canEdit {
		respondNotFound(w, r, "Item")
		return
	}

	// Get descendant IDs
	descendantIDs, err := repo.GetDescendantIDs(id)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}

	// Get hierarchy level for the item type (needed for filtering reparent candidates)
	var hierarchyLevel sql.NullInt64
	if item.ItemTypeID != nil {
		if err := h.db.QueryRow("SELECT hierarchy_level FROM item_types WHERE id = ?", *item.ItemTypeID).Scan(&hierarchyLevel); err != nil && !errors.Is(err, sql.ErrNoRows) {
			respondInternalError(w, r, err)
			return
		}
	}

	response := map[string]any{
		"hasChildren":     len(descendantIDs) > 0,
		"descendantCount": len(descendantIDs),
		"parentId":        item.ParentID,
		"title":           item.Title,
		"itemTypeId":      item.ItemTypeID,
		"workspaceId":     item.WorkspaceID,
		"hierarchyLevel":  utils.NullInt64ToPtr(hierarchyLevel),
	}

	respondJSONOK(w, response)
}

// ReparentChildren moves all direct children of an item to a new parent
func (h *ItemHandler) ReparentChildren(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}

	// Require authentication
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}

	var req struct {
		NewParentID *int `json:"newParentId"`
	}
	if err := newJSONDecoder(w, r).Decode(&req); err != nil {
		respondValidationError(w, r, "Invalid request body: "+err.Error())
		return
	}

	repo := repository.NewItemRepository(h.db)
	item, err := repo.FindByID(id)
	if err != nil {
		if err == repository.ErrNotFound {
			respondNotFound(w, r, "item")
			return
		}
		respondInternalError(w, r, err)
		return
	}

	// Check permission
	canEdit, err := h.canEditItem(user.ID, item.WorkspaceID)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}
	if !canEdit {
		respondNotFound(w, r, "Item")
		return
	}

	// If new parent is specified, verify it exists and is in the same workspace
	if req.NewParentID != nil {
		var newParent *models.Item
		newParent, err = repo.FindByID(*req.NewParentID)
		if err != nil {
			if err == repository.ErrNotFound {
				respondNotFound(w, r, "item")
				return
			}
			respondInternalError(w, r, err)
			return
		}
		if newParent.WorkspaceID != item.WorkspaceID {
			respondValidationError(w, r, "New parent must be in the same workspace")
			return
		}
	}

	// Get direct children
	children, err := repo.GetChildren(id)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}

	if len(children) == 0 {
		respondJSONOK(w, map[string]any{"reparentedCount": 0})
		return
	}

	// Start transaction
	tx, err := h.db.Begin()
	if err != nil {
		respondInternalError(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Cycle check inside the transaction so concurrent reparents cannot race
	// past each other's individual checks. WouldCreateCycleTx locks the rows
	// it walks (FOR UPDATE on Postgres); combined with UpdateParent below in
	// the same tx, the check and the write are atomic.
	if req.NewParentID != nil {
		wouldCycle, cycleErr := h.hierarchyService.WouldCreateCycleTx(tx, id, *req.NewParentID)
		if cycleErr != nil {
			respondInternalError(w, r, cycleErr)
			return
		}
		if wouldCycle {
			respondValidationError(w, r, "Reparenting would create a hierarchy cycle")
			return
		}
	}

	// Update parent_id for all direct children
	for _, child := range children {
		if child.ItemTypeID != nil {
			if err := validation.ValidateParentForItemType(tx, *child.ItemTypeID, req.NewParentID); err != nil {
				var validationErr *validation.ValidationError
				if errors.As(err, &validationErr) {
					respondValidationError(w, r, validationErr.Error())
					return
				}
				respondInternalError(w, r, err)
				return
			}
		}
		if err := repo.UpdateParent(tx, child.ID, req.NewParentID); err != nil {
			respondInternalError(w, r, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		respondInternalError(w, r, err)
		return
	}

	// Invalidate caches for reparented children
	if h.itemCache != nil {
		for _, child := range children {
			_ = h.itemCache.InvalidateItemHierarchy(child.ID, nil)
		}
	}

	respondJSONOK(w, map[string]any{"reparentedCount": len(children)})
}

// DeleteCascade deletes an item and all its descendants
func (h *ItemHandler) DeleteCascade(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}

	result, err := h.itemDeletion.Delete(services.ItemDeletionRequest{
		ItemID:        id,
		ActorUserID:   user.ID,
		ActorUsername: user.Username,
		Mode:          services.ItemDeletionCascade,
	})
	if h.respondItemDeletionError(w, r, err) {
		return
	}

	logAuditWithDetails(h.db, r, user, logger.ActionItemDeleteCascade, logger.ResourceItem, &id, result.Item.Title, map[string]any{
		"workspace_id":     result.Item.WorkspaceID,
		"item_type_id":     result.Item.ItemTypeID,
		"parent_id":        result.Item.ParentID,
		"status_id":        result.Item.StatusID,
		"assignee_id":      result.Item.AssigneeID,
		"creator_id":       result.Item.CreatorID,
		"deleted_count":    result.DeletedCount,
		"descendant_count": result.DescendantCount,
	})
	if h.eventCoordinator == nil {
		h.emitItemDeletedFallback(result.Item, user, result.DescendantCount)
	}

	respondJSONOK(w, map[string]any{
		"deletedCount": result.DeletedCount,
	})
}

func (h *ItemHandler) Copy(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}

	// Require authentication
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}

	// Get the original item using repository
	repo := repository.NewItemRepository(h.db)
	originalItem, err := repo.FindByID(id)
	if err != nil {
		if err == repository.ErrNotFound {
			respondNotFound(w, r, "item")
		} else {
			respondInternalError(w, r, err)
		}
		return
	}

	// Check permission
	canEdit, err := h.canEditItem(user.ID, originalItem.WorkspaceID)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}
	if !canEdit {
		respondNotFound(w, r, "Item")
		return
	}

	// Create copy title
	copyTitle := sanitize.PlainTextField.Sanitize(fmt.Sprintf("COPY - %s", originalItem.Title))

	result, err := services.NewItemCRUDService(h.db).Copy(id, services.CopyOptions{
		NewTitle:  copyTitle,
		CreatorID: user.ID,
	})
	if err != nil {
		respondInternalError(w, r, err)
		return
	}

	newItem, err := repo.FindByID(result.NewItemID)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}
	respondJSONOK(w, newItem)
}

// GetCacheStats returns cache performance statistics
// GET /api/items/cache-stats
func (h *ItemHandler) GetCacheStats(w http.ResponseWriter, r *http.Request) {
	if h.itemCache == nil {
		respondError(w, r, &restapi.APIError{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "SERVICE_UNAVAILABLE",
			Message:    "Item cache is not enabled",
		})
		return
	}

	stats := h.itemCache.GetStats()

	respondJSONOK(w, map[string]any{
		"cache_enabled": true,
		"statistics":    stats,
		"timestamp":     time.Now().Format(time.RFC3339),
	})
}
