---
name: xjobs
description: Parallel runner for long-lived independent jobs — one OS process per job, SQLite-backed queue, JSONL input, resume after interruption. Use when running batches of independent tasks that need durable state and clean restart (uploads, encodes, builds, scrapes, parallel agent runs).
---

# xjobs

`xargs -P` shape for **long-running** jobs. Pipe (or hand) a JSONL list of
`{id, argv}` to `xjobs`; it inserts them into a SQLite queue, drains the
queue across N worker processes, and emits a stream of `running` /
`success` / `error` events on stdout. Crash anywhere mid-run, fix the
problem, run `xjobs` again — it picks up where it left off.

- [Install](#install) — `make build` → `bin/xjobs`
- [Quickstart](#quickstart) — write JSONL, run, watch
- [CLI](#cli) — verbs, flags, input precedence
- [Writing a job](#writing-a-job) — JSONL shape, the `XJOBS` env, idempotency contract
- [State layout](#state-layout) — what lives in `.xjobs/`
- [Status](#status) — what's working, what's deferred

## Install

```sh
make build         # → bin/xjobs
```

No external runtime deps; SQLite is `modernc.org/sqlite` (pure Go).

For an editable install on your `$PATH`, use the `gobin` skill from the
main checkout (not from a worktree):

```sh
gobin install ./cmd/xjobs
```

## Quickstart

```sh
# produce a JSONL plan however you like — a script, a query, jq, anything
./plan-uploads.sh > jobs.jsonl
head jobs.jsonl                       # eyeball it
wc -l jobs.jsonl                      # know what you're committing to

xjobs jobs.jsonl                      # pump + drain
xjobs ls                              # peek at status mid-run from another shell
xjobs                                 # resume whatever's left after interruption
```

Each attempt produces two lines on stdout:

```jsonl
{"ts":"2026-05-13T14:02:11Z","kind":"running","id":"tt0133093:download","attempt":1,"pid":48211}
{"ts":"2026-05-13T14:02:23Z","kind":"success","id":"tt0133093:download","attempt":1,"dur_ms":12041,"exit":0}
{"ts":"2026-05-13T14:02:24Z","kind":"error",  "id":"tt0133093:transcode","attempt":1,"dur_ms":2103,"exit":1,"error":"exit 1"}
```

Pipe through `jq` for humans, into a file for downstream tooling, or just
watch the terminal scroll.

## CLI

```
xjobs [flags] [<file.jsonl> | -]   pump (file > stdin > none) + drain
xjobs run     [flags] [<file.jsonl>]   same as bare
xjobs resume  [flags]                  drain only; ignore any stdin
xjobs ls      [flags] [--json]
xjobs monitor [flags] [--id ID]
```

**Input precedence: file arg > piped stdin > none.**

```sh
xjobs jobs.jsonl                # pump from file, then drain
producer | xjobs                # pump from stdin, then drain
xjobs                           # no pump; just drain what's already in the DB
xjobs - < jobs.jsonl            # explicit stdin (useful in scripts)
xjobs resume                    # forced drain-only even if you piped stdin
```

Re-pumping the same file is safe: ids are deduped via `INSERT OR IGNORE`,
so an already-known id is a no-op. Useful when your plan script appends
new lines and you want to fold them in.

Flags (must come **after** the subcommand if you use one):

| flag             | default     | meaning                                                       |
|------------------|-------------|---------------------------------------------------------------|
| `--state-dir`    | `.xjobs`    | dir holding `db.sql3` + per-job session dirs                  |
| `--workers`      | `NumCPU`    | concurrent job processes                                      |
| `--max-attempts` | `3`         | retry ceiling for failed rows                                 |
| `--nice`         | `5`         | nice value applied to spawned children                        |
| `--where`        | (none)      | SQL fragment `AND`-combined with the work-queue predicate     |

`xjobs ls` shows one line per job, sorted `running → pending → failed →
done`. `--json` emits JSONL of the row with parsed argv — pipe through
`jq` for ad-hoc queries.

`xjobs monitor` prints the most recent event line, then blocks until the
next event lands, then exits. Agents poll it in a loop to wait for "the
next interesting thing." `--id <prefix>` filters to one job.

### Exit codes

- `0` — drain completed and no terminal `failed` rows remain.
- `1` — drain completed but some rows are still `failed` (out of retries), or a setup error occurred.

## Writing a job

A job is just a process. xjobs spawns it like a shell would: with your
`argv`, with your `cwd`, with your env plus a few injected vars. The
runner doesn't care what language the child is in, what it does, or how
long it takes — only its exit code.

### JSONL line shape

```jsonc
{
  "id":   "tt0133093:download",      // required, unique across all pumps
  "cwd":  "/abs/or/relative/path",   // optional; default = xjobs's CWD
  "argv": ["./worker", "download", "tt0133093"],
  "env":  { "FOO": "bar" },          // optional; merged onto inherited env
  "meta": { "size": 12345678 }       // optional; free-form, lands in jobs.meta
}
```

The only hard requirements: `id` is present and non-empty; `argv` is a
non-empty list. Everything else is optional.

**Id convention.** Any unique string works. For multi-phase pipelines, the
recommended shape is `<entity>:<phase>` (or `<entity>:<phase>:<sub>`):

```
tt0133093:download
tt0133093:transcode
tt0133093:upload:r2
tt0133093:upload:b2
```

`WHERE id LIKE 'tt0133093:%'` shows every job for one entity;
`WHERE id LIKE '%:download'` shows every download. `xjobs attach
tt0133093:download` is self-documenting in shell history. No part of the
runner enforces the convention — it's just useful.

### The `XJOBS` env var

Every child receives one extra env var, `XJOBS`, carrying a JSON blob:

```
XJOBS={"db":"/abs/path/.xjobs/db.sql3","state_dir":"/abs/path/.xjobs","job_id":"tt0133093:download","attempt":1}
```

| field       | meaning                                                        |
|-------------|----------------------------------------------------------------|
| `db`        | absolute path to the shared SQLite file                        |
| `state_dir` | absolute path to `.xjobs/` (or whatever `--state-dir` is)      |
| `job_id`    | the id from the JSONL line                                     |
| `attempt`   | 1-based attempt counter (>1 means a retry of a prior failure)  |

Parse it once at startup. Shell:

```sh
JOB_ID=$(printf '%s' "$XJOBS" | jq -r .job_id)
DB=$(printf '%s'   "$XJOBS" | jq -r .db)
ATTEMPT=$(printf '%s' "$XJOBS" | jq -r .attempt)
```

Go:

```go
type XJobs struct {
    DB       string `json:"db"`
    StateDir string `json:"state_dir"`
    JobID    string `json:"job_id"`
    Attempt  int    `json:"attempt"`
}
var Env XJobs
func init() { json.Unmarshal([]byte(os.Getenv("XJOBS")), &Env) }
```

### The idempotency contract

**Jobs MUST be idempotent on retry.** This is the runner's hardest
contract — every other design decision falls out of it.

Concretely, xjobs will re-run the same `(cwd, argv, env)` from scratch
when:

- The previous attempt exited non-zero and `attempts < --max-attempts`.
- The previous attempt's runner crashed mid-flight (the row's `running`
  flock was released without a terminal write); the reaper resets the row
  to `pending` on the next drain.

The runner has no way to know whether the prior attempt got "half-way"
through the work. It cannot pass crash breadcrumbs to the next attempt.
So the child must converge to the same observable outcome regardless of
how many times it ran or how far a previous run got.

Patterns that satisfy this naturally:

- **Content-addressed writes**: chunk + SHA1 → deterministic remote
  path. Re-upload is fine (or skip-if-exists at the remote).
- **Check-then-act with server-side uniqueness**: `INSERT OR IGNORE` on
  a unique key; `mv -n`; conditional `PUT-If-None-Match`.
- **Two-phase**: prepare with an idempotency key (the job id is a free
  one), then commit. Re-running the prepare with the same key is a
  no-op.

Patterns that don't, without wrapping:

- Appending to a shared log without a dedup key.
- Pop-from-queue-without-ack semantics that double-count on replay.

When in doubt, lead the child with an "is this already done?" check
keyed by `XJOBS.job_id`. If yes, exit 0 quietly. The runner stays simple;
the discipline lives in the child.

### Exit conventions

| exit               | row terminal state              | event           |
|--------------------|---------------------------------|-----------------|
| `0`                | `done` (`exit_code=0`)          | `success`       |
| non-zero           | `failed` (`exit_code=N`)        | `error` w/ exit |
| killed by signal   | `failed` (`signal=SIGKILL` etc) | `error` w/ signal |
| spawn / setup fail | `failed` (`error=...`)          | `error` w/ msg  |

Failed rows with `attempts < --max-attempts` re-queue automatically. Once
out of retries they remain `failed` and stop the bare `xjobs` invocation
from exiting `0`. To re-run them later: bump `--max-attempts` and run
`xjobs` again (currently no `xjobs retry` verb — coming).

### Observing a job's output

Jobs commonly produce rich terminal output — TUI redraws, progress bars,
inline `\r`-updates, ANSI colors. Tailing a line-oriented log of that
firehose is awful: escape codes appear inline, cursor moves render as
text, overwritten lines pile up.

The intended model is **one libghostty PTY per job**, hosted by the
[hootty](https://github.com/hayeah/hootty) library. Then:

- `xjobs attach <id>` — attach the current terminal to a running job;
  cursor positioning honored, alt-screen toggles work.
- `xjobs log <id> --format plain` — render the current PTY screen as
  plain text. The right thing for agents that need a one-shot snapshot
  ("what does this job look like right now?").
- `xjobs log <id>` — full VT replay of the recorded frame stream
  end-to-end.

For machine-readable structured output that downstream tools should
consume, write to the shared SQLite DB via an app-specific table (see
next section). The PTY captures what an operator would *look at*; the
DB carries what a script would *parse*.

**MVP today.** PTY integration isn't wired up yet. The current build
captures stdout + stderr to a plain-text `.xjobs/<id>/output.log` as a
transitional placeholder — usable for line-oriented children, useless
for TUI children. It'll be replaced by hootty's binary `pty.hootty.log`
frame stream when the PTY swap-in lands.

### Sharing the SQLite DB

The same DB file is intentionally shared between the runner and the
children. Convention:

- The runner owns `jobs` and `events`, and reserves the prefix `_xjobs_*`
  for any future runner-side tables.
- Children may `CREATE TABLE` and read/write their own tables freely, as
  long as the names don't collide with the reserved prefixes and they
  don't touch `jobs`/`events`.

This is how to get rich per-task results (parsed JSON output, bytes-sent
counters, remote URLs, computed hashes) out of children without inventing
a separate sidecar store. Later analytics join app rows back to `jobs` by
`job_id`:

```sql
SELECT j.id, j.status, u.bytes_sent, u.remote_url
FROM jobs j JOIN uploads u ON u.job_id = j.id
WHERE j.status = 'done';
```

The DB is opened in WAL mode with `busy_timeout=5000`, so concurrent
writers from N child processes work fine. Use whatever SQLite library
your language ships with; nothing xjobs-specific.

### A complete tiny example

`plan.py` — emit one JSONL line per job:

```python
#!/usr/bin/env python3
import json, sys
for n in range(1, 21):
    job = {
        "id":   f"demo:{n:02d}",
        "argv": ["./worker.py", str(n)],
    }
    print(json.dumps(job))
```

`worker.py` — parse the `XJOBS` env, do an idempotency check against an
app-owned table, do work, write a result row, occasionally fail to
exercise retries:

```python
#!/usr/bin/env python3
import json, os, sqlite3, sys, time

env  = json.loads(os.environ["XJOBS"])
db   = sqlite3.connect(env["db"], timeout=5)
job  = env["job_id"]
att  = env["attempt"]
n    = int(sys.argv[1])

print(f"starting {job} (attempt {att})", file=sys.stderr)

db.execute(
    "CREATE TABLE IF NOT EXISTS results "
    "(job_id TEXT PRIMARY KEY, result_n INTEGER, ts TEXT)"
)

# Idempotency check — if a prior attempt already wrote our row, exit clean.
done = db.execute(
    "SELECT 1 FROM results WHERE job_id = ?", (job,)
).fetchone()
if done:
    print("already done; skipping", file=sys.stderr)
    sys.exit(0)

# Pretend work.
time.sleep(0.1 + n / 50)

with db:
    db.execute(
        "INSERT INTO results(job_id, result_n, ts) VALUES(?, ?, datetime('now'))",
        (job, n),
    )

# Inject failure on the first attempt of one job to exercise the retry path.
if n == 7 and att == 1:
    print("synthetic fail", file=sys.stderr)
    sys.exit(7)
```

Run it:

```sh
chmod +x plan.py worker.py
./plan.py | xjobs --workers 4
sqlite3 .xjobs/db.sql3 'SELECT COUNT(*), AVG(result_n) FROM results;'
```

## State layout

```
.xjobs/
├── db.sql3              # SQLite, WAL
├── db.sql3-wal
├── db.sql3-shm
└── <job-id>/            # one dir per attempted job
    ├── lock             # exclusive flock held for the child's lifetime
    └── pty.hootty.log   # binary frame stream of the child's terminal output
                         #   (libghostty PTY; see "Observing a job's output")
```

Default state dir is `./.xjobs/`. Override with `--state-dir <path>`.
Multiple state dirs in different CWDs are independent queues.

**MVP today**: PTY integration is deferred, so the current build writes
a plain-text `<job-id>/output.log` (stdout + stderr captured via pipes)
instead of `pty.hootty.log`. Same `lock` file, same role.

`<job-id>/lock` is the liveness signal: while a runner is hosting the
child, the flock is held; if the runner dies, the OS releases it. On the
next drain, the **reaper pass** probes every `running` row's lock and
resets stranded ones (prior owner gone) to `pending`. This is what makes
`xjobs` after a crash a self-healing "just run it again" operation.

State dir must live on a **local filesystem** — SQLite WAL and the flock
semantics don't work over NFS.

## Status

Early MVP. Working:

- JSONL pump (file or stdin) with `INSERT OR IGNORE` dedup.
- Worker pool, per-job process spawn, `XJOBS` env injection, exit-code /
  signal capture (symbolic name via `unix.SignalName`).
- Flock-based reaper at drain start.
- Retry on failure up to `--max-attempts`.
- `running` / `success` / `error` events on stdout and in the `events`
  table.
- Verbs: bare / `run` / `resume` / `ls` / `monitor`.

Deferred (the spec covers these; not yet implemented):

- **libghostty PTY per job** (via the [hootty](https://github.com/hayeah/hootty)
  library), so `hoot attach --state-dir .xjobs <id>` works for live
  observation and `hoot log <id>` for replay. The Service seam in
  `internal/runner/service.go` is the swap point.
- `attach` / `log` / `kill` / `write` pass-throughs to `hoot`.
- `retry` / `rm` / `sql` verbs.
- Graceful SIGINT shutdown with a configurable grace window. Today,
  ctx-cancel propagates to `exec.CommandContext` and SIGKILLs the child
  immediately.

Full design: [`SPEC.md`](SPEC.md).
