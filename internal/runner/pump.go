package runner

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
)

// Pump reads JSONL job lines from r and INSERT OR IGNOREs each into jobs.
// Returns counts of (inserted, skipped, total). Skipped = duplicate id
// (already present from a prior pump).
func (rn *Runner) Pump(ctx context.Context, r io.Reader) (inserted, skipped, total int, err error) {
	stmt, err := rn.db.PrepareContext(ctx,
		`INSERT OR IGNORE INTO jobs(job_id, cwd, argv, env, meta) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	br := bufio.NewReaderSize(r, 1<<16)
	for {
		select {
		case <-ctx.Done():
			return inserted, skipped, total, ctx.Err()
		default:
		}
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := trimSpace(line)
			if len(trimmed) > 0 {
				total++
				if insErr := rn.pumpLine(ctx, stmt, trimmed); insErr != nil {
					if insErr == errDuplicate {
						skipped++
					} else {
						return inserted, skipped, total, insErr
					}
				} else {
					inserted++
				}
			}
		}
		if readErr == io.EOF {
			return inserted, skipped, total, nil
		}
		if readErr != nil {
			return inserted, skipped, total, fmt.Errorf("read jsonl: %w", readErr)
		}
	}
}

var errDuplicate = fmt.Errorf("duplicate id")

func (rn *Runner) pumpLine(ctx context.Context, stmt *sql.Stmt, line []byte) error {
	var j Job
	if err := json.Unmarshal(line, &j); err != nil {
		return fmt.Errorf("parse jsonl line: %w", err)
	}
	if err := j.validate(); err != nil {
		return fmt.Errorf("invalid job: %w", err)
	}
	argvJSON, err := json.Marshal(j.Argv)
	if err != nil {
		return fmt.Errorf("marshal argv: %w", err)
	}
	envJSON := []byte("{}")
	if j.Env != nil {
		envJSON, err = json.Marshal(j.Env)
		if err != nil {
			return fmt.Errorf("marshal env: %w", err)
		}
	}
	metaJSON := []byte("{}")
	if len(j.Meta) > 0 {
		metaJSON = j.Meta
	}

	rn.writeMu.Lock()
	defer rn.writeMu.Unlock()

	res, err := stmt.ExecContext(ctx, j.ID, j.CWD, string(argvJSON), string(envJSON), string(metaJSON))
	if err != nil {
		return fmt.Errorf("insert id=%q: %w", j.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errDuplicate
	}
	return nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}
