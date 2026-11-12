//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDiagnosticLogHistoryIsBoundedAndSecretFree proves capture stays bounded
// across rotation and reopening, and sensitive fields never reach storage.
func TestDiagnosticLogHistoryIsBoundedAndSecretFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "native.jsonl")
	logs := newDiagnosticLogBuffer(path)
	_, err := logs.Write(
		[]byte(
			"[INF] ROND: quote received\n[DBG] SWAP: " +
				"preimage=S3CR3T\n",
		),
	)
	require.NoError(t, err)
	require.Equal(t, "ROND", logs.entries[0].Source)
	require.Equal(t, "INFO", logs.entries[0].Level)
	require.Equal(t, "quote received", logs.entries[0].Message)
	require.NotContains(t, logs.entries[1].Message, "S3CR3T")
	stored, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(stored), "S3CR3T")
	for i := 0; i < 4500; i++ {
		_, err = logs.Write(
			[]byte(
				fmt.Sprintf(
					"[INF] ROND: message %d %s\n", i,
					strings.Repeat("x", 300),
				),
			),
		)
		require.NoError(t, err)
	}
	require.Len(t, logs.entries, 2000)
	for _, name := range []string{path, path + ".1"} {
		stat, err := os.Stat(name)
		require.NoError(t, err)
		require.LessOrEqual(t, stat.Size(), int64(1<<20))
	}
	reopened := newDiagnosticLogBuffer(path)
	require.NotEmpty(t, reopened.entries)
	require.LessOrEqual(t, len(reopened.entries), 2000)
	require.Equal(
		t, logs.entries[len(logs.entries)-1].Message,
		reopened.entries[len(reopened.entries)-1].Message,
	)
}

// TestDiagnosticLoggingOptInStaysScoped proves ordinary hosts keep capture
// disabled and external-seed startup reads only its nested configuration.
func TestDiagnosticLoggingOptInStaysScoped(t *testing.T) {
	require.False(t, diagnosticLoggingRequested(nil, false))
	require.True(t, diagnosticLoggingRequested(
		[]byte(`{"diagnostic_logs":true}`), false,
	))
	require.True(t, diagnosticLoggingRequested(
		[]byte(`{"config":{"diagnostic_logs":true}}`), true,
	))
	require.False(t, diagnosticLoggingRequested(
		[]byte(`{"config":{"diagnostic_logs":true}}`), false,
	))
	require.False(t, diagnosticLoggingRequested(
		[]byte(`{"diagnostic_logs":true}`), true,
	))
}

// TestDiagnosticLogWriteBoundsAllFieldsAndWork covers oversized metadata,
// many embedded newlines and escaped messages surviving history reload.
func TestDiagnosticLogWriteBoundsAllFieldsAndWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "native.jsonl")
	logs := newDiagnosticLogBuffer(path)
	_, err := logs.Write(
		[]byte("[INF] " +
			strings.Repeat("A", 7000) + ": message\n"),
	)
	require.NoError(t, err)
	require.LessOrEqual(t, len(logs.entries[0].Source), 64)
	before := len(logs.entries)
	_, err = logs.Write([]byte(strings.Repeat("line\n", 10000)))
	require.NoError(t, err)
	require.LessOrEqual(t, len(logs.entries)-before, 32)
	_, err = logs.Write(
		[]byte("[INF] ROND: " +
			strings.Repeat("\x01", 4096) + "\n"),
	)
	require.NoError(t, err)
	reopened := newDiagnosticLogBuffer(path)
	require.Equal(t, len(logs.entries), len(reopened.entries))
}
