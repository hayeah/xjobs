package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Event is one entry on the xjobs stdout JSONL stream.
// Mirrors a row in the events table.
type Event struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`            // "success" | "error"
	ID      string `json:"id"`
	Attempt int    `json:"attempt"`
	DurMS   int64  `json:"dur_ms"`
	Exit    *int   `json:"exit,omitempty"`
	Signal  string `json:"signal,omitempty"`
	Error   string `json:"error,omitempty"`
}

// eventSink fans events out to the events table and to a stdout-style writer.
type eventSink struct {
	rn  *Runner
	mu  sync.Mutex
	out io.Writer
	enc *json.Encoder
}

func newEventSink(rn *Runner, out io.Writer) *eventSink {
	s := &eventSink{rn: rn, out: out}
	if out != nil {
		s.enc = json.NewEncoder(out)
		s.enc.SetEscapeHTML(false)
	}
	return s
}

func (s *eventSink) emit(ctx context.Context, ev Event) error {
	if ev.TS == "" {
		ev.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	s.rn.writeMu.Lock()
	_, err = s.rn.db.ExecContext(ctx,
		`INSERT INTO events(ts, job_id, attempt, kind, data) VALUES(?, ?, ?, ?, ?)`,
		ev.TS, ev.ID, ev.Attempt, ev.Kind, string(data),
	)
	s.rn.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}

	if s.enc != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enc.Encode(ev); err != nil {
			return fmt.Errorf("encode event: %w", err)
		}
	}
	return nil
}

// fromResult converts a runResult to a user-visible Event.
func eventFromResult(id string, attempt int, dur time.Duration, res runResult) Event {
	ev := Event{
		ID:      id,
		Attempt: attempt,
		DurMS:   dur.Milliseconds(),
	}
	switch {
	case res.Err != nil:
		ev.Kind = "error"
		ev.Error = res.Err.Error()
	case res.Signal != "":
		ev.Kind = "error"
		ev.Signal = res.Signal
		ev.Error = "killed by " + res.Signal
		ex := res.ExitCode
		ev.Exit = &ex
	case res.ExitCode == 0:
		ev.Kind = "success"
		ex := 0
		ev.Exit = &ex
	default:
		ev.Kind = "error"
		ex := res.ExitCode
		ev.Exit = &ex
		ev.Error = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return ev
}
