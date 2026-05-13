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
	db       *sql.DB
	dbPath   string
	stateDir string
	writeMu  sync.Mutex
	tick     *time.Ticker
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
	return rn.db.Close()
}

// DB exposes the underlying handle for ls / monitor / sql verbs.
func (rn *Runner) DB() *sql.DB { return rn.db }

// StateDir returns the absolute state-dir path.
func (rn *Runner) StateDir() string { return rn.stateDir }

// DBPath returns the absolute DB path (the XJOBS env's "db" field).
func (rn *Runner) DBPath() string { return rn.dbPath }

// Drain runs the worker pool until no eligible row remains.
// If pumpDone is non-nil, the drain waits for it to close before deciding
// to exit on an empty work queue (i.e. it stays alive while a pump is
// streaming rows in concurrently).
//
// eventsOut, if non-nil, receives one JSONL line per terminal attempt.
func (rn *Runner) Drain(ctx context.Context, opts Options, pumpDone <-chan struct{}, eventsOut io.Writer) error {
	opts = opts.withDefaults()
	rn.tick = time.NewTicker(opts.PollEvery)
	defer rn.tick.Stop()

	// Reaper pass before claiming.
	reaped, err := rn.reapStaleRunning(ctx)
	if err != nil {
		return fmt.Errorf("reaper: %w", err)
	}
	if reaped > 0 {
		fmt.Fprintf(os.Stderr, "xjobs: reaped %d stale running row(s) from prior run\n", reaped)
	}

	sink := newEventSink(rn, eventsOut)

	queue := make(chan jobRow, opts.Workers*2)
	var wg sync.WaitGroup
	wg.Add(opts.Workers)

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
				if err := rn.runOne(ctx, row, sink); err != nil {
					recordErr(err)
				}
			}
		}()
	}

	idleSignal := pumpDone
	if idleSignal == nil {
		closed := make(chan struct{})
		close(closed)
		idleSignal = closed
	}
	feedErr := rn.feedQueue(ctx, opts, queue, idleSignal)
	close(queue)
	wg.Wait()

	if feedErr != nil && !errors.Is(feedErr, context.Canceled) {
		return feedErr
	}
	return firstErr
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
		_ = rn.setPID(ctx, row.ID, pid)
		_ = sink.emit(ctx, Event{
			Kind:    "running",
			ID:      row.ID,
			Attempt: attempt,
			PID:     pid,
		})
	}

	res := execAttempt(ctx, rn.stateDir, row, rn.dbPath, onSpawn)
	dur := time.Since(start)

	switch {
	case res.Err != nil:
		if termErr := rn.terminalFail(ctx, row.ID, sql.NullInt64{}, "", res.Err.Error()); termErr != nil {
			return termErr
		}
	case res.Signal != "":
		if termErr := rn.terminalFail(ctx, row.ID, sql.NullInt64{Int64: int64(res.ExitCode), Valid: true}, res.Signal, "killed by "+res.Signal); termErr != nil {
			return termErr
		}
	case res.ExitCode == 0:
		if termErr := rn.terminalOK(ctx, row.ID, 0); termErr != nil {
			return termErr
		}
	default:
		if termErr := rn.terminalFail(ctx, row.ID, sql.NullInt64{Int64: int64(res.ExitCode), Valid: true}, "", fmt.Sprintf("exit %d", res.ExitCode)); termErr != nil {
			return termErr
		}
	}

	return sink.emit(ctx, eventFromResult(row.ID, attempt, dur, res))
}
