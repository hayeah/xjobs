package runner

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// orderJSONL builds a JSONL plan whose ids are intentionally non-alphabetical
// in insertion order. Insertion order: zebra → apple → mango → banana.
// Alphabetical order: apple, banana, mango, zebra.
const orderJSONL = `{"id":"zebra","argv":["/bin/true"]}
{"id":"apple","argv":["/bin/true"]}
{"id":"mango","argv":["/bin/true"]}
{"id":"banana","argv":["/bin/true"]}
`

func newRunnerForTest(t *testing.T) *Runner {
	t.Helper()
	dir := t.TempDir()
	rn, err := Open(filepath.Join(dir, ".xjobs"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { rn.Close() })
	return rn
}

func fetchBatchIDs(t *testing.T, rn *Runner) []string {
	t.Helper()
	rows, err := rn.fetchBatch(context.Background(), Options{MaxAttempts: 3}, map[string]int64{})
	if err != nil {
		t.Fatalf("fetchBatch: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// TestPumpWorkQueueInsertionOrder: a JSONL plan whose ids are NOT in
// alphabetical order drains in insertion order, not id order.
func TestPumpWorkQueueInsertionOrder(t *testing.T) {
	rn := newRunnerForTest(t)
	ins, skip, total, err := rn.Pump(context.Background(), strings.NewReader(orderJSONL))
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if ins != 4 || skip != 0 || total != 4 {
		t.Fatalf("Pump counts: ins=%d skip=%d total=%d, want 4/0/4", ins, skip, total)
	}

	got := fetchBatchIDs(t, rn)
	want := []string{"zebra", "apple", "mango", "banana"}
	if !equalStrings(got, want) {
		t.Fatalf("work-queue order: got %v, want %v (insertion order)", got, want)
	}
}

// TestPumpSeqIsMonotonic: seq values are 1..N in the same order rows were
// inserted. Verifies the COALESCE(MAX(seq), 0) + 1 expression.
func TestPumpSeqIsMonotonic(t *testing.T) {
	rn := newRunnerForTest(t)
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(orderJSONL)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	rows, err := rn.db.Query(`SELECT id, seq FROM jobs ORDER BY seq`)
	if err != nil {
		t.Fatalf("query seq: %v", err)
	}
	defer rows.Close()
	type pair struct {
		ID  string
		Seq int64
	}
	var got []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.ID, &p.Seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, p)
	}
	want := []pair{{"zebra", 1}, {"apple", 2}, {"mango", 3}, {"banana", 4}}
	if len(got) != len(want) {
		t.Fatalf("row count: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// TestPumpDuplicatesDoNotBurnSeq: a duplicate id (INSERT OR IGNORE) does not
// consume a seq slot. The next genuine insert gets the next contiguous seq.
func TestPumpDuplicatesDoNotBurnSeq(t *testing.T) {
	rn := newRunnerForTest(t)
	first := `{"id":"a","argv":["/bin/true"]}
{"id":"b","argv":["/bin/true"]}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(first)); err != nil {
		t.Fatalf("Pump first: %v", err)
	}
	// Re-pump 'a' (dup) and add 'c'.
	second := `{"id":"a","argv":["/bin/true"]}
{"id":"c","argv":["/bin/true"]}
`
	ins, skip, total, err := rn.Pump(context.Background(), strings.NewReader(second))
	if err != nil {
		t.Fatalf("Pump second: %v", err)
	}
	if ins != 1 || skip != 1 || total != 2 {
		t.Fatalf("second Pump counts: ins=%d skip=%d total=%d, want 1/1/2", ins, skip, total)
	}
	rows, err := rn.db.Query(`SELECT id, seq FROM jobs ORDER BY seq`)
	if err != nil {
		t.Fatalf("query seq: %v", err)
	}
	defer rows.Close()
	var ids []string
	var seqs []int64
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
		seqs = append(seqs, seq)
	}
	if !equalStrings(ids, []string{"a", "b", "c"}) {
		t.Fatalf("ids in seq order: got %v want [a b c]", ids)
	}
	// 'a'=1 (first pump), 'b'=2 (first pump), 'c'=3 (second pump). The dup of
	// 'a' MUST NOT bump anyone's seq.
	if !equalInts(seqs, []int64{1, 2, 3}) {
		t.Fatalf("seqs: got %v want [1 2 3]", seqs)
	}
}

// TestMigrateAddJobsSeq exercises the upgrade path: build a "pre-seq" jobs
// table (no seq column), insert rows in a known order, run ensureSchema,
// then verify seq was added, backfilled from rowid, and the work-queue
// orders by it.
func TestMigrateAddJobsSeq(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sql3")

	// Hand-built pre-seq schema (mirrors the original CREATE TABLE before
	// this change shipped). No seq column.
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=synchronous(normal)"
	db, err := sql.Open("sqlite", "file:"+dbPath+"?"+pragmas)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE jobs (
			id          TEXT PRIMARY KEY,
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
		`CREATE INDEX idx_jobs_status ON jobs(status)`,
		`CREATE TABLE events (
			seq      INTEGER PRIMARY KEY AUTOINCREMENT,
			ts       TIMESTAMP NOT NULL,
			job_id   TEXT NOT NULL,
			attempt  INTEGER NOT NULL,
			kind     TEXT NOT NULL,
			data     TEXT NOT NULL DEFAULT '{}'
		)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed schema: %v", err)
		}
	}
	// Insert in non-alphabetical order. rowids will be 1,2,3,4.
	for _, id := range []string{"zebra", "apple", "mango", "banana"} {
		if _, err := db.Exec(`INSERT INTO jobs(id, cwd, argv) VALUES(?, '', '["/bin/true"]')`, id); err != nil {
			t.Fatalf("seed row %s: %v", id, err)
		}
	}
	// Sanity: column should NOT exist yet.
	var hasSeq int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='seq'`).Scan(&hasSeq); err != nil {
		t.Fatalf("probe pre-migrate: %v", err)
	}
	if hasSeq != 0 {
		t.Fatalf("pre-migrate: expected no seq column, got hasSeq=%d", hasSeq)
	}
	db.Close()

	// Now open via the production path; ensureSchema runs migrateAddJobsSeq.
	rn, err := Open(filepath.Dir(dbPath))
	if err != nil {
		// Open mkdir's its arg, so pass dir (parent) and let it find db.sql3.
		t.Fatalf("Open after seed: %v", err)
	}
	defer rn.Close()

	// Column must now exist.
	if err := rn.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='seq'`).Scan(&hasSeq); err != nil {
		t.Fatalf("probe post-migrate: %v", err)
	}
	if hasSeq != 1 {
		t.Fatalf("post-migrate: seq column missing")
	}

	// Backfill: seq must equal rowid (1..4 in insertion order).
	rows, err := rn.db.Query(`SELECT id, seq FROM jobs ORDER BY seq`)
	if err != nil {
		t.Fatalf("query seq: %v", err)
	}
	defer rows.Close()
	var ids []string
	var seqs []int64
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
		seqs = append(seqs, seq)
	}
	if !equalStrings(ids, []string{"zebra", "apple", "mango", "banana"}) {
		t.Fatalf("backfilled order: got %v want [zebra apple mango banana]", ids)
	}
	if !equalInts(seqs, []int64{1, 2, 3, 4}) {
		t.Fatalf("backfilled seqs: got %v want [1 2 3 4]", seqs)
	}

	// And the work-queue select honors it.
	got := fetchBatchIDs(t, rn)
	if !equalStrings(got, []string{"zebra", "apple", "mango", "banana"}) {
		t.Fatalf("post-migrate work-queue order: got %v want [zebra apple mango banana]", got)
	}

	// And a NEW Pump after migration extends seq monotonically (5, 6).
	more := `{"id":"yam","argv":["/bin/true"]}
{"id":"cherry","argv":["/bin/true"]}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(more)); err != nil {
		t.Fatalf("Pump post-migrate: %v", err)
	}
	var newSeqs []int64
	rows2, err := rn.db.Query(`SELECT seq FROM jobs WHERE id IN ('yam','cherry') ORDER BY seq`)
	if err != nil {
		t.Fatalf("query new seqs: %v", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var s int64
		if err := rows2.Scan(&s); err != nil {
			t.Fatalf("scan new seq: %v", err)
		}
		newSeqs = append(newSeqs, s)
	}
	if !equalInts(newSeqs, []int64{5, 6}) {
		t.Fatalf("post-migrate new seqs: got %v want [5 6]", newSeqs)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

