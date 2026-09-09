package auth

import (
	"testing"
	"time"
)

// TestPendingTypeSurvivesTheValidationCache is the INFRA-91 review finding:
// Session.AuthPendingType is json:"-" and the validation cache serialises the
// session, so a cache hit used to come back with no pending type at all. The
// deadline then fell back to the 30-minute enrollment window and a
// verification-pending session stayed usable past its 5-minute one — for the
// whole cache TTL, without the ceremony ever completing.
func TestPendingTypeSurvivesTheValidationCache(t *testing.T) {
	validator := newSessionValidator(5*time.Second, "session_validation_pending_test")
	if validator.cache == nil {
		t.Fatal("validator has no cache; the round-trip cannot be exercised")
	}
	t.Cleanup(func() { _ = validator.cache.Close() })

	// Older than the 5-minute verification window, far inside the 30-minute
	// enrollment one and inside the 30-day remember-me expiry.
	created := time.Now().Add(-6 * time.Minute)
	session := &Session{
		ID:                 7,
		UserID:             3,
		Token:              "plaintext-token",
		ExpiresAt:          created.Add(ExtendedSessionDuration),
		IsActive:           true,
		EnrollmentRequired: true,
		AuthPendingType:    AuthPendingPasskeyVerification,
		CreatedAt:          created,
	}

	const key = "sha256:cache-key"
	if !validator.setIfCurrent(key, session, validator.epoch.Load(), validator.sequence.Load()) {
		t.Fatal("setIfCurrent refused to store the session")
	}
	cached, ok := validator.get(key)
	if !ok {
		t.Fatal("get returned no entry for a fresh cache write")
	}

	if cached.AuthPendingType != AuthPendingPasskeyVerification {
		t.Errorf("cached AuthPendingType = %q, want %q", cached.AuthPendingType, AuthPendingPasskeyVerification)
	}
	want := created.Add(pendingVerificationDuration)
	if got := sessionDeadline(cached); !got.Equal(want) {
		t.Errorf("deadline of the cached pending session = %s, want %s (the verification window)", got, want)
	}
	if !time.Now().Before(session.ExpiresAt) {
		t.Fatal("test setup: the stored expiry must still be in the future")
	}
	if time.Now().Before(sessionDeadline(cached)) {
		t.Errorf("a verification-pending session round-tripped through the cache is still valid %s after it was created, past its %s window",
			time.Since(created), pendingVerificationDuration)
	}
}

// TestUnknownPendingTypeFailsClosed covers the other half: whatever the reason
// a pending session arrives without a type this build recognises, it gets the
// SHORTEST window, never the longer enrollment one.
func TestUnknownPendingTypeFailsClosed(t *testing.T) {
	created := time.Now().Add(-10 * time.Minute)
	for _, pendingType := range []string{"", "passkey_something_new", "PASSKEY_VERIFICATION"} {
		session := &Session{
			ID:                 1,
			UserID:             1,
			ExpiresAt:          created.Add(ExtendedSessionDuration),
			IsActive:           true,
			EnrollmentRequired: true,
			AuthPendingType:    pendingType,
			CreatedAt:          created,
		}
		want := created.Add(pendingVerificationDuration)
		if got := sessionDeadline(session); !got.Equal(want) {
			t.Errorf("pending type %q: deadline = %s, want %s (the shortest pending window)", pendingType, got, want)
		}
		if time.Now().Before(sessionDeadline(session)) {
			t.Errorf("pending type %q: session created %s ago is still valid", pendingType, time.Since(created))
		}
	}

	notPending := &Session{ID: 1, UserID: 1, ExpiresAt: created.Add(ExtendedSessionDuration), IsActive: true, CreatedAt: created}
	if got := sessionDeadline(notPending); !got.Equal(notPending.ExpiresAt) {
		t.Errorf("a session that is not pending must keep its stored expiry, got %s", got)
	}
}
