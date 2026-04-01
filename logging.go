package darepo

import (
	"fmt"
	"strings"

	"github.com/btcsuite/btclog"
	btclogv2 "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/batchsweeper"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/indexer"
	"github.com/lightninglabs/darepo/metrics"
	"github.com/lightninglabs/darepo/oor"
	"github.com/lightninglabs/darepo/rounds"
)

const (
	// adminRPCSubsystem is the subsystem tag for the admin RPC server.
	adminRPCSubsystem = "ARPC"

	// clientRPCSubsystem is the subsystem tag for the client RPC server.
	clientRPCSubsystem = "ORPC"

	// dbSubsystem is the subsystem tag for the database layer.
	dbSubsystem = "DABS"
)

// SubLoggers is a map of subsystem names to their loggers.
type SubLoggers map[string]btclogv2.Logger

// SetupLoggers creates a tagged logger for every server subsystem from
// the given handler. The returned map is keyed by four-character subsystem
// tag and can be passed to ApplyDebugLevel for per-subsystem level
// control.
func SetupLoggers(handler btclogv2.Handler) SubLoggers {
	newSubLogger := func(tag string) btclogv2.Logger {
		return btclogv2.NewSLogger(handler.SubSystem(tag))
	}

	return SubLoggers{
		Subsystem:              newSubLogger(Subsystem),
		adminRPCSubsystem:      newSubLogger(adminRPCSubsystem),
		clientRPCSubsystem:     newSubLogger(clientRPCSubsystem),
		rounds.Subsystem:       newSubLogger(rounds.Subsystem),
		batchsweeper.Subsystem: newSubLogger(batchsweeper.Subsystem),
		batchwatcher.Subsystem: newSubLogger(batchwatcher.Subsystem),
		oor.Subsystem:          newSubLogger(oor.Subsystem),
		clientconn.Subsystem:   newSubLogger(clientconn.Subsystem),
		indexer.Subsystem:      newSubLogger(indexer.Subsystem),
		metrics.Subsystem:      newSubLogger(metrics.Subsystem),
		dbSubsystem:            newSubLogger(dbSubsystem),
	}
}

// ApplyDebugLevel parses a debug level specification string and applies
// the requested levels to the given subsystem loggers.
//
// The spec may be a single global level that applies to every subsystem
// (e.g. "info"), or a comma-separated list of per-subsystem overrides
// with an optional trailing global default (e.g.
// "RNDS=debug,DABS=trace,info"). Per-subsystem entries take precedence
// over the global default.
func ApplyDebugLevel(loggers SubLoggers, spec string) error {
	// Separate the spec into per-subsystem entries and a potential
	// global default.
	var globalLevel btclog.Level

	globalSet := false

	parts := strings.Split(spec, ",")
	overrides := make(map[string]btclog.Level)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Check if this is a per-subsystem override (TAG=level).
		if idx := strings.IndexByte(part, '='); idx != -1 {
			tag := strings.ToUpper(part[:idx])
			levelStr := strings.ToLower(part[idx+1:])

			level, ok := btclogv2.LevelFromString(levelStr)
			if !ok {
				return fmt.Errorf(
					"unknown log level %q for "+
						"subsystem %s", levelStr, tag,
				)
			}

			overrides[tag] = level

			continue
		}

		// Otherwise treat it as the global default.
		level, ok := btclogv2.LevelFromString(
			strings.ToLower(part),
		)
		if !ok {
			return fmt.Errorf(
				"unknown log level %q", part,
			)
		}

		globalLevel = level
		globalSet = true
	}

	// First pass: apply the global default to every subsystem.
	if globalSet {
		for _, l := range loggers {
			l.SetLevel(globalLevel)
		}
	}

	// Second pass: apply per-subsystem overrides on top.
	for tag, level := range overrides {
		l, ok := loggers[tag]
		if !ok {
			return fmt.Errorf(
				"unknown subsystem %q", tag,
			)
		}

		l.SetLevel(level)
	}

	return nil
}
