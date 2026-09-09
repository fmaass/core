package v1_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"windshift/internal/auth"
	"windshift/internal/database"
	"windshift/internal/models"
	"windshift/internal/restapi"
	v1 "windshift/internal/restapi/v1"
	"windshift/internal/services"
)

// unreachableDB is the token store with its database gone: every read fails
// the way a dial against a stopped Postgres fails. Only the read path is
// replaced — everything else is the real SQLite database underneath.
type unreachableDB struct {
	database.Database
	err error
}

func (u unreachableDB) Query(string, ...any) (*sql.Rows, error) { return nil, u.err }

// refusedDial returns a REAL driver dial failure (not a hand-written error):
// a postgres handle pointed at a port nothing listens on, which is what the
// windshift process saw while Saturn's postgres17 was down (INFRA-328).
func refusedDial(t *testing.T) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	db, err := sql.Open("postgres", fmt.Sprintf("postgres://u:p@%s/x?sslmode=disable&connect_timeout=1", addr))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, queryErr := db.Query("SELECT 1")
	if queryErr == nil {
		_ = rows.Close()
		t.Fatalf("expected a refused dial against %s", addr)
	}
	return queryErr
}

type authHarness struct {
	mux   *http.ServeMux
	db    database.Database
	token string
}

// newAuthHarness seeds one active user with a real bearer token, then mounts
// the v1 router on the store storeFn returns — the real database by default,
// an unreachable one for the outage case.
func newAuthHarness(t *testing.T, create models.APITokenCreate, storeFn func(database.Database) database.Database) *authHarness {
	t.Helper()
	db, err := database.NewSQLiteDB("file:" + filepath.Join(t.TempDir(), "auth-test.db"))
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

	created, err := auth.NewTokenManager(db, nil).CreateToken(1, create)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	store := database.Database(db)
	if storeFn != nil {
		store = storeFn(db)
	}
	permissionService, err := services.NewPermissionService(db, services.DefaultPermissionCacheConfig())
	if err != nil {
		t.Fatalf("permission service: %v", err)
	}
	mux := http.NewServeMux()
	v1.RegisterRoutes(restapi.Deps{
		Mux:               mux,
		DB:                store,
		TokenManager:      auth.NewTokenManager(store, nil),
		PermissionService: permissionService,
	})
	return &authHarness{mux: mux, db: db, token: created.Token}
}

func (h *authHarness) get(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/rest/api/v1/iterations", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body must be JSON: %v body=%q", err, rec.Body.String())
	}
	return body.Code
}

func readOnlyToken() models.APITokenCreate {
	return models.APITokenCreate{Name: "test", Permissions: []string{"iterations:read", "items:read"}}
}

// TestUnreachableStoreAnswers503 is the INFRA-328 regression: a token check
// that failed because the database was down answered 401 INVALID_TOKEN, so a
// client — and its operator — was told a valid credential had been rejected.
func TestUnreachableStoreAnswers503(t *testing.T) {
	dialErr := refusedDial(t)
	h := newAuthHarness(t, readOnlyToken(), func(db database.Database) database.Database {
		return unreachableDB{Database: db, err: dialErr}
	})

	rec := h.get(t, h.token)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a dead database must answer 503, got %d (code %q) body=%q", rec.Code, errorCode(t, rec), rec.Body.String())
	}
	if code := errorCode(t, rec); code == restapi.ErrCodeInvalidToken {
		t.Fatalf("the answer must not blame the token, got code %q", code)
	} else if code != restapi.ErrCodeServiceUnavailable {
		t.Errorf("error code = %q, want %q", code, restapi.ErrCodeServiceUnavailable)
	}
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("503 must carry Retry-After so a client knows to come back")
	}
	if _, err := time.ParseDuration(retryAfter + "s"); err != nil {
		t.Errorf("Retry-After = %q, want a number of seconds", retryAfter)
	}
}

// The credential failures keep answering 401 — the 503 is for one failure mode
// only, and must not become the answer to a bad token.
func TestCredentialFailuresStay401(t *testing.T) {
	t.Run("unknown token", func(t *testing.T) {
		h := newAuthHarness(t, readOnlyToken(), nil)
		unknown := h.token[:len(h.token)-4] + strings.Repeat("z", 4)
		rec := h.get(t, unknown)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unknown token must be 401, got %d body=%q", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != restapi.ErrCodeInvalidToken {
			t.Errorf("error code = %q, want %q", code, restapi.ErrCodeInvalidToken)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		expired := time.Now().Add(-time.Hour)
		create := readOnlyToken()
		create.ExpiresAt = &expired
		h := newAuthHarness(t, create, nil)
		rec := h.get(t, h.token)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expired token must be 401, got %d body=%q", rec.Code, rec.Body.String())
		}
	})

	t.Run("disabled user", func(t *testing.T) {
		h := newAuthHarness(t, readOnlyToken(), nil)
		if _, err := h.db.ExecWrite(`UPDATE users SET is_active = FALSE WHERE id = 1`); err != nil {
			t.Fatalf("disable user: %v", err)
		}
		rec := h.get(t, h.token)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("disabled user must be 401, got %d body=%q", rec.Code, rec.Body.String())
		}
	})
}
