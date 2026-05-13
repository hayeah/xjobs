---
status: done
section: Remove --nice CLI flag; move to per-job in JSONL
slug: remove-nice-cli-flag-move-to-per-job-in-jsonl
mode: worktree
spec:
created: 2026-05-13T09:51:45Z
---

> ## Remove --nice CLI flag; move to per-job in JSONL
>
> ---
> status:
>   type: open
> ---
>
> Drop the global `--nice` flag. The current behavior (one nice value applied to every spawned job) doesn't match how `xjobs` should feel — the mental model is "launching from the current active shell", so by default a job inherits parent priority (no `setpriority` call at all).
>
> Niceness becomes an optional per-job field in the JSONL:
>
> ```jsonl
> {"id":"foo","argv":["..."],"nice":10}
> ```
>
> If set, the runner calls `setpriority` for that specific spawn; if absent, no call. Zero is a valid explicit value (POSIX default), so encode "absent" as `nil`/missing — not `0`.
>
> What to change:
>
> - `cmd/xjobs/main.go` — drop `--nice` from `bindCommon` and `Options.Nice`.
> - `internal/runner/service.go` — drop the `if niceN > 0 { setpriority(...) }` block; replace with a per-job call gated on the new field being present.
> - `internal/runner/runner.go` / `Job` shape — add `Nice *int` (or similar nil-sentinel encoding).
> - `internal/runner/pump.go` and the JSONL parser — accept the optional `nice` field. Persist it on the row (schema column) so resume/retry honors it; transient pump-only would lose it on resume.
> - `SPEC.md` / `README.md` — drop `--nice` from CLI tables; document the per-job `nice` field. Update the "running a job" prose to lean on "inherits parent priority by default."
>
> This subsumes finding B8 from the in-flight code review (`--nice` negative silently no-op) by deleting the broken global path entirely.
>
> Heads up: the human's own `merge-run-resume` work touches `cmd/xjobs/main.go` in parallel — a rebase conflict on flag definitions is likely at lgtm time. Resolve by keeping their main.go shape and re-applying this section's `--nice` deletion on top.
>
> Repo: `~/github.com/hayeah/xjobs/`.
>
> - [ ] implement and verify

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

- [x] Drop `--nice` + `--max-attempts` from `cmd/xjobs/main.go` (commonFlags, bindCommon, opts(), usage)
- [x] Add `nice INTEGER` (nullable) + `max_attempts INTEGER NOT NULL DEFAULT 1` columns to `jobs` schema
- [x] Add `Nice *int` + `MaxAttempts *int` to `Job` struct (job.go)
- [x] Update `pump.go` INSERT to persist `nice` (NULL when absent) and `max_attempts` (default 1 when absent)
- [x] Switch `claim.go` predicates to per-row `attempts < jobs.max_attempts`; carry `nice` through `jobRow`
- [x] Update `service.go::execAttempt` to take `nice *int` and call `setpriority` only when non-nil
- [x] Drop `Nice` and `MaxAttempts` from `runner.Options` and `withDefaults`
- [x] Update `SPEC.md` + `README.md` (CLI tables, JSONL shape, prose for inherited priority + per-job retry)
- [x] Update `order_test.go` to drop `Options{MaxAttempts:3}` reliance
- [x] Add tests: nice roundtrip (nil + set), max_attempts default=1 absent, per-row max_attempts honored in claim/feed
- [x] `go build ./...` + `go test ./...` pass; capture output for Evidence

## Agent log

