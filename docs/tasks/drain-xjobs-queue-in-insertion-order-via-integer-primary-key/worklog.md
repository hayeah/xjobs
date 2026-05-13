---
status: done
section: Drain xjobs queue in insertion order via integer primary key
slug: drain-xjobs-queue-in-insertion-order-via-integer-primary-key
mode: worktree
spec: spec.md
created: 2026-05-13T09:16:37Z
---

> ## Drain xjobs queue in insertion order via integer primary key
>
> ---
> status:
>   type: open
> ---
>
> A prior attempt (`fix-xjobs-work-queue-order-insertion-order-not-alphabetical`, merged at `8e62e99` then reverted at `495e3a9`) added a separate `seq` column populated via `MAX(seq)+1`. The human prefers a cleaner schema design instead: restructure `jobs`'s primary key.
>
> Direction:
>
> - Drop `id TEXT PRIMARY KEY` from `jobs`.
> - Add a new `INTEGER PRIMARY KEY AUTOINCREMENT` column — call it whatever fits (`rowid` is shadowing-ish; `id` is taken; consider `seq` or `n`).
> - Add `UNIQUE(id)` constraint so the user-supplied job id remains unique.
> - Work-queue `ORDER BY <int-pk>` for true insertion order.
> - `INSERT OR IGNORE` on the user id stays the dedupe knob.
>
> No backward compatibility — existing `.xjobs/db.sql3` files don't need to be migrated. Treat this as a fresh schema: update `ensureSchema` (or whatever bootstraps the DB) to define the new shape; users with old DBs can `rm -rf .xjobs/` and re-pump. Update any downstream code that referenced `id`'s primary-keyness (foreign keys from `events.job_id`, etc. — verify in the codebase).
>
> Read the prior workspace for context (`$BOSS_ROOT/tasks/fix-xjobs-work-queue-order-insertion-order-not-alphabetical/worklog.md`) — useful for the test shape; the schema design + work-queue change are new.
>
> Heads up: `clean-up-hootty-references-from-xjobs-docs` (agent `dxk`) is touching README/SPEC concurrently. Schema docs in SPEC.md may conflict at lgtm time — small.
>
> - [ ] implement and verify

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

- [x] Rewrite `ensureSchema` in `db.go` — new `jobs` shape with `n INTEGER PRIMARY KEY AUTOINCREMENT` + `id TEXT NOT NULL UNIQUE`.
- [x] Update `Pump`'s INSERT statement back to 5-column shape. *(already in shape on master after the revert; no change needed.)*
- [x] Change `fetchBatch` `ORDER BY id` → `ORDER BY n`.
- [x] Add `internal/runner/order_test.go` covering insertion order, n-monotonicity, dup-id behavior, and uniqueness constraint.
- [x] CLI smoke test: drive `xjobs` against a non-alphabetical JSONL plan, capture transcript into `tmp/`.
- [x] Update SPEC.md schema block + reserved-fields list.
- [x] Update README.md — insertion-order drain note next to the pump description.
- [x] Fill `## Evidence` and flip `status: done`.

## Agent log

- 2026-05-13T09:20Z schema + ORDER BY + tests landed (361a418): `jobs.n INTEGER PRIMARY KEY AUTOINCREMENT`, `UNIQUE(id)`, work-queue `ORDER BY n`. 4 new tests pass (`TestPumpWorkQueueInsertionOrder`, `TestPumpNIsMonotonic`, `TestPumpDuplicatesAreIgnored`, `TestJobsSchemaShape`). `go vet` clean.
- 2026-05-13T09:22Z CLI smoke captured at `tmp/162121_000-xjobs-smoke/` — `xjobs --workers 1 plan.jsonl` against `zebra/apple/mango/banana/cherry` emits running/success events in JSONL order; `SELECT n,id,status FROM jobs ORDER BY n` returns 1..5 in that order; re-pump shows dedup works (apple skipped, watermelon/durian get n=7/8 — gap on n=6 from the burned dup attempt; documented in design notes).
- 2026-05-13T09:24Z docs landed (c4d414a): SPEC.md jobs-table block + reserved-fields list updated for the new schema; README.md gains an insertion-order note next to the pump description.

## Boss log

## Evidence

