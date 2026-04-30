//go:build itest

package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	clientharness "github.com/lightninglabs/darepo-client/harness"
	darepoharness "github.com/lightninglabs/darepo/harness"
	"github.com/spf13/cobra"
)

const (
	defaultArtifactsDir   = "arktest-artifacts"
	defaultGroupName      = "arktest"
	defaultOperatorFunds  = btcutil.Amount(20 * btcutil.SatoshiPerBitcoin)
	defaultClientLNDFunds = btcutil.Amount(10 * btcutil.SatoshiPerBitcoin)
	defaultClientWallet   = darepoharness.ClientWalletBackendLND
)

type startConfig struct {
	artifactsDir   string
	groupName      string
	clientWallet   string
	lndImage       string
	operatorFunds  int64
	clientLNDFunds int64
	clientNames    []string
	logStdout      bool
}

var startCfg startConfig

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the local Ark test topology",
		Long: "Starts bitcoind, electrs, the operator's lnd, arkd, " +
			"and one darepod per --client. With wallet type " +
			"lnd (the default) each client also gets its own " +
			"LND container, funded with taproot UTXOs so " +
			"unrolls work. Blocks until interrupted with " +
			"Ctrl+C; use a second terminal for `arktest mine`, " +
			"`arktest info`, `eval $(arktest aliases)`.",
		Run: runStart,
	}

	f := cmd.Flags()
	f.StringVar(
		&startCfg.artifactsDir, "artifacts-dir", defaultArtifactsDir,
		"directory for harness artifacts (logs, etc.)",
	)
	f.StringVar(
		&startCfg.groupName, "group-name", defaultGroupName,
		"Docker container/network naming group",
	)
	f.StringVar(
		&startCfg.clientWallet, "client-wallet", defaultClientWallet,
		"client daemon wallet backend: lnd, lwwallet, or btcwallet "+
			"(unroll requires lnd)",
	)
	f.StringVar(
		&startCfg.lndImage, "lnd-image", "",
		"override the default LND docker image (e.g. "+
			"lightninglabs/lnd:daily-testing-only). Empty "+
			"keeps the harness default.",
	)
	f.Int64Var(
		&startCfg.operatorFunds, "operator-funds",
		int64(defaultOperatorFunds),
		"satoshis sent to the operator LND wallet for round txs",
	)
	f.Int64Var(
		&startCfg.clientLNDFunds, "client-lnd-funds",
		int64(defaultClientLNDFunds),
		"satoshis sent to each client's LND wallet for CPFP fee bumps "+
			"(only applied when --client-wallet=lnd)",
	)
	f.StringSliceVar(
		&startCfg.clientNames, "client",
		[]string{"alice", "bob"},
		"logical name for a client daemon; pass --client multiple "+
			"times to start more than one",
	)
	f.BoolVar(
		&startCfg.logStdout, "logstdout", false,
		"also print harness/operator logs to stdout",
	)

	return cmd
}

func runStart(_ *cobra.Command, _ []string) {
	// testing.Main parses the standard library flag set. Cobra has
	// already parsed our flags, so hide them from testing before it
	// boots the harness-backed pseudo-test.
	os.Args = []string{os.Args[0]}

	// testing.Main is used deliberately: arktest is a CLI entry
	// point borrowing the test runtime for lifecycle cleanup, not a
	// package test binary.
	testing.Main(
		regexp.MatchString,
		[]testing.InternalTest{{
			Name: "ArktestStart",
			F:    runHarness,
		}},
		nil, nil,
	)
}

