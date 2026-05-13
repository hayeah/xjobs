package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// LSRow is one row of `xjobs ls` output.
type LSRow struct {
	ID        string         `json:"id"`
	Status    string         `json:"status"`
	Attempts  int            `json:"attempts"`
	ExitCode  sql.NullInt64  `json:"-"`
	Signal    sql.NullString `json:"-"`
	StartedAt sql.NullString `json:"started_at,omitempty"`
	EndedAt   sql.NullString `json:"ended_at,omitempty"`
	Argv      string         `json:"argv"`
	Error     sql.NullString `json:"-"`
}

// LS returns all jobs ordered by (status precedence, started_at).
func (rn *Runner) LS(ctx context.Context, where string) ([]LSRow, error) {
	q := `SELECT id, status, attempts, exit_code, signal, started_at, ended_at, argv, error
	        FROM jobs`
	if where != "" {
		q += " WHERE " + where
	}
	q += ` ORDER BY CASE status
	                  WHEN 'running' THEN 0
	                  WHEN 'pending' THEN 1
	                  WHEN 'failed'  THEN 2
	                  WHEN 'done'    THEN 3
	                  ELSE 4 END,
	                started_at,
	                id`
	rows, err := rn.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ls query: %w", err)
	}
	defer rows.Close()
	var out []LSRow
	for rows.Next() {
		var r LSRow
		if err := rows.Scan(&r.ID, &r.Status, &r.Attempts, &r.ExitCode, &r.Signal, &r.StartedAt, &r.EndedAt, &r.Argv, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PrintLSText writes a tab-separated table of rows to w.
func PrintLSText(w io.Writer, rows []LSRow) {
	for _, r := range rows {
		var detail string
		switch r.Status {
		case "done":
			detail = fmt.Sprintf("exit=%d", nullInt(r.ExitCode))
		case "failed":
			if r.Signal.Valid {
				detail = "sig=" + r.Signal.String
			} else {
				detail = fmt.Sprintf("exit=%d", nullInt(r.ExitCode))
			}
		case "running":
			detail = fmt.Sprintf("att=%d", r.Attempts)
		default:
			detail = "—"
		}
		fmt.Fprintf(w, "[%s]\t%s\t%s\t%s\n", r.ID, r.Status, detail, oneline(r.Argv))
	}
}

// PrintLSJSON emits one JSONL line per row.
func PrintLSJSON(w io.Writer, rows []LSRow) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		obj := map[string]any{
			"id":       r.ID,
			"status":   r.Status,
			"attempts": r.Attempts,
		}
		if r.ExitCode.Valid {
			obj["exit_code"] = r.ExitCode.Int64
		}
		if r.Signal.Valid {
			obj["signal"] = r.Signal.String
		}
		if r.StartedAt.Valid {
			obj["started_at"] = r.StartedAt.String
		}
		if r.EndedAt.Valid {
			obj["ended_at"] = r.EndedAt.String
		}
		if r.Error.Valid {
			obj["error"] = r.Error.String
		}
		var argv []string
		_ = json.Unmarshal([]byte(r.Argv), &argv)
		obj["argv"] = argv
		if err := enc.Encode(obj); err != nil {
			return err
		}
	}
	return nil
}

func nullInt(n sql.NullInt64) int64 {
	if n.Valid {
		return n.Int64
	}
	return 0
}

func oneline(argvJSON string) string {
	var argv []string
	if err := json.Unmarshal([]byte(argvJSON), &argv); err != nil {
		return argvJSON
	}
	out := ""
	for i, a := range argv {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// Monitor tails the events table. If since == 0, prints the most recent
// event line and then blocks for the next. Otherwise blocks for any event
// with seq > since. Exits after one event.
func (rn *Runner) Monitor(ctx context.Context, idFilter string, sinceSeq int64, w io.Writer) error {
	// If sinceSeq == 0, print the most recent event right away (if any).
	if sinceSeq == 0 {
		last, ok, err := rn.lastEvent(ctx, idFilter)
		if err != nil {
			return err
		}
		if ok {
			if _, err := w.Write(append([]byte(last.Data), '\n')); err != nil {
				return err
			}
			sinceSeq = last.Seq
		}
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		next, ok, err := rn.nextEvent(ctx, idFilter, sinceSeq)
		if err != nil {
			return err
		}
		if ok {
			_, err := w.Write(append([]byte(next.Data), '\n'))
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type eventRow struct {
	Seq  int64
	Data string
}

func (rn *Runner) lastEvent(ctx context.Context, idFilter string) (eventRow, bool, error) {
	q := `SELECT seq, data FROM events`
	args := []any{}
	if idFilter != "" {
		q += ` WHERE job_id = ?`
		args = append(args, idFilter)
	}
	q += ` ORDER BY seq DESC LIMIT 1`
	var r eventRow
	err := rn.db.QueryRowContext(ctx, q, args...).Scan(&r.Seq, &r.Data)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	return r, true, nil
}

func (rn *Runner) nextEvent(ctx context.Context, idFilter string, since int64) (eventRow, bool, error) {
	q := `SELECT seq, data FROM events WHERE seq > ?`
	args := []any{since}
	if idFilter != "" {
		q += ` AND job_id = ?`
		args = append(args, idFilter)
	}
	q += ` ORDER BY seq ASC LIMIT 1`
	var r eventRow
	err := rn.db.QueryRowContext(ctx, q, args...).Scan(&r.Seq, &r.Data)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	return r, true, nil
}