Branch: `drain-xjobs-queue-in-insertion-order-via-integer-primary-key` in `~/github.com/hayeah/xjobs`, two commits ahead of `master`:

```
c4d414a Doc the n-column PK + insertion-order drain in SPEC.md and README.md
361a418 Make jobs.n the integer PK; drain queue in insertion order
```

### What changed

- `internal/runner/db.go`: `jobs.id TEXT PRIMARY KEY` replaced with two columns — `n INTEGER PRIMARY KEY AUTOINCREMENT` (the new PK and rowid alias) and `id TEXT NOT NULL UNIQUE` (the user-supplied job id, dedup target for `INSERT OR IGNORE`). No migration logic; `ensureSchema` defines the new shape directly. Pre-existing `.xjobs/db.sql3` files are not migrated (per section: `rm -rf .xjobs/` and re-pump).
- `internal/runner/claim.go`: work-queue `fetchBatch` `ORDER BY id` → `ORDER BY n`.
- `internal/runner/pump.go`: unchanged from master (the post-revert master is already in the 5-column INSERT shape — no `seq` subquery to remove).
- `internal/runner/order_test.go`: new file. Four tests:
  - `TestPumpWorkQueueInsertionOrder` — non-alphabetical JSONL drains in insertion order.
  - `TestPumpNIsMonotonic` — `n` is 1..4 in insertion order for a fresh-DB pump.
  - `TestPumpDuplicatesAreIgnored` — re-pumping a known id is silently skipped; the original row's `n` does not change; the work-queue order is preserved.
  - `TestJobsSchemaShape` — pins the schema: `jobs.n` is the PK (`pk=1` in `pragma_table_info`), `jobs.id` is not the PK (`pk=0`), and `id` has a `UNIQUE` index.
- `SPEC.md` — jobs-table block + reserved-fields list updated; added a paragraph under the work-queue predicate calling out `ORDER BY n` semantics; note that the schema is fresh-only (no migration).
- `README.md` — short paragraph next to the pump description calling out "queue drains in insertion order."

### Tests (`go test ./internal/runner -run 'TestPump|TestJobs' -v`)

```
=== RUN   TestPumpWorkQueueInsertionOrder
--- PASS: TestPumpWorkQueueInsertionOrder (0.01s)
=== RUN   TestPumpNIsMonotonic
--- PASS: TestPumpNIsMonotonic (0.00s)
=== RUN   TestPumpDuplicatesAreIgnored
--- PASS: TestPumpDuplicatesAreIgnored (0.00s)
=== RUN   TestJobsSchemaShape
--- PASS: TestJobsSchemaShape (0.00s)
PASS
ok  	github.com/hayeah/xjobs/internal/runner
```

Full `go test ./...`: `ok github.com/hayeah/xjobs/internal/runner 0.210s` (and `cmd/xjobs` has no test files). `go vet ./...` clean.

### CLI smoke — fresh DB

[`tmp/162121_000-xjobs-smoke/fresh-db.txt`](tmp/162121_000-xjobs-smoke/fresh-db.txt) — `xjobs --workers 1 plan.jsonl` against ids `zebra/apple/mango/banana/cherry`:

```
$ xjobs --workers 1 plan.jsonl
xjobs: pumped 5 / skipped 0 / total 5 from ...plan.jsonl
{"...","kind":"running","id":"zebra","attempt":1,...}
{"...","kind":"success","id":"zebra","attempt":1,...,"exit":0}
{"...","kind":"running","id":"apple","attempt":1,...}
{"...","kind":"success","id":"apple","attempt":1,...,"exit":0}
{"...","kind":"running","id":"mango","attempt":1,...}
{"...","kind":"success","id":"mango","attempt":1,...,"exit":0}
{"...","kind":"running","id":"banana","attempt":1,...}
{"...","kind":"success","id":"banana","attempt":1,...,"exit":0}
{"...","kind":"running","id":"cherry","attempt":1,...}
{"...","kind":"success","id":"cherry","attempt":1,...,"exit":0}

.schema jobs:
CREATE TABLE jobs (
    n           INTEGER PRIMARY KEY AUTOINCREMENT,
    id          TEXT NOT NULL UNIQUE,
    ...
);
CREATE INDEX idx_jobs_status ON jobs(status);

SELECT n, id, status FROM jobs ORDER BY n;
1|zebra|done
2|apple|done
3|mango|done
4|banana|done
5|cherry|done
```