func runHarness(t *testing.T) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir datadir: %v", err)
	}

	artifactsAbs, err := filepath.Abs(startCfg.artifactsDir)
	if err != nil {
		t.Fatalf("resolve artifacts dir: %v", err)
	}
	if err := os.MkdirAll(artifactsAbs, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}

	events, err := newEventLog(os.Stdout, "")
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer func() { _ = events.Close() }()

	events.Printf("start", map[string]any{
		"artifacts":     artifactsAbs,
		"group":         startCfg.groupName,
		"client_wallet": startCfg.clientWallet,
		"clients":       startCfg.clientNames,
	}, "arktest starting clients=%v wallet=%s artifacts=%s",
		startCfg.clientNames, startCfg.clientWallet, artifactsAbs)

	// DefaultOptions seeds image/tag defaults; we override only what the
	// CLI cares about. Skipping this leaves BitcoindImage, LNDImage etc.
	// blank and dockertest fails with "no such image".
	defaults := clientharness.DefaultOptions()
	clientOpts := &defaults
	clientOpts.ArtifactsBaseDir = artifactsAbs
	clientOpts.GroupName = startCfg.groupName
	clientOpts.HarnessLogStdOut = startCfg.logStdout
	clientOpts.ArkdLogStdOut = startCfg.logStdout
	if startCfg.lndImage != "" {
		clientOpts.LNDImage = startCfg.lndImage
	}

	hopts := &darepoharness.ArkHarnessOptions{
		ClientOptions:          clientOpts,
		ClientDaemonWalletType: startCfg.clientWallet,
	}
	applyDaemonLogStdout(hopts, startCfg.logStdout)

	events.Print("start",
		"starting bitcoind, electrs, operator lnd, and arkd", nil)
	h := darepoharness.NewArkHarness(t, hopts)
	h.Start()
	defer h.Stop()
	events.Printf("start", map[string]any{
		"ark_admin": h.ArkAdminAddr,
		"ark_rpc":   h.ArkRPCAddr,
	}, "operator arkd ready admin=%s rpc=%s", h.ArkAdminAddr,
		h.ArkRPCAddr)

	// Send funds to the operator LND wallet so it can pay round-tx
	// fees. FundOperatorLND mines blocks for confirmation.
	if startCfg.operatorFunds > 0 {
		events.Printf("fund", map[string]any{
			"amount_sat": startCfg.operatorFunds,
		}, "funding operator lnd amount=%d", startCfg.operatorFunds)
		h.Harness.FundOperatorLND(
			btcutil.Amount(startCfg.operatorFunds),
		)
		events.Printf("fund", map[string]any{
			"amount_sat": startCfg.operatorFunds,
		}, "operator lnd funded amount=%d", startCfg.operatorFunds)
	}

	state := buildBaseState(h, artifactsAbs)
	state.Clients = make(map[string]*arkClientState)
	state.ClientLNDs = make(map[string]*lndState)

	for _, name := range startCfg.clientNames {
		events.Printf("client_start", map[string]any{
			"client": name,
			"wallet": startCfg.clientWallet,
		}, "starting client %s wallet=%s", name,
			startCfg.clientWallet)
		client := h.StartClientDaemon(name)

		clientState := &arkClientState{
			Name:    name,
			RPCAddr: client.RPCAddr,
			DataDir: client.DataDir,
			Wallet:  startCfg.clientWallet,
		}

		// For LND-backed clients, record the per-client LND that
		// StartClientDaemon spawned so `<name>-lncli` aliases can
		// reach it.
		lndBackend := darepoharness.ClientWalletBackendLND
		if startCfg.clientWallet == lndBackend {
			lnd := h.Harness.GetAdditionalLND(name)
			if lnd != nil {
				state.ClientLNDs[name] = &lndState{
					Name: lnd.Name,
					GRPCAddr: "127.0.0.1:" +
						lnd.GRPCPort,
					TLSCertPath:   lnd.TLSCert,
					MacaroonPath:  lnd.Macaroon,
					DataDir:       lnd.DataDir,
					ContainerName: lnd.ContainerName,
				}
			}
		}
		events.Printf("client_ready", map[string]any{
			"client": name,
			"rpc":    client.RPCAddr,
		}, "client %s ready rpc=%s", name, client.RPCAddr)

		// Fee-input pre-fund: hit the daemon's NewWalletAddress
		// test hook (LND-backed → WalletKit.NextAddr) and faucet.
		// We deliberately avoid FundClientLND, which uses
		// LightningClient.NewAddress and produces UTXOs whose
		// internal metadata LND's WalletKit.FinalizePsbt cannot
		// later sign inside a partially-finalized CPFP-child PSBT
		// (the OOR-receiver unroll path needs this).
		if startCfg.clientLNDFunds > 0 {
			events.Printf("fund", map[string]any{
				"client":     name,
				"amount_sat": startCfg.clientLNDFunds,
			}, "funding client %s lnd wallet amount=%d",
				name, startCfg.clientLNDFunds)
			h.FundClientWallet(
				client,
				btcutil.Amount(startCfg.clientLNDFunds),
			)
			events.Printf("fund", map[string]any{
				"client":     name,
				"amount_sat": startCfg.clientLNDFunds,
			}, "client %s lnd wallet funded amount=%d",
				name, startCfg.clientLNDFunds)
		}

		// Boarding pre-fund happens via `arktest board <name>`,
		// not at start. Boarding outputs are taproot scripts LND
		// owns but cannot single-sign; pre-funding by default
		// would land such a UTXO in every client's wallet, where
		// `selectFeeInput` could later pick it for an unroll CPFP
		// child and fail to finalize.

		state.Clients[name] = clientState
	}

	if err := saveState(state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	defer func() { _ = deleteState() }()

	if err := events.AttachFile(
		filepath.Join(state.RunDir, defaultEventLogName),
	); err != nil {
		t.Fatalf("attach event log: %v", err)
	}

	printReady(events, state)

	waitForSignal()
}

