package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var xjobsBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "xjobs-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	xjobsBin = filepath.Join(dir, "xjobs")
	cmd := exec.Command("go", "build", "-o", xjobsBin, ".")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestE2EExitCodeMatrix(t *testing.T) {
	t.Run("clean success exits zero", func(t *testing.T) {
		state := stateDir(t)
		stdout, _, err := runXJobs(t, state, jobJSONL(t, "ok", []string{"/usr/bin/true"}, 0))
		if code := exitCode(err); code != 0 {
			t.Fatalf("exit code: got %d, err=%v", code, err)
		}
		if !strings.Contains(stdout, `"kind":"success"`) {
			t.Fatalf("stdout missing success event:\n%s", stdout)
		}
	})

	t.Run("stuck failed row exits one", func(t *testing.T) {
		state := stateDir(t)
		_, stderr, err := runXJobs(t, state, jobJSONL(t, "fail", []string{"/bin/sh", "-c", "exit 7"}, 0))
		if code := exitCode(err); code != 1 {
			t.Fatalf("exit code: got %d, want 1, stderr=%s err=%v", code, stderr, err)
		}
		job := readJob(t, state, "fail")
		if job.Status != "failed" || job.Attempts != 1 {
			t.Fatalf("job state: %+v, want failed attempt 1", job)
		}
	})

	t.Run("pump parse error exits one", func(t *testing.T) {
		state := stateDir(t)
		_, stderr, err := runXJobs(t, state, "{bad json}\n")
		if code := exitCode(err); code != 1 {
			t.Fatalf("exit code: got %d, want 1, stderr=%s err=%v", code, stderr, err)
		}
		if !strings.Contains(stderr, "pump stdin") {
			t.Fatalf("stderr missing pump error:\n%s", stderr)
		}
	})

	t.Run("path traversal id exits one", func(t *testing.T) {
		state := stateDir(t)
		_, stderr, err := runXJobs(t, state, jobJSONL(t, "../escape", []string{"/usr/bin/true"}, 0))
		if code := exitCode(err); code != 1 {
			t.Fatalf("exit code: got %d, want 1, stderr=%s err=%v", code, stderr, err)
		}
		if !strings.Contains(stderr, "path separators") {
			t.Fatalf("stderr missing validation error:\n%s", stderr)
		}
	})
}

func TestE2ESIGINTMidDrainFinalizesFailed(t *testing.T) {
	state := stateDir(t)
	cmd := xjobsCommand(state)
	cmd.Stdin = strings.NewReader(jobJSONL(t, "sig", []string{"/bin/sleep", "30"}, 0))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForLine(t, stdout, `"kind":"running"`)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt xjobs: %v", err)
	}
	err = cmd.Wait()
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code: got %d, want 1, stderr=%s err=%v", code, stderr.String(), err)
	}
	job := readJob(t, state, "sig")
	if job.Status != "failed" || !job.Signal.Valid {
		t.Fatalf("job state after SIGINT: %+v, want failed with signal", job)
	}
}

func TestE2ERetrySucceedsOnSecondAttemptAndKeepsLogs(t *testing.T) {
	state := stateDir(t)
	script := writeExecutable(t, "retry.sh", `#!/bin/sh
case "$XJOBS" in
  *'"attempt":1'*) echo first-fail; exit 42 ;;
  *) echo second-ok; exit 0 ;;
esac
`)
	stdout, stderr, err := runXJobs(t, state, jobJSONL(t, "retry", []string{script}, 3))
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code: got %d, want 0, stdout=%s stderr=%s err=%v", code, stdout, stderr, err)
	}
	job := readJob(t, state, "retry")
	if job.Status != "done" || job.Attempts != 2 {
		t.Fatalf("job state: %+v, want done attempt 2", job)
	}
	log := readFile(t, filepath.Join(state, "retry", "output.log"))
	for _, want := range []string{"--- attempt 1 at ", "first-fail", "--- attempt 2 at ", "second-ok"} {
		if !strings.Contains(log, want) {
			t.Fatalf("output.log missing %q:\n%s", want, log)
		}
	}
}

func TestE2EDrainReapsAndRerunsAfterKilledRunner(t *testing.T) {
	state := stateDir(t)
	script := writeExecutable(t, "slow-ok.sh", `#!/bin/sh
sleep 0.4
echo slow-ok
`)
	cmd := xjobsCommand(state)
	cmd.Stdin = strings.NewReader(jobJSONL(t, "stranded", []string{script}, 0))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForLine(t, stdout, `"kind":"running"`)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill xjobs: %v", err)
	}
	_ = cmd.Wait()
	time.Sleep(700 * time.Millisecond)

	// Bare xjobs with no positional and no stdin = drain-only (the
	// behavior `resume` used to give us via a dedicated verb).
	_, drainStderr, err := runXJobs(t, state, "")
	if code := exitCode(err); code != 0 {
		t.Fatalf("drain code: got %d, want 0, stderr=%s err=%v", code, drainStderr, err)
	}
	job := readJob(t, state, "stranded")
	if job.Status != "done" || job.Attempts != 2 {
		t.Fatalf("job state after drain: %+v, want done attempt 2", job)
	}
}

func TestE2EConcurrentWorkersComplete(t *testing.T) {
	state := stateDir(t)
	var plan strings.Builder
	for i := 0; i < 4; i++ {
		plan.WriteString(jobJSONL(t, fmt.Sprintf("sleep-%d", i), []string{"/bin/sh", "-c", "sleep 0.2"}, 0))
	}
	_, stderr, err := runXJobs(t, state, plan.String(), "--workers", "4")
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code: got %d, want 0, stderr=%s err=%v", code, stderr, err)
	}
	if got := countJobs(t, state, "done"); got != 4 {
		t.Fatalf("done jobs: got %d want 4", got)
	}
}

