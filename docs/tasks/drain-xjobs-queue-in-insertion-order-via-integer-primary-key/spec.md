# Drain xjobs queue in insertion order via integer PK

## Goal

Restructure the `jobs` table so its primary key is an `INTEGER PRIMARY KEY AUTOINCREMENT` (the natural insertion-order ordinal) and the user-supplied job id is a `UNIQUE` text column. The work-queue then `ORDER BY` the integer PK to drain jobs in insertion order. This replaces the prior reverted approach (a separate `seq` column populated via `MAX(seq)+1`) with a cleaner schema where the queue ordering and the autoincrement are the same mechanism.

Out of scope: backwards compatibility / migration of existing `.xjobs/db.sql3` files. Users with old DBs `rm -rf .xjobs/` and re-pump. `ensureSchema` defines the new shape only — no `ALTER TABLE` probe path.

## Architecture

- **`internal/runner/db.go`** — rewrite `jobs` schema:
  - `n INTEGER PRIMARY KEY AUTOINCREMENT` as the table's integer PK (also its `rowid` alias).
  - `id TEXT NOT NULL UNIQUE` (user-supplied id; loses PRIMARY KEY but keeps uniqueness for `INSERT OR IGNORE` dedup).
  - Drop the `migrateAddJobsSeq` helper from the prior attempt — not on master, but watch for any equivalent in case rebase pulls it back in.
