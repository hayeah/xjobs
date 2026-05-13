---
status: done
section: Merge xjobs run and resume into a single command
slug: merge-xjobs-run-and-resume-into-a-single-command
mode: worktree
spec: /Users/me/Dropbox/notes/2026-05-13/xjobs-merge-run-resume_claude.md
created: 2026-05-13T10:55:16Z
---

> ## Merge xjobs run and resume into a single command
>
> ---
> status:
>   type: open
> ---
>
> Implement per the spec note at:
>
> `/Users/me/Dropbox/notes/2026-05-13/xjobs-merge-run-resume_claude.md`
>
> TL;DR: drop the `resume` verb. `run` (or bare) sniffs inputs (positional > piped stdin > none) and naturally degrades to drain-only when there's no input — exactly what `resume` did. The "I have piped stdin but want to drain only" case is recoverable via `xjobs < /dev/null`.
>
> Master tip is `a7851fa`. The note also includes a "CLI flag scoping" cleanup section — recent merges already removed `--nice` and `--max-attempts` from `bindCommon` (`d2076e9`), so part of that cleanup is done. Check the current state and only do what's still relevant (e.g. scoping `--workers` to `run`-only, hiding `--where` from `monitor`'s help, etc.).
>
> Update SPEC.md + README.md to drop `resume` from the verb table and reflect the new help output.
>
> Repo: `~/github.com/hayeah/xjobs/`.
>
> - [ ] implement and verify

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

- [x] cmd/xjobs/main.go: drop `cmdResume` + dispatch case (43994cb)
- [x] cmd/xjobs/main.go: scope flags per-subcommand — narrowed shared helper to `bindStateDir` (state-dir is the only flag every subcommand needs); `cmdRun` adds workers + where, `cmdLS` adds where + json, `cmdMonitor` adds id (43994cb + e960091)
- [x] cmd/xjobs/main.go: rewrote `usage()` — dropped the global `flags:` block, added "see `xjobs <cmd> -h`" pointer, dropped resume line + example (43994cb)
- [x] cmd/xjobs/e2e_test.go: renamed `TestE2EResumeReapsAndRerunsAfterKilledRunner` → `TestE2EDrainReapsAndRerunsAfterKilledRunner`; uses bare invocation with `cmd.Stdin = nil` (= /dev/null = drain-only); dropped `"resume"` from `isVerb` (e960091)
- [x] SPEC.md: dropped `xjobs resume` from CLI surface table + input-precedence block; rewrote "Resume semantics"; fixed subcommand-flag-ordering example; widened flag table with "subcommands" column (0ba22a2)
- [x] README.md: same drops + matching flag table column; status verbs list dropped resume (0ba22a2)
- [x] `go build ./...` + `go test ./...` clean
- [x] captured `-h` transcripts in `tmp/180350_760-xjobs-help.txt`

## Agent log

- 2026-05-13T18:00Z — Landed three commits on branch `merge-xjobs-run-and-resume-into-a-single-command` (off master tip `a7851fa`):
  - `43994cb` cmd/xjobs: drop resume verb; scope flags per-subcommand
  - `e960091` cmd/xjobs: narrow shared helper to state-dir only
  - `0ba22a2` SPEC + README: drop resume verb; per-subcommand flag scoping
- 2026-05-13T18:00Z — Boss steered mid-task: "only state-dir is common?" — yes, narrowed `bindCommon` (which had been `bindDrainFlags` for one commit) to a single `bindStateDir(fs) *string` helper. Each subcommand now registers everything else directly on its FlagSet. Captured in commit `e960091`.

## Boss log

## Evidence

### Help-text scoping (the main motivation)

Full transcript at `tmp/180350_760-xjobs-help.txt`. Highlights:

- `xjobs -h` now lists three subcommands (no `resume`), explains the
  bare-with-no-input = resume path, points at `xjobs <cmd> -h` for
  flags, and no longer prints a global `flags:` block.
- `xjobs run -h`: `state-dir`, `where`, `workers` (only).
- `xjobs ls -h`: `state-dir`, `where`, `json` (no `workers` leaking
  in any more).
- `xjobs monitor -h`: `state-dir`, `id` (no `where` / `workers`).
- `xjobs resume` errors out — hard break, no deprecation alias (per
  the spec note: personal project, single user, fine).

### Functional smoke

```
$ xjobs --state-dir .x jobs.jsonl              # pump + drain works
xjobs: pumped 3 / skipped 0 / total 3 from .../jobs.jsonl
{"kind":"running","id":"a",...}
{"kind":"running","id":"c",...}
{"kind":"running","id":"b",...}
{"kind":"error","id":"c","exit":1,...}
{"kind":"success","id":"a","exit":0,...}
{"kind":"success","id":"b","exit":0,...}
xjobs: 1 job failed                            # exit 1, one row failed

$ xjobs --state-dir .x < /dev/null             # bare drain (was `resume`)
xjobs: 1 job failed                            # exit 1 — drained, terminal failed row remains

$ xjobs ls --state-dir .x --where 'status="failed"'   # ls flag scoping works
[c]	failed	exit=1	/bin/sh -c exit 1

$ xjobs resume                                  # verb gone
xjobs: open resume: open resume: no such file or directory
exit=1
```

The reap-after-killed-runner case (the original `resume` use case) is
covered by `TestE2EDrainReapsAndRerunsAfterKilledRunner` in
`cmd/xjobs/e2e_test.go`, which kills the runner mid-job and re-runs
xjobs with `cmd.Stdin = nil` (= /dev/null) to drain.

### Test suite

```
$ go test ./...
ok  	github.com/hayeah/xjobs/cmd/xjobs	6.2s
ok  	github.com/hayeah/xjobs/internal/runner	0.5s
```

All e2e tests including the renamed drain-reaper test pass.

## Trouble report

- **zsh `multios` quirk for `pipe | cmd < /dev/null`.** While
  smoke-testing the hatch, I noticed that in zsh (default option:
  `multios`), `echo X | xjobs < /dev/null` still pumps the piped row
  — the pipe wins over the redirect. In bash and POSIX sh, the
  redirect wins (drain-only, as the spec note describes). The
  spec-note recipe `xjobs < /dev/null` (no pipe) works fine in every
  shell, so I left the docs as-is — but worth noting in case a zsh
  user hits this. Not a code issue; an interactive-shell footgun.
- No other friction. Master tip stayed at `a7851fa` throughout; no
  conflicts.
