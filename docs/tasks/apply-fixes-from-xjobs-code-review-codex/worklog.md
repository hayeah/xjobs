---
status: done
section: Apply fixes from xjobs code review (codex)
slug: apply-fixes-from-xjobs-code-review-codex
mode: worktree
spec: spec.md
created: 2026-05-13T10:13:24Z
---

> ## Apply fixes from xjobs code review (codex)
>
> ---
> status:
>   type: open
> ---
>
> Two independent code reviews of xjobs landed in their respective workspaces (sections closed; spec.md kept as reference):
>
> - claude review (26 findings, P0–P3):
>   `/Users/me/Dropbox/boss/tasks/code-review-xjobs-and-add-unit-e2e-tests/spec.md`
> - codex review (8 findings, High–Low):
>   `/Users/me/Dropbox/boss/tasks/2nd-opinion-code-review-of-xjobs-codex/spec.md`
>
> Read both. Apply the fixes you judge worth doing inline, write unit + e2e tests covering what you fix, and surface things you intentionally skip in `## Trouble report` so the boss can spin them out as follow-up sections.
>
> Already addressed by recent merges — don't re-do:
>
> - B8 (`--nice` negative silently no-op) — flag deleted by `d2076e9`; subsumed.
>
> Design call before implementing:
>
> - B6 / codex-M3 (`output.log` truncated on each retry, loses earlier-attempt evidence) — needs a decision. Two reasonable shapes: append with `--- attempt N at <ts> ---` separators, or per-attempt files (`output.log.1`, `output.log.2`, with `output.log` symlink/copy of most recent). Pick one, justify in the commit body, ship it. Agent's call.
>
> E2e tests should cover at minimum:
>
> - Exit-code matrix: clean success (0), stuck-failed row (1), pump parse error (1), SIGINT mid-drain (1).
> - Retry: synthetic-failing worker + `max_attempts: 3` succeeds on attempt 2.
> - Resume: `kill -9 xjobs` mid-job, re-run xjobs, reaped + resumed.
> - Concurrent workers (sleep workers, all complete).
> - Process-group cleanup: shell that spawns a background grandchild — verify no descendant survives after SIGINT to xjobs.
> - Path-traversal rejection: `job_id` with `/`, `..`, etc. is rejected by `Job.validate()`.
> - ls and monitor verbs (incl. `monitor --id <bad>` returning an error fast instead of blocking forever).
>
> Repo: `~/github.com/hayeah/xjobs/` (master tip is `d2076e9`).
>
> - [ ] implement and verify

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

- [x] Read the boss loop instructions, spawned section, current worklog, and both review specs.
- [x] Check out `github.com/hayeah/xjobs` through `boss checkout`.
- [x] Write the implementation spec for this fix pass.
- [x] Implement safe job-id validation and safe per-job filesystem paths.
- [x] Preserve retry logs with append separators and document the design call in commit history.
- [x] Fix drain/pump exit-code propagation, queue in-flight accounting, signal finalization, and process-group cleanup.
- [x] Fix monitor bad-id, `ls` signal context, running event error logging, reaper count accuracy, and README wording.
- [x] Add focused unit tests for the runner fixes.
- [x] Add required CLI e2e tests.
- [x] Run verification, commit remaining changes, and fill evidence/trouble report.

## Agent log

- 2026-05-13T10:14:27Z — Started work, checked out `github.com/hayeah/xjobs` via `boss checkout`, read both review specs, and wrote `spec.md` for the implementation plan.
- 2026-05-13T10:24:00Z — Landed xjobs correctness fixes in `48901f6`: path-id rejection, pump/drain exit errors, local in-flight queue accounting, interrupted-job finalization, process-group kill, monitor bad-id, signal-aware ls, reaper count, and docs.
- 2026-05-13T10:24:10Z — Landed regression coverage in `44e3ed1`: runner unit tests plus real-binary CLI e2e tests for the required matrix.

## Boss log

## Evidence

Repo: `repos/github.com/hayeah/xjobs`

Commits:

- `48901f6` — `Fix xjobs drain and retry correctness`
- `44e3ed1` — `Add xjobs review regression coverage`

Verification:

```text
$ go test -count=1 ./...
ok  	github.com/hayeah/xjobs/cmd/xjobs	6.089s
ok  	github.com/hayeah/xjobs/internal/runner	0.318s
```

Coverage notes:

- `cmd/xjobs/e2e_test.go` builds a real `xjobs` binary and covers clean success, terminal failed exit, pump parse exit, SIGINT mid-drain, retry success on attempt 2 with retained logs, resume after killed runner, concurrent workers, process-group cleanup, `ls`, `monitor --id <good>`, and `monitor --id <bad>`.
- `internal/runner/fixes_test.go` covers path-shaped id rejection through `Job.validate()`, terminal failed drain errors, retry output append separators, and monitor unknown-id errors.

## Trouble report

- Skipping for now unless implementation changes the calculus: schema `CHECK` hardening, timestamp normalization, monitor poll configurability, LS text table polish, LS JSON `meta`, pump transaction batching, and state-dir lazy creation. These are lower-risk follow-ups relative to the required exit/signal/retry/process fixes.
- Also intentionally skipped B15 (skip-and-continue malformed JSONL) because returning the pump error while retaining already-inserted rows is now explicit behavior, and B16 (multi-error joining) because the new e2e-visible failures are covered by primary errors without changing the reporting shape.
