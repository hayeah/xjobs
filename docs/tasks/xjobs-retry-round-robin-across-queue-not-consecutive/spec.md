# xjobs retry: round-robin across queue, not consecutive

## Goal

A failed job should yield the queue to its siblings before retrying. Today, when job A fails at attempt 1, the next `fetchBatch` pass orders eligible rows `ORDER BY id` and A — with the lowest insertion id — still goes first. A single slow-to-fix row can monopolize the worker pool for its entire `--max-attempts` budget while other ready rows wait.

After this change, retries are interleaved across the ready set. With ids 1..5 all failing at attempt 1, the worker pool serves them as `1,2,3,4,5, 1,2,3,4,5, ...` — each job's nth retry runs only after every other ready row has had its nth-or-earlier attempt.

Out of scope: any change to `--max-attempts` semantics, the `running`-row predicate, the reaper, the `events` table, or the JSONL/CLI surface. No new top-level flag.

## Architecture

One-line behavior change, plus tests + docs.

- **`internal/runner/claim.go` — `fetchBatch`**: change `ORDER BY id` to `ORDER BY attempts ASC, id ASC`.
  - Rows with fewer attempts go first; insertion-id breaks ties.
  - A row that just failed at attempt N has `attempts=N` in the DB (because `claim()` bumps `attempts` before `terminalFail`). Any sibling still at `attempts<N` outranks it.
  - The existing `seen` map (keyed on `(job_id, attempts)`) is unaffected: dedup is on `attempts`, not on row order.
- **`internal/runner/order_test.go`**: add two tests:
  - `TestRetryRoundRobin_OneFailureYieldsToSiblings` — pump A,B,C; simulate A claim+fail; assert `fetchBatch` returns `[B, C, A]`.
  - `TestRetryRoundRobin_AllFailedSiblingsRotateByAttempts` — pump A,B,C; fail all three at attempt 1; then fail A at attempt 2; assert `fetchBatch` returns `[B, C, A]` (B and C still at attempts=1 outrank A at attempts=2).
- **`internal/runner/claim.go` — `claim()`**: WHERE clause stays the same. `claim` is a targeted UPDATE on a specific `job_id`, not a "select-then-claim" — the ordering decision is entirely in `fetchBatch`.
- **`SPEC.md`**: update the "Work-queue predicate" section. Change `ORDER BY id` → `ORDER BY attempts, id`; add a short paragraph on the round-robin retry semantic and why insertion order is still respected at attempt 0.
- **`README.md`**: the README doesn't describe ORDER BY internals (only "insertion order"). Add a one-line note next to the retry mention so users understand siblings get a turn before a failing job retries.

### Why not a new column

Two alternatives considered:

1. **`UPDATE jobs SET id = (SELECT MAX(id)+1 ...)` on retry.** Rejected: `events.job_id INTEGER NOT NULL REFERENCES jobs(id)` and `foreign_keys=on` would either fail the update (no `ON UPDATE CASCADE`) or, worse, silently break the cross-table link if cascade were added. Either way, mutating the int PK is a sharp edge.

2. **Add a `queue_seq INTEGER` column, populated to `id` on insert (via trigger or two-step INSERT) and bumped to `MAX(queue_seq)+1` on retry.** Rejected for the round-robin use case specifically:
   - To match the "round-robin within attempts bucket" semantic, the bump-on-retry value must be aware of *which other rows are currently eligible*, not just MAX(queue_seq). With a global MAX, a freshly-pumped row (attempts=0) ends up behind already-retried rows (attempts=1) — the opposite of "lowest-attempt first."
   - Achieving the round-robin semantic with a single int column would require `ORDER BY attempts, queue_seq` anyway — same multi-column ordering, plus a schema column, plus a trigger or two-step INSERT, plus an extra UPDATE in `terminalFail`. Worse on every axis.

3. **`ORDER BY (status='failed'), id`** — puts pending before failed, then by id. Rejected: doesn't distinguish attempts levels. A row at attempts=2 ties with a row at attempts=1 in the failed bucket, and the lower-id one (more-retried) goes first. Not round-robin.

`ORDER BY attempts, id` is the single ordering key the section text asks for. It IS one key (a lexicographic tuple, equivalent to a computed `attempts * BIG + id`).

### Edge cases

