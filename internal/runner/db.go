package runner

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

func openDB(path string) (*sql.DB, error) {
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=synchronous(normal)"
	dsn := "file:" + path + "?" + pragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := ensureSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func ensureSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS jobs (
			n           INTEGER PRIMARY KEY AUTOINCREMENT,
			id          TEXT NOT NULL UNIQUE,
			cwd         TEXT NOT NULL,
			argv        TEXT NOT NULL,
			env         TEXT NOT NULL DEFAULT '{}',
			status      TEXT NOT NULL DEFAULT 'pending',
			attempts    INTEGER NOT NULL DEFAULT 0,
			pid         INTEGER,
			exit_code   INTEGER,
			signal      TEXT,
			session_key TEXT,
			started_at  TIMESTAMP,
			ended_at    TIMESTAMP,
			error       TEXT,
			meta        TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status)`,
		`CREATE TABLE IF NOT EXISTS events (
			seq      INTEGER PRIMARY KEY AUTOINCREMENT,
			ts       TIMESTAMP NOT NULL,
			job_id   TEXT NOT NULL,
			attempt  INTEGER NOT NULL,
			kind     TEXT NOT NULL,
			data     TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_job_id ON events(job_id)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	return nil
}
