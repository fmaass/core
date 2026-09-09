package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"windshift/internal/models"
	"windshift/internal/repository"
)

// RunnerLeaseReaper is the liveness backstop for remote agent runs
// (Initiative WI-141). A remote runner heartbeats on an interval; if it dies
// mid-run its heartbeat goes stale and its in-flight runs would otherwise
// hang in 'running' forever. On each tick this sweeper fails those runs.
//
// Stale runner instances are intentionally NOT auto-revoked (WI-545): an idle
// runner host may be stopped for longer than the liveness window, then restart
// with its persisted per-instance credential. Auto-revoking would force a new
// one-time registration token and make otherwise healthy pools stall after
// inactivity. Manual admin revocation remains the eviction path.
//
// It mirrors the other in-process schedulers' lifecycle: Start/Stop are wired
// into server.go alongside cfvCleanupScheduler et al.
type RunnerLeaseReaper struct {
	runs    *repository.AgentRunRepository
	runners *repository.RunnerRepository

	ticker   *time.Ticker
	stopChan chan struct{}
	mu       sync.RWMutex
	running  bool

	interval         time.Duration
	staleAfter       time.Duration // a runner with no heartbeat for this long is dead
	queuedStallAfter time.Duration // a remote run queued unclaimed for this long is flagged
	maxRunDuration   time.Duration // a run 'running' for this long is failed regardless of heartbeat
	now              func() time.Time

	dbBackoff *dbOutageBackoff
}

const (
	defaultReaperInterval   = 60 * time.Second
	defaultReaperStaleAfter = models.RunnerLivenessWindow

	// Remote claims normally arrive in seconds; longer queues indicate no runner,
	// exhausted capacity, or a dead pool.
	defaultQueuedStallAfter = 3 * time.Minute

	// Fail long-running phantom runs even with heartbeats, freeing capacity and
	// per-item dedup when terminal reports are lost.
	defaultMaxRunDuration = 8 * time.Hour
)

// NewRunnerLeaseReaper builds a reaper for server lifecycle wiring.
func NewRunnerLeaseReaper(runs *repository.AgentRunRepository, runners *repository.RunnerRepository) *RunnerLeaseReaper {
	return &RunnerLeaseReaper{
		runs:             runs,
		runners:          runners,
		interval:         defaultReaperInterval,
		staleAfter:       defaultReaperStaleAfter,
		queuedStallAfter: defaultQueuedStallAfter,
		maxRunDuration:   defaultMaxRunDuration,
		now:              func() time.Time { return time.Now().UTC() },
		dbBackoff:        newDBOutageBackoff("runner_lease_reaper"),
	}
}

// Start begins the sweep loop. Idempotent.
func (s *RunnerLeaseReaper) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	s.ticker = time.NewTicker(s.interval)
	s.stopChan = make(chan struct{})
	s.running = true
	slog.Info("starting runner lease reaper", "interval", s.interval, "stale_after", s.staleAfter)
	go s.loop(s.ticker, s.stopChan)
}

// Stop halts the sweep loop. Idempotent.
func (s *RunnerLeaseReaper) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	s.running = false
	if s.ticker != nil {
		s.ticker.Stop()
		s.ticker = nil
	}
	close(s.stopChan)
	slog.Info("runner lease reaper stopped")
}

func (s *RunnerLeaseReaper) loop(ticker *time.Ticker, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

func (s *RunnerLeaseReaper) tick() {
	// Skipped while the database is unreachable (INFRA-328).
	if s.dbBackoff.Skip() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reaped, revoked, err := s.Sweep(ctx)
	if err != nil {
		if !s.dbBackoff.Failure(err) {
			slog.Error("runner lease reaper sweep", "error", err)
		}
		return
	}
	s.dbBackoff.Success()
	if reaped > 0 || revoked > 0 {
		slog.Info("runner lease reaper swept", "reaped_runs", reaped, "revoked_instances", revoked)
	}
}

// Sweep runs one reap pass: fail runs of stale runners, fail runs that have
// exceeded the max-run-duration backstop, then flag remote runs that have sat
// queued past the stall threshold. Exported for testing. The revokedInstances
// return is kept for API/log compatibility and is always 0.
func (s *RunnerLeaseReaper) Sweep(ctx context.Context) (reapedRuns, revokedInstances int, err error) {
	now := s.now()
	staleBefore := now.Add(-s.staleAfter)
	reapedRuns, err = s.runs.ReapStaleRuns(ctx, staleBefore, now)
	if err != nil {
		return reapedRuns, 0, err
	}
	// Duration backstop (WI-331): a healthy runner whose terminal report was
	// lost keeps the run 'running' indefinitely — heartbeat staleness never
	// triggers, so an absolute bound on time-in-running is the only way out.
	overdue, err := s.runs.ReapOverdueRuns(ctx, now.Add(-s.maxRunDuration), now)
	if err != nil {
		return reapedRuns, 0, err
	}
	if overdue > 0 {
		slog.Warn("failed agent runs stuck in running past the max duration",
			"count", overdue, "max_run_duration", s.maxRunDuration)
	}
	reapedRuns += overdue
	// Do not revoke stale runner instances automatically (WI-545). ReapStaleRuns
	// above is enough to free pool capacity for any in-flight work owned by a
	// dead runner, while preserving the runner's persisted credential so an idle
	// host can come back without minting a fresh registration token.
	revokedInstances = 0
	s.flagStalledQueuedRuns(ctx, now, staleBefore)
	return reapedRuns, revokedInstances, nil
}

// flagStalledQueuedRuns surfaces remote-pool runs nobody has claimed: a
// recurring WARN per sweep keeps the signal alive in the server log while
// the stall persists, and a one-time "warning" event lands in the run's own
// event stream so the stall is visible in the UI next to the queued event.
// Best-effort: diagnostics must never fail the sweep.
func (s *RunnerLeaseReaper) flagStalledQueuedRuns(ctx context.Context, now, staleBefore time.Time) {
	stalled, err := s.runs.ListStaleQueuedPoolRuns(ctx, now.Add(-s.queuedStallAfter))
	if err != nil {
		slog.Error("runner lease reaper: list stalled queued runs", "error", err)
		return
	}
	liveByPool := map[int]int{}
	for _, run := range stalled {
		live, ok := liveByPool[run.PoolID]
		if !ok {
			if live, err = s.runners.CountLiveInstancesForPool(ctx, run.PoolID, staleBefore); err != nil {
				slog.Error("runner lease reaper: count live instances", "pool_id", run.PoolID, "error", err)
				continue
			}
			liveByPool[run.PoolID] = live
		}
		age := now.Sub(run.QueuedAt).Round(time.Second)
		slog.Warn("agent run queued but unclaimed",
			"run_id", run.RunID, "pool_id", run.PoolID, "queued_for", age.String(), "live_runners", live)
		if has, err := s.runs.HasEvent(ctx, run.RunID, "warning"); err != nil || has {
			continue
		}
		payload := fmt.Sprintf(
			`{"message":"queued for %s without being claimed — pool %d has %d live runner(s)","live_runners":%d,"target_pool_id":%d}`,
			age, run.PoolID, live, live, run.PoolID)
		if err := s.runs.AppendEvent(ctx, run.RunID, "warning", payload); err != nil {
			slog.Warn("runner lease reaper: append stall event", "run_id", run.RunID, "error", err)
		}
	}
}
