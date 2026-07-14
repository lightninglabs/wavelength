package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
)

var errStdoutWrite = errors.New("stdout write failed")

type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) {
	return 0, errStdoutWrite
}

// TestConfigureDaemonLogWriterWritesStdoutAndDefaultFile verifies standalone
// daemon logs are teed to stdout and the default network-scoped log file.
func TestConfigureDaemonLogWriterWritesStdoutAndDefaultFile(t *testing.T) {
	t.Parallel()

	cfg := waved.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Network = "regtest"
	stdout := &bytes.Buffer{}

	logFile, err := configureDaemonLogWriter(cfg, stdout)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, logFile.Close())
	})

	_, err = cfg.LogWriter.Write([]byte("hello logs\n"))
	require.NoError(t, err)

	require.Equal(t, "hello logs\n", stdout.String())

	logPath := filepath.Join(
		cfg.DataDir, "logs", "regtest", daemonLogFileName,
	)
	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Equal(t, "hello logs\n", string(logBytes))
}

// TestConfigureDaemonLogWriterUsesExplicitLogDir verifies --logdir controls
// where the persistent daemon log file is created.
func TestConfigureDaemonLogWriterUsesExplicitLogDir(t *testing.T) {
	t.Parallel()

	logDir := filepath.Join(t.TempDir(), "explicit")
	cfg := waved.DefaultConfig()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Network = "regtest"
	cfg.LogDirPath = logDir
	stdout := &bytes.Buffer{}

	logFile, err := configureDaemonLogWriter(cfg, stdout)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, logFile.Close())
	})

	_, err = cfg.LogWriter.Write([]byte("custom logs\n"))
	require.NoError(t, err)

	logBytes, err := os.ReadFile(filepath.Join(logDir, daemonLogFileName))
	require.NoError(t, err)
	require.Equal(t, "custom logs\n", string(logBytes))
	require.Equal(t, "custom logs\n", stdout.String())
}

// TestConfigureDaemonLogWriterKeepsFileLoggingWhenStdoutFails verifies stdout
// write errors do not prevent persistent log writes.
func TestConfigureDaemonLogWriterKeepsFileLoggingWhenStdoutFails(t *testing.T) {
	t.Parallel()

	cfg := waved.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Network = "regtest"

	logFile, err := configureDaemonLogWriter(cfg, failingWriter{})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, logFile.Close())
	})

	_, err = cfg.LogWriter.Write([]byte("still persisted\n"))
	require.NoError(t, err)

	logPath := filepath.Join(
		cfg.DataDir, "logs", "regtest", daemonLogFileName,
	)
	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Equal(t, "still persisted\n", string(logBytes))
}

// TestConfigureDaemonLogWriterKeepsInjectedWriter verifies callers that set a
// custom LogWriter retain ownership of daemon log output.
func TestConfigureDaemonLogWriterKeepsInjectedWriter(t *testing.T) {
	t.Parallel()

	cfg := waved.DefaultConfig()
	var injected bytes.Buffer
	cfg.LogWriter = &injected

	logFile, err := configureDaemonLogWriter(cfg, &bytes.Buffer{})
	require.NoError(t, err)
	require.Nil(t, logFile)
	require.Same(t, &injected, cfg.LogWriter)
}
