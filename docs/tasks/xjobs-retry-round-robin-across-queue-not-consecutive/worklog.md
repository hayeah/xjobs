---
status: done
section: xjobs retry: round-robin across queue, not consecutive
slug: xjobs-retry-round-robin-across-queue-not-consecutive
mode: worktree
spec: spec.md
created: 2026-05-13T09:37:54Z
---

> ## xjobs retry: round-robin across queue, not consecutive
>
> ---
> status:
>   type: open
> ---
>
> On a job failure, xjobs should move on to the next job in the queue and loop back for the retry attempt later — instead of retrying the same failing job consecutively. A single slow-to-fix job shouldn't block siblings from making progress.
>
> Likely shape:
> - On failure, re-enqueue the row with `attempt+1` and an ordering key that puts it behind other ready work (depends on the schema's ordering column — the integer-PK restructure landing in `drain-xjobs-queue-in-insertion-order-via-integer-primary-key` is the foundation).
> - Drain picks the next ready row by the queue's natural order; the just-failed row naturally falls to the back.
> - Bounded by `--max-attempts` as today.
>
> Read the PK-restructure section's worklog before starting — it changes the ordering column and the schema invariants this builds on.
>
> Repo: `~/github.com/hayeah/xjobs/`.
>
> - [ ] implement and verify

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

- [x] `fetchBatch`: `ORDER BY id` → `ORDER BY attempts, id` in `internal/runner/claim.go`.
- [x] Add `TestRetryRoundRobin_OneFailureYieldsToSiblings` to `internal/runner/order_test.go`.
- [x] Add `TestRetryRoundRobin_AllFailedSiblingsRotateByAttempts` to `internal/runner/order_test.go`.
- [x] Confirm existing tests still pass (insertion order preserved at attempts=0).
- [x] `go test ./...` and `go vet ./...` clean.
- [x] CLI smoke: jobs pumped mid-drain, capture event interleave into `tmp/164409_435-xjobs-retry-smoke/`.
- [x] Update SPEC.md "Work-queue predicate" section with the new ORDER BY + round-robin paragraph.
- [x] Update README.md retry mention with a one-line fairness note.
- [x] Default `--max-attempts` 3 → 1 (per user nudge mid-work); doc the new floor in SPEC + README flag tables.
- [x] Fill `## Evidence`, set `status: done`.

## Agent log
- 2026-05-13T09:43Z ORDER BY change + 2 round-robin tests pass (1c0c5bc); full go test ./... + go vet clean
- 2026-05-13T09:50Z default --max-attempts 3 → 1 + SPEC/README round-robin docs (2fcb64d); per-user request bundled into this section

## Boss log

## Evidence

Branch `xjobs-retry-round-robin-across-queue-not-consecutive` in `~/github.com/hayeah/xjobs`, two commits ahead of `master`:

```
2fcb64d Default --max-attempts to 1; document round-robin retry rule
1c0c5bc Round-robin retries: ORDER BY attempts, id
```

### What changed

- `internal/runner/claim.go` — `fetchBatch` `ORDER BY id` → `ORDER BY attempts, id`. A row that just failed at attempt `N` carries `attempts=N` in the DB; any sibling at `attempts<N` outranks it on the next scan. Insertion order is preserved as the within-bucket tiebreaker.
- `internal/runner/runner.go` + `cmd/xjobs/main.go` — `--max-attempts` default 3 → 1. The drain predicate (`attempts < MaxAttempts`) is unchanged; only the floor moved. `1` = no auto-retry.
- `internal/runner/order_test.go` — new tests:
  - `TestRetryRoundRobin_OneFailureYieldsToSiblings` — pump A,B,C; simulate A claim+fail; assert `fetchBatch` returns `[B, C, A]` (A's attempts=1 outranked by siblings at 0).
  - `TestRetryRoundRobin_AllFailedSiblingsRotateByAttempts` — pump A,B,C; fail all three at attempt 1; then fail A again; assert order is `[B, C, A]` (B,C at attempts=1 outrank A at attempts=2).
- `SPEC.md` — work-queue-predicate section rewritten to describe the new ORDER BY and round-robin retry rule; flag table updated for the new default.
- `README.md` — flag table updated; one-line round-robin note added to the `--max-attempts` row.

### Tests

`go test ./internal/runner -count=1 -v`:

```
=== RUN   TestPumpWorkQueueInsertionOrder
--- PASS: TestPumpWorkQueueInsertionOrder (0.00s)
=== RUN   TestPumpIDIsMonotonic
--- PASS: TestPumpIDIsMonotonic (0.00s)
=== RUN   TestPumpDuplicatesAreIgnored
--- PASS: TestPumpDuplicatesAreIgnored (0.00s)
=== RUN   TestJobsSchemaShape
--- PASS: TestJobsSchemaShape (0.00s)
=== RUN   TestRetryRoundRobin_OneFailureYieldsToSiblings
--- PASS: TestRetryRoundRobin_OneFailureYieldsToSiblings (0.00s)
=== RUN   TestRetryRoundRobin_AllFailedSiblingsRotateByAttempts
--- PASS: TestRetryRoundRobin_AllFailedSiblingsRotateByAttempts (0.00s)
PASS
ok  	github.com/hayeah/xjobs/internal/runner	0.481s
```

`go vet ./...` clean. `go test ./...` clean.

### CLI smoke — bug reproduction (old) vs. fix (new)

`tmp/164409_435-xjobs-retry-smoke/staged-{old,new}-v2/events.jsonl`. Same input both times, run against `/tmp/xjobs-old` (built from `master`'s `claim.go`) and `/tmp/xjobs-rr` (this branch). Staged pump: `alpha` at t=0, sleep 0.8s, `bravo` at t=0.8s. `--workers 1 --max-attempts 5`. Both jobs are `bash -c "sleep 0.1; exit 1"`.

Sequence of `running` events:

```
OLD (master, ORDER BY id):
  alpha:1  alpha:2  alpha:3  alpha:4  bravo:1  alpha:5  bravo:2  bravo:3  bravo:4  bravo:5

NEW (this branch, ORDER BY attempts, id):
  alpha:1  alpha:2  alpha:3  bravo:1  alpha:4  bravo:2  alpha:5  bravo:3  bravo:4  bravo:5
```

Read: old code burns 4 alpha retries before bravo gets a turn, then `alpha:5` still preempts `bravo:2` (alpha has lower id and the old ORDER BY can't see the attempts gap). New code lets bravo cut in as soon as it lands (`bravo:1` before `alpha:4`), and from that point on retries truly alternate. Each row is at most one attempt ahead of its siblings.

Reproducibility: 5 independent runs of each binary produced identical sequences (`tmp/164409_435-xjobs-retry-smoke/repro-{old,new}-*.jsonl`).

### CLI smoke — default --max-attempts = 1

A single `/usr/bin/false` job under the new default emits exactly one `running` + one `error` event and exits:

```
{"...","kind":"running","id":"x","attempt":1,...}
{"...","kind":"error","id":"x","attempt":1,...,"exit":1,"error":"exit 1"}
```

No retry. Users opt in with `--max-attempts N`.

## Trouble report

- First-pass CLI smoke (3 always-failing jobs pumped together, `--workers 1`) showed identical interleave on old vs new code. The naive read would be "the fix is a no-op." It isn't — `feedQueue`'s `seen` map gives natural round-robin when all rows are in the channel together. The bug only surfaces when timing causes one row's failures to interleave with another row's arrival or running state at scan time. Captured the redesigned staged-pump smoke (`staged-{old,new}-v2`) that actually exhibits the difference. Lesson: e2e behavior here is timing-sensitive; the unit tests assert the SQL-level invariant directly to avoid flake.
- The `--max-attempts` default change was a mid-section nudge from the user, not part of the original section text. I bundled it (same commit-locality, related semantic). If the boss prefers it in its own section, the commit (`2fcb64d`) cleanly isolates it from the ORDER BY change (`1c0c5bc`).
- Default change has user-visible impact: anyone relying on the implicit 3-retry default needs to pass `--max-attempts 3` explicitly now. Worth a release note.