// printReady emits the sparse ready banner once the topology has been
// persisted and can be driven by the other arktest subcommands.
func printReady(events *eventLog, state *harnessState) {
	fields := map[string]any{
		"state":     state.StateFile,
		"run_dir":   state.RunDir,
		"clients":   clientNames(state),
		"ark_admin": state.ArkAdminAddr,
		"ark_rpc":   state.ArkRPCAddr,
	}

	events.Print("ready", "arktest ready", fields)
	events.Printf("ready", nil, "state: %s", state.StateFile)
	events.Printf("ready", nil, "artifacts: %s", state.RunDir)
	events.Printf("ready", nil, "operator admin rpc: %s",
		state.ArkAdminAddr)
	events.Printf("ready", nil, "operator client rpc: %s",
		state.ArkRPCAddr)
	events.Printf("ready", nil, "clients: %v", clientNames(state))
	events.Print("ready", "in another terminal:", nil)
	events.Print("ready", `  eval "$(arktest aliases)"`, nil)
	events.Print("ready", "  arktest info", nil)
	events.Print("ready", "  arktest logs operator", nil)
	events.Print("ready", "  arktest mine 6", nil)
	events.Print("ready", "Ctrl+C to stop.", nil)
}

// buildBaseState extracts the harness-level endpoints into our state
// struct. The per-client fields are populated by the caller as each
// client comes up.
func buildBaseState(h *darepoharness.ArkHarness,
	artifactsAbs string) *harnessState {

	// The arktest binary's own directory is the canonical bin dir:
	// `make arktest` writes ./arktest, ./arkcli, and ./darepocli all
	// side-by-side, so aliases can resolve siblings without any
	// extra configuration.
	binDir, err := os.Executable()
	if err == nil {
		binDir = filepath.Dir(binDir)
	} else {
		binDir = "."
	}

	return &harnessState{
		ArtifactsDir:     artifactsAbs,
		RunDir:           h.Harness.BaseDir(),
		BinDir:           binDir,
		ArkAdminAddr:     h.ArkAdminAddr,
		ArkRPCAddr:       h.ArkRPCAddr,
		EsploraURL:       h.Harness.EsploraURL,
		BitcoindRPC:      h.Harness.BitcoindRPC,
		BitcoindRPCUser:  clientharness.BitcoindRPCUser,
		BitcoindRPCPass:  clientharness.BitcoindRPCPass,
		BitcoindZMQBlock: h.Harness.BitcoindZMQBlock,
		BitcoindZMQTx:    h.Harness.BitcoindZMQTx,
		OperatorLND: lndState{
			Name:     "operator-lnd",
			GRPCAddr: "127.0.0.1:" + h.Harness.LNDGRPCPort,
		},
	}
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	sig := <-ch
	fmt.Fprintf(os.Stderr, "\ngot signal %s, shutting down...\n", sig)
}

// applyDaemonLogStdout configures whether in-process daemon logs stream to the
// terminal in addition to their artifact log files.
func applyDaemonLogStdout(hopts *darepoharness.ArkHarnessOptions,
	logStdout bool) {

	if logStdout {
		return
	}

	hopts.OperatorLogWriter = io.Discard
	hopts.ClientLogWriterFactory = func(string) io.Writer {
		return io.Discard
	}
}
