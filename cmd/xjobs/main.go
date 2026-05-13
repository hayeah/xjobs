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
	"strings"
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
		case "resume":
			return cmdResume(argv[1:])
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
  xjobs [flags] [<file.jsonl> | -]   pump (file > stdin > none) + drain
  xjobs run     [flags] [<file.jsonl>]   same as bare
  xjobs resume  [flags]                  drain only; ignore any stdin
  xjobs ls      [flags] [--json] [--where SQL]
  xjobs monitor [flags] [--id ID]

flags:
  --state-dir <path>     default .xjobs
  --workers N            default NumCPU
  --max-attempts N       default 3
  --nice N               default 5
  --where '<sql>'        AND-combined with the work-queue predicate

input:
  JSONL lines: {"id":"…", "cwd":"…", "argv":["…"], "env":{}, "meta":{}}
  Duplicate ids are silently skipped (INSERT OR IGNORE).`)
}

type commonFlags struct {
	StateDir    string
	Workers     int
	MaxAttempts int
	Nice        int
	Where       string
}

func bindCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.StateDir, "state-dir", ".xjobs", "state dir holding db.sql3 + per-job session dirs")
	fs.IntVar(&c.Workers, "workers", 0, "concurrent job processes (default: NumCPU)")
	fs.IntVar(&c.MaxAttempts, "max-attempts", 3, "retry ceiling for failed rows")
	fs.IntVar(&c.Nice, "nice", 5, "nice value applied to spawned children")
	fs.StringVar(&c.Where, "where", "", "SQL fragment AND-combined with the work-queue predicate")
	return c
}

func (c *commonFlags) opts() runner.Options {
	return runner.Options{
		StateDir:    c.StateDir,
		Workers:     c.Workers,
		MaxAttempts: c.MaxAttempts,
		Nice:        c.Nice,
		Where:       c.Where,
	}
}

func cmdRun(argv []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	rest := fs.Args()
	rn, err := runner.Open(c.StateDir)
	if err != nil {
		return err
	}
	defer rn.Close()

	ctx, cancel := signalCtx()
	defer cancel()

	pumpDone := make(chan struct{})

	src, srcName, srcOpen, err := openPumpSource(rest)
	if err != nil {
		return err
	}
	if !srcOpen {
		close(pumpDone) // nothing to pump; skip straight to drain
	} else {
		go func() {
			defer close(pumpDone)
			ins, skip, total, err := rn.Pump(ctx, src)
			if c, ok := src.(io.Closer); ok {
				_ = c.Close()
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "xjobs: pump %s: %v\n", srcName, err)
				return
			}
			fmt.Fprintf(os.Stderr, "xjobs: pumped %d / skipped %d / total %d from %s\n", ins, skip, total, srcName)
		}()
	}

	return rn.Drain(ctx, c.opts(), pumpDone, os.Stdout)
}

func cmdResume(argv []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("resume: unexpected positional args %v", fs.Args())
	}
	rn, err := runner.Open(c.StateDir)
	if err != nil {
		return err
	}
	defer rn.Close()
	ctx, cancel := signalCtx()
	defer cancel()
	return rn.Drain(ctx, c.opts(), nil, os.Stdout)
}

func cmdLS(argv []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	c := bindCommon(fs)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "emit JSONL rows instead of text")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	rn, err := runner.Open(c.StateDir)
	if err != nil {
		return err
	}
	defer rn.Close()
	rows, err := rn.LS(context.Background(), c.Where)
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
	c := bindCommon(fs)
	var id string
	fs.StringVar(&id, "id", "", "filter to a single job id")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	rn, err := runner.Open(c.StateDir)
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

// init to keep the strings import alive if we trim flags later.
var _ = strings.TrimSpace
