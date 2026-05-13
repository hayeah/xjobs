---
status: done
section: Rename xjobs jobs schema: id (int) + job_id (text)
slug: rename-xjobs-jobs-schema-id-int-job-id-text
mode: worktree
spec:
created: 2026-05-13T09:28:28Z
---

> ## Rename xjobs jobs schema: id (int) + job_id (text)
>
> ---
> status:
>   type: open
> ---
>
> The schema 8gf landed (`966d432`) uses `n INTEGER PRIMARY KEY AUTOINCREMENT` + `id TEXT NOT NULL UNIQUE` — too many shapes (`n` vs `seq` vs `id`) for "what's the key here". Rename to the standard SQLite convention:
>
> - `jobs.id` — `INTEGER PRIMARY KEY` (rowid alias). Drop AUTOINCREMENT — xjobs doesn't delete rows, so the rowid-reuse semantics don't matter and plain `INTEGER PRIMARY KEY` is the conventional shape.
> - `jobs.job_id` — `TEXT NOT NULL UNIQUE`. The user-supplied job identifier.
>
> Downstream: anything that references jobs by id should use the internal integer FK, not the text. Audit and update foreign-key-shaped references (most likely `events.job_id` — make it `INTEGER` referring to `jobs.id`; rename the column to keep things uniform).
>
> Update everywhere that referenced the old column names: `claim.go` (`ORDER BY n` → `ORDER BY id`), `pump.go` INSERT, event emission, SPEC.md, README.md, and the recently-added tests under `internal/runner/order_test.go`.
>
> No backward compat — fresh schema, `rm -rf .xjobs/`. Same posture as 8gf.
>
> Repo: `~/github.com/hayeah/xjobs/`.
>
> - [ ] implement and verify

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

Schema target:

- `jobs`: `id INTEGER PRIMARY KEY` (was `n ... AUTOINCREMENT`), `job_id TEXT NOT NULL UNIQUE` (was `id`).
- `events`: `id INTEGER PRIMARY KEY` (was `seq ... AUTOINCREMENT`), `job_id INTEGER REFERENCES jobs(id)` (was `TEXT`, by user id). Drop AUTOINCREMENT on both PKs (no row deletion → rowid reuse irrelevant).

Tasks:

- [x] `db.go`: rewrite both CREATE TABLE statements + index names — 89612b8
- [x] `pump.go`: INSERT into `jobs(job_id, ...)` — 008e4bf
- [x] `claim.go`: `WHERE id=?` (text-id by user) → `WHERE job_id=?`; `ORDER BY n` → `ORDER BY id`; `SELECT id, ...` → `SELECT job_id, ...` — 008e4bf
- [x] `reap.go`: same `id`→`job_id` swap for user-id WHEREs and SELECTs — 008e4bf
- [x] `events.go`: INSERT translates `ev.ID` (text) → integer FK via subquery — 008e4bf
- [x] `inspect.go`: LS `SELECT id` → `SELECT job_id`; Monitor queries: rename `seq` → `id` (events PK), translate `idFilter` (text) via subquery / JOIN — 008e4bf
- [x] `order_test.go`: column-name swap + rename schema-shape probe — 00f6a7a
- [x] `SPEC.md` + `README.md`: schema blocks and prose — 28121b3
- [x] `go test ./...` clean
- [x] end-to-end smoke: pump → drain → ls → monitor against fresh `.xjobs/`

## Agent log

- 2026-05-13T16:36Z — Schema + runtime + tests + docs landed across 4 commits (89612b8, 008e4bf, 00f6a7a, 28121b3). Fresh-schema smoke at `tmp/163531_468-xjobs-smoke/` confirms `jobs(id INT PK, job_id TEXT UNIQUE)`, `events(id INT PK, job_id INT FK→jobs.id)`, insertion-order drain, dedup across re-pumps, and Monitor `--id` JOIN-by-text-id filter working.
- Judgment calls (not asked of boss):
  - Read "rename the column to keep things uniform" as renaming `events.seq → events.id` (PK column parallel to `jobs.id`) while keeping the `events.job_id` name. That preserves the SQL convention `<table>_id = FK to that table's id`, which is now correctly satisfied since `events.job_id` is now `INTEGER REFERENCES jobs(id)`.
  - Dropped `AUTOINCREMENT` on `events` too (not just `jobs`) — same rationale (no row deletion → rowid-reuse semantics moot) and keeps the two tables uniform.
  - `events.go` emit() resolves text → int FK with an inline `(SELECT id FROM jobs WHERE job_id = ?)` subquery rather than plumbing the integer through `jobRow`. Self-contained; no surgery to the runOne / execAttempt signatures.
  - `inspect.go` Monitor: filter by `idFilter` (user-supplied text) uses a `JOIN jobs j ON e.job_id = j.id WHERE j.job_id = ?`. No backing index on `jobs.job_id` is needed beyond the existing UNIQUE auto-index.

## Boss log

## Evidence

### `go test ./...`

```
$ go test ./...
?   	github.com/hayeah/xjobs/cmd/xjobs	[no test files]
ok  	github.com/hayeah/xjobs/internal/runner	0.528s
```

The four schema-pin / order tests pass against the new column names:

