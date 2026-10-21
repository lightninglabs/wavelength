// Package bridge loads concrete traces shared by the P forfeit-signing model
// and the Go conformance tests at the production session and broker boundaries.
package bridge

import (
	"encoding/json"
	"fmt"
	"os"
)

// Trace is one concrete forfeit-signing lifecycle scenario.
type Trace struct {
	TraceID     string `json:"trace_id"`
	Description string `json:"description"`
	Steps       []Step `json:"steps"`
}

// Step describes one authority, delivery, restart, or broker operation.
type Step struct {
	Op        string `json:"op"`
	Identity  string `json:"identity,omitempty"`
	Signature string `json:"signature,omitempty"`
	Ack       string `json:"ack,omitempty"`
	Expect    string `json:"expect,omitempty"`
	Durable   bool   `json:"durable,omitempty"`
}

// ParseTrace parses a checked-in forfeit-signing bridge trace.
func ParseTrace(path string) (*Trace, error) {
	//nolint:gosec // The bridge reads checked-in model traces only.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read forfeit-signing trace: %w", err)
	}

	var trace Trace
	if err := json.Unmarshal(data, &trace); err != nil {
		return nil, fmt.Errorf("parse forfeit-signing trace: %w", err)
	}
	if trace.TraceID == "" {
		return nil, fmt.Errorf("forfeit-signing trace missing trace_id")
	}
	if len(trace.Steps) == 0 {
		return nil, fmt.Errorf("forfeit-signing trace %s has no steps",
			trace.TraceID)
	}

	return &trace, nil
}
