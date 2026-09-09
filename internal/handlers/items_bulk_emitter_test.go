package handlers

import (
	"path/filepath"
	"testing"

	"windshift/internal/database"
	"windshift/internal/models"
	"windshift/internal/services"
)

func newItemHandlerForTest(t *testing.T) *ItemHandler {
	t.Helper()
	db, err := database.NewSQLiteDB("file:" + filepath.Join(t.TempDir(), "handlers-test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Initialize(); err != nil {
		t.Fatalf("initialize schema: %v", err)
	}
	permissionService, err := services.NewPermissionService(db, services.DefaultPermissionCacheConfig())
	if err != nil {
		t.Fatalf("permission service: %v", err)
	}
	return NewItemHandler(db, permissionService, nil, nil)
}

// The cookie-auth surface must fan out through the very emitter it hands other
// surfaces (restapi.Deps.BulkUpdateEmitter -> REST v1). A handler that emitted
// through a private copy of the logic is how the v1 route came to emit nothing
// at all (INFRA-295).
func TestBulkUpdateResultsGoThroughTheSharedEmitter(t *testing.T) {
	h := newItemHandlerForTest(t)

	if h.BulkUpdateEmitter() == nil {
		t.Fatal("BulkUpdateEmitter() is nil: other surfaces would silently emit nothing")
	}
	if h.BulkUpdateEmitter() != h.bulkEmitter {
		t.Fatal("BulkUpdateEmitter() must expose the emitter this handler emits through")
	}

	var emitted []int
	h.bulkEmitter = services.NewBulkUpdateEmitter(services.BulkUpdateSideEffects{
		EmitItemUpdated: func(_, updated *models.Item, _, _ bool, _ int, _ []services.HistoryEntry, _ string) {
			emitted = append(emitted, updated.ID)
		},
	})

	items := h.emitBulkUpdateResults(1, "tester", []services.UpdateItemResult{{
		OriginalItem: &models.Item{ID: 42, WorkspaceID: 1},
		Item:         &models.Item{ID: 42, WorkspaceID: 1},
	}})

	if len(emitted) != 1 || emitted[0] != 42 {
		t.Fatalf("session-route emission recorded %v, want [42]", emitted)
	}
	if len(items) != 1 || items[0].ID != 42 {
		t.Fatalf("returned items = %v, want the updated item", items)
	}
}
