package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"windshift/internal/database"
)

// newSessionTestDB opens a real SQLite database (the driver the app itself
// uses) with the shipped users/user_sessions schema. Nothing is mocked: the
// UPDATEs under test are the UPDATEs that run in production.
func newSessionTestDB(t *testing.T) (database.Database, *SessionManager, int) {
	t.Helper()

	db, err := database.NewSQLiteDBWithPoolSizes(filepath.Join(t.TempDir(), "auth.db"), 2, 1)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// users.sql declares an FK to oauth_clients; the table itself belongs to
	// another schema file this test does not need.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS oauth_clients (id INTEGER PRIMARY KEY AUTOINCREMENT)`); err != nil {
		t.Fatalf("create oauth_clients stub: %v", err)
	}

	schema, err := os.ReadFile(filepath.Join("..", "database", "schema", "users.sql"))
	if err != nil {
		t.Fatalf("read users schema: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("apply users schema: %v", err)
	}

	var userID int64
	if err := db.QueryRow(`
		INSERT INTO users (email, username, first_name, last_name, is_active)
		VALUES (?, ?, ?, ?, true) RETURNING id
	`, "passkey@example.test", "passkey", "Pass", "Key").Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	sm := NewSessionManager(db, false, false, nil, "session-test-secret", "off")
	return db, sm, int(userID)
}

func storedExpiry(t *testing.T, db database.Database, sessionID int) time.Time {
	t.Helper()
	var expiresAt time.Time
	if err := db.QueryRow(`SELECT expires_at FROM user_sessions WHERE id = ?`, sessionID).Scan(&expiresAt); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	return expiresAt
}

// TestPasskeyCompletionKeepsSessionDuration is INFRA-91 itself: completing the
// required WebAuthn ceremony rebuilt the session with the hardcoded
// DefaultSessionDuration, so a remember-me login that went through
// password+passkey verification silently dropped from 30 days to 24 hours.
// user_sessions has no remember_me column, so the row's own expires_at is the
// only record of the choice — and it must survive the ceremony.
func TestPasskeyCompletionKeepsSessionDuration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rememberMe bool
		want       time.Duration
	}{
		{"remember me keeps 30 days", true, ExtendedSessionDuration},
		{"plain login keeps 24 hours", false, DefaultSessionDuration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, sm, userID := newSessionTestDB(t)

			session, err := sm.CreateSession(userID, "192.0.2.10", "go-test", tc.rememberMe)
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			atLogin := storedExpiry(t, db, session.ID)

			// The password step marks the session pending until a fresh
			// assertion succeeds.
			if err := sm.SetAuthPending(session.ID, AuthPendingPasskeyVerification); err != nil {
				t.Fatalf("SetAuthPending: %v", err)
			}
			// The assertion succeeds and the session is elevated.
			if err := sm.ClearEnrollmentRequired(session.ID); err != nil {
				t.Fatalf("ClearEnrollmentRequired: %v", err)
			}

			after := storedExpiry(t, db, session.ID)
			if drift := after.Sub(atLogin); drift < -time.Minute || drift > time.Minute {
				t.Errorf("expiry changed across the passkey ceremony: login %s -> after %s (drift %s)", atLogin, after, drift)
			}
			if lifetime := time.Until(after); lifetime < tc.want-time.Minute {
				t.Errorf("session lifetime after passkey completion = %s, want ~%s", lifetime, tc.want)
			}
		})
	}
}

// TestFirstPasskeyEnrollmentKeepsSessionDuration covers the by-user-id path,
// which elevates enrollment sessions after a first credential is registered.
func TestFirstPasskeyEnrollmentKeepsSessionDuration(t *testing.T) {
	db, sm, userID := newSessionTestDB(t)

	session, err := sm.CreateSession(userID, "192.0.2.11", "go-test", true)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	atLogin := storedExpiry(t, db, session.ID)

	if err := sm.SetAuthPending(session.ID, AuthPendingEnrollment); err != nil {
		t.Fatalf("SetAuthPending: %v", err)
	}
	if err := sm.ClearEnrollmentRequiredByUserID(userID); err != nil {
		t.Fatalf("ClearEnrollmentRequiredByUserID: %v", err)
	}

	after := storedExpiry(t, db, session.ID)
	if drift := after.Sub(atLogin); drift < -time.Minute || drift > time.Minute {
		t.Errorf("expiry changed across first-passkey enrollment: login %s -> after %s (drift %s)", atLogin, after, drift)
	}
	if lifetime := time.Until(after); lifetime < ExtendedSessionDuration-time.Minute {
		t.Errorf("remember-me lifetime after enrollment = %s, want ~%s", lifetime, ExtendedSessionDuration)
	}

	var pendingType any
	var enrollmentRequired bool
	if err := db.QueryRow(`SELECT auth_pending_type, COALESCE(enrollment_required, false) FROM user_sessions WHERE id = ?`, session.ID).
		Scan(&pendingType, &enrollmentRequired); err != nil {
		t.Fatalf("read pending state: %v", err)
	}
	if pendingType != nil || enrollmentRequired {
		t.Errorf("session still pending after enrollment: auth_pending_type=%v enrollment_required=%v", pendingType, enrollmentRequired)
	}
}
