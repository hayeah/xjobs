package runner

import (
	"encoding/json"
	"fmt"
)

// Job is the JSONL line shape pumped into xjobs.
//
// Nice and MaxAttempts are pointer-typed so the JSONL can distinguish
// "field absent" (nil — runner default) from "field explicitly zero"
// (a valid value the user picked deliberately).
type Job struct {
	ID          string            `json:"id"`
	CWD         string            `json:"cwd,omitempty"`
	Argv        []string          `json:"argv"`
	Env         map[string]string `json:"env,omitempty"`
	Meta        json.RawMessage   `json:"meta,omitempty"`
	Nice        *int              `json:"nice,omitempty"`
	MaxAttempts *int              `json:"max_attempts,omitempty"`
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