- **`internal/runner/pump.go`** — `INSERT OR IGNORE` shrinks back to its original 5-column form (the int PK auto-assigns; no explicit `MAX(...)+1` subquery).
- **`internal/runner/claim.go`** — `fetchBatch` work-queue query: `ORDER BY id` → `ORDER BY n`.
- **`events.job_id`** still references the user-supplied text id, NOT `n`. No FK constraint exists; the `idx_events_job_id` index stays as-is. No event-table changes.
- **`internal/runner/order_test.go`** — new test file:
  - `TestPumpWorkQueueInsertionOrder` — non-alphabetical JSONL drains in insertion order.
  - `TestPumpNIsMonotonic` — `n` is 1..N in insertion order.
  - `TestPumpDuplicatesDoNotBurnN` — `INSERT OR IGNORE` of a duplicate id does not advance the autoincrement (verify with `sqlite_sequence`'s actual behavior — AUTOINCREMENT does reserve sequence numbers on conflict; needs verification, may need a different assertion).
  - `TestIdUniqueness` — re-inserting the same id is silently ignored (already covered by Pump's dedup, but a focused test pins the schema constraint).
- **`SPEC.md`** + **`README.md`** — update the schema docs and add a note about insertion-order drain.

### Naming: `n` vs `seq`

`seq` would clash with `events.seq` (the events table's own autoincrement PK). They're parallel concepts but easy to confuse in a `JOIN`. Picking `n` (the section author's other suggestion) — short, unambiguous, "the integer ordinal." See design note.

### AUTOINCREMENT vs plain INTEGER PRIMARY KEY

SQLite distinguishes:

- `INTEGER PRIMARY KEY` (alias for `rowid`) — may reuse ids after a delete; on `INSERT OR IGNORE` of a conflict, the discarded number can be reclaimed.
- `INTEGER PRIMARY KEY AUTOINCREMENT` — uses `sqlite_sequence` to guarantee monotonic-forever ids; conflicting INSERTs do "consume" a rowid (see <https://sqlite.org/autoinc.html>).

For insertion-order drain we only need monotonicity within a single pump session, not across deletes. Plain `INTEGER PRIMARY KEY` would suffice and avoids the `sqlite_sequence` write per insert. But: the section text explicitly says `AUTOINCREMENT`, and the durability benefit (never reuse after a `failed` row's delete in a future `xjobs rm`) is small but real. Going with `AUTOINCREMENT` as specified.

## Steps

1. Rewrite `ensureSchema` in `db.go` — new `jobs` shape, no migration helper.
2. Update `Pump`'s INSERT statement — drop the `seq` column / subquery.
3. Update `fetchBatch`'s `ORDER BY` from `id` to `n`.
4. Add `internal/runner/order_test.go` with the four tests above.
5. Run `go test ./...` and `go vet ./...`.
6. CLI smoke test: build, drive a non-alphabetical JSONL plan against `xjobs --workers 1`, capture transcript into `tmp/`.
7. Update `SPEC.md`'s schema block + reserved-fields list and the README's `state layout` section.
8. Final commit pass, fill `## Evidence`, set `status: done`.

## Verification

- `go test ./internal/runner -run 'TestPump|TestId|TestN' -v` — all pass.
- CLI smoke: `xjobs --workers 1 plan.jsonl` against ids `zebra/apple/mango/banana/cherry` emits `running`/`success` events in that exact order; a `SELECT n, id, status FROM jobs ORDER BY n` dump shows `1..5` in insertion order.
- `sqlite3 .xjobs/db.sql3 '.schema jobs'` shows the expected shape.

## Open questions

None. Naming + AUTOINCREMENT decisions resolved above; design notes capture the alternatives.

## Design notes

- 2026-05-13T16:18Z — Picked `n` over `seq` for the integer PK column name.
  - Alternatives:
    - **`seq`** (prior attempt used this for a non-PK column): clean concept, but clashes with `events.seq` (the events table's own auto-incrementing PK). `j.seq` vs `e.seq` in a JOIN is correct-but-confusing. Loser on clarity grounds.
    - **`n`** (picked): unambiguous, short, reads as "the nth row." No collision anywhere in the schema.
    - **`ord` / `idx`**: also fine, but `idx` reads as "index" (database-index sense), and `ord` is one of those names that's clear once you know it.
  - The section text explicitly mentioned both `seq` and `n` as candidates; either was acceptable. Going with `n` for the no-collision win.

- 2026-05-13T16:18Z — Picked `INTEGER PRIMARY KEY AUTOINCREMENT` over plain `INTEGER PRIMARY KEY`.
  - Section text was explicit (`AUTOINCREMENT`). Followed it.
  - Plain `INTEGER PRIMARY KEY` would also work for insertion order within a single pump session — rowid is monotonic until a row is deleted, and xjobs never deletes rows today.
  - AUTOINCREMENT's guarantee is "never reuse, across the lifetime of the DB, even after deletes" via `sqlite_sequence`. The cost is one extra write to `sqlite_sequence` per insert.
  - Decided: follow the section. The future `xjobs rm` verb (sketched in SPEC) would benefit from never-reuse, and the per-insert cost is negligible at xjobs's scale (jobs are seconds-to-hours, not milliseconds).

- 2026-05-13T16:18Z — No migration path. The reverted prior attempt had a `migrateAddJobsSeq` probe that `ALTER TABLE`d existing pre-seq DBs. The section text explicitly says: "No backward compatibility — existing `.xjobs/db.sql3` files don't need to be migrated. Treat this as a fresh schema."
  - Rationale (per section author): xjobs is early MVP; the cost of `rm -rf .xjobs/` for any user who has an in-progress queue is small; the schema-design clarity of "this is what the table looks like, period" outweighs migration plumbing.
  - Consequence: anyone with a pre-revert DB that has the old `seq` column will get a SQLite error on first open after this lands (the old column persists; `CREATE TABLE IF NOT EXISTS` is a no-op). They `rm -rf .xjobs/`.

- 2026-05-13T16:22Z — `n` can have gaps when `INSERT OR IGNORE` hits a UNIQUE(id) conflict.
  - Observed in CLI smoke: pumped `apple/watermelon/durian` on top of an existing DB containing `apple` (n=2). Got `apple` skipped, `watermelon` → n=7, `durian` → n=8. The dup attempt on `apple` consumed n=6.
  - This is per-spec for SQLite `AUTOINCREMENT` — `sqlite_sequence` is bumped before the conflict resolution, and `OR IGNORE` discards the row but not the reserved rowid (see <https://sqlite.org/autoinc.html>).
  - Functional impact on the work-queue: none. `n` is still monotonic in insertion order; `ORDER BY n` still drains in JSONL order. The only observable difference vs the prior `MAX(seq)+1` approach is that `n` is not contiguous when duplicates are pumped.
  - Why this is fine: `n` is an internal ordering key, not a user-facing counter. The CLI never displays it. If we ever want contiguous numbering for display, that's a `ROW_NUMBER() OVER (ORDER BY n)` away.
  - Test note: `TestPumpNIsMonotonic` covers contiguous 1..4 *for a single-pump fresh DB* (where no conflicts happen). It does NOT assert contiguity across re-pumps — and we don't want it to.

- 2026-05-13T16:22Z — `events.job_id` is still TEXT (mirrors the user-supplied id), not the new `n` integer.
  - No FK constraint to update — there was never one; only an index `idx_events_job_id`.
  - Keeps the event JSONL shape (`"id":"tt0133093:download"`) human-readable. Switching to int would break the `xjobs monitor --id <prefix>` filter and the JSONL stdout contract.
