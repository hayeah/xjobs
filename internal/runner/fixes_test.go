package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJobValidateRejectsPathIDs(t *testing.T) {
	badIDs := []string{
		".",
		"..",
		"../escape",
		"a/b",
		`a\b`,
		"line\nbreak",
		"nul\x00byte",
	}
	for _, id := range badIDs {
		t.Run(id, func(t *testing.T) {
			j := Job{ID: id, Argv: []string{"/bin/true"}}
			if err := j.validate(); err == nil {
				t.Fatalf("validate(%q): got nil, want error", id)
			}
		})
	}
}

func TestDrainReturnsFailedJobsError(t *testing.T) {
	rn := newRunnerForTest(t)
	if _, _, _, err := rn.Pump(context.Background(), strings.NewReader(`{"id":"fail","argv":["/bin/false"]}`)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if _, err := rn.db.Exec(`UPDATE jobs SET status='failed', attempts=1, max_attempts=1 WHERE job_id='fail'`); err != nil {
		t.Fatalf("seed failed row: %v", err)
	}

	err := rn.Drain(context.Background(), Options{PollEvery: time.Millisecond}, nil, io.Discard)
	var failed FailedJobsError
	if !errors.As(err, &failed) {
		t.Fatalf("Drain error: got %v, want FailedJobsError", err)
	}
	if failed.Count != 1 {
		t.Fatalf("failed count: got %d want 1", failed.Count)
	}
}

func TestRetryOutputLogAppendsAttempts(t *testing.T) {
	stateDir := t.TempDir()
	attempts := []struct {
		n    int
		text string
	}{
		{1, "first"},
		{2, "second"},
	}
	for _, attempt := range attempts {
		row := jobRow{
			ID:      "retry",
			Argv:    []string{"/bin/sh", "-c", "echo " + attempt.text},
			Attempt: attempt.n,
		}
		res := execAttempt(context.Background(), stateDir, row, filepath.Join(stateDir, "db.sql3"), nil)
		if res.Err != nil || res.ExitCode != 0 {
			t.Fatalf("attempt %d result: %+v", attempt.n, res)
		}
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "retry", "output.log"))
	if err != nil {
		t.Fatalf("read output.log: %v", err)
	}
	for _, want := range []string{"--- attempt 1 at ", "first", "--- attempt 2 at ", "second"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Fatalf("output.log missing %q:\n%s", want, data)
		}
	}
}

func TestMonitorUnknownIDReturnsError(t *testing.T) {
	rn := newRunnerForTest(t)
	err := rn.Monitor(context.Background(), "missing", 0, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `unknown job id "missing"`) {
		t.Fatalf("Monitor missing id error: got %v", err)
	}
}
