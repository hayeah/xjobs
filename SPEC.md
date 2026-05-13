# xjobs — Design

`xjobs` is the long-running-process answer to `xargs -P`. This document
covers the design — the contracts, the rationale, what was deliberately
left out. For day-to-day usage see [`README.md`](README.md).

## Problem

A common shape of work: dozens to thousands of small-to-medium tasks —
uploads, transcodes, scrapes, builds, parallel agentic runs. Per-job
runtime is seconds to hours. They're independent. They're idempotent (or
can be made so).

`xargs -P` is the right shape — fan out, run in parallel, exploit the
shell — but it's the wrong scale: per-job runtime is milliseconds, no
durable state, no mid-run inspection, no "resume after I hit Ctrl-C and
fixed the bug."

What goes wrong without a convention:

- No sense of "what's left to do" mid-run.
- Restarting means re-deriving the plan and re-checking what already ran.
- Hard to run a subset (do 100, eyeball, continue).
- Per-job logs disappear into a tmux pane nobody reads.
- Each project reinvents its own progress file.

`xjobs` is one convention for all of that.

## Core Idea

- **JSONL on stdin (or a file)** describes the plan — one line per job:
  `{id, argv, cwd?, env?, meta?}`. Stream-and-drain: workers start
  claiming as soon as the first row lands.
- **SQLite in WAL mode** at `.xjobs/db.sql3` is the canonical store.
  Workers write; observers read concurrently. Resume is just "open the
  same file."
- **Each job is its own OS process.** The runner spawns it like a shell
  would (argv, cwd, env), captures its stdout/stderr to a per-job log,
  and uses exit code to decide success/failure. No language constraint
  on the child.
- **Per-job flock** at `.xjobs/<id>/lock` is the liveness signal. Held
  by the runner for the child's lifetime; released on crash. The reaper
  probes this on the next drain to reclaim stranded rows. No heartbeats.
- **Process-shape, not daemon-shape.** `xjobs` is a foreground command
  that inherits env + cwd from the calling shell. There is no `xjobs
  daemon`; a fresh `xjobs` in the same directory resumes the existing
  queue.

