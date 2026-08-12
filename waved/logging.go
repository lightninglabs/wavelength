package waved

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/btcwbackend"
	"github.com/lightninglabs/wavelength/chainbackends"
	"github.com/lightninglabs/wavelength/chainfees"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	lndbuild "github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/sweep"
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
	timeout.Subsystem,
	chainbackends.Subsystem,
	chainbackends.LndClientSubsystem,
	chainfees.Subsystem,
	lndbackend.Subsystem,
	indexer.Subsystem,
	db.Subsystem,
	SwapSubsystem,
	WalletRPCSubsystem,
	PprofSubsystem,
	MetricsSubsystem,
	"TXCF",
	"UNRL",
	VHTLCRecoverySubsystem,
	funding.Subsystem,
	"CHFD",
	"LNWL",
	"HSWC",
	"INVC",
	routing.Subsystem,
	"CNCT",
	"BRAR",
	"UTXN",
	"SWPR",
	"CHDB",
	chainio.Subsystem,
}

const (
	// SwapSubsystem is the subsystem tag used for daemon-owned swap runtime
	// logs. It is exported so optional subservers can reuse the daemon log
	// manager without reaching into Server internals.
	SwapSubsystem = "SWAP"

	// WalletRPCSubsystem is the subsystem tag used for the optional
	// wavewalletrpc subserver (the simplified wallet facade composed over
	// the swap runtime and ark/leave subsystems). It is exported so the
	// swapwallet package can reuse the daemon log manager.
	WalletRPCSubsystem = "WRPC"

	// VHTLCRecoverySubsystem is the subsystem tag used for vHTLC
	// on-chain recovery coordination logs.
	VHTLCRecoverySubsystem = "VREC"

	// PprofSubsystem is the subsystem tag used for the optional pprof
	// debug server so its logs can be level-tuned independently of the
	// main daemon logs.
	PprofSubsystem = "PPRF"

	// MetricsSubsystem is the subsystem tag used for the optional
	// Prometheus metrics HTTP server so its logs can be level-tuned
	// independently of the main daemon logs.
	MetricsSubsystem = "PROM"
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

	funding.UseLogger(loggers[funding.Subsystem])
	chanfunding.UseLogger(loggers["CHFD"])
	lnwallet.UseLogger(loggers["LNWL"])
	htlcswitch.UseLogger(loggers["HSWC"])
	invoices.UseLogger(loggers["INVC"])
	routing.UseLogger(loggers[routing.Subsystem])
	contractcourt.UseLogger(loggers["CNCT"])
	contractcourt.UseBreachLogger(loggers["BRAR"])
	contractcourt.UseNurseryLogger(loggers["UTXN"])
	sweep.UseLogger(loggers["SWPR"])
	channeldb.UseLogger(loggers["CHDB"])
	chainio.UseLogger(loggers[chainio.Subsystem])

	return loggers
}
