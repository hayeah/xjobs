package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// runResult summarizes one attempt's outcome.
type runResult struct {
	ExitCode int    // 0 on success; valid when Signal == ""
	Signal   string // symbolic name when killed by signal; otherwise ""
	Err      error  // setup / spawn error (no exit info available)
	PID      int    // child PID once spawned
}

// execAttempt runs one attempt for jobRow. It:
//
//  1. ensures .xjobs/<id>/ exists,
//  2. flocks .xjobs/<id>/lock for the child's lifetime,
//  3. writes XJOBS env + opens output.log,
//  4. spawns the child with that env / cwd / stdio,
//  5. waits and returns the exit info.
//
// The flock is the liveness signal the reaper probes — held while the
// child is alive, released on this function's return (success, failure,
// or panic via defer).
func execAttempt(ctx context.Context, stateDir string, row jobRow, dbPath string, niceN int) runResult {
	jobDir := filepath.Join(stateDir, row.ID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return runResult{Err: fmt.Errorf("mkdir %s: %w", jobDir, err)}
	}

	lock, ok, err := flockAcquire(filepath.Join(jobDir, "lock"))
	if err != nil {
		return runResult{Err: fmt.Errorf("acquire lock: %w", err)}
	}
	if !ok {
		// Another xjobs in this state dir already holds this row. Treat as
		// a soft error and let the caller re-queue / move on.
		return runResult{Err: errors.New("job lock held by another runner")}
	}
	defer lock.Close()

	logPath := filepath.Join(jobDir, "output.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return runResult{Err: fmt.Errorf("open log %s: %w", logPath, err)}
	}
	defer logFile.Close()

	xjobsEnv := xjobsEnvJSON(dbPath, stateDir, row.ID, row.Attempt)

	cmd := exec.CommandContext(ctx, row.Argv[0], row.Argv[1:]...)
	cmd.Dir = row.CWD
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = buildEnv(os.Environ(), row.Env, "XJOBS="+xjobsEnv)

	// Run in its own process group so we can deliver signals to the whole
	// tree on shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return runResult{Err: fmt.Errorf("start: %w", err)}
	}

	res := runResult{PID: cmd.Process.Pid}

	// Apply nice if requested. Best-effort; ignore failures.
	if niceN > 0 {
		_ = setpriority(cmd.Process.Pid, niceN)
	}

	waitErr := cmd.Wait()
	if waitErr == nil {
		res.ExitCode = 0
		return res
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		ws := ee.Sys().(syscall.WaitStatus)
		if ws.Signaled() {
			sig := ws.Signal()
			res.Signal = unix.SignalName(sig)
			if res.Signal == "" {
				res.Signal = sig.String()
			}
			res.ExitCode = 128 + int(sig)
		} else {
			res.ExitCode = ws.ExitStatus()
		}
		return res
	}
	res.Err = waitErr
	return res
}

func xjobsEnvJSON(dbPath, stateDir, jobID string, attempt int) string {
	buf, _ := json.Marshal(map[string]any{
		"db":        dbPath,
		"state_dir": stateDir,
		"job_id":    jobID,
		"attempt":   attempt,
	})
	return string(buf)
}

// buildEnv merges base env, per-job overrides, and runner-injected vars.
// Per-job env wins over base; injected wins over per-job.
func buildEnv(base []string, jobEnv map[string]string, inject ...string) []string {
	merged := make(map[string]string, len(base)+len(jobEnv)+len(inject))
	for _, kv := range base {
		k, v := splitKV(kv)
		merged[k] = v
	}
	for k, v := range jobEnv {
		merged[k] = v
	}
	for _, kv := range inject {
		k, v := splitKV(kv)
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

func splitKV(kv string) (string, string) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:]
		}
	}
	return kv, ""
}
