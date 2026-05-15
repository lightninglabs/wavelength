package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/lightninglabs/darepo-client/sdk/walletdk"
)

// main runs the walletdk TUI and exits non-zero when startup fails.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run parses flags, starts walletdk, and hands control to Bubble Tea.
func run() error {
	cfg := walletdk.DefaultConfig()

	flag.StringVar(
		&cfg.DataDir, "datadir", cfg.DataDir, "root data directory",
	)
	flag.StringVar(&cfg.Network, "network", cfg.Network,
		"bitcoin network")
	flag.BoolVar(
		&cfg.AllowMainnet, "allow-mainnet", cfg.AllowMainnet,
		"allow mainnet",
	)
	flag.StringVar(
		&cfg.ServerAddress, "server", cfg.ServerAddress,
		"Ark operator mailbox address",
	)
	flag.BoolVar(
		&cfg.ServerInsecure, "server-insecure", cfg.ServerInsecure,
		"disable Ark operator TLS",
	)
	flag.StringVar(
		&cfg.WalletType, "wallet-type", cfg.WalletType,
		"wallet backend type",
	)
	flag.StringVar(
		&cfg.WalletEsploraURL, "esplora", cfg.WalletEsploraURL,
		"lwwallet Esplora URL",
	)
	flag.StringVar(
		&cfg.WalletFeeURL, "fee-url", cfg.WalletFeeURL,
		"btcwallet fee estimator URL",
	)
	flag.StringVar(
		&cfg.SwapServerAddress, "swap-server", cfg.SwapServerAddress,
		"swap server address",
	)
	flag.BoolVar(
		&cfg.SwapServerInsecure, "swap-server-insecure",
		cfg.SwapServerInsecure, "disable swap server TLS",
	)
	flag.Parse()

	logs := newLogSink(2_000)
	cfg.LogWriter = logs

	ctx := context.Background()
	client, err := walletdk.Start(ctx, cfg)
	if err != nil {
		return err
	}

	program := tea.NewProgram(newWalletModel(ctx, client, logs.Lines()))
	_, runErr := program.Run()

	return errors.Join(runErr, client.Stop())
}
