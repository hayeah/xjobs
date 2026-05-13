---
status: done
section: Single-drainer lock per state-dir with --pump-if-up fallback
slug: single-drainer-lock-per-state-dir-with-pump-if-up-fallback
mode: worktree
spec:
created: 2026-05-13T13:58:39Z
---

> ## Single-drainer lock per state-dir with --pump-if-up fallback
>
> ---
> status:
>   type: open
> ---
>
> Enforce single-drainer-per-state-dir via `<state_dir>/runner.lock` (singular, distinct from per-job `<state_dir>/<id>/lock`). The lock is always tried — no flag to opt out of safety.
>
> Behavior of `xjobs [file.jsonl]` / `xjobs run`:
>
> 1. Open DB.
> 2. Pump input (if any) — always. `INSERT OR IGNORE` is already safe under concurrent writers.
> 3. Try `flock(LOCK_EX|LOCK_NB)` on `<state_dir>/runner.lock`.
>    - **Acquired**: run reaper, then drain as today. Hold the lock for the runner's lifetime; OS releases on exit or crash.
>    - **Held + no flag**: print one stderr line (e.g. `xjobs: runner.lock held by pid N; pumped X / skipped Y; not draining`) and exit **1** (loud failure).
>    - **Held + `--pump-if-up`**: print the same informational line and exit **0** (the live runner will drain the pumped rows on its next tick).
> 4. The no-input "drain only" path (`xjobs < /dev/null`, today's resume use case) goes through the same lock: acquired → drain; held → exits 0 with `--pump-if-up`, exits 1 without.
>
> `ls` and `monitor` are unaffected — read-only, don't touch the lock.
>
> Flag:
>
> ```
> --pump-if-up    If another xjobs runner already holds the
>                 state-dir lock, pump new rows and exit 0
>                 (the live runner drains them). Without this
>                 flag, that case fails with exit 1.
> ```
>
> Implementation notes:
>
> - Lock path: `<state_dir>/runner.lock` (singular).
> - Reuse the flock primitive that per-job claim already uses.
> - Reaper ordering: acquire the state-dir lock *before* the reaper pass. If the lock is held by another runner, skip the reaper entirely — that runner's reaper handles its own strays. Running the reaper from a pump-only invocation would race the live drainer's per-job locks.
> - Lock fd lifetime ties to the runner process: deferred close alongside the existing ticker stop in `runner.go`.
>
> E2e tests in `cmd/xjobs/e2e_test.go`:
>
> - Two drainers without flag: start a drainer in a goroutine against a tmp state-dir with a slow job pending. Run a second `xjobs file.jsonl` against the same state-dir. Assert: exit code 1, stderr mentions the lock, the new row is still pumped.
> - Two drainers with flag: same setup; run second invocation with `--pump-if-up`. Assert: exit 0 quickly, new row claimed and completed by the first drainer within a few ticks.
> - Single drainer regression: existing tests should still pass — first invocation acquires the lock cleanly.
>
> Repo: `~/github.com/hayeah/xjobs/` (master tip is `caaae10`).
>
> - [ ] implement and verify

## Todos

- [x] Add `acquireRunnerLock` helper (flock + write/read holder PID via runner.lock file contents).
- [x] Expose `Runner.AcquireDrainerLock` + `Runner.ReapStaleRunning` so cmdRun controls ordering.
- [x] Refactor `Drain` to NOT call reaper internally — caller runs reaper after lock acquisition.
- [x] Rework `cmdRun`: open DB → open input → try lock → branch.
  - Lock acquired: reaper, then drain (concurrent pump as today).
  - Lock held + `--pump-if-up`: synchronous pump, stderr line, exit 0.
  - Lock held + no flag: synchronous pump, stderr line, exit 1.
- [x] Add `--pump-if-up` flag to `cmdRun`.
- [x] Drain-only path (no input) routes through the same lock.
- [x] E2e: TestE2EPumpIfUpDrainerLockHeldExitsOne — two-drainer no-flag (exit 1, row pumped).
- [x] E2e: TestE2EPumpIfUpDrainerLockHeldDrainsThroughLiveRunner — with-flag (exit 0, row eventually drained).
- [x] Regression: existing single-drainer tests pass.
- [x] `go test ./...` clean; capture transcript to `tmp/`.
- [x] Update README.md + SPEC.md to document `--pump-if-up` and `runner.lock`.

## Design notes

- **Pump ordering decision.** Section text lists "1. Open DB. 2. Pump. 3. Try lock." I'm reading this as a *user-visible-semantics* listing, not an implementation order. The implementation acquires the lock first (decisive step); on success it runs pump in background concurrently with drain (today's behavior — preserves the "Workers begin claiming as rows land" SPEC.md promise); on contention it pumps synchronously (no drain to be concurrent with) and then exits. The end-state observable to the user matches the section: pump always happens; drain only when lock acquired.
- **PID-in-lock-file.** To surface the holder's PID in the stderr message, we write our own PID into `runner.lock` after acquiring (truncate + write). On contention, the contender reads the file to get the holder's PID. Failure to read or parse falls back to PID 0 in the message — non-fatal.

