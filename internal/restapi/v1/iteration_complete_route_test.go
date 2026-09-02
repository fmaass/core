package v1_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"windshift/internal/auth"
	"windshift/internal/database"
	"windshift/internal/models"
	"windshift/internal/restapi"
	v1 "windshift/internal/restapi/v1"
	"windshift/internal/services"
)

// harness boots a real SQLite-backed v1 router with a real bearer token, so
// these tests exercise routing, token scope enforcement and the completion
// service together rather than a hand-built stub.
type harness struct {
	mux   *http.ServeMux
	db    database.Database
	token string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dsn := "file:" + filepath.Join(t.TempDir(), "windshift-test.db")
	db, err := database.NewSQLiteDB(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Initialize(); err != nil {
		t.Fatalf("initialize schema: %v", err)
	}

	if _, err := db.ExecWrite(`
		INSERT INTO users (id, email, username, first_name, last_name, is_active)
		VALUES (1, 'tester@example.invalid', 'tester', 'Test', 'User', TRUE)
	`); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	tokenManager := auth.NewTokenManager(db, nil)
	permissionService, err := services.NewPermissionService(db, services.DefaultPermissionCacheConfig())
	if err != nil {
		t.Fatalf("permission service: %v", err)
	}

	created, err := tokenManager.CreateToken(1, models.APITokenCreate{
		Name:        "test",
		Permissions: []string{"iterations:read", "iterations:write", "items:read", "items:write"},
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	mux := http.NewServeMux()
	v1.RegisterRoutes(restapi.Deps{
		Mux:               mux,
		DB:                db,
		TokenManager:      tokenManager,
		PermissionService: permissionService,
	})

	return &harness{mux: mux, db: db, token: created.Token}
}

// seedIteration creates a workspace-scoped iteration and returns its id.
func (h *harness) seedIteration(t *testing.T, name, status string) int {
	t.Helper()
	if _, err := h.db.ExecWrite(`
		INSERT INTO workspaces (id, name, key) VALUES (1, 'Infrastructure', 'INFRA')
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	res, err := h.db.ExecWrite(`
		INSERT INTO iterations (name, start_date, end_date, status, is_global, workspace_id)
		VALUES (?, '2026-01-01', '2026-01-14', ?, FALSE, 1)
	`, name, status)
	if err != nil {
		t.Fatalf("seed iteration %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("iteration id: %v", err)
	}
	return int(id)
}

// seedIncompleteItem puts one open item on the iteration so completion has
// something real to carry forward.
func (h *harness) seedIncompleteItem(t *testing.T, iterationID int, title string) int {
	t.Helper()
	var statusID int
	if err := h.db.QueryRow(`
		SELECT st.id FROM statuses st
		LEFT JOIN status_categories sc ON sc.id = st.category_id
		WHERE COALESCE(sc.is_completed, false) = false
		ORDER BY st.id LIMIT 1
	`).Scan(&statusID); err != nil {
		t.Fatalf("find an incomplete status: %v", err)
	}
	// description is written as '' rather than left NULL: the item repository's
	// detail scanner reads it into a string, matching what the creation service
	// persists for a real item.
	res, err := h.db.ExecWrite(`
		INSERT INTO items (workspace_id, title, description, frac_index, iteration_id, status_id, creator_id)
		VALUES (1, ?, '', ?, ?, ?, 1)
	`, title, "a"+title, iterationID, statusID)
	if err != nil {
		t.Fatalf("seed item %q: %v", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("item id: %v", err)
	}
	return int(id)
}

func (h *harness) do(t *testing.T, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// TestCompleteIterationRouteIsRegistered is the INFRA-280 regression: the
// route was absent from the v1 router, so the CLI's POST answered 405 with a
// text/plain body. Any JSON status other than 405 proves it is wired; the
// happy path below proves it is wired to the right thing.
func TestCompleteIterationRouteIsRegistered(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodPost, "/rest/api/v1/iterations/999999/complete", `{}`, h.token)

	if rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("POST /iterations/{id}/complete still answers 405 (INFRA-280); body=%q", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown iteration id must be 404, got %d body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("error response must be JSON, got Content-Type %q body=%q", ct, rec.Body.String())
	}
}

func TestCompleteIterationRequiresAuth(t *testing.T) {
	h := newHarness(t)
	id := h.seedIteration(t, "WP-04", "active")

	rec := h.do(t, http.MethodPost, fmt.Sprintf("/rest/api/v1/iterations/%d/complete", id), `{}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated completion must be 401, got %d body=%q", rec.Code, rec.Body.String())
	}

	var iterationStatus string
	if err := h.db.QueryRow(`SELECT status FROM iterations WHERE id = ?`, id).Scan(&iterationStatus); err != nil {
		t.Fatalf("read back iteration: %v", err)
	}
	if iterationStatus != "active" {
		t.Errorf("rejected request must not have completed the iteration, status is %q", iterationStatus)
	}
}

// TestCompleteIterationHappyPath verifies the authoritative state, not just
// the response: the iteration row must read back completed and the incomplete
// item must have moved to the carry-forward target.
func TestCompleteIterationHappyPath(t *testing.T) {
	h := newHarness(t)
	source := h.seedIteration(t, "WP-03", "active")
	target := h.seedIteration(t, "WP-04", "active")
	itemID := h.seedIncompleteItem(t, source, "carried-forward")

	body := fmt.Sprintf(`{"move_incomplete_to_iteration_id": %d}`, target)
	rec := h.do(t, http.MethodPost, fmt.Sprintf("/rest/api/v1/iterations/%d/complete", source), body, h.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("completion must be 200, got %d body=%q", rec.Code, rec.Body.String())
	}

	// The CLI keys off iteration_id; a response without it fails its contract check.
	var resp struct {
		IterationID       int  `json:"iteration_id"`
		TargetIterationID *int `json:"target_iteration_id"`
		Status            string
		AlreadyCompleted  bool `json:"already_completed"`
		MovedCount        int  `json:"moved_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response must be JSON: %v body=%q", err, rec.Body.String())
	}
	if resp.IterationID != source {
		t.Errorf("iteration_id = %d, want %d", resp.IterationID, source)
	}
	if resp.TargetIterationID == nil || *resp.TargetIterationID != target {
		t.Errorf("target_iteration_id = %v, want %d", resp.TargetIterationID, target)
	}
	if resp.Status != "completed" || resp.AlreadyCompleted {
		t.Errorf("status=%q already_completed=%v, want completed/false", resp.Status, resp.AlreadyCompleted)
	}
	if resp.MovedCount != 1 {
		t.Errorf("moved_count = %d, want 1", resp.MovedCount)
	}

	var iterationStatus string
	if err := h.db.QueryRow(`SELECT status FROM iterations WHERE id = ?`, source).Scan(&iterationStatus); err != nil {
		t.Fatalf("read back iteration: %v", err)
	}
	if iterationStatus != "completed" {
		t.Errorf("iteration row status = %q, want completed", iterationStatus)
	}

	var movedTo int
	if err := h.db.QueryRow(`SELECT iteration_id FROM items WHERE id = ?`, itemID).Scan(&movedTo); err != nil {
		t.Fatalf("read back item: %v", err)
	}
	if movedTo != target {
		t.Errorf("item landed on iteration %d, want %d", movedTo, target)
	}
}

// A repeat completion is the retry the CLI may issue; it must succeed and
// report already_completed rather than erroring or moving anything again.
func TestCompleteIterationIsIdempotent(t *testing.T) {
	h := newHarness(t)
	source := h.seedIteration(t, "WP-03", "active")

	path := fmt.Sprintf("/rest/api/v1/iterations/%d/complete", source)
	if rec := h.do(t, http.MethodPost, path, `{}`, h.token); rec.Code != http.StatusOK {
		t.Fatalf("first completion must be 200, got %d body=%q", rec.Code, rec.Body.String())
	}

	rec := h.do(t, http.MethodPost, path, `{}`, h.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat completion must be 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		AlreadyCompleted bool `json:"already_completed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response must be JSON: %v", err)
	}
	if !resp.AlreadyCompleted {
		t.Error("repeat completion must report already_completed = true")
	}
}

// An empty body is legal — it means "move the incomplete items to the backlog".
func TestCompleteIterationAcceptsEmptyBody(t *testing.T) {
	h := newHarness(t)
	source := h.seedIteration(t, "WP-03", "active")
	itemID := h.seedIncompleteItem(t, source, "to-backlog")

	rec := h.do(t, http.MethodPost, fmt.Sprintf("/rest/api/v1/iterations/%d/complete", source), "", h.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty body must be accepted, got %d body=%q", rec.Code, rec.Body.String())
	}

	var iterationID *int
	if err := h.db.QueryRow(`SELECT iteration_id FROM items WHERE id = ?`, itemID).Scan(&iterationID); err != nil {
		t.Fatalf("read back item: %v", err)
	}
	if iterationID != nil {
		t.Errorf("item must have moved to the backlog, still on iteration %d", *iterationID)
	}
}
