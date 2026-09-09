package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// refusedDialErr is a REAL postgres dial failure against a port nothing
// listens on — the error the token_updates batcher met while Saturn's
// postgres17 was down (INFRA-328).
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

// INFRA-328 asked for backoff in the token_updates batcher. This is the
// evidence that it is already there and that it covers a connection refusal
// like any other flush failure: the store is called once, and the next flush
// is refused by the retry window instead of dialling the dead database again.
func TestWriteBatcherBacksOffOnRefusedDial(t *testing.T) {
	dialErr := refusedDialErr(t)
	calls := 0
	batcher := NewWriteBatcher(WriteBatcherConfig{
		FlushInterval:       time.Hour, // the test drives Flush itself
		MaxBatchSize:        10,
		RetryInitialBackoff: 2 * time.Second,
		RetryMaxBackoff:     30 * time.Second,
		Name:                "token_updates_test",
	}, func(context.Context, []int) error {
		calls++
		return fmt.Errorf("update token last_used_at: %w", dialErr)
	})

	if !batcher.Add(1) {
		t.Fatal("queue the first update")
	}
	if err := batcher.Flush(); err == nil {
		t.Fatal("the first flush must report the dial failure")
	}
	if calls != 1 {
		t.Fatalf("store called %d times, want 1", calls)
	}

	if !batcher.Add(2) {
		t.Fatal("queue a second update")
	}
	err := batcher.Flush()
	if !errors.Is(err, ErrWriteBatcherBackoff) {
		t.Fatalf("the next flush must be held by the retry window, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("store called %d times during the backoff window, want 1 — a dead database must not be re-dialled every flush", calls)
	}

	stats := batcher.Stats()
	if stats.RetryAt.IsZero() {
		t.Error("stats must expose the retry deadline")
	}
}

// And the window is released the moment the database answers again — no
// process restart, which is the other half of what the ticket asked for.
func TestWriteBatcherResumesAfterRecovery(t *testing.T) {
	dialErr := refusedDialErr(t)
	fail := true
	calls := 0
	batcher := NewWriteBatcher(WriteBatcherConfig{
		FlushInterval:       time.Hour,
		MaxBatchSize:        10,
		RetryInitialBackoff: 10 * time.Millisecond,
		RetryMaxBackoff:     10 * time.Millisecond,
		RetryJitter:         0,
		Name:                "token_updates_test",
	}, func(context.Context, []int) error {
		calls++
		if fail {
			return dialErr
		}
		return nil
	})

	batcher.Add(1)
	if err := batcher.Flush(); err == nil {
		t.Fatal("first flush must fail")
	}
	fail = false
	deadline := time.Now().Add(2 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		err = batcher.Flush()
		if err == nil || !errors.Is(err, ErrWriteBatcherBackoff) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the batcher must flush again once the database answers, got %v", err)
	}
	if calls < 2 {
		t.Fatalf("store called %d times, want the retry", calls)
	}
	if got := batcher.Stats().Pending; got != 0 {
		t.Errorf("pending items = %d, want the queued update written", got)
	}
}
