package bridge

import (
	"path/filepath"
	"testing"
)

// TestReplayMailboxModelTraces replays the checked-in P-model scenarios
// against the real actor-delivery SQLite store and claim SQL.
func TestReplayMailboxModelTraces(t *testing.T) {
	traces, err := ParseMailboxTraceDir(filepath.Join("..", "traces"))
	if err != nil {
		t.Fatalf("parse mailbox traces: %v", err)
	}

	if len(traces) == 0 {
		t.Fatal("no mailbox traces found")
	}

	for _, trace := range traces {
		trace := trace
		t.Run(trace.TraceID, func(t *testing.T) {
			ReplayMailboxTrace(t, trace)
		})
	}
}
