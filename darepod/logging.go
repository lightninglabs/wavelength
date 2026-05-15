package darepod

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/btcwbackend"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/lwwallet"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	lndbuild "github.com/lightningnetwork/lnd/build"
)

// SubLoggers maps subsystem tags to their registered btclog instances.
type SubLoggers map[string]btclog.Logger

// allSubsystems is the authoritative list of subsystem tags registered with
// the daemon's root SubLoggerManager.
var allSubsystems = []string{
	Subsystem,
	actor.Subsystem,
	round.Subsystem,
	oor.Subsystem,
	vtxo.Subsystem,
	wallet.Subsystem,
	lwwallet.Subsystem,
	btcwbackend.Subsystem,
	serverconn.Subsystem,
	chainbackends.Subsystem,
	chainbackends.LndClientSubsystem,
	lndbackend.Subsystem,
	indexer.Subsystem,
	db.Subsystem,
	SwapSubsystem,
	WalletRPCSubsystem,
	"TXCF",
	"UNRL",
}

const (
	// SwapSubsystem is the subsystem tag used for daemon-owned swap runtime
	// logs. It is exported so optional subservers can reuse the daemon log
	// manager without reaching into Server internals.
	SwapSubsystem = "SWAP"

	// WalletRPCSubsystem is the subsystem tag used for the optional
	// walletrpc subserver (the simplified wallet facade composed over the
	// swap runtime and ark/leave subsystems). It is exported so the
	// swapwallet package can reuse the daemon log manager.
	WalletRPCSubsystem = "WRPC"
)

// SetupLoggersWithShutdownFn registers all subsystem loggers using a plain
// shutdown callback instead of a signal.Interceptor. This is the
// context-friendly variant used by RunWithContext where the daemon lifecycle
// is managed via context cancellation rather than OS signals.
func SetupLoggersWithShutdownFn(root *lndbuild.SubLoggerManager,
	shutdownFn func()) SubLoggers {

	return setupLoggers(root, func(tag string) btclog.Logger {
		return root.GenSubLogger(tag, shutdownFn)
	})
}

func setupLoggers(root *lndbuild.SubLoggerManager,
	genLogger func(string) btclog.Logger) SubLoggers {

	loggers := make(SubLoggers, len(allSubsystems))

	for _, sub := range allSubsystems {
		logger := lndbuild.NewSubLogger(sub, genLogger)
		root.RegisterSubLogger(sub, logger)
		loggers[sub] = logger
	}

	return loggers
}
