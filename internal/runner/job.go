package runner

import (
	"encoding/json"
	"fmt"
)

// Job is the JSONL line shape pumped into xjobs.
type Job struct {
	ID   string            `json:"id"`
	CWD  string            `json:"cwd,omitempty"`
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env,omitempty"`
	Meta json.RawMessage   `json:"meta,omitempty"`
}

func (j *Job) validate() error {
	if j.ID == "" {
		return fmt.Errorf("missing id")
	}
	if len(j.Argv) == 0 {
		return fmt.Errorf("missing or empty argv (id=%q)", j.ID)
	}
	return nil
}
