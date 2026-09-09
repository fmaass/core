package auth

import (
	"errors"
	"testing"
	"time"

	"windshift/internal/models"
)

func activeTestUser() *models.User {
	return &models.User{ID: 1, Username: "passkey", IsActive: true}
}

// TestPendingSessionStillDiesOnItsOwnWindow is the other half of INFRA-91:
// the pending window is now derived from created_at instead of being written
// over expires_at, so it must still cut a long-lived session short while the
// required ceremony is outstanding.
func TestPendingSessionStillDiesOnItsOwnWindow(t *testing.T) {
	created := time.Now().Add(-10 * time.Minute)
	rememberMe := created.Add(ExtendedSessionDuration)

	for _, tc := range []struct {
		name               string
		pendingType        string
		enrollmentRequired bool
		wantExpired        bool
	}{
		{"verification window elapsed", AuthPendingPasskeyVerification, true, true},
		{"enrollment window still open", AuthPendingEnrollment, true, false},
		// Fail closed: pending with no recognisable type gets the shortest
		// window, so at 10 minutes it is already dead.
		{"enrollment flag without a type", "", true, true},
		{"not pending at all", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &Session{
				ID:                 1,
				UserID:             1,
				ExpiresAt:          rememberMe,
				IsActive:           true,
				EnrollmentRequired: tc.enrollmentRequired,
				AuthPendingType:    tc.pendingType,
				CreatedAt:          created,
			}
			expired := !time.Now().Before(sessionDeadline(session))
			if expired != tc.wantExpired {
				t.Errorf("sessionDeadline = %s (expired=%v), want expired=%v", sessionDeadline(session), expired, tc.wantExpired)
			}
			if !tc.wantExpired && sessionDeadline(session).After(rememberMe) {
				t.Errorf("deadline %s must never exceed the stored expiry %s", sessionDeadline(session), rememberMe)
			}
		})
	}
}

// TestValidateSessionStateHonoursThePendingWindow proves the derived deadline
// is the one the validator applies, not just a helper nobody calls.
func TestValidateSessionStateHonoursThePendingWindow(t *testing.T) {
	session := &Session{
		ID:                 1,
		UserID:             1,
		ExpiresAt:          time.Now().Add(ExtendedSessionDuration),
		IsActive:           true,
		EnrollmentRequired: true,
		AuthPendingType:    AuthPendingPasskeyVerification,
		CreatedAt:          time.Now().Add(-10 * time.Minute),
		User:               activeTestUser(),
	}
	if err := validateSessionState(session); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("validateSessionState on a stale pending session = %v, want %v", err, ErrSessionExpired)
	}

	session.CreatedAt = time.Now()
	if err := validateSessionState(session); err != nil {
		t.Errorf("validateSessionState on a fresh pending session = %v, want nil", err)
	}
}

// TestCleanupReapsAbandonedPendingSession keeps the property SetAuthPending's
// short expires_at used to provide: a ceremony nobody finishes is deactivated
// on its own window, not thirty days later.
func TestCleanupReapsAbandonedPendingSession(t *testing.T) {
	db, sm, userID := newSessionTestDB(t)

	session, err := sm.CreateSession(userID, "192.0.2.12", "go-test", true)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := sm.SetAuthPending(session.ID, AuthPendingPasskeyVerification); err != nil {
		t.Fatalf("SetAuthPending: %v", err)
	}
	if _, err := db.ExecWrite(`UPDATE user_sessions SET created_at = ? WHERE id = ?`, time.Now().Add(-time.Hour), session.ID); err != nil {
		t.Fatalf("backdate session: %v", err)
	}

	if err := sm.CleanupExpiredSessions(); err != nil {
		t.Fatalf("CleanupExpiredSessions: %v", err)
	}

	var isActive bool
	if err := db.QueryRow(`SELECT is_active FROM user_sessions WHERE id = ?`, session.ID).Scan(&isActive); err != nil {
		t.Fatalf("read is_active: %v", err)
	}
	if isActive {
		t.Errorf("abandoned pending session is still active an hour after a %s window", pendingVerificationDuration)
	}

	fresh, err := sm.CreateSession(userID, "192.0.2.13", "go-test", true)
	if err != nil {
		t.Fatalf("CreateSession (control): %v", err)
	}
	if err := sm.CleanupExpiredSessions(); err != nil {
		t.Fatalf("CleanupExpiredSessions (control): %v", err)
	}
	if err := db.QueryRow(`SELECT is_active FROM user_sessions WHERE id = ?`, fresh.ID).Scan(&isActive); err != nil {
		t.Fatalf("read is_active (control): %v", err)
	}
	if !isActive {
		t.Errorf("cleanup deactivated a healthy remember-me session")
	}
}
