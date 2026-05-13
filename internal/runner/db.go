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
			id          TEXT PRIMARY KEY,
			seq         INTEGER NOT NULL DEFAULT 0,
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
	if err := migrateAddJobsSeq(db); err != nil {
		return err
	}
	return nil
}

// migrateAddJobsSeq adds jobs.seq + idx_jobs_seq for pre-seq databases.
// On a fresh DB the column is already there from CREATE TABLE; the ALTER
// is skipped via the pragma_table_info probe. On an old DB the column is
// added with DEFAULT 0 and backfilled from rowid (preserves the original
// insertion order, since SQLite rowids on a TEXT-PK table grow monotonically
// with insert order absent VACUUM).
func migrateAddJobsSeq(db *sql.DB) error {
	var hasSeq int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='seq'`).Scan(&hasSeq); err != nil {
		return fmt.Errorf("probe jobs.seq: %w", err)
	}
	if hasSeq == 0 {
		if _, err := db.Exec(`ALTER TABLE jobs ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add jobs.seq: %w", err)
		}
		if _, err := db.Exec(`UPDATE jobs SET seq = rowid WHERE seq = 0`); err != nil {
			return fmt.Errorf("backfill jobs.seq: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_seq ON jobs(seq)`); err != nil {
		return fmt.Errorf("create idx_jobs_seq: %w", err)
	}
	return nil
}
