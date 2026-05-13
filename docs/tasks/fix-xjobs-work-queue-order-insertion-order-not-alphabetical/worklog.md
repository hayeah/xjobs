---
status: done
section: Fix xjobs work-queue order: insertion order not alphabetical
slug: fix-xjobs-work-queue-order-insertion-order-not-alphabetical
mode: worktree
spec:
created: 2026-05-13T09:05:24Z
---

> ## Fix xjobs work-queue order: insertion order not alphabetical
>
> ---
> status:
>   type: open
> ---
>
> xjobs's work-queue currently uses `ORDER BY id` (alphabetical on the user-provided job id), not insertion order. Change to drain in insertion order so the order of the JSONL plan is respected.
>
> Likely shape: add a `seq INTEGER PRIMARY KEY AUTOINCREMENT` column (or similar) to the `jobs` table; ORDER BY that in the work-queue select. Schema migration so existing `.xjobs/db.sql3` files keep working — pick whatever migration story xjobs already uses, or document the bump if there isn't one yet.
>
> Heads up: a parallel section (`wire-actual-hootty-dep-into-xjobs`, agent thb) is touching xjobs's runner. Both branches will rebase at lgtm time; this one is smaller so it'll likely land first.
>
> Repo: `~/github.com/hayeah/xjobs/`.
>
> - [ ] implement and verify

## Todos

- [x] Add `seq INTEGER NOT NULL DEFAULT 0` column to `jobs` schema + `idx_jobs_seq` index, with column-presence guard for existing DBs (backfill `seq = rowid`).
- [x] In `Pump`, populate `seq` explicitly on INSERT via `(SELECT COALESCE(MAX(seq), 0) + 1 FROM jobs)` (safe under `writeMu`, single SQLite writer).
- [x] Change work-queue `ORDER BY id` → `ORDER BY seq` in `claim.go` `fetchBatch`.
- [x] Add a Go test exercising insertion order via Pump + work-queue select (DB-only; no exec). Verifies new-DB and migrated-DB paths.
- [x] CLI smoke test: drive `xjobs` against a JSONL plan whose ids sort differently from insertion order, capture transcript into `tmp/`.
- [x] Update README/SPEC if any of the schema docs need a touch-up.

## Agent log
- 2026-05-13T09:08Z schema + pump + claim landed (c8b07a3): jobs.seq + idx_jobs_seq, Pump populates via MAX(seq)+1, work-queue ORDER BY seq. Build + vet clean.
- 2026-05-13T09:09Z tests landed (f34d419): 4 tests covering insertion order, seq monotonicity, dup-doesn't-burn-seq, and migration from pre-seq DB. All pass.
- 2026-05-13T09:12Z smoke + docs landed (f120182): fresh-DB and migrate-path CLI smoke under tmp/161042_836-xjobs-smoke/, README/SPEC updated. Branch is 3 commits ahead of master.

## Boss log

## Evidence

Branch: `fix-xjobs-work-queue-order-insertion-order-not-alphabetical` in `~/github.com/hayeah/xjobs`, three commits ahead of `master`:

```
f120182 Doc the seq column + insertion-order drain in SPEC.md and README.md
f34d419 Add tests for work-queue insertion-order + seq migration
c8b07a3 Drain work-queue in insertion order, not alphabetical id
```

### What changed

- `internal/runner/db.go`: `jobs.seq INTEGER NOT NULL DEFAULT 0` + `idx_jobs_seq`. `migrateAddJobsSeq` probes `pragma_table_info('jobs')`; for pre-seq DBs it `ALTER TABLE ... ADD COLUMN seq` then `UPDATE jobs SET seq = rowid WHERE seq = 0` (rowid is monotonic with insertion order on a `TEXT PRIMARY KEY` table absent `VACUUM`).
- `internal/runner/pump.go`: `INSERT OR IGNORE INTO jobs(id, cwd, argv, env, meta, seq) VALUES(?, ?, ?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM jobs))`. Single-writer under `writeMu` — no race on the subquery.
- `internal/runner/claim.go`: work-queue select changed from `ORDER BY id` to `ORDER BY seq`.
- `SPEC.md` / `README.md` updated to document the new column and insertion-order semantics.

### Tests (`go test ./internal/runner -run 'TestPump|TestMigrate' -v`)

```
=== RUN   TestPumpWorkQueueInsertionOrder
--- PASS: TestPumpWorkQueueInsertionOrder (0.01s)
=== RUN   TestPumpSeqIsMonotonic
--- PASS: TestPumpSeqIsMonotonic (0.00s)
=== RUN   TestPumpDuplicatesDoNotBurnSeq
--- PASS: TestPumpDuplicatesDoNotBurnSeq (0.00s)
=== RUN   TestMigrateAddJobsSeq
--- PASS: TestMigrateAddJobsSeq (0.00s)
PASS
ok  	github.com/hayeah/xjobs/internal/runner
```

