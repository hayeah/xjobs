package runner

import (
	"context"
	"fmt"
	"path/filepath"
)

// reapStaleRunning resets any 'running' row whose session-dir flock is no
// longer held by its prior owner. Returns the number of rows reaped.
//
// Silent on the event stream by design: the next claim of a reaped row
// produces the user-visible success/error event for the new attempt.
func (rn *Runner) reapStaleRunning(ctx context.Context) (int, error) {
	rows, err := rn.db.QueryContext(ctx, `SELECT job_id FROM jobs WHERE status='running'`)
	if err != nil {
		return 0, fmt.Errorf("scan running rows: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	reaped := 0
	for _, id := range ids {
		lockPath := filepath.Join(rn.stateDir, id, "lock")
		avail, err := flockAvailable(lockPath)
		if err != nil {
			// Treat unlinkable / unreadable lock as "owner unknown — leave
			// alone." A future startup will revisit.
			continue
		}
		if !avail {
			continue
		}
		rn.writeMu.Lock()
		_, err = rn.db.ExecContext(ctx,
			`UPDATE jobs SET status='pending', pid=NULL WHERE job_id=? AND status='running'`, id)
		rn.writeMu.Unlock()
		if err != nil {
			return reaped, fmt.Errorf("reset id=%q: %w", id, err)
		}
		reaped++
	}
	return reaped, nil
}
