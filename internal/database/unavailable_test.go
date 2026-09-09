package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// refusedDialError produces a REAL "connection refused" from the postgres
// driver — the exact error a windshift process got while Saturn's postgres17
// was down (INFRA-328) — by pointing it at a port nothing listens on.
func refusedDialError(t *testing.T) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("free the port: %v", err)
	}

	db, err := sql.Open("postgres", fmt.Sprintf("postgres://u:p@%s/x?sslmode=disable&connect_timeout=1", addr))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query("SELECT 1")
	if err == nil {
		_ = rows.Close()
		t.Fatalf("expected a dial failure against %s", addr)
	}
	return err
}

func TestDialRefusedIsUnavailable(t *testing.T) {
	err := refusedDialError(t)
	if !IsUnavailable(err) {
		t.Fatalf("a refused dial must classify as unavailable, got %v (%T)", err, err)
	}
	if !errors.Is(MarkUnavailable(err), ErrUnavailable) {
		t.Fatal("MarkUnavailable must make the outage visible to errors.Is")
	}
	// The wrapper must not hide the original: callers log it.
	if !errors.Is(MarkUnavailable(err), err) {
		t.Error("MarkUnavailable must keep the driver error unwrappable")
	}
}

func TestDeadlineAndDeadConnectionsAreUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	for name, err := range map[string]error{
		"deadline":  fmt.Errorf("query items: %w", ctx.Err()),
		"conn done": fmt.Errorf("query items: %w", sql.ErrConnDone),
	} {
		if !IsUnavailable(err) {
			t.Errorf("%s must classify as unavailable", name)
		}
	}
}

// The other half of the promise: an answer FROM the database is not an outage.
// Pausing a scheduler or answering 503 for these would hide a real bug.
func TestAnsweredErrorsAreNotUnavailable(t *testing.T) {
	for name, err := range map[string]error{
		"no rows":        fmt.Errorf("load token: %w", sql.ErrNoRows),
		"constraint":     errors.New("pq: duplicate key value violates unique constraint \"users_email_key\""),
		"syntax":         errors.New("pq: syntax error at or near \"SELCT\""),
		"invalid token":  errors.New("invalid token"),
		"context cancel": fmt.Errorf("query: %w", context.Canceled),
		// a client that hangs up mid-dial: the driver hands back a net error
		// whose cause is the cancellation, not the database's state
		"cancel in dial":  &net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled},
		"nil":             nil,
		"disabled_string": errors.New("user account is disabled"),
	} {
		if IsUnavailable(err) {
			t.Errorf("%s must NOT classify as unavailable", name)
		}
		if got := MarkUnavailable(err); !errors.Is(got, err) && !(err == nil && got == nil) {
			t.Errorf("%s must pass through MarkUnavailable unchanged", name)
		}
	}
}
