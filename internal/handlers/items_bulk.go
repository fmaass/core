package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"windshift/internal/logger"
	"windshift/internal/models"
	"windshift/internal/services"
)

type bulkUpdateItemsRequest struct {
	ItemIDs []int          `json:"item_ids"`
	Set     map[string]any `json:"set"`
}

type completeIterationRequest struct {
	MoveIncompleteToIterationID *int `json:"move_incomplete_to_iteration_id"`
}

// BulkUpdate applies one flexible field patch atomically to up to 500 items.
// The service rejects workflow/status/type/workspace/order fields that require
// dedicated mutation semantics.
func (h *ItemHandler) BulkUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	req, ok := decodeJSON[bulkUpdateItemsRequest](w, r)
	if !ok {
		return
	}
	ctx, cancel := h.requestDBContext(r)
	defer cancel()
	started := time.Now()

	if h.bulkUpdate == nil {
		h.bulkUpdate = services.NewItemUpdateService(h.db).WithPermissionService(h.permissionService)
	}
	result, err := h.bulkUpdate.BulkUpdateItems(ctx, services.BulkUpdateItemsRequest{
		ItemIDs: req.ItemIDs,
		Fields:  req.Set,
		UserID:  user.ID,
		AuthorizeWorkspace: func(workspaceID int) (bool, error) {
			return h.canEditItem(user.ID, workspaceID)
		},
	})
	if err != nil {
		h.observeBulkOperation("item_bulk_update", len(req.ItemIDs), 0, 0, 0, time.Since(started), true)
		h.respondBulkMutationError(w, r, err, "Item")
		return
	}

	items := h.emitBulkUpdateResults(user.ID, user.Username, result.Results)
	h.observeBulkOperation("item_bulk_update", result.RequestedCount, result.UpdatedCount, result.SQLStatements, len(result.Results), result.Duration, false)
	slog.Info("bulk item update completed",
		"component", "bulk_item_update",
		"requested_count", result.RequestedCount,
		"updated_count", result.UpdatedCount,
		"unchanged_count", result.UnchangedCount,
		"sql_statements", result.SQLStatements,
		"duration_ms", result.Duration.Milliseconds(),
	)
	respondJSONOK(w, map[string]any{
		"atomic":          true,
		"requested_count": result.RequestedCount,
		"updated_count":   result.UpdatedCount,
		"unchanged_count": result.UnchangedCount,
		"items":           items,
	})
}

// CompleteIteration moves incomplete items and completes the iteration in one
// transaction, then emits the same item-update side effects as normal edits.
func (h *ItemHandler) CompleteIteration(w http.ResponseWriter, r *http.Request) {
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	iterationID, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}
	req, ok := decodeOptionalJSON[completeIterationRequest](w, r)
	if !ok {
		return
	}
	ctx, cancel := h.requestDBContext(r)
	defer cancel()
	started := time.Now()

	if h.iterationComplete == nil {
		h.iterationComplete = services.NewIterationCompletionService(h.db)
	}
	result, err := h.iterationComplete.Complete(ctx, services.CompleteIterationRequest{
		IterationID:       iterationID,
		TargetIterationID: req.MoveIncompleteToIterationID,
		UserID:            user.ID,
		AuthorizeWorkspace: func(workspaceID int) (bool, error) {
			return h.canEditItem(user.ID, workspaceID)
		},
		AuthorizeGlobal: func() (bool, error) {
			if h.permissionService == nil {
				return false, nil
			}
			return h.permissionService.HasGlobalPermission(user.ID, models.PermissionIterationManage)
		},
	})
	if err != nil {
		h.observeBulkOperation("iteration_completion", 0, 0, 0, 0, time.Since(started), true)
		h.respondBulkMutationError(w, r, err, "Iteration")
		return
	}

	result.Items = h.emitBulkUpdateResults(user.ID, user.Username, result.Updates)
	h.observeBulkOperation("iteration_completion", result.MovedCount, result.MovedCount, result.SQLStatements, len(result.Updates), time.Since(started), false)
	logAuditWithDetails(h.db, r, user, logger.ActionIterationUpdate, logger.ResourceIteration, &iterationID, "", map[string]any{
		"operation":           "complete",
		"moved_count":         result.MovedCount,
		"target_iteration_id": result.TargetIterationID,
		"already_completed":   result.AlreadyCompleted,
	})
	slog.Info("iteration completion bulk operation completed",
		"component", "iteration_completion",
		"iteration_id", iterationID,
		"moved_count", result.MovedCount,
		"already_completed", result.AlreadyCompleted,
		"sql_statements", result.SQLStatements,
		"duration_ms", result.DurationMS,
	)
	respondJSONOK(w, result)
}

func (h *ItemHandler) observeBulkOperation(kind string, requested, changed, sqlStatements, sideEffects int, duration time.Duration, failed bool) {
	if h.bulkMetrics == nil {
		return
	}
	h.bulkMetrics.Observe(services.BulkOperationObservation{
		Kind: kind, RequestedItems: requested, ChangedItems: changed,
		SQLStatements: sqlStatements, SideEffectsEmitted: sideEffects,
		PoolInUse: h.db.GetDB().Stats().InUse, Duration: duration, Failed: failed,
	})
}

func (h *ItemHandler) respondBulkMutationError(w http.ResponseWriter, r *http.Request, err error, resource string) {
	switch {
	case errors.Is(err, services.ErrBulkItemNotFound), errors.Is(err, services.ErrIterationCompletionNotFound):
		respondNotFound(w, r, resource)
	case errors.Is(err, services.ErrBulkItemForbidden), errors.Is(err, services.ErrIterationCompletionForbidden):
		respondForbidden(w, r)
	case errors.Is(err, services.ErrBulkItemLimit), errors.Is(err, services.ErrIterationCompletionLimit):
		respondBadRequest(w, r, "operation exceeds the 500-item limit")
	case errors.Is(err, services.ErrBulkFieldsRequired), services.IsBulkItemFieldError(err), services.IsBulkItemValidationError(err):
		respondValidationError(w, r, err.Error())
	case errors.Is(err, services.ErrIterationCompletionConflict):
		respondConflict(w, r, err.Error())
	default:
		h.respondItemReadError(w, r, err)
	}
}

// emitBulkUpdateResults hands the results to the shared emitter both this
// surface and REST v1 use, so the two cannot drift apart (INFRA-295).
func (h *ItemHandler) emitBulkUpdateResults(userID int, username string, results []services.UpdateItemResult) []*models.Item {
	return h.bulkEmitter.Emit(userID, username, results)
}
