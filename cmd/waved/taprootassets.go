package main

import (
	"context"
	"path/filepath"

	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/waved"
	"google.golang.org/grpc"
)

// configureTaprootAssets installs a lazy daemon registrar. The authenticated
// tapd connection is opened only after waved has validated and initialized its
// own runtime, and is closed by the normal daemon shutdown path.
func configureTaprootAssets(cfg *waved.Config) {
	if cfg == nil || cfg.TaprootAssets == nil ||
		!cfg.TaprootAssets.Enabled ||
		cfg.TaprootAssetOORPreparer != nil {
		return
	}

	cfg.RPCServiceRegistrars = append(
		cfg.RPCServiceRegistrars,
		func(_ context.Context, _ *grpc.Server,
			rpcServer *waved.RPCServer, daemonCfg *waved.Config) (
			func(), error) {

			assetCfg := daemonCfg.TaprootAssets
			clientCfg := &tapgrpc.Config{
				Host:       assetCfg.Host,
				Network:    tapsdk.Network(daemonCfg.Network),
				RPCTimeout: assetCfg.RPCTimeout,
			}
			if assetCfg.MacaroonPath != "" {
				clientCfg.Macaroon = macaroon.FromPath(
					assetCfg.MacaroonPath,
				)
			}
			switch {
			case assetCfg.Insecure:
				clientCfg.TLS = tapgrpc.TLSInsecure()

			case assetCfg.TLSCertPath != "":
				clientCfg.TLS = tapgrpc.TLSFromPath(
					assetCfg.TLSCertPath,
				)
			}

			client, err := tapgrpc.NewClient(clientCfg)
			if err != nil {
				return nil, err
			}
			wallet := tapsdk.NewWallet(
				client, tapsdk.Network(daemonCfg.Network),
			)
			closeWallet := func() {
				_ = wallet.Close()
			}

			journalDir := assetCfg.PreparationDir
			if journalDir == "" {
				journalDir = filepath.Join(
					daemonCfg.NetworkDir(),
					"taproot-assets-oor",
				)
			}
			store, err := tapassets.NewFileStore(journalDir)
			if err != nil {
				closeWallet()

				return nil, err
			}
			preparer, err := tapassets.NewPreparer(
				tapassets.PreparerConfig{
					Wallet: wallet,
					Store:  store,
				},
			)
			if err != nil {
				closeWallet()

				return nil, err
			}
			daemonCfg.TaprootAssetOORPreparer = preparer
			if err := rpcServer.ConfigureTaprootAssetOnboarding(
				wallet, store,
			); err != nil {

				closeWallet()

				return nil, err
			}

			return closeWallet, nil
		},
	)
}
