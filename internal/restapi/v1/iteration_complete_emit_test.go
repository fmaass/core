package v1_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"windshift/internal/models"
	"windshift/internal/restapi"
	"windshift/internal/services"
)

// emissionRecorder stands in for the leaf collaborators the shared emitter
// fans out to (activity tracker, event coordinator, mention service, project
// masking). The emitter itself is the real one — this records what it did.
type emissionRecorder struct {
	mu        sync.Mutex
	edits     []int
	updated   []int
	mentions  []int
	maskedFor []int
}

func (r *emissionRecorder) sideEffects() services.BulkUpdateSideEffects {
	return services.BulkUpdateSideEffects{
		TrackEdit: func(_, itemID int) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.edits = append(r.edits, itemID)
			return nil
		},
		EmitItemUpdated: func(_, updated *models.Item, _, _ bool, _ int, _ []services.HistoryEntry, _ string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.updated = append(r.updated, updated.ID)
		},
		ProcessMentions: func(params services.ProcessMentionsParams) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.mentions = append(r.mentions, params.ItemID)
			return nil
		},
		MaskProjectNames: func(userID int, _ []models.Item) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.maskedFor = append(r.maskedFor, userID)
		},
	}
}

// TestCompleteIterationEmitsSideEffects is the INFRA-295 regression. The v1
// route reused the completion service but not the emitter the cookie-auth
// route calls, so a CLI-driven completion moved the items and fired no
// webhook, notification or mention at all. Removing the Emit call from
// IterationHandler.Complete makes this test fail.
func TestCompleteIterationEmitsSideEffects(t *testing.T) {
	rec := &emissionRecorder{}
	h := newHarness(t, func(d *restapi.Deps) {
		d.BulkUpdateEmitter = services.NewBulkUpdateEmitter(rec.sideEffects())
	})
	source := h.seedIteration(t, "WP-03", "active")
	target := h.seedIteration(t, "WP-04", "active")
	itemID := h.seedIncompleteItem(t, source, "carried-forward")

	body := fmt.Sprintf(`{"move_incomplete_to_iteration_id": %d}`, target)
	resp := h.do(t, http.MethodPost, fmt.Sprintf("/rest/api/v1/iterations/%d/complete", source), body, h.token)
	if resp.Code != http.StatusOK {
		t.Fatalf("completion must be 200, got %d body=%q", resp.Code, resp.Body.String())
	}

	if len(rec.updated) != 1 || rec.updated[0] != itemID {
		t.Fatalf("v1 completion emitted item.updated for %v, want exactly [%d] (INFRA-295: the v1 route emitted nothing)", rec.updated, itemID)
	}
	if len(rec.edits) != 1 || rec.edits[0] != itemID {
		t.Errorf("edit activity tracked for %v, want [%d]", rec.edits, itemID)
	}
	if len(rec.maskedFor) != 1 {
		t.Errorf("project-name masking ran %d times, want 1", len(rec.maskedFor))
	}
	// The description did not change, so no mention pass is due — the emitter
	// must not manufacture one.
	if len(rec.mentions) != 0 {
		t.Errorf("mention processing ran for %v, want none (description unchanged)", rec.mentions)
	}
}

// A completion that moves nothing must emit nothing: the emitter is driven by
// the service's per-item updates, not by the request.
func TestCompleteEmptyIterationEmitsNothing(t *testing.T) {
	rec := &emissionRecorder{}
	h := newHarness(t, func(d *restapi.Deps) {
		d.BulkUpdateEmitter = services.NewBulkUpdateEmitter(rec.sideEffects())
	})
	source := h.seedIteration(t, "WP-03", "active")

	resp := h.do(t, http.MethodPost, fmt.Sprintf("/rest/api/v1/iterations/%d/complete", source), `{}`, h.token)
	if resp.Code != http.StatusOK {
		t.Fatalf("completion must be 200, got %d body=%q", resp.Code, resp.Body.String())
	}
	if len(rec.updated) != 0 {
		t.Errorf("emitted item.updated for %v, want none", rec.updated)
	}
}

// An embedder that wires no emitter must still get a working completion — the
// pre-INFRA-295 behaviour, not a panic.
func TestCompleteIterationWithoutEmitterStillCompletes(t *testing.T) {
	h := newHarness(t)
	source := h.seedIteration(t, "WP-03", "active")
	itemID := h.seedIncompleteItem(t, source, "to-backlog")

	resp := h.do(t, http.MethodPost, fmt.Sprintf("/rest/api/v1/iterations/%d/complete", source), `{}`, h.token)
	if resp.Code != http.StatusOK {
		t.Fatalf("completion must be 200 with no emitter wired, got %d body=%q", resp.Code, resp.Body.String())
	}
	var iterationID *int
	if err := h.db.QueryRow(`SELECT iteration_id FROM items WHERE id = ?`, itemID).Scan(&iterationID); err != nil {
		t.Fatalf("read back item: %v", err)
	}
	if iterationID != nil {
		t.Errorf("item must have moved to the backlog, still on iteration %d", *iterationID)
	}
}
