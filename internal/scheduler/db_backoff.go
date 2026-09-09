package scheduler

import (
	"log/slog"
	"sync"
	"time"

	"windshift/internal/database"
)

const (
	// dbBackoffInitial is the first pause after the database goes away. It is
	// longer than the shortest scheduler interval (60s) on purpose: a pause
	// shorter than the tick would skip no tick at all.
	dbBackoffInitial = 1 * time.Minute
	// dbBackoffMax bounds how long a recovered database waits to be noticed.
	dbBackoffMax = 5 * time.Minute
)

// dbOutageBackoff pauses a ticking scheduler while the database is
// unreachable. Without it a minute-interval loop re-dialled a dead Postgres
// every 60 seconds forever and wrote one error line per tick (INFRA-328); the
// pool itself needs no help — database/sql redials on the next statement — so
// this is about not spending the attempt, and not burying the outage in noise.
//
// It engages ONLY for a database outage (database.IsUnavailable). A statement
// the database answered with an error is a bug or a data problem, and pausing
// the scheduler for five minutes would hide it.
type dbOutageBackoff struct {
	name string

	mu       sync.Mutex
	failures int
	retryAt  time.Time

	now func() time.Time // overridable in tests
}

func newDBOutageBackoff(name string) *dbOutageBackoff {
	return &dbOutageBackoff{name: name, now: time.Now}
}

// Skip reports whether this tick falls inside an active pause and must not run.
func (b *dbOutageBackoff) Skip() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.retryAt.IsZero() && b.now().Before(b.retryAt)
}

// Failure records the outcome of a tick that failed. It returns true when the
// error was an outage and the pause engaged.
func (b *dbOutageBackoff) Failure(err error) bool {
	if b == nil || err == nil || !database.IsUnavailable(err) {
		return false
	}
	b.mu.Lock()
	b.failures++
	delay := dbBackoffInitial
	for i := 1; i < b.failures && delay < dbBackoffMax; i++ {
		delay *= 2
	}
	if delay > dbBackoffMax {
		delay = dbBackoffMax
	}
	b.retryAt = b.now().Add(delay)
	failures := b.failures
	b.mu.Unlock()

	slog.Warn("scheduler pausing: database unreachable",
		"scheduler", b.name, "consecutive_failures", failures, "retry_in", delay, "error", err)
	return true
}

// Success clears an active pause. Every tick that completed calls it, so the
// first successful pass after an outage resumes the normal interval.
func (b *dbOutageBackoff) Success() {
	if b == nil {
		return
	}
	b.mu.Lock()
	resumed := b.failures > 0
	b.failures = 0
	b.retryAt = time.Time{}
	b.mu.Unlock()
	if resumed {
		slog.Info("scheduler resuming: database reachable again", "scheduler", b.name)
	}
}