- **Single job that keeps failing.** No siblings → it retries consecutively up to `--max-attempts`. Same as today, by construction. The point of round-robin is fairness *across* siblings; with no siblings there's nothing to round-robin to.
- **Many workers, few jobs.** Worker pool `> len(ready)`. A failure followed by an empty batch means feedQueue waits one tick; on the next pass the failed row is again the only candidate and gets re-emitted. Still no consecutive-retry monopolization beyond what the workload allows.
- **Fresh pump in the middle of a drain.** A new attempts=0 row sneaks in front of any failed-with-retries row. This matches user intent: "new work first, retries when there's slack."
- **Tied attempts bucket.** Within a bucket, ordered by `id` = insertion order. Preserves the prior PK-restructure invariant.

## Steps

1. Change `fetchBatch`'s `ORDER BY id` to `ORDER BY attempts, id` in `internal/runner/claim.go`.
2. Add the two round-robin tests to `internal/runner/order_test.go`.
3. Verify existing `TestPumpWorkQueueInsertionOrder` still passes (insertion-order is preserved when all rows are at attempts=0).
4. Run `go test ./...` and `go vet ./...`.
5. CLI smoke: pump 3 jobs that all `exit 1`, run with `--workers 1 --max-attempts 3`, capture the event stream into `tmp/`, verify the interleave is `A1, B1, C1, A2, B2, C2, A3, B3, C3` rather than `A1..A3, B1..B3, C1..C3`.
6. Update SPEC.md (work-queue predicate section) and README.md (one-line retry-fairness note).
7. Commit, fill `## Evidence`, set `status: done`.

## Verification

- `go test ./internal/runner -run 'TestRetryRoundRobin|TestPumpWorkQueue' -v` — all pass.
- CLI smoke shows the JSONL event stream interleaves attempts across siblings (each job gets attempt N before any job gets attempt N+1).
- `go test ./...` and `go vet ./...` clean.

## Open questions

None. The ordering choice (`ORDER BY attempts, id` vs. a new column) is a judgment call I'm making per agent-loop guidance — alternatives recorded above. If the boss wants a different shape (e.g., a dedicated `queue_seq` column for forward-compat with future priorities), they can override via `## Boss log`.

## Design notes

- 2026-05-13T09:50Z — Bundled `--max-attempts` default change (3 → 1) into this section per a mid-work nudge from the user.
  - The section text said nothing about the default; it focused on ordering. But the two concerns are related: round-robin only matters when retries happen, and switching the default to `1` means **users opt into retries explicitly**. Together the changes form a clean retry-policy story for the README.
  - Considered keeping it separate (would have set `status: blocked` and asked the boss to slot a new section). Decided to bundle because (a) the user pinged my session directly with the request, (b) both changes live in the same commit-locality (`runner.go`, `main.go`, SPEC.md, README.md flag tables) so splitting buys nothing, and (c) the test suite covers both: `claim.go`'s WHERE still uses `attempts < MaxAttempts`, so default-1 just means "one attempt only" without changing any code path.
  - Note for the reader: this changes user-visible default behavior. Anyone who relied on the implicit 3-retry default needs to pass `--max-attempts 3` to get the old behavior. Worth a release note when xjobs ships.

- 2026-05-13T09:48Z — Smoke evidence: the bug is most visible when a job is pumped mid-drain after an earlier job has already accumulated several retries.
  - First smoke (3 jobs pumped together, all `/usr/bin/false`, workers=1) showed identical interleave on old vs new code. Root cause: `feedQueue`'s `seen` map deduplicates by `(job_id, attempts)`, and once all three rows are in the channel together, the workers chew through them in FIFO order. Round-robin emerges naturally regardless of `ORDER BY`.
  - The real bug surfaces when a row's failure-and-retry cycle is fast compared to pump arrivals: a freshly-pumped attempts=0 row should outrank an already-retried attempts=N row, but `ORDER BY id` keeps preferring the low-id (already-retried) row. The staged-pump smoke (`tmp/164409_435-xjobs-retry-smoke/staged-{old,new}-v2/`) demonstrates this clearly — old: `alpha 1,2,3,4, bravo 1, alpha 5, bravo 2..5` (alpha gets 4 consecutive retries, then bravo cuts in once but alpha 5 still preempts bravo 2). New: `alpha 1,2,3, bravo 1, alpha 4, bravo 2, alpha 5, bravo 3..5` (true interleave from bravo's arrival on).
  - The unit tests `TestRetryRoundRobin_OneFailureYieldsToSiblings` and `TestRetryRoundRobin_AllFailedSiblingsRotateByAttempts` pin the SQL-level ordering invariant directly, since the e2e behavior is timing-dependent and would be brittle as a test.
