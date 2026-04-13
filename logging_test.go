package darepo

import (
	"os"
	"testing"

	"github.com/btcsuite/btclog"
	btclogv2 "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/metrics"
	"github.com/stretchr/testify/require"
)

// TestSetupLoggers verifies that SetupLoggers populates a logger for
// every expected subsystem.
func TestSetupLoggers(t *testing.T) {
	t.Parallel()

	handler := btclogv2.NewDefaultHandler(os.Stdout)
	loggers := SetupLoggers(handler)

	expectedSubsystems := []string{
		Subsystem, adminRPCSubsystem, clientRPCSubsystem,
		"RNDS", "BSWP", "BWCH", "OORS", "CLNC", "INDX",
		metrics.Subsystem, dbSubsystem, mailboxSubsystem,
	}
	for _, tag := range expectedSubsystems {
		_, ok := loggers[tag]
		require.True(t, ok, "missing logger for subsystem %s", tag)
	}
}

// TestApplyDebugLevelGlobal verifies that a bare level string sets every
// subsystem to that level.
func TestApplyDebugLevelGlobal(t *testing.T) {
	t.Parallel()

	handler := btclogv2.NewDefaultHandler(os.Stdout)
	loggers := SetupLoggers(handler)

	err := ApplyDebugLevel(loggers, "debug")
	require.NoError(t, err)

	for tag, l := range loggers {
		require.Equal(
			t, btclog.LevelDebug, l.Level(),
			"subsystem %s should be debug", tag,
		)
	}
}

// TestApplyDebugLevelPerSubsystem verifies that TAG=level overrides
// take precedence over the global default.
func TestApplyDebugLevelPerSubsystem(t *testing.T) {
	t.Parallel()

	handler := btclogv2.NewDefaultHandler(os.Stdout)
	loggers := SetupLoggers(handler)

	err := ApplyDebugLevel(loggers, "RNDS=trace,DABS=warn,info")
	require.NoError(t, err)

	require.Equal(
		t, btclog.LevelTrace, loggers["RNDS"].Level(),
	)
	require.Equal(
		t, btclog.LevelWarn, loggers[dbSubsystem].Level(),
	)

	// All other subsystems should be at info (the global default).
	require.Equal(
		t, btclog.LevelInfo, loggers[Subsystem].Level(),
	)
}

// TestApplyDebugLevelUnknownSubsystem returns an error for unrecognized
// subsystem tags.
func TestApplyDebugLevelUnknownSubsystem(t *testing.T) {
	t.Parallel()

	handler := btclogv2.NewDefaultHandler(os.Stdout)
	loggers := SetupLoggers(handler)

	err := ApplyDebugLevel(loggers, "NOPE=debug")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown subsystem")
}

// TestApplyDebugLevelUnknownLevel returns an error for unrecognized
// level strings.
func TestApplyDebugLevelUnknownLevel(t *testing.T) {
	t.Parallel()

	handler := btclogv2.NewDefaultHandler(os.Stdout)
	loggers := SetupLoggers(handler)

	err := ApplyDebugLevel(loggers, "banana")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown log level")
}
