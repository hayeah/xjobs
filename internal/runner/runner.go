// Package runner is the xjobs core: SQLite-backed work queue, JSONL pump,
// per-job process spawn, and stream of success/error events.
//
// This is the MVP shape — plain exec.Cmd children with output captured to
// a per-job log file. The Service seam (execAttempt) is the swap point
// where hootty/libghostty PTY support will land later.
package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Options tune one Run.
//
// Per-job knobs (nice, max_attempts) live on the JSONL row, not here —
// the runner's mental model is "launching from the current active
// shell," so process-wide priority/retry globals don't belong on the
// runner. See Job in job.go.
type Options struct {
	StateDir  string        // default ".xjobs"
	Workers   int           // default runtime.NumCPU()
	Where     string        // optional SQL fragment AND-combined with the work-queue predicate
	PollEvery time.Duration // how often feedQueue re-scans when idle (default 250ms)
}

func (o Options) withDefaults() Options {
	if o.StateDir == "" {
		o.StateDir = ".xjobs"
	}
	if o.Workers <= 0 {
		o.Workers = runtime.NumCPU()
	}
	if o.PollEvery <= 0 {
		o.PollEvery = 250 * time.Millisecond
	}
	return o
}

// Runner owns the DB handle and serializes writes for one xjobs process.
type Runner struct {
	db          *sql.DB
	dbPath      string
	stateDir    string
	writeMu     sync.Mutex
	tick        *time.Ticker
	drainerLock *flockHandle
}

// Open initializes (or opens) the state dir, creates the DB, and returns
// a Runner. Close releases the DB handle.
func Open(stateDir string) (*Runner, error) {
	if stateDir == "" {
		stateDir = ".xjobs"
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", stateDir, err)
	}
	absDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("abs %s: %w", stateDir, err)
	}
	dbPath := filepath.Join(absDir, "db.sql3")
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	return &Runner{db: db, dbPath: dbPath, stateDir: absDir}, nil
}

func (rn *Runner) Close() error {
	if rn.tick != nil {
		rn.tick.Stop()
	}
	if rn.drainerLock != nil {
		_ = rn.drainerLock.Close()
		rn.drainerLock = nil
	}
	return rn.db.Close()
}

// AcquireDrainerLock takes the exclusive flock on
// `<state_dir>/runner.lock` so this process is the sole drainer. On
// success the handle is owned by the Runner and released on Close. On
// contention returns (false, holderPID, nil) — the caller can decide
// whether to fail loudly or pump-and-exit.
//
// Calling twice is a programmer error; the second call returns an error.
func (rn *Runner) AcquireDrainerLock() (acquired bool, holderPID int, err error) {
	if rn.drainerLock != nil {
		return false, 0, fmt.Errorf("drainer lock already held")
	}
	h, ok, holder, err := acquireRunnerLock(filepath.Join(rn.stateDir, "runner.lock"))
	if err != nil {
		return false, 0, err
	}
	if !ok {
		return false, holder, nil
	}
	rn.drainerLock = h
	return true, 0, nil
}

// ReapStaleRunning runs the reaper pass: resets any 'running' row whose
// per-job flock has been released (i.e. its prior owner is gone).
// Returns the number of rows reaped. Caller-driven so the state-dir
// drainer lock can gate it — see AcquireDrainerLock.
func (rn *Runner) ReapStaleRunning(ctx context.Context) (int, error) {
	return rn.reapStaleRunning(ctx)
}

// DB exposes the underlying handle for ls / monitor / sql verbs.
func (rn *Runner) DB() *sql.DB { return rn.db }

// StateDir returns the absolute state-dir path.
func (rn *Runner) StateDir() string { return rn.stateDir }

// DBPath returns the absolute DB path (the XJOBS env's "db" field).
func (rn *Runner) DBPath() string { return rn.dbPath }

