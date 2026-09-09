package services

import (
	"log/slog"

	"windshift/internal/models"
)

// BulkUpdateSideEffects is the collaborator set one bulk result fans out to.
// Every hook is optional; the emitter skips a nil one.
//
// They are funcs rather than concrete services because the two callers resolve
// the same collaborators differently — the cookie-auth ItemHandler owns them as
// fields wired by setters after construction, so a hook that reads those fields
// at emit time cannot go stale, and REST v1 must not import that package at all
// (scripts/check-layering.sh).
type BulkUpdateSideEffects struct {
	// TrackEdit records an edit activity for the actor on one item.
	TrackEdit func(userID, itemID int) error
	// InvalidateProjectSubtree drops cached hierarchy entries for an item and
	// its descendants; called only when the update changed project resolution.
	InvalidateProjectSubtree func(itemID int)
	// EmitItemUpdated is the item.updated fan-out: webhooks, notifications and
	// automation events.
	EmitItemUpdated func(original, updated *models.Item, statusChanged, assigneeChanged bool, userID int, fieldChanges []HistoryEntry, username string)
	// ProcessMentions handles @mentions in a changed description.
	ProcessMentions func(params ProcessMentionsParams) error
	// MaskProjectNames blanks project names the actor may not see, in place.
	MaskProjectNames func(userID int, items []models.Item)
}

// BulkUpdateEmitter turns the results of a bulk item mutation into the side
// effects a single item edit produces. It is shared by the cookie-auth bulk
// update / iteration completion handlers and the REST v1 iteration completion
// route so the two surfaces cannot drift apart (INFRA-295): before it, the v1
// route persisted the completion but emitted nothing, so a CLI-driven
// completion fired no webhooks, notifications or mentions.
type BulkUpdateEmitter struct {
	hooks BulkUpdateSideEffects
}

func NewBulkUpdateEmitter(hooks BulkUpdateSideEffects) *BulkUpdateEmitter {
	return &BulkUpdateEmitter{hooks: hooks}
}

// Emit fans every usable result out and returns the updated items with
// inaccessible project names masked, in result order.
//
// A nil emitter emits nothing and still returns the items, which is what an
// embedder that has not wired the side effects gets.
func (e *BulkUpdateEmitter) Emit(userID int, username string, results []UpdateItemResult) []*models.Item {
	var hooks BulkUpdateSideEffects
	if e != nil {
		hooks = e.hooks
	}

	items := make([]*models.Item, 0, len(results))
	for i := range results {
		result := &results[i]
		if result.OriginalItem == nil || result.Item == nil {
			continue
		}
		original, updated := result.OriginalItem, result.Item
		if hooks.TrackEdit != nil {
			if err := hooks.TrackEdit(userID, updated.ID); err != nil {
				slog.Warn("failed to track bulk item edit activity", "item_id", updated.ID, "error", err)
			}
		}
		if hooks.InvalidateProjectSubtree != nil && projectResolutionChanged(original, updated) {
			hooks.InvalidateProjectSubtree(updated.ID)
		}
		if hooks.EmitItemUpdated != nil {
			assigneeChanged := !intPointerEqual(original.AssigneeID, updated.AssigneeID)
			hooks.EmitItemUpdated(original, updated, result.StatusChanged, assigneeChanged, userID, result.FieldChanges, username)
		}
		if hooks.ProcessMentions != nil && original.Description != updated.Description {
			if err := hooks.ProcessMentions(ProcessMentionsParams{
				SourceType: "item_description", SourceID: updated.ID, Content: updated.Description,
				ItemID: updated.ID, WorkspaceID: updated.WorkspaceID, ActorUserID: userID,
			}); err != nil {
				slog.Warn("failed to process bulk item description mentions", "item_id", updated.ID, "error", err)
			}
		}
		items = append(items, updated)
	}

	masked := make([]models.Item, len(items))
	for i, item := range items {
		masked[i] = *item
	}
	if hooks.MaskProjectNames != nil {
		hooks.MaskProjectNames(userID, masked)
	}
	out := make([]*models.Item, len(masked))
	for i := range masked {
		out[i] = &masked[i]
	}
	return out
}

// projectResolutionChanged reports whether an update touched a field that can
// change an item's (or its descendants') effective project.
func projectResolutionChanged(original, updated *models.Item) bool {
	return original.InheritProject != updated.InheritProject ||
		!intPointerEqual(original.ProjectID, updated.ProjectID) ||
		!intPointerEqual(original.ParentID, updated.ParentID)
}

func intPointerEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
