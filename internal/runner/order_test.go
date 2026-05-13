package runner

import (
	"context"
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

// TestPumpNIsMonotonic: the integer PK `n` is assigned monotonically in
// insertion order. `n` values are 1..N in the order rows were inserted.
func TestPumpNIsMonotonic(t *testing.T) {
	rn := newRunnerForTest(t)
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(orderJSONL)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	rows, err := rn.db.Query(`SELECT id, n FROM jobs ORDER BY n`)
	if err != nil {
		t.Fatalf("query n: %v", err)
	}
	defer rows.Close()
	type pair struct {
		ID string
		N  int64
	}
	var got []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.ID, &p.N); err != nil {
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

// TestPumpDuplicatesAreIgnored: re-inserting a known id is a no-op via
// `INSERT OR IGNORE` on the UNIQUE(id) constraint. Pump counts skip vs.
// ins correctly, and the original `n` of the surviving row is unchanged.
func TestPumpDuplicatesAreIgnored(t *testing.T) {
	rn := newRunnerForTest(t)
	first := `{"id":"a","argv":["/bin/true"]}
{"id":"b","argv":["/bin/true"]}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(first)); err != nil {
		t.Fatalf("Pump first: %v", err)
	}
	// Snapshot a's n before re-pumping.
	var aBefore int64
	if err := rn.db.QueryRow(`SELECT n FROM jobs WHERE id='a'`).Scan(&aBefore); err != nil {
		t.Fatalf("read a.n: %v", err)
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

	// 'a' still has its original n (the dup INSERT OR IGNORE didn't
	// touch the row).
	var aAfter int64
	if err := rn.db.QueryRow(`SELECT n FROM jobs WHERE id='a'`).Scan(&aAfter); err != nil {
		t.Fatalf("read a.n after: %v", err)
	}
	if aAfter != aBefore {
		t.Fatalf("a.n changed across dup pump: before=%d after=%d", aBefore, aAfter)
	}

	// Insertion order for the work-queue is a, b, c — the dup of 'a'
	// did not bump 'a' to the end.
	got := fetchBatchIDs(t, rn)
	if !equalStrings(got, []string{"a", "b", "c"}) {
		t.Fatalf("work-queue order after dup: got %v want [a b c]", got)
	}
}

// TestJobsSchemaShape: pin the schema. `n` is the integer PRIMARY KEY
// (rowid alias) and `id` is UNIQUE. Catches accidental reverts.
func TestJobsSchemaShape(t *testing.T) {
	rn := newRunnerForTest(t)
	// `n` is the rowid alias: its `pk` flag in table_info is 1.
	var pk int
	if err := rn.db.QueryRow(
		`SELECT pk FROM pragma_table_info('jobs') WHERE name='n'`).Scan(&pk); err != nil {
		t.Fatalf("probe n.pk: %v", err)
	}
	if pk != 1 {
		t.Fatalf("expected jobs.n to be the primary key (pk=1), got pk=%d", pk)
	}
	// `id` is NOT the primary key (pk=0) — it lost PRIMARY KEY to `n`.
	if err := rn.db.QueryRow(
		`SELECT pk FROM pragma_table_info('jobs') WHERE name='id'`).Scan(&pk); err != nil {
		t.Fatalf("probe id.pk: %v", err)
	}
	if pk != 0 {
		t.Fatalf("expected jobs.id to NOT be the primary key (pk=0), got pk=%d", pk)
	}
	// But `id` must have a UNIQUE index for `INSERT OR IGNORE` dedup.
	// `pragma_index_list` reports indexes; the auto-index for UNIQUE has
	// `unique=1`.
	var nUnique int
	if err := rn.db.QueryRow(`
		SELECT COUNT(*) FROM pragma_index_list('jobs') il
		  JOIN pragma_index_info(il.name) ii ON 1=1
		 WHERE il."unique" = 1 AND ii.name = 'id'`).Scan(&nUnique); err != nil {
		t.Fatalf("probe id uniqueness: %v", err)
	}
	if nUnique == 0 {
		t.Fatalf("expected jobs.id to have a UNIQUE index, found none")
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