- 2026-05-13T10:00Z spawned, read section + boss-log note adding `--max-attempts` to scope. Single-repo work in `~/github.com/hayeah/xjobs/` worktree `002`. No spec doc needed — section text fully specifies the design; this is a straightforward parameter-relocation refactor across `cmd/xjobs/main.go`, `internal/runner/{runner,service,pump,claim,db,job}.go`, plus docs. Pointer-typed `*int` for `Nice` (NULL surface = no setpriority call) and for `MaxAttempts` (with default-to-1 on insert when absent). `max_attempts` becomes a per-row column so the claim/feed predicate reads it from the row.
- 2026-05-13T10:02Z core code change landed — `49a55a9` "Move nice + max_attempts from CLI flags to per-job JSONL fields" — and dropped `opts Options` from `runOne` since once `Nice` and `MaxAttempts` left `Options` neither caller used the param.
- 2026-05-13T10:03Z tests landed — `d567f63` "Tests for per-job nice + max_attempts" — covering nice persistence (NULL / explicit-0 / explicit-N), max_attempts defaulting to 1, work-queue feed predicate honoring per-row max_attempts, claim-guard refusing to re-claim a row at its own ceiling.
- 2026-05-13T10:04Z docs landed — `cca6019` "SPEC + README: per-job nice / max_attempts; drop CLI flags" — CLI tables, JSONL shape, schema block, retry prose, all rewritten. All 9 tests pass; smoke transcript at `tmp/170224_423-smoke-transcript.txt` demonstrates renice taking effect (`NI=10`), per-row retries running 3 attempts, and default-1 jobs failing after one attempt.
- 2026-05-13T10:10Z boss rebase nudge — master picked up `1c0c5bc` (round-robin `ORDER BY attempts, id`) + `2fcb64d` (`--max-attempts` default 3→1) + the `51c31c2` merge in flight. Re-read master's versions of all six listed files end-to-end before resolving so I knew which features had to survive.
- 2026-05-13T10:12Z rebase resolved. Three conflicts on commit-1 (`main.go`, `claim.go`, `runner.go`), two on commit-3 (`SPEC.md`, `README.md`); commit-2 (tests) auto-merged textually (master's `simulateAttempt` + two `TestRetryRoundRobin_*` tests landed beside my `perjob_test.go`) but left the test file referencing the now-deleted `claim(ctx, id, Options{MaxAttempts: N})` API, so I added a fixup commit `49b950a` that adapts `simulateAttempt` to the new two-arg `claim` and bakes `"max_attempts":3` into the round-robin test JSONL plans so the rows survive multiple simulated failures.
- 2026-05-13T10:13Z full test pass on rebased branch (all 11 tests, including master's two `TestRetryRoundRobin_*`). Post-rebase smoke at `tmp/171047_705-post-rebase-smoke.txt` — five-row plan now exercises three round-robin retries (A/B/C each at `max_attempts:3`) alongside the renice case and a default-1 oneshot. Branch tip `49b950a`; commits `61b9f0d`, `098d3cb`, `6829d25`, `49b950a` from master.

### Rebase conflict resolution detail

`cmd/xjobs/main.go` — two conflict blocks, both my-side-wins:
- `usage()` flags block: master added `--max-attempts default 1 (no auto-retry; raise to allow N total tries)` and kept `--nice`; my branch deleted both lines. The flags are gone; my deletion stands.
- `bindCommon()`: master changed `fs.IntVar(&c.MaxAttempts, "max-attempts", 1, …)` (default 3→1) and kept the `--nice` binding; my branch deleted both. My deletion stands.

`internal/runner/claim.go` — one block, composed both sides:
- `fetchBatch()`: master changed `ORDER BY id` → `ORDER BY attempts, id` (round-robin); my branch changed the predicate from `attempts < %d` (interpolated `opts.MaxAttempts`) to `attempts < max_attempts` (per-row column) and dropped the `%d` arg. Resolution: kept master's `ORDER BY attempts, id` AND my per-row predicate AND my single-arg `fmt.Sprintf` call list — they compose cleanly because master's ordering tweak doesn't depend on the predicate shape.
- `claim()`: master kept the old `opts Options` arg and `%d` interpolation; my branch dropped both. My side wins (the entire `Options.MaxAttempts` field is gone).

`internal/runner/runner.go` — two blocks, both my-side-wins:
- `Options` struct: master narrowed comment on `MaxAttempts` to "default 1 (no auto-retry)"; my branch removed the field entirely (along with `Nice`).
- `withDefaults()`: master kept the `MaxAttempts <= 0` default-1 guard; my branch removed it.

`SPEC.md` — one block at the CLI table, plus master had separately rewritten the "Eligible rows are drained ORDER BY id" paragraph into the longer "round-robin retries" prose (no textual conflict because it's a different region). Resolution: kept my reshaped flag table (no `--max-attempts`, no `--nice`); replaced master's `--max-attempts` reference inside the surviving round-robin paragraph with "burns through its own `max_attempts`"; cross-linked the per-job-knobs section to the work-queue-predicate section so the round-robin behavior is discoverable from both directions.

`README.md` — one block at the CLI table. Same shape: kept my reshaped flag table, but folded master's "Retries round-robin across siblings" line into the prose right after the table so the round-robin contract is still surfaced where readers expect it.

`internal/runner/order_test.go` — no merge conflict at git level, but the auto-merge created a broken state because master's `simulateAttempt` helper calls the three-arg `claim()` API I had just deleted. Added one fixup commit (`49b950a`) that:
  - drops `maxAttempts int` from `simulateAttempt`'s signature and calls the new two-arg `claim`;
  - encodes `"max_attempts":3` inline in both round-robin test JSONL plans so the rows can survive the simulated failures the tests drive (without it, the schema default of 1 would make every row ineligible after one failure).

## Boss log
- 2026-05-13T09:54Z scope add-on from the human: also remove the --max-attempts CLI flag and make it a per-job JSONL field, exactly the same shape as nice. bundle into your current section — same mental model, same files touched.
  
  what to add:
  
  - JSON field: `"max_attempts": <int>` (optional). Default 1 when absent (matches qpn's just-landed new default; 1 = no auto-retry).
  - Persist as a schema column on `jobs` (e.g. `max_attempts INTEGER NOT NULL DEFAULT 1`).
  - Work-queue predicate in claim.go: `attempts < Options.MaxAttempts` becomes `attempts < jobs.max_attempts`.
  - Drop `--max-attempts` from `bindCommon` and `Options.MaxAttempts`.
  - SPEC.md / README.md: drop `--max-attempts` from flag tables; document `max_attempts` as a per-job JSONL field; update the retry prose.
  
  heads-up — there's an unmerged sibling branch (`xjobs-retry-round-robin-across-queue-not-consecutive`, agent qpn) that changed `--max-attempts` default 3 → 1 in a separate commit. that commit becomes redundant after your section deletes the flag entirely. boss will handle qpn's rebase after your section lands; you don't need to coordinate with it.
  
  mental model the human is going for: nice + max_attempts both have a "shell-launch flavor" — JSONL owns them, no global knob.
- 2026-05-13T10:04Z rebase conflict on master during boss lgtm. aborted — worktree is clean.
  
  commits that landed on master since you branched (6c52c3b):
  
  - 1c0c5bc "Round-robin retries: ORDER BY attempts, id" — internal/runner/claim.go (fetchBatch ORDER BY id → ORDER BY attempts, id), internal/runner/order_test.go (2 new tests TestRetryRoundRobin_*).
  - 2fcb64d "Default --max-attempts to 1; document round-robin retry rule" — cmd/xjobs/main.go (--max-attempts default 3→1), internal/runner/runner.go (withDefaults default 3→1), SPEC.md + README.md (work-queue predicate + flags table).
  - 51c31c2 merge commit.
  
  likely conflict areas:
  - internal/runner/claim.go (your per-row max_attempts predicate vs their ORDER BY change — these compose, just need to merge)
  - internal/runner/runner.go (your delete of Options.MaxAttempts vs their default change to 1 — your side wins, the field is gone)
  - cmd/xjobs/main.go (your delete of --max-attempts flag vs their default change — your side wins)
  - SPEC.md + README.md (both branches rewrote flag tables + retry prose — preserve master's round-robin documentation on top of your per-job-fields restructure)
  - internal/runner/order_test.go (your `Options{MaxAttempts:3}` removal vs their 2 new TestRetryRoundRobin_* tests — keep both their tests; update them if they reference Options.MaxAttempts since that field is gone in your branch)
  
  resolve by PRESERVING features from master, not by taking your side blindly. for each conflicted file, read master's version end-to-end first and understand what features the new lines implement before overwriting. specifically:
  
  - master's `ORDER BY attempts, id` in fetchBatch must compose with your per-row max_attempts predicate. keep both.
  - master's two TestRetryRoundRobin_* tests must keep passing. if they construct `Options{MaxAttempts: N}`, update them to use the new per-row API (a `markFailed` helper or whatever your test scaffolding looks like).
  - master's "ORDER BY attempts, id" + "round-robin" documentation in SPEC/README should land on top of your per-job-fields prose, not replace it.
  
  re-run go test ./... after resolution. when done: update your commit(s), append a note to ## Agent log, flip status back to done. i'll re-run boss lgtm.

## Evidence

### Four branch commits (post-rebase onto master @ 51c31c2)

```
49b950a order_test.go: adapt round-robin tests to per-row max_attempts API
6829d25 SPEC + README: per-job nice / max_attempts; drop CLI flags
098d3cb Tests for per-job nice + max_attempts
61b9f0d Move nice + max_attempts from CLI flags to per-job JSONL fields
```

The first three are my original three commits rewritten by the rebase; the fourth is the new fixup that re-points master's `simulateAttempt` helper at the new two-arg `claim` API. Pre-rebase shas (for reference): `cca6019`, `d567f63`, `49a55a9`.

### Unit tests post-rebase (`go test -v ./...`)

```
=== RUN   TestPumpWorkQueueInsertionOrder
--- PASS: TestPumpWorkQueueInsertionOrder (0.01s)
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
=== RUN   TestPumpNiceRoundtrip
--- PASS: TestPumpNiceRoundtrip (0.00s)
=== RUN   TestFetchBatchCarriesNice
--- PASS: TestFetchBatchCarriesNice (0.00s)
=== RUN   TestPumpMaxAttemptsDefault
--- PASS: TestPumpMaxAttemptsDefault (0.00s)
=== RUN   TestWorkQueueRespectsPerRowMaxAttempts
--- PASS: TestWorkQueueRespectsPerRowMaxAttempts (0.00s)
=== RUN   TestClaimRespectsPerRowMaxAttempts
--- PASS: TestClaimRespectsPerRowMaxAttempts (0.00s)
PASS
ok  	github.com/hayeah/xjobs/internal/runner	0.512s
```

All 11 tests pass: 4 pre-existing (schema-shape probe + insertion-order drain), 2 from master's round-robin work (`TestRetryRoundRobin_*`, adapted to per-row API), 5 new in this section (`TestPumpNiceRoundtrip`, `TestFetchBatchCarriesNice`, `TestPumpMaxAttemptsDefault`, `TestWorkQueueRespectsPerRowMaxAttempts`, `TestClaimRespectsPerRowMaxAttempts`).

### End-to-end smoke (post-rebase)

Full transcript: [`tmp/171047_705-post-rebase-smoke.txt`](tmp/171047_705-post-rebase-smoke.txt). Pre-rebase transcript still in tree at [`tmp/170224_423-smoke-transcript.txt`](tmp/170224_423-smoke-transcript.txt) for comparison.

Plan exercises round-robin retries (A, B, C all opt in to `max_attempts:3`) alongside the renice and default-1 cases:

```jsonl
{"id":"A","argv":["/bin/sh","-c","exit 7"],"max_attempts":3}
{"id":"B","argv":["/bin/sh","-c","exit 7"],"max_attempts":3}
{"id":"C","argv":["/bin/sh","-c","exit 7"],"max_attempts":3}
{"id":"renice","argv":["/bin/sh","-c","echo renice; ps -o pid,nice,comm -p $$"],"nice":10}
{"id":"oneshot","argv":["/bin/sh","-c","exit 2"]}
```

DB rows after drain (`SELECT job_id, status, attempts, max_attempts, COALESCE(nice,"NULL") FROM jobs ORDER BY id;`):

```
A|failed|3|3|NULL
B|failed|3|3|NULL
C|failed|3|3|NULL
renice|done|1|1|10
oneshot|failed|1|1|NULL
```

`renice/output.log` proves `setpriority` still takes effect post-rebase (`NI` column from `ps`):

```
renice
  PID NI COMM
83648 10 /bin/sh
```

A/B/C each ran 3 attempts (per-row max_attempts honored); `oneshot` ran exactly once (default 1, no auto-retry); `renice` got `NI=10`. Round-robin ordering is unit-tested in `TestRetryRoundRobin_*` rather than asserted here (with 5 workers in the smoke, all three are claimed concurrently so the strict `[A1,B1,C1,A2,B2,C2,…]` pattern isn't visible across the event stream).

### Removed flags are rejected

```
$ /tmp/xjobs-perjob --nice 5
flag provided but not defined: -nice
...
$ /tmp/xjobs-perjob --max-attempts 3
flag provided but not defined: -max-attempts
...
```

### `xjobs --help` (post-change)

```
flags:
  --state-dir <path>     default .xjobs
  --workers N            default NumCPU
  --where '<sql>'        AND-combined with the work-queue predicate

input:
  JSONL lines: {"id":"…", "cwd":"…", "argv":["…"], "env":{}, "meta":{}, "nice":N, "max_attempts":N}
  Duplicate ids are silently skipped (INSERT OR IGNORE).
  Children inherit the parent process's priority by default; set per-job "nice" to renice.
  "max_attempts" defaults to 1 (no auto-retry) when absent.
```

## Trouble report

Nothing surprising — the section text fully specified the design and every file the boss listed was the right one. Two minor in-flight notes:

- After dropping `MaxAttempts` and `Nice` from `runner.Options`, `runOne` no longer used its `opts` parameter at all, so I dropped it from the function signature too. Cleaner than leaving an unused arg around for a future caller to misread. The `feedQueue` path still takes `Options` because it reads `Workers`, `Where`, `PollEvery`.
- The pre-existing `internal/runner/service.go::buildEnv` triggers a `mapsloop` lint suggestion ("Replace m[k]=v loop with maps.Copy"). Out of scope for this section — not touching style decisions on code I'm not otherwise modifying.

### Heads-up for boss at lgtm time

The section briefing flagged a likely rebase conflict on `cmd/xjobs/main.go` from the human's parallel `merge-run-resume` work. My diff against `master` for `main.go`:

- Drops `Nice` + `MaxAttempts` fields from `commonFlags` struct.
- Drops the two `fs.IntVar` registrations from `bindCommon`.
- Drops the two fields from `(c *commonFlags) opts()`.
- Rewrites the `flags:` block in `usage()` to remove `--nice` and `--max-attempts` lines, and the `input:` block to document the new per-job fields and the inherit-parent-priority default.

If `merge-run-resume` reshapes `commonFlags` / `bindCommon` / `opts()`, the resolution is "keep their structural shape, re-apply my four deletions on top." The expanded `input:` blockquote in `usage()` and the new "Per-job knobs" sections in `SPEC.md` / `README.md` are new content that should land verbatim regardless.

### Sibling-branch coordination note (from boss log)

The unmerged `xjobs-retry-round-robin-across-queue-not-consecutive` branch (agent `qpn`) flipped `--max-attempts` default from 3 → 1 in a separate commit. After this section lands, that commit becomes pure churn (the flag no longer exists). Boss owns that rebase per the boss-log note — no action needed from me.

### Post-rebase status

Master already absorbed `qpn`'s work (commits `1c0c5bc` + `2fcb64d` + merge `51c31c2`) before the boss tried to lgtm me. Round-robin ordering (`ORDER BY attempts, id`) and the round-robin documentation prose both survived the rebase — `qpn`'s ordering semantics now compose with this section's per-row `max_attempts` predicate.

### `qpn` redundancy after this section lands

After this section is merged, the parts of `qpn`'s work that are still meaningful:

- `ORDER BY attempts, id` in `fetchBatch` (round-robin ordering) — kept, still live.
- The two `TestRetryRoundRobin_*` tests + `simulateAttempt` helper — kept, adapted to the per-row API in `49b950a`.
- The "round-robin retries" prose in SPEC.md — kept (with `--max-attempts` references rewritten to `max_attempts`).

The parts that become churn:

- `2fcb64d`'s flag-default change `--max-attempts: 3 → 1` — the flag is now gone; the new schema default `max_attempts INTEGER NOT NULL DEFAULT 1` carries the "1 = no auto-retry" semantics.
- The flag-table entry rewriting in `2fcb64d`'s SPEC/README — superseded by this section's flag-table reshape.
