---
status: done
section: Claude refactor pass on xjobs (post-codex fixes)
slug: claude-refactor-pass-on-xjobs-post-codex-fixes
mode: worktree
spec:
created: 2026-05-13T10:24:56Z
---

> ## Claude refactor pass on xjobs (post-codex fixes)
>
> ---
> status:
>   type: open
> ---
>
> Codex just landed a substantial fix bundle in `ddc5b81` covering correctness fixes from two prior reviews (path-id validation, exit codes, signal finalization, process-group kill, retry log retention, monitor bad-id, ls SIGINT, etc.), plus new unit + e2e regression coverage.
>
> Do a code-quality pass on the codex output (and the surrounding xjobs code it touches). No new functionality — style, structure, and clarity only.
>
> What to look for, in order of importance:
>
> - **"Stupid code" patterns codex tends toward**: over-defensive `if err != nil { return err }` chains that obscure flow, unnecessary nil-checks for guaranteed-non-nil values, error-path duplication where a helper would cover all the call sites, mechanical-but-awkward boolean composition that would read better as early returns, double-handling (e.g. logging then returning a wrapped error that callers then log again).
> - **Redundancy**: similar code in multiple places that should be a helper. Repeated SQL string-builders. Repeated test scaffolding that could be a `t.Helper()`.
> - **Structure**: package-level concerns mixed in functions, function bodies past ~30 lines that don't read top-to-bottom, file organization that doesn't match the read order, public API surface bigger than needed (export only what's actually called from outside the package).
> - **Naming clarity**: cryptic variable names, helper-named-after-its-call-site, types/fields where the name doesn't carry intent.
>
> What NOT to do:
>
> - Don't change behavior. Tests stay green. No new features.
> - Don't sweep the entire codebase — focus on `48901f6` (the fix bundle) and the recent test additions. Pre-existing code is in scope only if it's in the immediate vicinity of a fix you're cleaning up.
> - Don't add comments unless removing one would confuse a future reader. The point of cleaner structure is that comments become unnecessary.
>
> When you're done: short `## Trouble report` listing the substantive changes you considered and rejected with one-line rationale (so a reviewer can see your judgment, not just your output).
>
> Repo: `~/github.com/hayeah/xjobs/` (master tip is `ddc5b81`).
>
> - [ ] implement and verify

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

- [x] Audit the 7 files changed by 48901f6 and the 2 new test files for cleanup candidates.
- [x] Run baseline `go test ./...` (green).
- [x] `cmd/xjobs/main.go`: extract pump goroutine body into a `runPump` helper so `cmdRun` reads top-to-bottom.
- [x] `internal/runner/runner.go`: collapse the 4-arm switch in `runOne` so the `return termErr` boilerplate appears once.
- [x] `internal/runner/runner.go`: drop the `idleSignal` closed-chan substitution in `Drain` — `feedQueue`/`isClosed` already handle `nil`.
- [x] `internal/runner/inspect.go`: factor `lastEvent`/`nextEvent` to share a `scanOneEvent` helper that handles `sql.ErrNoRows`.
- [x] `go test ./...` again to confirm green after refactor.
- [x] Commit, update worklog Evidence + Trouble report, set `status: done`.

## Agent log
- 2026-05-13T10:30Z landed 22bc8fb — cmd/xjobs runPump helper, runOne switch collapse, Drain nil-channel cleanup, lastEvent/nextEvent scanOneEvent share

## Boss log

## Evidence

One commit on `claude-refactor-pass-on-xjobs-post-codex-fixes`:

```
22bc8fb Tighten codex fix-bundle code for readability
 cmd/xjobs/main.go          | 37 ++++++++++++++++++++-----------------
 internal/runner/inspect.go | 14 +++++---------
 internal/runner/runner.go  | 30 +++++++++++-------------------
 3 files changed, 36 insertions(+), 45 deletions(-)
```

Tests — re-run with `-count=1` to bypass cache:

```
$ go test -count=1 ./...
ok  	github.com/hayeah/xjobs/cmd/xjobs	6.094s
ok  	github.com/hayeah/xjobs/internal/runner	0.365s
```

Same suite runs the codex regression coverage (`fixes_test.go` +
`e2e_test.go`): path-id validation, terminal failed drain exit code,
SIGINT mid-drain, retry-log retention, monitor bad ids, ls verb, exit
code matrix, process-group cleanup. All green.

Build & vet are clean. The three `inspect.go` LSP diagnostics
(`omitempty` on `sql.NullString` fields, `+=` in `oneline`) are
pre-existing, sit outside the fix-bundle scope, and have nothing to do
with this pass — leaving them alone.

### What changed and why

- `cmd/xjobs/main.go` — pump goroutine body extracted into `runPump`.
  `cmdRun` now reads as: parse → open runner → open source → spawn pump
  (one line) → drain → reconcile errors. The previous shape inlined the
  Pump call, the io.Closer cast, the `fmt.Errorf` wrap, the success
  stderr, AND two manual `pumpErrC <- ...` sends inside a goroutine
  literal — about 10 lines deep inside a 30-line function. The
  no-source branch still does the manual `close(pumpDone); pumpErrC <-
  nil`, which is the cheapest way to keep the drain blocking-on-receive
  contract uniform; not worth a second helper.
- `internal/runner/runner.go` — `runOne` switch: four arms each ending
  in `if termErr := ...; termErr != nil { return termErr }`. Collapsed
  to a single `var termErr error` + one nil-check after the switch, and
  pulled the repeated `sql.NullInt64{...exit code...}` literal into a
  local `exitCode` since the signal and default arms both use it. This
  is exactly the "stupid code" pattern the section flagged: error-path
  duplication across sibling branches.
- `internal/runner/runner.go` — `Drain` was wrapping a nil `pumpDone`
  into a freshly-closed channel before passing to `feedQueue`. But
  `feedQueue` calls `isClosed(pumpDone)` which already treats `nil` as
  "already done". Dead code that obscured the fact that nil is a valid
  caller value (which is exactly what `cmdResume` passes). Replaced
  with a one-line comment pointing at `isClosed`.
- `internal/runner/inspect.go` — `lastEvent` and `nextEvent` had
  identical `var r eventRow; err := ...QueryRowContext(...).Scan(...);
  if err == sql.ErrNoRows {...}; if err != nil {...}; return r, true,
  nil` tails. Factored into `scanOneEvent`, leaving each function as
  query-construction + one tail call. Same query construction stays
  inline because it differs (ORDER BY direction, the `e.id > ?`
  predicate, and the conditional JOIN) — extracting that would have
  cost clarity to save very little.

## Trouble report

Substantive changes considered and rejected:

- **Combine `pumpDone` + `pumpErrC` in `cmdRun` into a single channel**
  (e.g. `pumpResult chan error` whose close-vs-receive doubles as the
  done signal). Tempting — two channels for one event smells redundant.
  Rejected: the two channels are doing genuinely different jobs.
  `pumpDone` is what `feedQueue` polls via `isClosed` while draining;
  it must be a `chan struct{}` (or nil) so feedQueue's select-default
  pattern stays cheap. `pumpErrC` carries the value after the work is
  done. Merging them would force `feedQueue` to know about pump errors,
  which is a regression in separation.
- **Simplify the trailing `if pumpErr != nil && !errors.Is(pumpErr,
  context.Canceled) {...}` ladder in `cmdRun`.** The shape is awkward,
  but it's load-bearing: SIGINT must propagate as a non-nil error
  (exit 1) but drain's error is the more informative one for the
  user, so pump's `context.Canceled` gets demoted while drain's error
  wins. Rewriting it as a `switch` doesn't shorten anything.
- **Extract a `terminalFromResult(res) (termFn, args...)` helper** so
  the `runOne` switch becomes one line. Rejected: the switch arms are
  selecting between two *different* methods (`terminalOK` vs.
  `terminalFail`) with different argument shapes. Forcing them through
  a common signature would invent a sum type for a 4-arm switch — net
  loss in clarity.
- **`reapStaleRunning` uses an explicit `rows.Close()` instead of
  `defer rows.Close()`.** Looked replace-able, but the explicit Close
  is positioned before the per-id `writeMu.Lock()` UPDATE loop, and
  Go's `defer` would only release at function return. The current
  shape correctly frees the read connection before contending for the
  write mutex on the same DB handle, which matters under load.
- **`oneline` (`inspect.go`) uses `out += a` in a loop** when
  `strings.Join` is one line. Pre-existing, not touched by the codex
  fix bundle. Out of scope per the section's "immediate vicinity"
  rule — left alone.
- **The `*int` / `*sql.NullInt64` style for `Event.Exit` and
  `eventFromResult`'s repeated `ex := res.ExitCode; ev.Exit = &ex`
  pattern.** Cosmetic. `eventFromResult` wasn't touched by the fix
  bundle; the switch lives next to the runOne one I did refactor, but
  the local-address-of pattern is idiomatic Go for assigning a pointer
  to a struct field — no obviously cleaner shape.
- **The `*int` Job.Nice / Job.MaxAttempts shape and `nullStr` helper
  in claim.go.** Pre-existing API. Out of scope.
- **PrintLSJSON builds a `map[string]any` per row instead of a struct
  with `omitempty`.** Pre-existing; the LSRow struct uses
  `sql.NullInt64`/`sql.NullString` which don't satisfy the `omitempty`
  semantics needed here. A struct-with-pointer-fields refactor is a
  legitimate cleanup, but it's outside the fix-bundle scope and would
  touch the public type signature.
- **`flockHandle.Close`'s `h == nil || h.f == nil` guard.** Pre-
  existing. The `h == nil` half is dead (every caller goes through
  `flockAcquire` which returns a non-nil handle on success and never
  calls Close on a nil receiver in this codebase), but the `h.f ==
  nil` guard makes it idempotent. Touching this is bigger than the
  win.
- **Pre-existing LSP diagnostics on `inspect.go` lines 19/20 (omitempty
  on `sql.NullString` struct fields) and 128 (`+=` in oneline).**
  Out of scope; not introduced by codex's fix bundle and not in the
  immediate vicinity of one.
