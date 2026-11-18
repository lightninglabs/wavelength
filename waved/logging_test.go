package waved

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btclog/v2"
	lndbuild "github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/stretchr/testify/require"
)

// TestLNDLoggerReconfiguration checks output, level and shutdown ownership
// after another daemon replaces the delegate, without redirecting per-channel
// copies.
func TestLNDLoggerReconfiguration(t *testing.T) {
	var logger redirectLogger
	require.Same(t, btclog.Disabled, logger.logger())
	logger.Info("not configured")

	var firstOutput, secondOutput bytes.Buffer
	firstRoot := lndbuild.NewSubLoggerManager(
		btclog.NewDefaultHandler(&firstOutput),
	)
	secondRoot := lndbuild.NewSubLoggerManager(
		btclog.NewDefaultHandler(&secondOutput),
	)
	var firstShutdown, secondShutdown atomic.Int32
	first := firstRoot.GenSubLogger("TEST", func() {
		firstShutdown.Add(1)
	})
	second := secondRoot.GenSubLogger("TEST", func() {
		secondShutdown.Add(1)
	})
	firstRoot.SetLogLevels("info")
	secondRoot.SetLogLevels("info")
	logger.setLogger(first)
	prefixed := logger.WithPrefix("channel")
	child := logger.SubSystem("CHILD")

	firstRoot.SetLogLevel("TEST", "debug")
	require.Equal(t, btclog.LevelDebug, logger.Level())
	prefixed.Debug("inherited debug")
	child.Debug("filtered child")
	logger.Debugf("first %d", 1)

	logger.setLogger(second)
	logger.Debug("filtered")
	logger.InfoS(context.Background(), "second", "key", "value")
	logger.Critical("current shutdown")
	logger.Criticalf("current shutdown %d", 2)
	logger.CriticalS(context.Background(), "current shutdown", nil)
	require.Zero(t, firstShutdown.Load())
	require.EqualValues(t, 3, secondShutdown.Load())
	require.Equal(t, btclog.LevelInfo, logger.Level())

	// Existing channel prefixes retain the old output and share its level;
	// they must not silently become loggers for a different daemon.
	firstRoot.SetLogLevel("TEST", "trace")
	require.Equal(t, btclog.LevelInfo, logger.Level())
	prefixed.Trace("channel snapshot")
	child.Info("subsystem snapshot")
	logger.SetLevel(btclog.LevelWarn)
	require.Equal(t, btclog.LevelWarn, second.Level())
	secondRoot.SetLogLevel("TEST", "debug")
	require.Equal(t, btclog.LevelDebug, logger.Level())
	logger.Debug("latest level")

	require.Contains(t, firstOutput.String(), "first 1")
	require.Contains(t, firstOutput.String(), "inherited debug")
	require.Contains(t, firstOutput.String(), "channel snapshot")
	require.Contains(t, firstOutput.String(), "CHILD")
	require.NotContains(t, firstOutput.String(), "second")
	require.NotContains(t, firstOutput.String(), "filtered child")
	require.Contains(t, secondOutput.String(), "second key=value")
	require.Contains(t, secondOutput.String(), "current shutdown 2")
	require.Contains(t, secondOutput.String(), "latest level")
	require.NotContains(t, secondOutput.String(), "filtered")
	require.NotContains(t, secondOutput.String(), "snapshot")
}

// TestSetupLoggersConcurrentReconfiguration exercises LND package-global reads
// while multiple daemon instances register their outputs and change log levels.
func TestSetupLoggersConcurrentReconfiguration(t *testing.T) {
	const workers = 4
	const iterations = 100
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		wg.Go(func() {
			root := lndbuild.NewSubLoggerManager(
				btclog.NewDefaultHandler(io.Discard),
			)
			<-start
			for range iterations {
				SetupLoggersWithShutdownFn(root, func() {})
				root.SetLogLevels("trace")
				root.SetLogLevel("CHFD", "debug")
			}
		})
		wg.Go(func() {
			<-start
			for range iterations {
				// Both operations read real LND logger globals.
				// The intent logs directly; a beat derives its
				// own prefix.
				intent := &chanfunding.PsbtIntent{
					PsbtReady: make(chan error, 1),
				}
				intent.Cancel()
				chainio.NewBeat(chainntnfs.BlockEpoch{})
				for _, logger := range lndLoggers {
					logger.Infof("reconfiguration %d", 1)
					logger.DebugS(
						context.Background(),
						"concurrent",
					)
					logger.WithPrefix(
						"channel").Info("snapshot")
				}
			}
		})
	}
	close(start)
	wg.Wait()
}

// TestSetupLoggersUsesLatestDelegate checks that setup changes every installed
// LND delegate while instance-local loggers remain attached to their own root.
func TestSetupLoggersUsesLatestDelegate(t *testing.T) {
	firstOutput, err := os.CreateTemp(t.TempDir(), "first.log")
	require.NoError(t, err)
	var secondOutput bytes.Buffer
	firstRoot := lndbuild.NewSubLoggerManager(
		btclog.NewDefaultHandler(firstOutput),
	)
	secondRoot := lndbuild.NewSubLoggerManager(
		btclog.NewDefaultHandler(&secondOutput),
	)
	first := SetupLoggersWithShutdownFn(firstRoot, func() {})
	second := SetupLoggersWithShutdownFn(secondRoot, func() {})
	for tag, logger := range lndLoggers {
		require.Same(t, second[tag], logger.logger(), tag)
	}
	require.NoError(t, firstOutput.Close())

	// Development nolog/stdlog builds deliberately bypass daemon outputs.
	// The forwarding logger must preserve that choice as well.
	if lndbuild.Deployment == lndbuild.Development &&
		lndbuild.LoggingType != lndbuild.LogTypeDefault {
		return
	}
	for tag, logger := range lndLoggers {
		require.NotSame(t, first[tag], logger.logger(), tag)
	}
	firstRoot.SetLogLevels("error")
	first["CHFD"].Info("filtered first instance")
	secondRoot.SetLogLevel("CHFD", "debug")
	intent := &chanfunding.PsbtIntent{
		PsbtReady: make(chan error, 1),
	}
	intent.Cancel()
	firstLogs, err := os.ReadFile(firstOutput.Name())
	require.NoError(t, err)
	require.Empty(t, firstLogs)
	require.Contains(
		t, secondOutput.String(),
		"PSBT funding intent canceled",
	)
}