// Drain runs the worker pool until no eligible row remains. The caller
// must hold the state-dir drainer lock (see AcquireDrainerLock) and must
// have run ReapStaleRunning before this — Drain itself does neither.
//
// If pumpDone is non-nil, the drain waits for it to close before deciding
// to exit on an empty work queue (i.e. it stays alive while a pump is
// streaming rows in concurrently).
//
// eventsOut, if non-nil, receives one JSONL line per terminal attempt.
func (rn *Runner) Drain(ctx context.Context, opts Options, pumpDone <-chan struct{}, eventsOut io.Writer) error {
	opts = opts.withDefaults()
	rn.tick = time.NewTicker(opts.PollEvery)
	defer rn.tick.Stop()

	sink := newEventSink(rn, eventsOut)

	queue := make(chan jobRow, opts.Workers*2)
	var wg sync.WaitGroup
	wg.Add(opts.Workers)
	var inflight atomic.Int64

	var firstErr error
	var firstErrMu sync.Mutex
	recordErr := func(e error) {
		if e == nil {
			return
		}
		firstErrMu.Lock()
		defer firstErrMu.Unlock()
		if firstErr == nil {
			firstErr = e
		}
	}

	for i := 0; i < opts.Workers; i++ {
		go func() {
			defer wg.Done()
			for row := range queue {
				func() {
					defer inflight.Add(-1)
					if err := rn.runOne(ctx, row, sink); err != nil {
						recordErr(err)
					}
				}()
			}
		}()
	}

	// feedQueue treats a nil pumpDone as "already done" via isClosed.
	feedErr := rn.feedQueue(ctx, opts, queue, pumpDone, &inflight)
	close(queue)
	wg.Wait()

	if feedErr != nil && !errors.Is(feedErr, context.Canceled) {
		return feedErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failed, err := rn.terminalFailedCount(ctx, opts.Where)
	if err != nil {
		return err
	}
	if failed > 0 {
		return FailedJobsError{Count: failed}
	}
	return firstErr
}

// FailedJobsError reports that drain reached quiescence with terminal
// failed rows still present.
type FailedJobsError struct {
	Count int
}

func (e FailedJobsError) Error() string {
	if e.Count == 1 {
		return "1 job failed"
	}
	return fmt.Sprintf("%d jobs failed", e.Count)
}

// runOne claims, executes, and terminal-writes a single row.
func (rn *Runner) runOne(ctx context.Context, row jobRow, sink *eventSink) error {
	claimed, attempt, err := rn.claim(ctx, row.ID)
	if err != nil {
		return err
	}
	if !claimed {
		return nil // someone else got it, or it's no longer eligible
	}
	row.Attempt = attempt

	start := time.Now()
	onSpawn := func(pid int) {
		// Best-effort: record PID and emit "running". A failure to insert
		// the event is logged-not-fatal — the terminal write is what the
		// row state depends on.
		if err := rn.setPID(ctx, row.ID, pid); err != nil {
			fmt.Fprintf(os.Stderr, "xjobs: set pid id=%q: %v\n", row.ID, err)
		}
		if err := sink.emit(ctx, Event{
			Kind:    "running",
			ID:      row.ID,
			Attempt: attempt,
			PID:     pid,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "xjobs: emit running id=%q: %v\n", row.ID, err)
		}
	}

	res := execAttempt(ctx, rn.stateDir, row, rn.dbPath, onSpawn)
	dur := time.Since(start)
	finalCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		finalCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}

	exitCode := sql.NullInt64{Int64: int64(res.ExitCode), Valid: true}
	var termErr error
	switch {
	case res.Err != nil:
		termErr = rn.terminalFail(finalCtx, row.ID, sql.NullInt64{}, "", res.Err.Error())
	case res.Signal != "":
		termErr = rn.terminalFail(finalCtx, row.ID, exitCode, res.Signal, "killed by "+res.Signal)
	case res.ExitCode == 0:
		termErr = rn.terminalOK(finalCtx, row.ID, 0)
	default:
		termErr = rn.terminalFail(finalCtx, row.ID, exitCode, "", fmt.Sprintf("exit %d", res.ExitCode))
	}
	if termErr != nil {
		return termErr
	}

	return sink.emit(finalCtx, eventFromResult(row.ID, attempt, dur, res))
}