func TestE2EProcessGroupCleanupOnSIGINT(t *testing.T) {
	state := stateDir(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	childScript := fmt.Sprintf("sleep 30 & echo $! > %s; wait", shellQuote(pidFile))
	cmd := xjobsCommand(state)
	cmd.Stdin = strings.NewReader(jobJSONL(t, "pg", []string{"/bin/sh", "-c", childScript}, 0))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForLine(t, stdout, `"kind":"running"`)
	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt xjobs: %v", err)
	}
	err = cmd.Wait()
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code: got %d, want 1, stderr=%s err=%v", code, stderr.String(), err)
	}
	waitForDeadProcess(t, pid)
}

func TestE2ELSAndMonitorVerbs(t *testing.T) {
	state := stateDir(t)
	_, stderr, err := runXJobs(t, state, jobJSONL(t, "inspect", []string{"/usr/bin/true"}, 0))
	if code := exitCode(err); code != 0 {
		t.Fatalf("seed xjobs: code=%d stderr=%s err=%v", code, stderr, err)
	}

	lsOut, lsErrOut, err := runXJobs(t, state, "", "ls", "--json")
	if code := exitCode(err); code != 0 {
		t.Fatalf("ls code=%d stderr=%s err=%v", code, lsErrOut, err)
	}
	if !strings.Contains(lsOut, `"id":"inspect"`) || !strings.Contains(lsOut, `"status":"done"`) {
		t.Fatalf("ls output missing inspect done row:\n%s", lsOut)
	}

	mon := xjobsCommand(state, "monitor", "--id", "inspect")
	monOut, err := mon.StdoutPipe()
	if err != nil {
		t.Fatalf("monitor StdoutPipe: %v", err)
	}
	var monErr bytes.Buffer
	mon.Stderr = &monErr
	if err := mon.Start(); err != nil {
		t.Fatalf("monitor Start: %v", err)
	}
	line := waitForLine(t, monOut, `"id":"inspect"`)
	if !strings.Contains(line, `"kind":"success"`) {
		t.Fatalf("monitor first line: %s, want success event", line)
	}
	_ = mon.Process.Kill()
	_ = mon.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, badErrOut, err := runXJobsContext(ctx, t, state, "", "monitor", "--id", "missing")
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("monitor --id missing blocked; stderr=%s", badErrOut)
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("monitor bad id code=%d, want 1, stderr=%s err=%v", code, badErrOut, err)
	}
	if !strings.Contains(badErrOut, "unknown job id") {
		t.Fatalf("monitor bad id stderr missing unknown id:\n%s", badErrOut)
	}
}

type jobState struct {
	Status   string
	Attempts int
	Signal   sql.NullString
}

func stateDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".xjobs")
}

func jobJSONL(t *testing.T, id string, argv []string, maxAttempts int) string {
	t.Helper()
	obj := map[string]any{
		"id":   id,
		"argv": argv,
	}
	if maxAttempts > 0 {
		obj["max_attempts"] = maxAttempts
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	return string(b) + "\n"
}

func runXJobs(t *testing.T, state, stdin string, args ...string) (string, string, error) {
	t.Helper()
	return runXJobsContext(context.Background(), t, state, stdin, args...)
}

func runXJobsContext(ctx context.Context, t *testing.T, state, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := xjobsCommandContext(ctx, state, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func xjobsCommand(state string, args ...string) *exec.Cmd {
	return xjobsCommandContext(context.Background(), state, args...)
}

func xjobsCommandContext(ctx context.Context, state string, args ...string) *exec.Cmd {
	full := []string{}
	if len(args) > 0 && isVerb(args[0]) {
		full = append(full, args[0], "--state-dir", state)
		full = append(full, args[1:]...)
	} else {
		full = append(full, "--state-dir", state)
		full = append(full, args...)
	}
	return exec.CommandContext(ctx, xjobsBin, full...)
}

func isVerb(arg string) bool {
	switch arg {
	case "run", "ls", "monitor":
		return true
	default:
		return false
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func waitForLine(t *testing.T, r io.Reader, contains string) string {
	t.Helper()
	lines := make(chan string, 1)
	errs := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, contains) {
				lines <- line
				return
			}
		}
		if err := sc.Err(); err != nil {
			errs <- err
			return
		}
		errs <- io.EOF
	}()
	select {
	case line := <-lines:
		return line
	case err := <-errs:
		t.Fatalf("waiting for line containing %q: %v", contains, err)
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for line containing %q", contains)
	}
	return ""
}

func writeExecutable(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func readJob(t *testing.T, state, id string) jobState {
	t.Helper()
	db := openTestDB(t, state)
	defer db.Close()
	var job jobState
	if err := db.QueryRow(`SELECT status, attempts, signal FROM jobs WHERE job_id = ?`, id).Scan(&job.Status, &job.Attempts, &job.Signal); err != nil {
		t.Fatalf("read job %q: %v", id, err)
	}
	return job
}

func countJobs(t *testing.T, state, status string) int {
	t.Helper()
	db := openTestDB(t, state)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status = ?`, status).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

func openTestDB(t *testing.T, state string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "db.sql3")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return db
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); scanErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}

func waitForDeadProcess(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after process-group cleanup", pid)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