Events arrive in JSONL insertion order (not alphabetical), `n` is 1..5 in that order, and the on-disk schema is the new shape.

### CLI smoke — re-pump (dedup)

[`tmp/162121_000-xjobs-smoke/repump.txt`](tmp/162121_000-xjobs-smoke/repump.txt) — on top of the same DB, pump `apple/watermelon/durian` (one known id + two new):

```
xjobs: pumped 2 / skipped 1 / total 3 from ...plan2.jsonl
{"...","kind":"running","id":"watermelon","attempt":1,...}
{"...","kind":"success","id":"watermelon","attempt":1,...,"exit":0}
{"...","kind":"running","id":"durian","attempt":1,...}
{"...","kind":"success","id":"durian","attempt":1,...,"exit":0}

SELECT n, id, status FROM jobs ORDER BY n;
1|zebra|done
2|apple|done
3|mango|done
4|banana|done
5|cherry|done
7|watermelon|done
8|durian|done
```

`apple` is dedup-skipped; only watermelon/durian spawn. The `n=6` slot is a documented gap — SQLite `AUTOINCREMENT` reserves the rowid before the `OR IGNORE` conflict check, so the dup attempt burned n=6. Functionally irrelevant to the work queue (monotonicity preserved); see `spec.md` design notes.

## Trouble report

- Picked `n` over `seq` for the integer PK column name. `events.seq` already exists (the events table's own autoincrement PK); reusing `seq` on `jobs` is correct-but-confusing in any JOIN. Section author offered both candidates; chose `n` for the no-collision win. Captured in spec design notes.
- AUTOINCREMENT vs plain `INTEGER PRIMARY KEY`: the section explicitly said `AUTOINCREMENT`, so used it. Functional difference is small at xjobs's scale — `AUTOINCREMENT` writes to `sqlite_sequence` per insert and never reuses a rowid after a delete; plain `INTEGER PRIMARY KEY` can reuse. xjobs doesn't delete rows today, so the practical difference is zero. Captured in spec design notes.
- `AUTOINCREMENT` reserves a rowid on `INSERT OR IGNORE` UNIQUE conflicts — so re-pumping a known id burns one `n` value. Observed in the re-pump smoke (gap at n=6 between the first pump's 1..5 and the second pump's 7..8). Doesn't affect work-queue monotonicity or ordering, but the test (`TestPumpNIsMonotonic`) deliberately only checks contiguity on a fresh-DB single pump. Documented in spec design notes.
- `/bin/true` is not present on macOS; first smoke-test attempt failed every job with `fork/exec /bin/true: no such file or directory`. Re-ran with `/usr/bin/true` and it worked. The `xjobs` runner correctly surfaced the spawn error as `error` events — the test plan is now using `/usr/bin/true`. Worth noting for future cross-platform smoke tests; the *go* tests under `internal/runner` don't spawn anything (DB-only paths) so they sidestep this.
- The prior reverted attempt (`fix-xjobs-work-queue-order-...`) added a `migrateAddJobsSeq` probe via `pragma_table_info`. The current section explicitly forbids migration; `ensureSchema` only ever creates the new shape via `CREATE TABLE IF NOT EXISTS`. Side effect: anyone with a stale `.xjobs/db.sql3` from a pre-revert build (with the old `seq` column) will silently keep using the old schema — `CREATE TABLE IF NOT EXISTS` is a no-op against an existing table. That's per-section ("rm -rf .xjobs/ and re-pump"), but operators ignoring the instruction will see `no such column: n` errors at the first work-queue scan. Acceptable for an MVP, but worth a heads-up.
- The concurrent `clean-up-hootty-references-from-xjobs-docs` branch (agent `dxk`) was forked from `8e62e99` (the pre-revert merge), so its SPEC.md still carries the old `seq INTEGER NOT NULL DEFAULT 0` schema doc lines. When `boss lgtm` rebases either branch, the SPEC.md jobs-table block + reserved-fields paragraph will conflict. Both edits target the same lines; the conflict resolution is "take this branch's `n INTEGER PRIMARY KEY AUTOINCREMENT` + `id TEXT NOT NULL UNIQUE` schema and merge `dxk`'s hootty deletions on top." Heads-up for the boss; not action-required from this section.
