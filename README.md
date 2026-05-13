# xjobs

Parallel job runner with SQLite state. Like `xargs -P`, but for long-running
jobs that need a durable queue, resume after interruption, and per-job
process isolation.

Spec: `~/Dropbox/notes/2026-05-13/xjobs-spec_claude.md` (work in progress).

## Status

Early MVP. Current scope:

- Pump JSONL (file or stdin) into a SQLite `jobs` table.
- Drain via a worker pool, one OS process per job.
- Per-job flock for crash-resilient stale-row reaping.
- `success` / `error` JSONL events on stdout.
- Verbs: bare/run/resume/ls/monitor.

Deferred:

- libghostty PTY via the hootty library (`xjobs attach/log/kill/write`).
- `retry`, `rm`, `sql` verbs.
- `--snapshot-on-heartbeat` (deliberately dropped — see spec).

## Build

```sh
make build         # → bin/xjobs
```

## Usage

```sh
# generate a jsonl plan and pump it
./plan.sh > jobs.jsonl
xjobs jobs.jsonl

# resume whatever's left
xjobs

# stream events
xjobs jobs.jsonl | jq .

# inspect mid-run
xjobs ls
xjobs monitor
```

JSONL line shape:

```jsonc
{
  "id":   "tt0133093:download",
  "cwd":  "/abs/path",
  "argv": ["./worker", "tt0133093"],
  "env":  { "FOO": "bar" },
  "meta": { "size": 12345 }
}
```
