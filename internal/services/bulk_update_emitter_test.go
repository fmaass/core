package services

import (
	"testing"

	"windshift/internal/models"
)

func itemPair(id int, mutate func(updated *models.Item)) UpdateItemResult {
	original := &models.Item{ID: id, WorkspaceID: 1, Description: "before"}
	updated := &models.Item{ID: id, WorkspaceID: 1, Description: "before"}
	if mutate != nil {
		mutate(updated)
	}
	return UpdateItemResult{OriginalItem: original, Item: updated}
}

func TestEmitFansOutEveryUsableResult(t *testing.T) {
	var edits, updates []int
	e := NewBulkUpdateEmitter(BulkUpdateSideEffects{
		TrackEdit:       func(_, itemID int) error { edits = append(edits, itemID); return nil },
		EmitItemUpdated: func(_, u *models.Item, _, _ bool, _ int, _ []HistoryEntry, _ string) { updates = append(updates, u.ID) },
	})

	items := e.Emit(7, "tester", []UpdateItemResult{itemPair(1, nil), itemPair(2, nil)})

	if len(items) != 2 || items[0].ID != 1 || items[1].ID != 2 {
		t.Fatalf("returned items = %v, want ids 1,2", items)
	}
	if len(edits) != 2 || len(updates) != 2 {
		t.Fatalf("edits=%v updates=%v, want two of each", edits, updates)
	}
}

// A result the mutation could not produce a before/after pair for is skipped
// rather than emitted with a nil item.
func TestEmitSkipsIncompleteResults(t *testing.T) {
	var updates []int
	e := NewBulkUpdateEmitter(BulkUpdateSideEffects{
		EmitItemUpdated: func(_, u *models.Item, _, _ bool, _ int, _ []HistoryEntry, _ string) { updates = append(updates, u.ID) },
	})

	items := e.Emit(7, "tester", []UpdateItemResult{
		{OriginalItem: nil, Item: &models.Item{ID: 9}},
		{OriginalItem: &models.Item{ID: 9}, Item: nil},
		itemPair(3, nil),
	})

	if len(items) != 1 || items[0].ID != 3 {
		t.Fatalf("returned items = %v, want only id 3", items)
	}
	if len(updates) != 1 {
		t.Fatalf("emitted %v, want one", updates)
	}
}

// Mentions are reprocessed only when the description actually changed;
// project-cache invalidation only when project resolution changed.
func TestEmitAppliesConditionalHooks(t *testing.T) {
	var mentioned, invalidated []int
	hooks := BulkUpdateSideEffects{
		ProcessMentions:          func(p ProcessMentionsParams) error { mentioned = append(mentioned, p.ItemID); return nil },
		InvalidateProjectSubtree: func(itemID int) { invalidated = append(invalidated, itemID) },
	}
	project := 4
	results := []UpdateItemResult{
		itemPair(1, nil),
		itemPair(2, func(u *models.Item) { u.Description = "after" }),
		itemPair(3, func(u *models.Item) { u.ProjectID = &project }),
	}

	NewBulkUpdateEmitter(hooks).Emit(7, "tester", results)

	if len(mentioned) != 1 || mentioned[0] != 2 {
		t.Errorf("mentions processed for %v, want [2]", mentioned)
	}
	if len(invalidated) != 1 || invalidated[0] != 3 {
		t.Errorf("project subtree invalidated for %v, want [3]", invalidated)
	}
}

// The returned items are copies handed to the masking hook, so a masked
// project name reaches the caller and the stored item is left alone.
func TestEmitReturnsMaskedCopies(t *testing.T) {
	stored := itemPair(1, func(u *models.Item) { u.ProjectName = "Secret" })
	e := NewBulkUpdateEmitter(BulkUpdateSideEffects{
		MaskProjectNames: func(_ int, items []models.Item) {
			for i := range items {
				items[i].ProjectName = ""
			}
		},
	})

	items := e.Emit(7, "tester", []UpdateItemResult{stored})

	if len(items) != 1 || items[0].ProjectName != "" {
		t.Fatalf("returned item project name = %q, want masked", items[0].ProjectName)
	}
	if stored.Item.ProjectName != "Secret" {
		t.Errorf("masking must not mutate the stored item, got %q", stored.Item.ProjectName)
	}
}

// An unwired surface still gets its items back — that is the fallback the v1
// router relies on when restapi.Deps carries no emitter.
func TestNilEmitterReturnsItemsWithoutSideEffects(t *testing.T) {
	var e *BulkUpdateEmitter
	items := e.Emit(7, "tester", []UpdateItemResult{itemPair(1, nil)})
	if len(items) != 1 || items[0].ID != 1 {
		t.Fatalf("nil emitter returned %v, want the item", items)
	}
}
