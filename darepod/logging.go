package darepod

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
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
	"github.com/lightningnetwork/lnd/signal"
)

// genSubLogger creates a subsystem logger factory from the root manager. The
// shutdown callback is invoked on critical log events, allowing the daemon to
// initiate a graceful shutdown when an unrecoverable error is logged.
func genSubLogger(root *lndbuild.SubLoggerManager,
	interceptor signal.Interceptor) func(string) btclog.Logger {

	shutdown := func() {
		if !interceptor.Listening() {
			return
		}

		interceptor.RequestShutdown()
	}

	return func(tag string) btclog.Logger {
		return root.GenSubLogger(tag, shutdown)
	}
}

// SetupLoggers initializes all package-level subsystem loggers and registers
// them with the root SubLoggerManager. This must be called early in daemon
// startup before any subsystem initialization to ensure log output is routed
// to the configured backend.
//
// Packages that use fn.Option[btclog.Logger] in their config (e.g.,
// chainsource) receive their logger at actor construction time via
// the genSubLogger factory, not through a global UseLogger call.
func SetupLoggers(root *lndbuild.SubLoggerManager,
	interceptor signal.Interceptor) {

	// Register all subsystem loggers. Each AddSubLogger call creates a
	// child logger from the root backend, registers it with the manager
	// for level control, and calls the package's UseLogger to activate
	// the package-level logger.
	subsystems := []struct {
		name      string
		useLogger func(btclog.Logger)
	}{
		{Subsystem, UseLogger},
		{actor.Subsystem, actor.UseLogger},
		{round.Subsystem, round.UseLogger},
		{oor.Subsystem, oor.UseLogger},
		{vtxo.Subsystem, vtxo.UseLogger},
		{wallet.Subsystem, wallet.UseLogger},
		{lwwallet.Subsystem, lwwallet.UseLogger},
		{serverconn.Subsystem, serverconn.UseLogger},
		{chainbackends.Subsystem, chainbackends.UseLogger},
		{
			chainbackends.LndClientSubsystem,
			chainbackends.UseLndClientLogger,
		},
		{lndbackend.Subsystem, lndbackend.UseLogger},
		{indexer.Subsystem, indexer.UseLogger},
		{db.Subsystem, db.UseLogger},
	}
	for _, sub := range subsystems {
		AddSubLogger(root, sub.name, interceptor, sub.useLogger)
	}
}

// AddSubLogger creates a new subsystem logger from the root manager,
// registers it for centralized level control, and calls each provided
// UseLogger function to wire the package-level logger variable. This
// mirrors lnd's AddSubLogger pattern.
func AddSubLogger(root *lndbuild.SubLoggerManager, subsystem string,
	interceptor signal.Interceptor,
	useLoggers ...func(btclog.Logger)) {

	genLogger := genSubLogger(root, interceptor)

	logger := lndbuild.NewSubLogger(subsystem, genLogger)
	root.RegisterSubLogger(subsystem, logger)

	for _, useLogger := range useLoggers {
		useLogger(logger)
	}
}
