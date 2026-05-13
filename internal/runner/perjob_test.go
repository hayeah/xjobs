package runner

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestPumpNiceRoundtrip: a JSONL row with "nice":N persists to jobs.nice,
// a row without "nice" leaves jobs.nice NULL, and an explicit "nice":0
// persists as 0 (NOT absent — 0 is a valid POSIX nice value).
func TestPumpNiceRoundtrip(t *testing.T) {
	rn := newRunnerForTest(t)
	jsonl := `{"id":"a","argv":["/bin/true"]}
{"id":"b","argv":["/bin/true"],"nice":10}
{"id":"c","argv":["/bin/true"],"nice":0}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(jsonl)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	rows, err := rn.db.Query(`SELECT job_id, nice FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	type row struct {
		ID   string
		Nice sql.NullInt64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Nice); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("row count: got %d want 3", len(got))
	}
	if got[0].ID != "a" || got[0].Nice.Valid {
		t.Errorf("row 'a': want nice=NULL, got %+v", got[0].Nice)
	}
	if got[1].ID != "b" || !got[1].Nice.Valid || got[1].Nice.Int64 != 10 {
		t.Errorf("row 'b': want nice=10, got %+v", got[1].Nice)
	}
	if got[2].ID != "c" || !got[2].Nice.Valid || got[2].Nice.Int64 != 0 {
		t.Errorf("row 'c': want nice=0 (explicit), got %+v", got[2].Nice)
	}
}

// TestFetchBatchCarriesNice: jobRow surfaces *int Nice matching the row's
// nullable nice column.
func TestFetchBatchCarriesNice(t *testing.T) {
	rn := newRunnerForTest(t)
	jsonl := `{"id":"a","argv":["/bin/true"]}
{"id":"b","argv":["/bin/true"],"nice":7}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(jsonl)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	batch, err := rn.fetchBatch(context.Background(), Options{}, map[string]int64{})
	if err != nil {
		t.Fatalf("fetchBatch: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("batch len: got %d want 2", len(batch))
	}
	byID := map[string]jobRow{}
	for _, r := range batch {
		byID[r.ID] = r
	}
	if byID["a"].Nice != nil {
		t.Errorf("row 'a': want Nice=nil, got %d", *byID["a"].Nice)
	}
	if byID["b"].Nice == nil || *byID["b"].Nice != 7 {
		t.Errorf("row 'b': want Nice=*7, got %v", byID["b"].Nice)
	}
}

// TestPumpMaxAttemptsDefault: absent "max_attempts" yields jobs.max_attempts=1
// (default), explicit values persist verbatim.
func TestPumpMaxAttemptsDefault(t *testing.T) {
	rn := newRunnerForTest(t)
	jsonl := `{"id":"a","argv":["/bin/true"]}
{"id":"b","argv":["/bin/true"],"max_attempts":5}
{"id":"c","argv":["/bin/true"],"max_attempts":1}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(jsonl)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	rows, err := rn.db.Query(`SELECT job_id, max_attempts FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var id string
		var ma int64
		if err := rows.Scan(&id, &ma); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = ma
	}
	if got["a"] != 1 {
		t.Errorf("row 'a': want max_attempts=1 (default), got %d", got["a"])
	}
	if got["b"] != 5 {
		t.Errorf("row 'b': want max_attempts=5, got %d", got["b"])
	}
	if got["c"] != 1 {
		t.Errorf("row 'c': want max_attempts=1, got %d", got["c"])
	}
}

// TestWorkQueueRespectsPerRowMaxAttempts: failed rows are eligible iff
// their attempts < their own max_attempts, not against a global option.
// A failed row at attempts=1 with max_attempts=1 is NOT eligible.
// A failed row at attempts=1 with max_attempts=3 IS eligible.
func TestWorkQueueRespectsPerRowMaxAttempts(t *testing.T) {
	rn := newRunnerForTest(t)
	jsonl := `{"id":"stop","argv":["/bin/true"]}
{"id":"go","argv":["/bin/true"],"max_attempts":3}
`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(jsonl)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	// Simulate one prior failed attempt on each.
	if _, err := rn.db.Exec(
		`UPDATE jobs SET status='failed', attempts=1 WHERE job_id IN ('stop','go')`,
	); err != nil {
		t.Fatalf("seed failed rows: %v", err)
	}
	got := fetchBatchIDs(t, rn)
	want := []string{"go"} // 'stop' is at its max_attempts=1; 'go' has retries left
	if !equalStrings(got, want) {
		t.Fatalf("eligible after one failure each: got %v want %v", got, want)
	}
}

// TestClaimRespectsPerRowMaxAttempts: claim() refuses to flip a failed
// row whose attempts have hit its own max_attempts.
func TestClaimRespectsPerRowMaxAttempts(t *testing.T) {
	rn := newRunnerForTest(t)
	jsonl := `{"id":"stop","argv":["/bin/true"]}`
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(jsonl)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if _, err := rn.db.Exec(
		`UPDATE jobs SET status='failed', attempts=1 WHERE job_id='stop'`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	claimed, _, err := rn.claim(context.Background(), "stop")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatalf("claim returned true for row at its max_attempts; should refuse")
	}
}
