# Apply xjobs Review Fixes

## Goal

Apply the correctness and observability fixes from the two xjobs review artifacts that are worth shipping now, with unit and e2e coverage for the required behavior matrix. This pass prioritizes bugs that change exit status, lose job evidence, leak processes, hang user-facing commands, or allow unsafe state-dir paths. Lower-severity schema hardening, formatting polish, and future feature work stay out of scope and will be listed in the worklog trouble report if skipped.

## Architecture

- `cmd/xjobs/main.go`
  - Propagate pump errors from the pump goroutine back to `cmdRun`.
  - Use a signal-aware context for `ls`.
  - Remove the stale `strings` keepalive once no longer needed.
- `internal/runner/job.go`
  - Extend `Job.validate()` to reject path-shaped or traversal-capable job ids.
- `internal/runner/service.go`
  - Map job ids to a filesystem-safe directory name before touching locks/logs.
  - Preserve retry evidence by appending to `output.log` with attempt separators.
  - Replace `exec.CommandContext` cancellation with process-group termination so descendants are killed on interrupt.
- `internal/runner/runner.go`, `claim.go`, `reap.go`, and `events.go`
  - Keep local in-flight accounting so `feedQueue` does not exit while a queued row has not yet claimed.
  - Return an error when terminal failed rows remain after drain, respecting `--where`.
  - Finalize rows/events with a bounded non-canceled context after child exit, even when the lifecycle context was canceled.
  - Surface best-effort running event errors to stderr and fix reaper double-counting.
- `internal/runner/inspect.go`
  - Make `monitor --id <bad>` return an error immediately.
  - Keep monitor id semantics exact-match and update README wording.
- Tests
  - Add focused runner unit tests for id validation, terminal failed detection, and monitor bad-id handling.
  - Add command e2e tests that build the xjobs binary once and exercise real SQLite state dirs, signals, retry, resume, concurrent workers, process-group cleanup, ls, and monitor.

## Steps

- Read both review specs, inspect current master-tip code, and write this implementation spec.
- Implement safe job-id validation and filesystem directory mapping.
- Implement retry log retention via append separators and document the design call.
- Fix drain exit-code propagation, pump error propagation, signal finalization, in-flight queue accounting, and process-group cleanup.
- Fix monitor bad-id, `ls` signal context, running event error logging, reaper count accuracy, and README wording.
- Add unit tests for validation, failed-row drain status, retry log content, and monitor id handling.
- Add e2e tests for the required CLI matrix: success, stuck failed, pump parse error, SIGINT, retry, resume, concurrency, process-group cleanup, traversal rejection, ls, and monitor.
- Run `go test ./...` plus any targeted e2e command needed for evidence.
- Commit in focused slices and keep `worklog.md` updated after each meaningful step.

## Verification

- `go test ./...`
- The e2e suite must cover:
  - clean success exits `0`
  - stuck failed row exits `1`
  - pump parse error exits `1`
  - SIGINT mid-drain exits `1`
  - synthetic worker with `"max_attempts":3` succeeds on attempt 2
  - killed xjobs process leaves a running row that a later `resume` reaps and completes
  - concurrent sleep workers all complete
  - shell-spawned background descendant does not survive SIGINT to xjobs
  - path-shaped ids are rejected by validation/pump
  - `ls` returns status rows and `monitor --id <bad>` errors without blocking

## Open questions

- None. For B6 / codex-M3, this spec picks append-with-separators because it preserves a single stable path for users tailing `.xjobs/<id>/output.log` while retaining earlier attempts. Per-attempt files would be cleaner for tooling but would change the primary log navigation story and require deciding symlink/copy behavior across platforms.

## Design notes

- 2026-05-13T10:14:27Z - Chose append-with-attempt-separators for retry output retention.
  - Alternatives considered:
    - Per-attempt files plus `output.log` symlink/copy: stronger machine boundaries and easy archival, but introduces symlink portability/copy freshness choices and makes the documented `tail -f .xjobs/<id>/output.log` path less obvious during retries.
    - Append to one `output.log` with separators (picked): preserves the documented single log path, works without symlinks, and keeps first-attempt evidence visible after later success. The trade-off is that tooling must parse separators if it wants exact per-attempt chunks.
