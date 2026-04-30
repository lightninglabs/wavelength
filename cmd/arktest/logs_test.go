//go:build itest

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLogTargetsIncludesOperatorAndClients verifies the persisted state is
// enough to locate the sparse event, operator, client, and client-LND logs.
func TestLogTargetsIncludesOperatorAndClients(t *testing.T) {
	state := &harnessState{
		RunDir: t.TempDir(),
		Clients: map[string]*arkClientState{
			"client05": {
				Name:    "client05",
				DataDir: filepath.Join(t.TempDir(), "client05"),
			},
		},
		ClientLNDs: map[string]*lndState{
			"client05": {
				Name: "client05",
				DataDir: filepath.Join(
					t.TempDir(), "client05-lnd",
				),
			},
		},
	}

	targets := logTargets(state)
	assertTarget := func(name, want string) {
		t.Helper()

		target, ok := findLogTarget(targets, name)
		require.True(t, ok, "target %s missing", name)
		require.Equal(t, want, target.Path)
	}

	assertTarget(
		"events", filepath.Join(state.RunDir, defaultEventLogName),
	)
	assertTarget(
		"operator", filepath.Join(state.RunDir, "arkd", "arkd.log"),
	)
	assertTarget(
		"client05",
		filepath.Join(state.Clients["client05"].DataDir, "darepod.log"),
	)
	assertTarget(
		"client05-lnd",
		lndLogPath(state.ClientLNDs["client05"].DataDir),
	)
}

// TestEventLogWritesConsoleAndJSONL verifies sparse events are mirrored to the
// terminal writer and the JSON-lines artifact.
func TestEventLogWritesConsoleAndJSONL(t *testing.T) {
	var stdout bytes.Buffer
	path := filepath.Join(t.TempDir(), defaultEventLogName)

	events, err := newEventLog(&stdout, path)
	require.NoError(t, err)

	events.Print("ready", "arktest ready", map[string]any{
		"clients": []string{"alice", "bob"},
	})
	require.NoError(t, events.Close())

	require.Contains(t, stdout.String(), "arktest ready")

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(body), `"kind":"ready"`)
	require.Contains(t, string(body), `"message":"arktest ready"`)
}

// TestEventLogAttachFileFlushesEarlyEvents verifies startup events printed
// before the run directory exists are preserved once the artifact is attached.
func TestEventLogAttachFileFlushesEarlyEvents(t *testing.T) {
	var stdout bytes.Buffer
	path := filepath.Join(t.TempDir(), defaultEventLogName)

	events, err := newEventLog(&stdout, "")
	require.NoError(t, err)

	events.Print("start", "starting bitcoind", nil)
	require.NoError(t, events.AttachFile(path))
	require.Empty(t, events.history)

	events.Print("ready", "arktest ready", nil)
	require.Empty(t, events.history)
	require.NoError(t, events.Close())

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(body), `"message":"starting bitcoind"`)
	require.Contains(t, string(body), `"message":"arktest ready"`)
}