Compound benefit: the same `.xjobs/` directory is, simultaneously, a job
queue (SQLite) and a fleet of per-job log files. One state dir, all
views. (A future libghostty/hootty integration would extend the same
directory into a set of attachable terminal sessions — parked at
[`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).)

## State Layout

```
.xjobs/
├── db.sql3              # SQLite, WAL
├── db.sql3-wal
├── db.sql3-shm
└── <job-id>/
    ├── lock             # exclusive flock held for the child's lifetime
    └── output.log       # captured stdout + stderr from the child
```

Default state dir is `./.xjobs/` (CWD-relative). Override with
`--state-dir <path>`. Two state dirs in different CWDs are independent
queues — the natural extension of "every directory is its own scratch
workspace."

Each child is spawned through plain `os.exec` pipes; its combined
stdout + stderr is written to `<job-id>/output.log`. This is enough for
line-oriented children but degrades on TUI children (escape codes pile
up as raw bytes). A deferred libghostty PTY integration would replace
`output.log` with a binary frame stream (`pty.hootty.log`) plus live
attach / replay verbs — see
[`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).
The swap point is `execAttempt` in `internal/runner/service.go`.

**Local FS only.** SQLite WAL doesn't work over NFS, and flock semantics
across boundaries are fragile. Don't.

## Job Input Format

JSONL on stdin or in a file. One line per job:

```jsonc
{
  "id":   "tt0133093:download",   // required, unique, stable
  "cwd":  "/abs/or/relative/path", // optional; default = xjobs CWD
  "argv": ["./worker", "tt0133093"],
  "env":  { "FOO": "bar" },        // optional; merged onto inherited env
  "meta": { "size": 12345678 },    // optional; free-form, lives in jobs.meta
  "nice": 10,                      // optional; setpriority(2) target for this spawn
  "max_attempts": 3                // optional; retry ceiling for this row, default 1
}
```

Hard requirements: `id` is present and non-empty; `argv` is a non-empty
list. Pumps `INSERT OR IGNORE` on `id`, so re-pumping a known id is a
no-op (not a re-queue). To force a retry of a `failed` row past its
own `max_attempts`, delete the row and re-pump it (the dedicated
`retry` verb is future work).

### Per-job knobs

The mental model of `xjobs` is "launching from the current active
shell" — process-wide CLI flags for priority and retry don't fit, so
these are per-row instead:

- **`nice`** (int, optional). When present, the runner calls
  `setpriority(PRIO_PROCESS, pid, nice)` on the spawned child. When
  absent, the runner makes no call and the child inherits the parent
  process's priority. Zero is a valid explicit value (POSIX default);
  encode "no renice" as omitting the field, not as `0`.
- **`max_attempts`** (int, optional). The retry ceiling for this row
  specifically. Defaults to `1` when absent (i.e. no auto-retry —
  failure is final). The work-queue predicate compares this row's
  `attempts` against its own `max_attempts`, so different jobs in the
  same plan can have different retry budgets.

### Id convention: `<entity>:<phase>`

Any string works. For multi-phase pipelines the recommended shape is
**`<entity>:<phase>`**, optionally `<entity>:<phase>:<sub>`:

```
tt0133093:download
tt0133093:transcode
tt0133093:upload:r2
tt0133093:upload:b2
```

Wins:

- **Cheap grouping in SQL.** `WHERE id LIKE 'tt0133093:%'` (one entity);
  `WHERE id LIKE '%:download'` (one phase across entities).
- **Prefix-friendly for tooling.** Self-documenting in shell history.
- **Subset retries are obvious.** "Re-download what failed" is a one-liner.

Colon over slash (slashes collide with path semantics) and over space
(arg-parsing pressure). No part of the runner enforces this; flat ids
keep working.

### Pump streaming

Workers begin claiming as rows land — the producer (file read or piped
stream) can keep going while early jobs already run. After the input is
exhausted the runner continues draining until the work-queue predicate
matches zero rows (or `max_attempts` is hit for every remaining
failure), then exits.

## The `jobs` Table

Runner-owned, fixed schema. Singular table — `(cwd, argv, env)` is the
"payload" and the runner already owns those three columns. Free-form
caller data goes in `meta`.

```sql
CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,                   -- rowid alias; insertion-order ordinal; work-queue ORDER BY
    job_id       TEXT NOT NULL UNIQUE,                  -- user-supplied job id; INSERT OR IGNORE dedups on this
    cwd          TEXT NOT NULL,
    argv         TEXT NOT NULL,                         -- JSON array
    env          TEXT NOT NULL DEFAULT '{}',            -- JSON object
    status       TEXT NOT NULL DEFAULT 'pending',       -- pending | running | done | failed
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 1,            -- per-row retry ceiling (JSONL "max_attempts")
    nice         INTEGER,                               -- per-row setpriority target (JSONL "nice"); NULL = inherit
    pid          INTEGER,                               -- current attempt's PID
    exit_code    INTEGER,                               -- 0 on done; non-zero on failed
    signal       TEXT,                                  -- 'SIGKILL', 'SIGTERM', ... if killed
    session_key  TEXT,                                  -- reserved for hootty integration
    started_at   TIMESTAMP,
    ended_at     TIMESTAMP,
    error        TEXT,                                  -- short message; full output in output.log
    meta         TEXT NOT NULL DEFAULT '{}'             -- JSON; caller scratch
);
CREATE INDEX idx_jobs_status ON jobs(status);
```

The primary key is the integer `id` — a rowid alias auto-assigned by
SQLite at INSERT time (no AUTOINCREMENT; xjobs never deletes rows, so
rowid-reuse semantics are irrelevant). The user-supplied `job_id` keeps
its uniqueness via `UNIQUE(job_id)` so `INSERT OR IGNORE` continues to
dedup pumps. This is what lets the work-queue drain in JSONL insertion
order — `ORDER BY id` is true insertion order, not the alphabetical
order of user-supplied job ids.

State-column names (`id`, `status`, `attempts`, `pid`, `exit_code`,
`signal`, `session_key`, `started_at`, `ended_at`, `error`, `meta`) are
reserved — the JSONL line's `id` is mapped onto the `job_id` column,
and the other state columns cannot be used as top-level JSONL fields.
The runner stores the caller's free-form data only in `meta`.
`nice` and `max_attempts` are also reserved at the top level (they
populate their respective columns directly — see "Per-job knobs").

**Fresh schema only.** `ensureSchema` defines the shape above and runs
no migrations. A pre-existing `.xjobs/db.sql3` from an earlier xjobs
build does not carry over — `rm -rf .xjobs/` and re-pump.

### Work-queue predicate

```sql
WHERE status = 'pending'
   OR (status = 'failed' AND attempts < max_attempts)
```

`max_attempts` is the row's own column, so different jobs in the same
plan retry to different depths. `running` rows are **not** in the
predicate. They're handled out-of-band by the reaper pass at drain
start (next section). User `--where` fragments AND-combine after the
built-in predicate.

Eligible rows are drained `ORDER BY attempts, id` — primary key is
the attempt counter (ascending), tiebreaker is the integer PK `id`
(insertion order). The id-tiebreaker preserves the JSONL-insertion-order
guarantee for fresh work: when every eligible row is at `attempts=0`,
the order is exactly `ORDER BY id`. This matters when a hand-written
plan is ordered by some real-world dependency (download → transcode →
upload, or a deliberate priority hand-sort).

The `attempts` term makes retries round-robin across siblings. A row
that failed at attempt `N` carries `attempts=N` in the DB; any sibling
still at `attempts<N` is served first. Concretely: with three rows that
all fail at attempt 1, retries are interleaved `A2, B2, C2, A3, B3, C3,
…` rather than `A2, A3, …, B2, B3, …`. The point is that one
slow-to-fix row can't starve its siblings out of the worker pool while
it burns through its own `max_attempts`. Fresh `attempts=0` rows pumped
mid-drain also cut in front of failed-with-retries-remaining rows —
"new work first, retries when there's slack."

### Why a single table

The earlier design split immutable payload (`items`) from runner-owned
state (`tasks`) because the payload schema was inferred from a Go struct
per project. `xjobs`'s payload is just three fixed columns — `cwd`,
`argv`, `env` — so the split adds no value. The discipline "never UPDATE
the payload columns after insert" is enforced by convention. Richer
per-task data lives in app-specific tables (see below).

## Lifecycle of an Attempt

```
pending
   │  claim: UPDATE … SET status='running', attempts=attempts+1,
   │         started_at=now, pid=NULL, ended_at=NULL, error=NULL
   ▼
running ──── xjobs holds an flock on .xjobs/<id>/lock for the child's lifetime
   │
   ├──► child exits 0          ──► done   (ended_at, exit_code=0)
   ├──► child exits non-zero   ──► failed (ended_at, exit_code, error="exit N")
   ├──► child killed by signal ──► failed (ended_at, signal, error="killed by SIG")
   ├──► setup/spawn error      ──► failed (error=...)
   └──► xjobs crashes mid-run  ──► child dies with PTY/parent; flock is released;
                                   row stays 'running' until the next drain's
                                   reaper pass resets it to 'pending'.
```

Claim and terminal writes go through a single `writeMu`-serialized
SQLite writer per `xjobs` process. Cross-process contention (two `xjobs`
against the same state dir, or a child writing to its app-specific
tables) is absorbed by `busy_timeout=5000`. WAL gives 1W+NR to readers
(`xjobs monitor`, `xjobs ls`, ad-hoc `sqlite3` queries) without blocking
writers.

## Reaping Stale Rows

No heartbeats. Liveness piggy-backs on the per-job flock at
`.xjobs/<id>/lock`. A live runner holds the flock; a dead runner doesn't.

At the start of every drain (and again at start of every pump), `xjobs`
performs a **reaper pass**:

```
for each row WHERE status = 'running':
    try to flock(.xjobs/<id>/lock) non-blocking
    if acquired:
        # prior runner is gone (crashed, oom-killed, host rebooted)
        UPDATE jobs SET status = 'pending', pid = NULL WHERE id = ?
        # attempts counter stays; the crash counts against max-attempts
    else:
        # row is actually running under another xjobs in this state dir
        # leave it alone
```

This subsumes everything a heartbeat would have done — detecting crashes,
reclaiming stranded rows, distinguishing "stuck" from "still alive" —
for zero per-tick write cost, no `--stale` threshold to tune, no
clock-skew edge cases. The trade is granularity: reclamation happens at
drain start rather than continuously. For long-running workloads
(seconds-to-hours per job), this is fine, and a fresh `xjobs` invocation
is the operator's natural "kick it again" gesture.

Cross-process safety follows for free: two `xjobs` against the same
state dir each hold the flocks for their own in-flight jobs and ignore
each other's `running` rows.

Reaping is reported as an aggregate count on stderr (`xjobs: reaped N
stale running row(s) from prior run`). It is **not** emitted as an event
— the next claim of a reaped row produces the user-visible `running` /
`success` / `error` event for the new attempt.

## Events

Each attempt produces two events: a `running` event when the worker
claims and spawns the child, then `success` or `error` when it
terminates. Events are stored in an `events` table and mirrored to
xjobs's stdout as JSONL when running in foreground.

### Events schema

```sql
CREATE TABLE events (
    id       INTEGER PRIMARY KEY,                          -- rowid alias; events are append-only
    ts       TIMESTAMP NOT NULL,
    job_id   INTEGER NOT NULL REFERENCES jobs(id),         -- integer FK to jobs.id
    attempt  INTEGER NOT NULL,
    kind     TEXT NOT NULL,                                -- running | success | error
    data     TEXT NOT NULL DEFAULT '{}'                    -- JSON of the full event
);
CREATE INDEX idx_events_job_id ON events(job_id);
```

### Event JSONL shape

```jsonl
{"ts":"2026-05-13T14:02:11Z","kind":"running","id":"tt0133093:download","attempt":1,"pid":48211}
{"ts":"2026-05-13T14:02:23Z","kind":"success","id":"tt0133093:download","attempt":1,"dur_ms":12041,"exit":0}
{"ts":"2026-05-13T14:02:24Z","kind":"error",  "id":"tt0133093:transcode","attempt":1,"dur_ms":2103,"exit":1,"error":"exit 1"}
{"ts":"2026-05-13T14:03:11Z","kind":"error",  "id":"tt0133093:upload",   "attempt":1,"dur_ms":48210,"signal":"SIGKILL","error":"killed by SIGKILL"}
```

| field    | when present                | notes                                                          |
|----------|-----------------------------|----------------------------------------------------------------|
| `ts`     | always                      | ISO 8601 UTC                                                   |
| `kind`   | always                      | `"running"` \| `"success"` \| `"error"`                        |
| `id`     | always                      | job id                                                         |
| `attempt`| always                      | 1-based; matches `jobs.attempts` after the claim               |
| `pid`    | `running`                   | spawned child's PID                                            |
| `dur_ms` | `success` / `error`         | wall-clock duration of this attempt                            |
| `exit`   | `success` or exited-`error` | omitted when killed by signal                                  |
| `signal` | killed-`error` only         | symbolic name (`"SIGKILL"`, `"SIGTERM"`, …)                    |
| `error`  | `error` only                | short human message; full output in `<id>/output.log`          |

`kind="error"` is the catch-all for "this attempt did not succeed" — the
payload distinguishes exit-code error, signal-killed error, and
setup/decode error via the fields present.

The `running` event is what makes `xjobs monitor` actually useful as a
"tell me when something is happening" channel — without it, a long job
looks identical to a stuck one. The event carries no outcome — it's a
tip-off that the worker started the child.

Each attempt produces exactly one `running` and one terminal event,
paired by `(id, attempt)`. Multi-attempt jobs interleave: attempt 1's
`running` and `error`, then attempt 2's `running` and `error`, and so
on, with the final attempt's terminal kind determining the row's status
in the `jobs` table.

### `xjobs monitor` — agent-facing wait verb

```
xjobs monitor                  # print most recent event, then block for next
xjobs monitor --id ID          # filter to one job's events
```

The `monitor` verb tails the events table via `SELECT ... WHERE id >
:since ORDER BY id LIMIT 1` in a 200ms poll loop (filtering by user-
supplied job id joins against `jobs.job_id`). Returns after one event.
Agents loop on it to wait for "the next interesting thing."

## Observing a Job's Output

Events describe **state transitions**. This section is about the
orthogonal question: what's happening *inside* a running job, right now?

Today the answer is "tail the log file." Each child is spawned through
plain pipes and its combined stdout + stderr is written to
`.xjobs/<id>/output.log`:

```sh
tail -f .xjobs/tt0133093:download/output.log
```

This is enough for line-oriented children (downloaders that print one
URL per line, build tools that print one rule per line, scrapers, etc).

It is **not** enough for TUI children — claude/codex runs, `cargo
build`'s progress UI, `apt` installers, `huggingface-cli` downloads.
Their output is a stream of ANSI escapes, `\r` overwrites, cursor
moves, and alt-screen toggles; a flat append-only log captures it as
unreadable bytes. For those workloads:

- Have the child write structured progress / results to the shared
  SQLite DB via an app-specific table (see
  [App-Specific Tables](#app-specific-tables)). The DB carries what a
  script would *parse*.
- Or wait for the deferred libghostty PTY integration, which adds a
  proper VT-emulator capture per job plus live attach and replay
  verbs. Full design parked at
  [`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).

There are no `xjobs attach` / `xjobs log` verbs today; they'd land
alongside the PTY integration as pass-throughs to hootty.

## CLI Surface

```
xjobs [flags] [<file.jsonl> | -]   pump (file > stdin > none) + drain
xjobs run     [flags] [<file.jsonl>]   same as bare
xjobs resume  [flags]                  drain only; ignore any stdin
xjobs ls      [flags] [--json]
xjobs monitor [flags] [--id ID]
```

Flags come **after** the subcommand if you use one:

| flag          | default  | meaning                                                   |
|---------------|----------|-----------------------------------------------------------|
| `--state-dir` | `.xjobs` | dir holding `db.sql3` + per-job session dirs              |
| `--workers`   | `NumCPU` | concurrent job processes                                  |
| `--where`     | (none)   | SQL fragment `AND`-combined with the work-queue predicate |

Per-job `nice` and `max_attempts` live on the JSONL row, not on the
CLI — see [Per-job knobs](#per-job-knobs). The runner has no
process-wide knob for either: children inherit the parent shell's
priority by default, and failure is final unless the row's
`max_attempts` opts in to retries. When a row does retry, it
round-robins across siblings — see [Work-queue predicate](#work-queue-predicate)
for the ordering rule.

### Input precedence: file arg > piped stdin > none

```
xjobs jobs.jsonl                # pump from file, then drain
producer | xjobs                # pump from stdin, then drain
xjobs                           # no pump; just drain what's already in the DB
xjobs - < jobs.jsonl            # explicit stdin (matches `cat`, `jq` conventions)
xjobs resume                    # forced drain-only even if stdin is piped
```

File mode is the script-friendly path: generate JSONL, eyeball it,
commit. Re-pumping the same file folds in any newly-appended lines (the
`INSERT OR IGNORE` makes the previous ids no-ops).

### Exit codes

| code | meaning                                                            |
|------|--------------------------------------------------------------------|
| `0`  | drain completed and no terminal `failed` rows remain               |
| `1`  | drain completed but some rows are stuck as `failed` (out of retries), or a setup error occurred |

`bare-xjobs` after a successful pump exits `0` iff every row terminated
in `done`. This is the contract scripts can rely on.

## App-Specific Tables

The same SQLite file is intentionally shared between the runner and the
job processes. Convention:

- The runner owns `jobs` and `events`, and reserves the prefix `_xjobs_*`
  for future runner-side tables.
- Job processes may `CREATE TABLE` and `INSERT`/`UPDATE`/`SELECT`
  against their own tables freely, as long as the names don't collide
  with reserved prefixes and they don't touch `jobs`/`events`.

This removes the temptation to invent a separate "results" sidecar;
everything an operator might want to query lives in one file. Later
analytics join app rows back to `jobs` by `job_id`:

```sql
SELECT j.job_id, j.status, u.bytes_sent, u.remote_url
FROM jobs j JOIN uploads u ON u.job_id = j.job_id
WHERE j.status = 'done';
```

(App tables typically key on the user-supplied id, since that's what
the child receives via `XJOBS.job_id`. The runner's own `events.job_id`
is the integer FK to `jobs.id` instead — see the events schema above.)

WAL + `busy_timeout=5000` handles the contention. The runner writes only
on state transitions (claim, terminal, reap) — no per-tick heartbeat
traffic — so even at high worker counts the write lane is nowhere near
saturated.

### The `XJOBS` env var

Each child receives a single env var, `XJOBS`, carrying a JSON blob:

```
XJOBS={"db":"/abs/path/.xjobs/db.sql3","state_dir":"/abs/path/.xjobs","job_id":"tt0133093:download","attempt":1}
```

| field       | meaning                                                            |
|-------------|--------------------------------------------------------------------|
| `db`        | absolute path to the shared SQLite file                            |
| `state_dir` | absolute path to `.xjobs/` (or `--state-dir` value)                |
| `job_id`    | the id from the JSONL line                                         |
| `attempt`   | 1-based attempt counter (>1 means a retry of a prior failure)      |

One var instead of three keeps the env clean, leaves room to add fields
without re-coordinating naming, and gives the child a single thing to
parse and stash. Children parse once at startup and stash the values on
a context object.

## Idempotency Contract

**Jobs MUST be idempotent on retry.** The runner cannot enforce this;
it's a caller-side discipline.

`xjobs` will re-run the same `(cwd, argv, env)` from scratch when:

- The previous attempt exited non-zero and the row's own `attempts <
  max_attempts` (default `max_attempts=1`, i.e. failure is final unless
  the JSONL row opts in to retries).
- The previous attempt's runner crashed mid-flight (lock released
  without terminal write); the reaper resets the row to `pending` on
  the next drain. The crash counts against `max_attempts`.

The runner has no way to know whether the prior attempt got half-way
through the work. It passes no breadcrumbs to the next attempt. The
child must converge to the same observable outcome regardless of how
many times it ran or how far a previous run got.

Patterns that satisfy this naturally:

- **Content-addressed writes** (chunk + SHA1 → deterministic remote
  path; re-upload is fine or skip-if-exists at the remote).
- **Check-then-act with server-side uniqueness** (`INSERT OR IGNORE` on
  a unique key; `mv -n`; conditional `PUT-If-None-Match`).
- **Two-phase**: prepare with an idempotency key (the job id is a free
  one), then commit. Re-running prepare is a no-op.

Patterns that don't, without wrapping:

- Appending to a shared log without a dedup key.
- Pop-from-queue-without-ack semantics that double-count on replay.

When in doubt, lead the child with an "is this already done?" check
keyed by `XJOBS.job_id`. If yes, exit 0 quietly. The runner stays simple;
the discipline lives in the child.

## Concurrency

```
producer ──► xjobs (foreground)
              │
              ├── stdin or file reader: INSERT OR IGNORE rows into jobs
              ├── work-queue selector: re-scans every 250ms while drain in flight
              ├── worker pool (N goroutines)
              │     each worker:
              │       claim → spawn child (flocked) → waitpid → terminal-write → emit events
              │
              ├── writeMu serializes claim / terminal / reap writes
              └── stdout: JSONL events for each state transition
```

`--workers` defaults to `runtime.NumCPU()`. Workers are goroutines; the
upper bound on concurrent **processes** is `--workers`. Each in-flight
job consumes one OS process + one open log fd + one flock.

Cross-process safety: a second `xjobs` against the same state dir claims
distinct rows via the conditional UPDATE. Per-job flock prevents two
runners from spawning the same job id concurrently even if both pass the
SQL claim race (defense in depth — the reaper relies on flock
already, this just makes it tight).

### Resume semantics

`xjobs` with no stdin pipe (tty stdin or `< /dev/null`) is the resume
verb. It opens the existing state dir, runs the work-queue predicate,
and drains whatever is eligible. Same operation as the initial pump —
only the "INSERT new rows from stdin" prefix differs.

There is no separate `init` verb. The state dir is created lazily on
first write. Wiping a run is `rm -rf .xjobs/`.

## Shutdown

Today: SIGINT / SIGTERM cancels the context, which `exec.CommandContext`
propagates as an immediate SIGKILL to every running child. Rows that
were running terminal-write as `failed` with `signal="SIGKILL"`.

This is the simplest correct behavior — no rows leak, no orphan
processes — but it gives the child no chance to clean up. A future
shutdown protocol (SIGTERM then a configurable grace window then
SIGKILL) is sketched in **Future Work** below; today the recommendation
is to make children fast to recover (idempotency carries you).

A double SIGINT is the same as a single one (already KILL).

## Future Work

Items the design spec covers but the current MVP does not implement.

### libghostty PTY per job (plus attach / log / kill / write verbs)

The biggest deferred capability. Each job would spawn on a
libghostty-backed PTY using the [hootty](https://github.com/hayeah/hootty)
library, with `xjobs attach` / `xjobs log` / `xjobs kill` / `xjobs
write` riding on top as pass-throughs to `hoot`. The full design — why
a PTY, the verb table, state-layout under the integration, the
`execAttempt` swap, and the libghostty cgo build-prereq cost that made
us defer — is parked at
[`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).

### Additional CLI verbs

- **`xjobs retry --where '<sql>'`** — reset matched rows to `pending`
  with `attempts=0`. Today the workaround is bumping the row's
  `max_attempts` in SQL (or deleting and re-pumping).
- **`xjobs rm --where '<sql>'`** — delete matched rows (in terminal
  states by default).
- **`xjobs sql '<query>'`** — ad-hoc query over `jobs` + app tables.
  Workaround today: `sqlite3 .xjobs/db.sql3 '<query>'`.

### Graceful shutdown with grace window

Replace today's "immediate SIGKILL on ctx-cancel" with: on first SIGINT,
stop accepting new claims and SIGTERM every in-flight child; wait up to
`--shutdown-grace 10s` for them to terminal-write; SIGKILL stragglers.
Second SIGINT escalates: skip the grace, SIGKILL immediately.

### Stuck-but-not-crashed jobs

Flock-based reaping only reclaims rows whose owning `xjobs` is gone. A
hung child (deadlocked, network-blocked) sits as `running` indefinitely
while the runner is alive. Operator workaround: `xjobs kill <id>`
(future) or `kill <pid>` from outside. A future `--max-runtime <dur>`
flag would auto-kill children that run longer than a threshold.

### Continuous pump (`--keep-open`)

Today: stdin EOF → finish remaining work and exit. A `--keep-open` mode
that kept the reader open and the drain alive would turn `xjobs` into a
long-running broker. Conflicts with the no-daemon stance; defer until a
concrete use case appears.

### Failure classification

Currently `failed` is a single bucket — every non-zero exit and every
signal-kill lands there. Some workloads want "retryable" vs "permanent"
(transient network vs. auth error). Options under consideration:

- Caller picks an exit-code convention and emits per-job
  `"max_attempts": 1` for permanent classes (or omits it — 1 is the
  default).
- Classification encoded in `meta.error_class` written by the child
  before exit.

Leaning toward the second; revisit when a real workload presses on it.

## Notes

- **`xjobs sql` injection surface.** SQLite has no truly safe "inject
  this SELECT fragment" path. `xjobs` is a single-user tool by design;
  the user supplying `--where` or `xjobs sql` is the user who owns the
  DB file. Documented, not sandboxed.

- **Subcommand-flag ordering.** `xjobs resume --workers 4` works;
  `xjobs --workers 4 resume` does not — the dispatcher routes by
  `argv[0]`. Standard for subcommand CLIs.

- **State dir on local FS.** SQLite WAL doesn't work over NFS, and
  flock semantics are fragile across network boundaries. Don't put
  `.xjobs/` on a network share.