## Agent log
- 2026-05-13T14:04Z core impl landed (24b4859 internal/runner, 57fd5ae cmd/xjobs). existing tests still pass; next: new e2e cases.
- 2026-05-13T14:08Z all todos done. 4 commits, 9 e2e tests pass (2 new), docs updated, live CLI demo captured in tmp/cli_demo.txt. status:done.

## Boss log

## Evidence

### Test suite — `go test -v -count=1 -run 'TestE2E' ./cmd/xjobs/`

```
=== RUN   TestE2EExitCodeMatrix
=== RUN   TestE2EExitCodeMatrix/clean_success_exits_zero
=== RUN   TestE2EExitCodeMatrix/stuck_failed_row_exits_one
=== RUN   TestE2EExitCodeMatrix/pump_parse_error_exits_one
=== RUN   TestE2EExitCodeMatrix/path_traversal_id_exits_one
--- PASS: TestE2EExitCodeMatrix (1.77s)
=== RUN   TestE2ESIGINTMidDrainFinalizesFailed
--- PASS: TestE2ESIGINTMidDrainFinalizesFailed (0.28s)
=== RUN   TestE2ERetrySucceedsOnSecondAttemptAndKeepsLogs
--- PASS: TestE2ERetrySucceedsOnSecondAttemptAndKeepsLogs (0.78s)
=== RUN   TestE2EDrainReapsAndRerunsAfterKilledRunner
--- PASS: TestE2EDrainReapsAndRerunsAfterKilledRunner (1.51s)
=== RUN   TestE2EConcurrentWorkersComplete
--- PASS: TestE2EConcurrentWorkersComplete (0.52s)
=== RUN   TestE2EProcessGroupCleanupOnSIGINT
--- PASS: TestE2EProcessGroupCleanupOnSIGINT (0.30s)
=== RUN   TestE2EPumpIfUpDrainerLockHeldExitsOne
--- PASS: TestE2EPumpIfUpDrainerLockHeldExitsOne (0.29s)
=== RUN   TestE2EPumpIfUpDrainerLockHeldDrainsThroughLiveRunner
--- PASS: TestE2EPumpIfUpDrainerLockHeldDrainsThroughLiveRunner (0.55s)
=== RUN   TestE2ELSAndMonitorVerbs
--- PASS: TestE2ELSAndMonitorVerbs (0.55s)
PASS
ok  	github.com/hayeah/xjobs/cmd/xjobs	6.940s
```

Full module: `go test ./...` → both packages PASS (`tmp/go_test.txt`).
Verbose e2e transcript: `tmp/go_test_e2e_v.txt`.

### Live CLI demo (`tmp/cli_demo.txt`)

First drainer (pid 7068) pumps a slow `sleep 2` job and starts draining.
While it's alive:

- `xjobs file.jsonl` (no flag) → exit **1**, stderr `xjobs: runner.lock held by pid 7068; pumped 1 / skipped 0; not draining`.
- `xjobs --pump-if-up file.jsonl` → exit **0**, same stderr line.

Both newly-pumped rows (`newrow`, `newrow2`) appear in the first drainer's
event stream and reach `success` within ~500 ms — proving the
"top-up while a live runner is up" flow works end-to-end.

### Commits on the branch

- `24b4859` internal/runner: add state-dir drainer lock + caller-driven reaper
- `57fd5ae` cmd/xjobs: --pump-if-up + runner.lock single-drainer enforcement
- `c0e1cc0` cmd/xjobs: e2e for --pump-if-up + lock-held branches
- `ec643c3` docs: --pump-if-up, runner.lock semantics, single-drainer section

## Trouble report

- No friction. The pre-existing `flockAcquire` primitive composed cleanly:
  one new helper (`acquireRunnerLock`) reused it and added PID-into-file
  for the holder-PID message.
- One judgment call: I sequence pump *concurrent with* drain on the
  lock-acquired branch (preserves the streaming SPEC promise), even
  though the section lists "pump, then try lock" as steps 2 and 3.
  Recorded in `## Design notes`.
