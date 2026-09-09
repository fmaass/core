package scheduler

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func refusedDialErr(t *testing.T) error {
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

func fixedClock() (*dbOutageBackoff, *time.Time) {
	now := time.Date(2026, 9, 3, 20, 21, 0, 0, time.UTC)
	b := newDBOutageBackoff("test")
	clock := &now
	b.now = func() time.Time { return *clock }
	return b, clock
}

// A refused dial pauses the loop, and every tick inside the pause is skipped —
// that is the whole point: a minute-interval scheduler re-dialled a dead
// Postgres every minute and logged every attempt (INFRA-328).
func TestOutagePausesTicks(t *testing.T) {
	b, clock := fixedClock()
	dialErr := refusedDialErr(t)

	if b.Skip() {
		t.Fatal("a healthy scheduler must not skip its tick")
	}
	if !b.Failure(dialErr) {
		t.Fatal("a refused dial must engage the pause")
	}
	if !b.Skip() {
		t.Fatal("the tick right after an outage must be skipped")
	}

	*clock = clock.Add(dbBackoffInitial - time.Second)
	if !b.Skip() {
		t.Error("ticks inside the pause window must be skipped")
	}
	*clock = clock.Add(2 * time.Second)
	if b.Skip() {
		t.Error("the pause must expire so the outage is re-probed")
	}
}

// Consecutive outages back off further, up to the ceiling that bounds how long
// a recovered database waits to be noticed.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	b, clock := fixedClock()
	dialErr := refusedDialErr(t)

	b.Failure(dialErr)
	first := b.retryAt.Sub(*clock)
	b.Failure(dialErr)
	second := b.retryAt.Sub(*clock)
	if second <= first {
		t.Fatalf("second delay %s must exceed the first %s", second, first)
	}
	for i := 0; i < 20; i++ {
		b.Failure(dialErr)
	}
	if capped := b.retryAt.Sub(*clock); capped != dbBackoffMax {
		t.Fatalf("delay = %s, want the %s ceiling", capped, dbBackoffMax)
	}
}

// The first pass that works clears the pause: recovery needs no restart.
func TestSuccessClearsThePause(t *testing.T) {
	b, _ := fixedClock()
	b.Failure(refusedDialErr(t))
	if !b.Skip() {
		t.Fatal("precondition: paused")
	}
	b.Success()
	if b.Skip() {
		t.Fatal("a successful pass must resume the normal interval")
	}
	if b.failures != 0 {
		t.Errorf("failure count = %d, want 0", b.failures)
	}
}

// An error the database ANSWERED with is not an outage. Pausing for it would
// hide a bug behind a five-minute silence.
func TestAnsweredErrorDoesNotPause(t *testing.T) {
	b, _ := fixedClock()
	for _, err := range []error{errors.New("pq: syntax error at or near \"SELCT\""), sql.ErrNoRows} {
		if b.Failure(err) {
			t.Errorf("%v must not engage the pause", err)
		}
	}
	if b.Skip() {
		t.Error("an answered error must leave the scheduler ticking")
	}
}