- `TestPumpWorkQueueInsertionOrder` — JSONL plan `zebra, apple, mango, banana` is drained in that exact order via `fetchBatch`, not alphabetical.
- `TestPumpSeqIsMonotonic` — same plan yields `seq` 1..4 in insertion order.
- `TestPumpDuplicatesDoNotBurnSeq` — a duplicate id (`INSERT OR IGNORE` skipped) does not consume a `seq`; the next genuine insert gets the next contiguous value (1, 2, 3 for `a`, `b`, `c` even after re-pumping `a`).
- `TestMigrateAddJobsSeq` — seeds a hand-built pre-seq `jobs` table with rows in non-alphabetical order, `Open()` runs `migrateAddJobsSeq`, verifies the column is added, `seq` is backfilled from `rowid`, the work-queue drains in insertion order, and a subsequent `Pump` continues `seq` monotonically (5, 6 after the four backfilled rows).

### CLI smoke — fresh DB

[`tmp/161042_836-xjobs-smoke/fresh-db.txt`](tmp/161042_836-xjobs-smoke/fresh-db.txt) — `xjobs --workers 1 plan.jsonl` against ids `zebra/apple/mango/banana/cherry`:

```
{"...","kind":"running","id":"zebra",...}
{"...","kind":"success","id":"zebra",...}
{"...","kind":"running","id":"apple",...}
{"...","kind":"success","id":"apple",...}
{"...","kind":"running","id":"mango",...}
{"...","kind":"success","id":"mango",...}
{"...","kind":"running","id":"banana",...}
{"...","kind":"success","id":"banana",...}
{"...","kind":"running","id":"cherry",...}
{"...","kind":"success","id":"cherry",...}

seq dump after run:
1|zebra|done
2|apple|done
3|mango|done
4|banana|done
5|cherry|done
```

The `running`/`success` events arrive in insertion order, `seq` is 1..5 in that order.

### CLI smoke — migration path

[`tmp/161042_836-xjobs-smoke/migrate-path.txt`](tmp/161042_836-xjobs-smoke/migrate-path.txt) — hand-seed a pre-seq schema (no `seq` column) with `zebra, apple, mango, banana` rows, then `xjobs resume --workers 1`:

```
pre-migrate schema (no seq col expected):
(no seq column — matches pre-seq schema)
pre-migrate row order (by rowid):
1|zebra
2|apple
3|mango
4|banana
xjobs resume --workers 1: events arrive zebra → apple → mango → banana
post-migrate schema (seq col present, DEFAULT 0):
14|seq|INTEGER|1|0|0
final seq dump:
1|zebra|done
2|apple|done
3|mango|done
4|banana|done
```

`ensureSchema → migrateAddJobsSeq` runs on the existing `db.sql3`, the seq column is added with the expected definition (`INTEGER NOT NULL DEFAULT 0`), `seq` is backfilled from rowid, and the drain respects the original insertion order.

## Trouble report

- The section text suggested `seq INTEGER PRIMARY KEY AUTOINCREMENT`, but `jobs` already has `id TEXT PRIMARY KEY` and SQLite only permits one `INTEGER PRIMARY KEY` per table (it's an alias for `rowid`). Went with a plain `seq INTEGER NOT NULL DEFAULT 0` column + explicit `MAX(seq)+1` assignment from Pump (which is the only writer of new rows, serialized under `writeMu`). Functionally equivalent for the work-queue ordering and avoids restructuring the PK + downstream `id`-keyed code.
- `ALTER TABLE jobs ADD COLUMN seq INTEGER NOT NULL` requires a `DEFAULT` clause in SQLite, hence `DEFAULT 0`. The Pump path always supplies an explicit `seq` so the default is only ever observed transiently during backfill of pre-seq DBs.
- The reserved-field paragraph in SPEC.md got `seq` added to the list, but there's no code-enforced reserved-field check — `Job` is a fixed-shape struct (`ID/CWD/Argv/Env/Meta`), so a stray `"seq"` key in JSONL is silently dropped by the JSON decoder. No validate-list to update.
- xjobs had **no existing tests** before this section. The four new tests under `internal/runner/order_test.go` are the first ones in the repo; followed standard `testing` + `t.TempDir()` conventions. If the project adds CI later, these should run as-is via `go test ./...`.
- Heads-up reminder from the section: parallel `wire-actual-hootty-dep-into-xjobs` branch is touching the runner. The only file overlap risk is `internal/runner/service.go` (their territory) and possibly `claim.go` if they refactor the spawn path — this section's `claim.go` change is a one-line `ORDER BY` swap, easy to rebase. `db.go`, `pump.go`, `SPEC.md`, `README.md` are the heavier deltas and unlikely to collide with hootty work.
