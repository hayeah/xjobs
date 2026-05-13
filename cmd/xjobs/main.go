// Command xjobs is a parallel job runner backed by SQLite.
//
// See ~/Dropbox/notes/2026-05-13/xjobs-spec_claude.md for the design spec.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hayeah/xjobs/internal/runner"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "xjobs:", err)
		}
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) > 0 {
		switch argv[0] {
		case "ls":
			return cmdLS(argv[1:])
		case "monitor":
			return cmdMonitor(argv[1:])
		case "run":
			return cmdRun(argv[1:])
		case "-h", "--help", "help":
			usage(os.Stdout)
			return nil
		}
	}
	return cmdRun(argv)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `xjobs — parallel job runner backed by SQLite.

usage:
  xjobs [flags] [<file.jsonl> | -]       pump + drain (alias for `+"`"+`run`+"`"+`)
  xjobs run     [flags] [<file.jsonl>]
  xjobs ls      [flags] [--json] [--where SQL]
  xjobs monitor [flags] [--id ID]

With no input source, run/bare drains the queue and exits — the
former `+"`"+`xjobs resume`+"`"+` behavior. Pipe `+"`"+`< /dev/null`+"`"+` to force
drain-only when stdin is already piped.

Run `+"`"+`xjobs <command> -h`+"`"+` for command-specific flags.

input:
  JSONL lines: {"id":"…", "cwd":"…", "argv":["…"], "env":{}, "meta":{}, "nice":N, "max_attempts":N}
  Duplicate ids are silently skipped (INSERT OR IGNORE).`)
}

// bindStateDir registers --state-dir on fs and returns a pointer to the
// parsed value. It's the only flag every subcommand needs.
func bindStateDir(fs *flag.FlagSet) *string {
	return fs.String("state-dir", ".xjobs", "state dir holding db.sql3 + per-job session dirs")
}

func cmdRun(argv []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	stateDir := bindStateDir(fs)
	var workers int
	var where string
	fs.IntVar(&workers, "workers", 0, "concurrent job processes (default: NumCPU)")
	fs.StringVar(&where, "where", "", "SQL fragment AND-combined with the work-queue predicate")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	rest := fs.Args()
	rn, err := runner.Open(*stateDir)
	if err != nil {
		return err
	}
	defer rn.Close()

	ctx, cancel := signalCtx()
	defer cancel()

	src, srcName, srcOpen, err := openPumpSource(rest)
	if err != nil {
		return err
	}

	pumpDone := make(chan struct{})
	pumpErrC := make(chan error, 1)
	if srcOpen {
		go func() {
			defer close(pumpDone)
			pumpErrC <- runPump(ctx, rn, src, srcName)
		}()
	} else {
		close(pumpDone)
		pumpErrC <- nil
	}

	opts := runner.Options{StateDir: *stateDir, Workers: workers, Where: where}
	drainErr := rn.Drain(ctx, opts, pumpDone, os.Stdout)
	pumpErr := <-pumpErrC
	if pumpErr != nil && !errors.Is(pumpErr, context.Canceled) {
		return pumpErr
	}
	if drainErr != nil {
		return drainErr
	}
	return pumpErr
}

func runPump(ctx context.Context, rn *runner.Runner, src io.Reader, name string) error {
	ins, skip, total, err := rn.Pump(ctx, src)
	if c, ok := src.(io.Closer); ok {
		_ = c.Close()
	}
	if err != nil {
		return fmt.Errorf("pump %s: %w", name, err)
	}
	fmt.Fprintf(os.Stderr, "xjobs: pumped %d / skipped %d / total %d from %s\n", ins, skip, total, name)
	return nil
}

func cmdLS(argv []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	stateDir := bindStateDir(fs)
	var where string
	var jsonOut bool
	fs.StringVar(&where, "where", "", "SQL fragment AND-combined with the work-queue predicate")
	fs.BoolVar(&jsonOut, "json", false, "emit JSONL rows instead of text")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	rn, err := runner.Open(*stateDir)
	if err != nil {
		return err
	}
	defer rn.Close()
	ctx, cancel := signalCtx()
	defer cancel()
	rows, err := rn.LS(ctx, where)
	if err != nil {
		return err
	}
	if jsonOut {
		return runner.PrintLSJSON(os.Stdout, rows)
	}
	runner.PrintLSText(os.Stdout, rows)
	return nil
}

func cmdMonitor(argv []string) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	stateDir := bindStateDir(fs)
	var id string
	fs.StringVar(&id, "id", "", "filter to a single job id")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	rn, err := runner.Open(*stateDir)
	if err != nil {
		return err
	}
	defer rn.Close()
	ctx, cancel := signalCtx()
	defer cancel()
	return rn.Monitor(ctx, id, 0, os.Stdout)
}

// openPumpSource picks an input per precedence: positional file arg > "-" > piped stdin > none.
// If `srcOpen` is false, caller should skip pumping (drain-only).
func openPumpSource(positional []string) (src io.Reader, name string, srcOpen bool, err error) {
	if len(positional) > 1 {
		return nil, "", false, fmt.Errorf("at most one input file allowed, got %d", len(positional))
	}
	if len(positional) == 1 {
		p := positional[0]
		if p == "-" {
			return os.Stdin, "stdin", true, nil
		}
		f, oerr := os.Open(p)
		if oerr != nil {
			return nil, "", false, fmt.Errorf("open %s: %w", p, oerr)
		}
		abs, _ := filepath.Abs(p)
		return f, abs, true, nil
	}
	// No positional. Sniff stdin: only treat as a pump source when it's not a TTY.
	st, err := os.Stdin.Stat()
	if err != nil {
		return nil, "", false, nil
	}
	if (st.Mode() & os.ModeCharDevice) == 0 {
		return os.Stdin, "stdin", true, nil
	}
	return nil, "", false, nil
}

func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	return ctx, cancel
}
