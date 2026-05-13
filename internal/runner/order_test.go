package runner

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// orderJSONL is a JSONL plan whose ids are intentionally NOT alphabetical.
// Insertion order: zebra → apple → mango → banana.
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
	rows, err := rn.fetchBatch(context.Background(), Options{}, map[string]int64{})
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

// TestPumpIDIsMonotonic: the integer PK `id` is assigned monotonically
// in insertion order. `id` values are 1..N in the order rows were
// inserted.
func TestPumpIDIsMonotonic(t *testing.T) {
	rn := newRunnerForTest(t)
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(orderJSONL)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	rows, err := rn.db.Query(`SELECT job_id, id FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatalf("query id: %v", err)
	}
	defer rows.Close()
	type pair struct {
		JobID string
		ID    int64
	}
	var got []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.JobID, &p.ID); err != nil {
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

// TestPumpDuplicatesAreIgnored: re-inserting a known job_id is a no-op
// via `INSERT OR IGNORE` on the UNIQUE(job_id) constraint. Pump counts
// skip vs. ins correctly, and the original `id` of the surviving row is
// unchanged.
func TestPumpDuplicatesAreIgnored(t *testing.T) {
	rn := newRunnerForTest(t)
	first := `{"id":"a","argv":["/bin/true"]}
{"id":"b","argv":["/bin/true"]}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(first)); err != nil {
		t.Fatalf("Pump first: %v", err)
	}
	// Snapshot a's id before re-pumping.
	var aBefore int64
	if err := rn.db.QueryRow(`SELECT id FROM jobs WHERE job_id='a'`).Scan(&aBefore); err != nil {
		t.Fatalf("read a.id: %v", err)
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

	// 'a' still has its original id (the dup INSERT OR IGNORE didn't
	// touch the row).
	var aAfter int64
	if err := rn.db.QueryRow(`SELECT id FROM jobs WHERE job_id='a'`).Scan(&aAfter); err != nil {
		t.Fatalf("read a.id after: %v", err)
	}
	if aAfter != aBefore {
		t.Fatalf("a.id changed across dup pump: before=%d after=%d", aBefore, aAfter)
	}

	// Insertion order for the work-queue is a, b, c — the dup of 'a'
	// did not bump 'a' to the end.
	got := fetchBatchIDs(t, rn)
	if !equalStrings(got, []string{"a", "b", "c"}) {
		t.Fatalf("work-queue order after dup: got %v want [a b c]", got)
	}
}

// TestJobsSchemaShape: pin the schema. `id` is the integer PRIMARY KEY
// (rowid alias) and `job_id` is UNIQUE. Catches accidental reverts.
func TestJobsSchemaShape(t *testing.T) {
	rn := newRunnerForTest(t)
	// `id` is the rowid alias: its `pk` flag in table_info is 1.
	var pk int
	if err := rn.db.QueryRow(
		`SELECT pk FROM pragma_table_info('jobs') WHERE name='id'`).Scan(&pk); err != nil {
		t.Fatalf("probe id.pk: %v", err)
	}
	if pk != 1 {
		t.Fatalf("expected jobs.id to be the primary key (pk=1), got pk=%d", pk)
	}
	// `job_id` is NOT the primary key (pk=0).
	if err := rn.db.QueryRow(
		`SELECT pk FROM pragma_table_info('jobs') WHERE name='job_id'`).Scan(&pk); err != nil {
		t.Fatalf("probe job_id.pk: %v", err)
	}
	if pk != 0 {
		t.Fatalf("expected jobs.job_id to NOT be the primary key (pk=0), got pk=%d", pk)
	}
	// But `job_id` must have a UNIQUE index for `INSERT OR IGNORE`
	// dedup. `pragma_index_list` reports indexes; the auto-index for
	// UNIQUE has `unique=1`.
	var nUnique int
	if err := rn.db.QueryRow(`
		SELECT COUNT(*) FROM pragma_index_list('jobs') il
		  JOIN pragma_index_info(il.name) ii ON 1=1
		 WHERE il."unique" = 1 AND ii.name = 'job_id'`).Scan(&nUnique); err != nil {
		t.Fatalf("probe job_id uniqueness: %v", err)
	}
	if nUnique == 0 {
		t.Fatalf("expected jobs.job_id to have a UNIQUE index, found none")
	}
}

// simulateAttempt drives one full attempt cycle on `id`: claim (bumps
// attempts, flips status to running) then terminalFail (flips status to
// failed, leaving attempts at the post-claim value). Mirrors what
// runOne does on a non-zero exit, but without spawning a child — lets
// us test work-queue ordering in isolation.
//
// Eligibility now lives on the row's own max_attempts column; the test
// plans encode "max_attempts":3 inline so multiple failed attempts
// remain re-claimable.
func simulateAttempt(t *testing.T, rn *Runner, id string) {
	t.Helper()
	claimed, _, err := rn.claim(context.Background(), id)
	if err != nil {
		t.Fatalf("claim %q: %v", id, err)
	}
	if !claimed {
		t.Fatalf("claim %q: not claimed (already terminal or ineligible)", id)
	}
	if err := rn.terminalFail(context.Background(), id,
		sql.NullInt64{Int64: 1, Valid: true}, "", "exit 1"); err != nil {
		t.Fatalf("terminalFail %q: %v", id, err)
	}
}

// TestRetryRoundRobin_OneFailureYieldsToSiblings: when one job fails,
// the next fetchBatch puts unfailed siblings ahead of it. The failing
// row carries attempts=1; its siblings are still at attempts=0, so
// `ORDER BY attempts, id` ranks them first regardless of insertion id.
func TestRetryRoundRobin_OneFailureYieldsToSiblings(t *testing.T) {
	rn := newRunnerForTest(t)
	plan := `{"id":"A","argv":["/usr/bin/false"],"max_attempts":3}
{"id":"B","argv":["/usr/bin/false"],"max_attempts":3}
{"id":"C","argv":["/usr/bin/false"],"max_attempts":3}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(plan)); err != nil {
		t.Fatalf("Pump: %v", err)
	}

	// A is the lowest-id row (1). Simulate one failed attempt on A.
	simulateAttempt(t, rn, "A")

	// With the old `ORDER BY id` the order would be [A, B, C] — A's
	// id=1 wins. With `ORDER BY attempts, id`, A's attempts=1 is
	// outranked by B and C (still at attempts=0).
	got := fetchBatchIDs(t, rn)
	want := []string{"B", "C", "A"}
	if !equalStrings(got, want) {
		t.Fatalf("after one failure: got %v, want %v (failing row goes to back)", got, want)
	}
}

// TestRetryRoundRobin_AllFailedSiblingsRotateByAttempts: when every
// row has failed once and then the lowest-id row fails again,
// fetchBatch should serve the still-at-attempts=1 siblings before the
// now-at-attempts=2 row.
func TestRetryRoundRobin_AllFailedSiblingsRotateByAttempts(t *testing.T) {
	rn := newRunnerForTest(t)
	plan := `{"id":"A","argv":["/usr/bin/false"],"max_attempts":3}
{"id":"B","argv":["/usr/bin/false"],"max_attempts":3}
{"id":"C","argv":["/usr/bin/false"],"max_attempts":3}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(plan)); err != nil {
		t.Fatalf("Pump: %v", err)
	}

	// Fail every row at attempt 1 — all three end with attempts=1.
	for _, id := range []string{"A", "B", "C"} {
		simulateAttempt(t, rn, id)
	}

	// All tied at attempts=1 — id is the tiebreaker, so insertion
	// order is preserved: [A, B, C].
	if got := fetchBatchIDs(t, rn); !equalStrings(got, []string{"A", "B", "C"}) {
		t.Fatalf("all-failed-once order: got %v, want [A B C] (insertion order within attempts bucket)", got)
	}

	// Fail A again — A's attempts goes to 2. B and C still at 1.
	simulateAttempt(t, rn, "A")

	// Order should rotate: B and C come before A's third try.
	got := fetchBatchIDs(t, rn)
	want := []string{"B", "C", "A"}
	if !equalStrings(got, want) {
		t.Fatalf("after A's second failure: got %v, want %v (B and C at attempts=1 outrank A at attempts=2)", got, want)
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
