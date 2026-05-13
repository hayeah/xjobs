package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// jobRow is the subset of the jobs row needed to spawn an attempt.
type jobRow struct {
	ID       string
	CWD      string
	Argv     []string
	Env      map[string]string
	Attempt  int // post-claim attempts value
}

// feedQueue iterates the work-queue in passes and emits unclaimed rows.
// Each pass re-selects so newly pumped rows are picked up.
//
// Dedup across passes is by (id, attempts): once we've emitted row R at
// attempts=N, we won't emit it again until attempts > N (i.e. its previous
// attempt has terminal-written and either succeeded or re-queued as
// failed-with-retries-remaining).
//
// Exit condition: pumpDone is closed AND no work-queue rows AND no
// running rows. While `running > 0` we wait — those workers may terminal
// to 'failed' with retries remaining and re-queue.
func (rn *Runner) feedQueue(ctx context.Context, opts Options, queue chan<- jobRow, pumpDone <-chan struct{}) error {
	seen := map[string]int64{}
	for {
		batch, err := rn.fetchBatch(ctx, opts, seen)
		if err != nil {
			return err
		}
		for _, row := range batch {
			select {
			case queue <- row:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if len(batch) > 0 {
			continue
		}

		// Empty batch. Wait one tick (gives workers in flight time to
		// terminal-write any failed rows that would re-queue), then make
		// the exit decision: pumpDone closed AND fetchBatch empty AND no
		// running rows.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rn.tick.C:
		}

		if !isClosed(pumpDone) {
			continue
		}
		running, err := rn.runningCount(ctx)
		if err != nil {
			return err
		}
		if running > 0 {
			continue
		}
		// Final confirmation scan — picks up rows that just terminal-wrote
		// to 'failed' with retries remaining.
		final, err := rn.fetchBatch(ctx, opts, seen)
		if err != nil {
			return err
		}
		if len(final) == 0 {
			return nil
		}
		for _, row := range final {
			select {
			case queue <- row:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// runningCount reports the number of jobs currently in 'running' state.
func (rn *Runner) runningCount(ctx context.Context) (int, error) {
	var n int
	if err := rn.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status='running'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count running: %w", err)
	}
	return n, nil
}

func (rn *Runner) fetchBatch(ctx context.Context, opts Options, seen map[string]int64) ([]jobRow, error) {
	q := fmt.Sprintf(
		`SELECT id, cwd, argv, env, attempts
		   FROM jobs
		  WHERE (status='pending' OR (status='failed' AND attempts < %d))
		    %s
		  ORDER BY n`,
		opts.MaxAttempts, whereAnd(opts.Where),
	)
	rows, err := rn.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("select work queue: %w", err)
	}
	defer rows.Close()

	var batch []jobRow
	for rows.Next() {
		var (
			id, cwd, argvJSON, envJSON string
			attempts                   int64
		)
		if err := rows.Scan(&id, &cwd, &argvJSON, &envJSON, &attempts); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if prev, ok := seen[id]; ok && attempts <= prev {
			continue
		}
		seen[id] = attempts
		var argv []string
		if err := json.Unmarshal([]byte(argvJSON), &argv); err != nil {
			return nil, fmt.Errorf("decode argv for id=%q: %w", id, err)
		}
		env := map[string]string{}
		if envJSON != "" && envJSON != "{}" {
			if err := json.Unmarshal([]byte(envJSON), &env); err != nil {
				return nil, fmt.Errorf("decode env for id=%q: %w", id, err)
			}
		}
		batch = append(batch, jobRow{
			ID:      id,
			CWD:     cwd,
			Argv:    argv,
			Env:     env,
			Attempt: int(attempts), // pre-claim; claim() will bump
		})
	}
	return batch, rows.Err()
}

func whereAnd(extra string) string {
	if extra == "" {
		return ""
	}
	return " AND (" + extra + ")"
}

// claim flips an eligible row to 'running' and bumps attempts. Returns
// (claimed, newAttempts).
func (rn *Runner) claim(ctx context.Context, id string, opts Options) (bool, int, error) {
	rn.writeMu.Lock()
	defer rn.writeMu.Unlock()

	q := fmt.Sprintf(
		`UPDATE jobs
		    SET status     = 'running',
		        attempts   = attempts + 1,
		        started_at = CURRENT_TIMESTAMP,
		        ended_at   = NULL,
		        pid        = NULL,
		        exit_code  = NULL,
		        signal     = NULL,
		        error      = NULL
		  WHERE id = ?
		    AND (status = 'pending' OR (status = 'failed' AND attempts < %d))`,
		opts.MaxAttempts,
	)
	res, err := rn.db.ExecContext(ctx, q, id)
	if err != nil {
		return false, 0, fmt.Errorf("claim id=%q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, 0, nil
	}
	var attempts int
	if err := rn.db.QueryRowContext(ctx, `SELECT attempts FROM jobs WHERE id = ?`, id).Scan(&attempts); err != nil {
		return true, 0, fmt.Errorf("read attempts id=%q: %w", id, err)
	}
	return true, attempts, nil
}

// setPID records the spawned PID on the row.
func (rn *Runner) setPID(ctx context.Context, id string, pid int) error {
	rn.writeMu.Lock()
	defer rn.writeMu.Unlock()
	_, err := rn.db.ExecContext(ctx, `UPDATE jobs SET pid = ? WHERE id = ?`, pid, id)
	return err
}

// terminalOK marks the row done with exit 0.
func (rn *Runner) terminalOK(ctx context.Context, id string, exitCode int) error {
	rn.writeMu.Lock()
	defer rn.writeMu.Unlock()
	_, err := rn.db.ExecContext(ctx,
		`UPDATE jobs SET status='done', ended_at=CURRENT_TIMESTAMP, exit_code=?, signal=NULL, error=NULL WHERE id=?`,
		exitCode, id)
	return err
}

// terminalFail marks the row failed; exit_code/sig/errMsg may be empty.
func (rn *Runner) terminalFail(ctx context.Context, id string, exitCode sql.NullInt64, sig string, errMsg string) error {
	rn.writeMu.Lock()
	defer rn.writeMu.Unlock()
	_, err := rn.db.ExecContext(ctx,
		`UPDATE jobs SET status='failed', ended_at=CURRENT_TIMESTAMP, exit_code=?, signal=?, error=? WHERE id=?`,
		exitCode, nullStr(sig), errMsg, id)
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
