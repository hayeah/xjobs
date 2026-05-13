# hootty / libghostty PTY integration (deferred)

This doc is a parking spot for the design thinking behind running each
xjobs child on a libghostty-backed PTY via the
[hootty](https://github.com/hayeah/hootty) library. The integration was
sketched in detail in the first cut of xjobs's README/SPEC but was never
wired up — the libghostty cgo dependency (Zig + CMake + pkg-config) is a
meaningful build-prereq increase that the first cut deferred. Today
xjobs spawns children through plain pipes and writes their combined
stdout + stderr to `.xjobs/<id>/output.log`.

If/when xjobs revisits the integration, this is the design to start
from.

## Why a PTY at all

Job processes commonly produce rich terminal output — TUI redraws,
progress bars, inline `\r`-updates, ANSI colors. A line-oriented log of
that firehose is unreadable: escape codes inline, cursor moves rendered
as text, overwritten lines piling up. A claude/codex run, a `cargo
build`, an `apt` installer, a `huggingface-cli` download — all of these
look like garbage in a flat append-only log.

The answer is **one libghostty PTY per job**, hosted by hootty. The PTY
parses the child's byte stream through a real VT emulator, so:

- The current "screen" is always recoverable — what an operator would
  see if they looked at the terminal *now*.
- A binary frame stream (`pty.hootty.log`) is persisted to disk for
  later replay.
- Live attach is a real attach (cursor placement, alt-screen, kitty
  keyboard) — not a tail of bytes.

## Operator / agent affordances

Three verbs ride on the PTY:

| verb                                    | purpose                                                                |
|-----------------------------------------|------------------------------------------------------------------------|
| `xjobs attach <id>`                     | attach the current terminal to the live job                            |
| `xjobs log <id> --format plain`         | one-shot snapshot of the current screen as plain text                  |
| `xjobs log <id>`                        | full VT replay of the recorded frame stream                            |

All three are thin pass-throughs to `hoot --state-dir .xjobs`. The state
dir is intentionally hootty-compatible, so `hoot list/attach/log/kill/write
--state-dir .xjobs` work as native verbs against an xjobs queue.

`xjobs log <id> --format plain` is the right thing for agents that need
a one-shot snapshot ("what does this job look like right now?"). The
full replay form is for humans tailing a finished job.

For machine-readable structured output that downstream tools should
consume, write to the shared SQLite DB via an app-specific table. The
PTY captures what an operator would *look at*; the DB carries what a
script would *parse*.

## State layout under the integration

```
.xjobs/
├── db.sql3
├── db.sql3-wal
├── db.sql3-shm
└── <job-id>/
    ├── lock             # exclusive flock held for the child's lifetime
    └── pty.hootty.log   # binary frame stream of the child's terminal output
                         #   (libghostty PTY recording)
```

`pty.hootty.log` replaces today's `output.log` 1:1. Same `lock` file,
same role — per-job flock as the liveness signal probed by the reaper.

Under the integration, the per-job flock moves from xjobs's own
`.xjobs/<id>/lock` to hootty's session-dir flock; reaper logic stays
identical because hootty uses the same flock-on-session-dir convention.

## Implementation seam

The swap point is `execAttempt` in `internal/runner/service.go` — same
signature, different implementation:

- Today: `os.exec.Cmd` with stdout/stderr piped to `<id>/output.log`.
- Future: open a hootty session for `<id>` against the state dir; the
  child runs inside that session; hootty owns the frame stream and the
  flock.

The runner's `jobs` row already reserves `session_key TEXT` for this
purpose — populated on claim, used by the pass-through verbs to find the
right hootty session.

Everything else — the SQLite schema, the work-queue predicate, the
reaper, the events table — is unchanged.

## Build-prereq cost (why this is deferred)

Adding hootty pulls libghostty in transitively, which means the xjobs
build needs:

- Zig (libghostty is Zig under the hood)
- CMake
- pkg-config

…on top of the pure-Go toolchain. The first cut wanted xjobs to be a
single `go build`, hence the deferral. When the integration lands, the
README install section will need a "Build prereqs" subsection covering
these, and the Makefile likely grows a `make deps` target.

## Companion CLI verbs (also deferred)

Once the PTY is wired, several pass-through verbs become trivial:

- **`xjobs attach <id>`** — `hoot attach --state-dir .xjobs <id>`
- **`xjobs log <id>`** / `--format plain` — `hoot log --state-dir .xjobs <id>`
- **`xjobs kill <id>`** — `hoot kill --state-dir .xjobs <id>`
- **`xjobs write <id> "..."`** — `hoot write --state-dir .xjobs <id>`

All four are landed alongside the integration, not before — without the
PTY there's nothing for them to attach to.

## Status (2026-05-13)

Integration deferred. Earlier attempt
`wire-actual-hootty-dep-into-xjobs` was closed unmerged on 2026-05-13.
The PTY capabilities themselves are not future work — they already
exist in hootty as working CLI verbs against any hootty state dir.
xjobs just needs to import the library and swap `execAttempt`.
