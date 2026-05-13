---
status: done
section: Clean up hootty references from xjobs docs
slug: clean-up-hootty-references-from-xjobs-docs
mode: worktree
spec:
created: 2026-05-13T09:13:41Z
---

> ## Clean up hootty references from xjobs docs
>
> ---
> status:
>   type: open
> ---
>
> xjobs's README and SPEC currently describe the hootty/libghostty integration as the intended model (README lines ~230–262, plus scattered mentions of `pty.hootty.log` and `xjobs attach/log`). We just decided to defer that integration (`wire-actual-hootty-dep-into-xjobs` was closed unmerged on 2026-05-13).
>
> Tidy up:
>
> - Move the hootty integration design out of the main README/SPEC into a standalone future-feature doc (e.g. `docs/future/hootty-integration.md`). Don't delete it — the design thinking is worth keeping for whenever we revisit.
> - Update the README/SPEC to describe what xjobs actually does today: plain pipes → `.xjobs/<id>/output.log`. The current "MVP today" paragraph (lines 254-262) is mostly right but it's framed as a deviation from a future state; flip it to be the current state.
> - Drop or rephrase `xjobs attach`, `xjobs log` mentions that imply they exist as live verbs. If they're aspirational, they belong in the future-feature doc.
> - Path references: `pty.hootty.log` → `output.log` in the state-layout section.
>
> Repo: `~/github.com/hayeah/xjobs/`.
>
> - [ ] implement and verify

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

- [x] Draft `docs/future/hootty-integration.md` consolidating the deferred PTY design from README/SPEC (ca00a37)
- [x] Update `README.md`: replace "Observing a job's output" with current plain-pipes behavior, rephrase `xjobs attach` example, swap state-layout `pty.hootty.log` → `output.log`, slim the Status deferred bullet (6b45042)
- [x] Update `SPEC.md`: rewrite "Observing a Job's Output" + State Layout to describe current behavior, slim Future Work to pointers, scrub stray hootty refs in Core Idea / Notes (324212c)
- [x] Verify: grep for residual stale references; render-check via `markdown` tooling or eyeball; ensure TOCs and internal links still resolve
- [x] Commit per-step and record evidence

## Agent log
- 2026-05-13T09:15Z ca00a37 — parked hootty/libghostty PTY design under docs/future/hootty-integration.md
- 2026-05-13T09:16Z 6b45042 — README rewritten to describe current behavior; hootty references trimmed to pointers
- 2026-05-13T09:18Z 324212c — SPEC.md rewritten to describe current behavior; deferred PTY collapsed to pointers
- 2026-05-13T09:19Z status: done — three commits on branch (ca00a37, 6b45042, 324212c); residual hootty mentions are intentional pointers; smoke run confirms output.log layout matches docs

## Boss log

## Evidence

### Three commits on the feature branch

```
324212c SPEC: describe current plain-pipes behavior, not deferred PTY
6b45042 README: describe current plain-pipes behavior, not deferred PTY
ca00a37 Park hootty/libghostty PTY design under docs/future/
```

### Stale references swept

```
$ grep -nE "hootty|pty\.hootty|hoot |xjobs attach|xjobs log" README.md
253:integration — see [`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).
392:- **libghostty PTY per job** via the [hootty](https://github.com/hayeah/hootty)
395:  [`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).

$ grep -nE "hootty|pty\.hootty|hoot |xjobs attach|xjobs log" SPEC.md
51:views. (A future libghostty/hootty integration would extend the same
53:[`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).)
76:`output.log` with a binary frame stream (`pty.hootty.log`) plus live
78:[`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).
152:    session_key TEXT,                                   -- reserved for hootty integration
352:  [`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).
354:There are no `xjobs attach` / `xjobs log` verbs today; they'd land
355:alongside the PTY integration as pass-throughs to hootty.
538:libghostty-backed PTY using the [hootty](https://github.com/hayeah/hootty)
539:library, with `xjobs attach` / `xjobs log` / `xjobs kill` / `xjobs
544:[`docs/future/hootty-integration.md`](docs/future/hootty-integration.md).
```

All remaining mentions are either pointers to the parked design doc or
notes that the verb / capability is deferred. No bare claims about
live `xjobs attach` / `xjobs log` / `pty.hootty.log` remain.
`session_key TEXT` in the schema is correctly annotated as reserved.

### Docs now match code

```
$ grep -n "output.log" internal/runner/service.go
28://  3. writes XJOBS env + opens output.log,
56:	logPath := filepath.Join(jobDir, "output.log")
```

The runner writes to `output.log` — exactly the path the README and
SPEC state layouts now show.

### Build still works

```
$ make build
go build -o bin/xjobs ./cmd/xjobs
$ ls -l bin/xjobs
-rwxr-xr-x@ 1 me  staff  10173746 May 13 16:18 bin/xjobs
```

### CLI verb set matches the docs (no `attach` / `log` / `kill` / `write`)

```
$ bin/xjobs --help | head -7
xjobs — parallel job runner backed by SQLite.

usage:
  xjobs [flags] [<file.jsonl> | -]   pump (file > stdin > none) + drain
  xjobs run     [flags] [<file.jsonl>]   same as bare
  xjobs resume  [flags]                  drain only; ignore any stdin
  xjobs ls      [flags] [--json] [--where SQL]
  xjobs monitor [flags] [--id ID]
```

### Smoke run confirms `output.log` is what xjobs actually produces

Full transcript: `tmp/091842_000-xjobs-smoke-transcript.txt`. Key bits:

```
$ printf '{"id":"demo:hello","argv":["sh","-c","echo hello world; echo err 1>&2"]}\n' | xjobs --workers 1
xjobs: pumped 1 / skipped 0 / total 1 from stdin
{"ts":"...","kind":"running","id":"demo:hello","attempt":1,"pid":52296}
{"ts":"...","kind":"success","id":"demo:hello","attempt":1,"dur_ms":3,"exit":0}

$ find .xjobs -maxdepth 3 | sort
.xjobs
.xjobs/db.sql3
.xjobs/demo:hello
.xjobs/demo:hello/lock
.xjobs/demo:hello/output.log

$ cat .xjobs/demo:hello/output.log
hello world
err
```

Tree matches the diagram in `README.md` State Layout exactly (`lock` +
`output.log`, no `pty.hootty.log`).

## Trouble report

Nothing notable. The section was tightly scoped doc cleanup with no
design ambiguity — the SPEC's existing "MVP transitional" framing made
it easy to identify what was aspirational and what was current state.
A couple small judgment calls along the way:

- **`xjobs attach <id>` example in the id-convention section.** The
  original text leaned on `xjobs attach tt0133093:download` to show
  that the convention is self-documenting in shell history. Since
  `attach` doesn't exist today, I swapped to `xjobs ls --where "id
  LIKE 'tt0133093:%'"` which makes the same point with a verb that
  works.
- **`session_key TEXT` schema comment** (`-- reserved for hootty
  integration`). Kept as-is. It's a real reserved column the runner
  actually has, and the comment usefully signals to a future reader
  why an apparently-unused column is there.
- **Folded the deferred `attach` / `log` / `kill` / `write` verb
  bullet into the "libghostty PTY" Future Work entry** rather than
  keeping it as a separate item. They only make sense alongside the
  PTY integration — `hoot` is what they pass through to — so separating
  them was misleading.