```
$ go test -v -run 'TestPumpWorkQueueInsertionOrder|TestPumpIDIsMonotonic|TestPumpDuplicatesAreIgnored|TestJobsSchemaShape' ./internal/runner/
=== RUN   TestPumpWorkQueueInsertionOrder
--- PASS: TestPumpWorkQueueInsertionOrder (0.00s)
=== RUN   TestPumpIDIsMonotonic
--- PASS: TestPumpIDIsMonotonic (0.00s)
=== RUN   TestPumpDuplicatesAreIgnored
--- PASS: TestPumpDuplicatesAreIgnored (0.00s)
=== RUN   TestJobsSchemaShape
--- PASS: TestJobsSchemaShape (0.00s)
PASS
```

### Schema dump from a freshly-built `.xjobs/db.sql3`

```sql
CREATE TABLE jobs (
    id          INTEGER PRIMARY KEY,
    job_id      TEXT NOT NULL UNIQUE,
    cwd         TEXT NOT NULL,
    argv        TEXT NOT NULL,
    env         TEXT NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'pending',
    attempts    INTEGER NOT NULL DEFAULT 0,
    pid         INTEGER,
    exit_code   INTEGER,
    signal      TEXT,
    session_key TEXT,
    started_at  TIMESTAMP,
    ended_at    TIMESTAMP,
    error       TEXT,
    meta        TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_jobs_status ON jobs(status);
CREATE TABLE events (
    id       INTEGER PRIMARY KEY,
    ts       TIMESTAMP NOT NULL,
    job_id   INTEGER NOT NULL REFERENCES jobs(id),
    attempt  INTEGER NOT NULL,
    kind     TEXT NOT NULL,
    data     TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_events_job_id ON events(job_id);
```

No `AUTOINCREMENT` on either PK; both `id` columns are plain rowid aliases.

### End-to-end smoke

Smoke dir: `tmp/163531_468-xjobs-smoke/` (under `$MDNOTES_ROOT/2026-05-13/tmp/`).

Plan (insertion order non-alphabetical): `zebra apple mango banana cherry`.

```
$ ./xjobs --workers 1 plan.jsonl
{"ts":"…","kind":"running","id":"zebra","attempt":1,"pid":59706}
{"ts":"…","kind":"success","id":"zebra","attempt":1,"dur_ms":12,"exit":0}
{"ts":"…","kind":"running","id":"apple","attempt":1,"pid":59707}
{"ts":"…","kind":"success","id":"apple","attempt":1,"dur_ms":10,"exit":0}
…  # mango, banana, cherry — in insertion order, not alphabetical
```

`jobs` table after the run:

```
$ sqlite3 .xjobs/db.sql3 "SELECT id, job_id, status FROM jobs ORDER BY id"
1|zebra|done
2|apple|done
3|mango|done
4|banana|done
5|cherry|done
```

`events` table — integer FK + JOIN recovers user id:

```
$ sqlite3 -header -column .xjobs/db.sql3 "SELECT e.id AS eid, j.job_id, e.kind FROM events e JOIN jobs j ON e.job_id=j.id ORDER BY e.id"
eid  job_id  kind
1    zebra   running
2    zebra   success
…
9    cherry  running
10   cherry  success
```

`xjobs monitor --id mango` (filter by user-supplied id via the JOIN):

```
$ ./xjobs monitor --id mango
{"ts":"…","kind":"success","id":"mango","attempt":1,"dur_ms":5,"exit":0}
```

Dedup across re-pump (apple already present; durian is new):

```
$ ./xjobs --workers 1 plan2.jsonl
xjobs: pumped 1 / skipped 1 / total 2 from …/plan2.jsonl
{"ts":"…","kind":"running","id":"durian","attempt":1,"pid":59921}
{"ts":"…","kind":"success","id":"durian","attempt":1,"dur_ms":3,"exit":0}

$ sqlite3 -header -column .xjobs/db.sql3 "SELECT id, job_id, status FROM jobs ORDER BY id"
1   zebra   done
2   apple   done       # original id unchanged across the duplicate pump
3   mango   done
4   banana  done
5   cherry  done
6   durian  done       # MAX(id)+1 without AUTOINCREMENT — contiguous since no deletes
```

## Trouble report

None significant. Two-line notes worth flagging:

- The directive's "rename the column to keep things uniform" is ambiguous about whether the events FK column should change name. I read it as "also rename `events.seq → events.id` to mirror the jobs PK rename" and left `events.job_id` as the column name (now `INTEGER REFERENCES jobs(id)` — the SQL convention is already that `<table>_id` is the integer FK to `<table>.id`). Captured under "judgment calls" in the agent log.
- `events.go` emit() now does an extra `SELECT id FROM jobs WHERE job_id = ?` subquery on every event insert. The cost is a UNIQUE-index lookup (B-tree probe) per event — irrelevant at xjobs's traffic shape (two events per attempt). Mentioning it for completeness, not flagging it as a problem.
- The frozen `docs/tasks/drain-xjobs-queue-in-insertion-order-via-integer-primary-key/{spec,worklog}.md` from the prior section still reference the pre-rename schema (`n`, `seq`, `ORDER BY n`, etc.). Those are historical artifacts shipped with the prior merge commit and intentionally untouched.
