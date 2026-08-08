package waved

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"

	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
	"github.com/lightninglabs/wavelength/tapassets"
	"google.golang.org/grpc"
)

// ConfigureTaprootAssets registers the production tap-sdk and tapd runtime on
// cfg. The runtime remains opt-in through TaprootAssets.Enabled and is
// installed at most once per Config. A caller that injects its own Taproot
// Asset OOR preparer retains ownership of the integration, so this helper is a
// no-op in that case.
//
// Registration is lazy: no authenticated tapd connection or preparation store
// is opened until waved starts its gRPC services. The daemon shutdown path
// closes the connection returned by the registrar. Embedded consumers should
// call this helper before Main instead of recreating the tapd credential,
// journal, reservation, OOR preparation, and onboarding wiring.
func ConfigureTaprootAssets(cfg *Config) {
	if cfg == nil || cfg.TaprootAssets == nil ||
		!cfg.TaprootAssets.Enabled ||
		cfg.TaprootAssetOORPreparer != nil ||
		cfg.taprootAssetsRuntimeConfigured {
		return
	}

	cfg.taprootAssetsRuntimeConfigured = true
	cfg.RPCServiceRegistrars = append(
		cfg.RPCServiceRegistrars, registerTaprootAssets,
	)
}

// registerTaprootAssets constructs the production Taproot Assets services
// after waved has initialized the shared runtime dependencies they require.
func registerTaprootAssets(_ context.Context, _ *grpc.Server,
	rpcServer *RPCServer, daemonCfg *Config) (func(), error) {

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
	reservationStore, err := rpcServer.OORReservationStore()
	if err != nil {
		closeWallet()

		return nil, err
	}
	artifactStore := rpcServer.newLocalOORArtifactStore()
	loadCreatedPackage := tapassets.CreatedPackageLoader(
		func(ctx context.Context, outpoint wire.OutPoint) ([]byte,
			error) {

			// A VTXO created inside a round carries its own
			// sealed package on its row; only an OOR-created
			// output needs the artifact store.
			sealed, err := rpcServer.roundAssetLeafPackage(
				ctx, outpoint,
			)
			if err != nil {
				return nil, err
			}
			if len(sealed) > 0 {
				return sealed, nil
			}

			if artifactStore == nil {
				return nil, fmt.Errorf("OOR artifact store " +
					"is unavailable")
			}
			store := artifactStore
			bundle, err := store.GetCreatedPackageForOutpoint(
				ctx, outpoint,
			)
			if err != nil {
				return nil, err
			}
			if bundle == nil {
				return nil, fmt.Errorf("created OOR package " +
					"has no sealed Taproot Asset " +
					"transition")
			}
			transfer := bundle.TaprootAssetTransfer
			if transfer == nil || len(transfer.ArkPackage) == 0 {
				return nil, fmt.Errorf("created OOR package " +
					"has no sealed Taproot Asset " +
					"transition")
			}

			return bytes.Clone(transfer.ArkPackage), nil
		},
	)
	preparer, err := tapassets.NewPreparer(tapassets.PreparerConfig{
		Wallet:             wallet,
		Store:              store,
		ReservationStore:   reservationStore,
		LoadCreatedPackage: loadCreatedPackage,
	})
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
}

// roundAssetLeafPackage returns the sealed tap-sdk package behind an
// asset VTXO the daemon received as a round tree leaf, or nil when the
// outpoint is not one. A leaf carries its package on its own row: the
// operator hands it over with the round's batch info, and it is the only
// source for the leaf's proof path and OP_TRUE witness.
func (r *RPCServer) roundAssetLeafPackage(ctx context.Context,
	outpoint wire.OutPoint) ([]byte, error) {

	if r == nil || r.server == nil || r.server.vtxoStore == nil {
		return nil, nil
	}

	// A miss is expected: an OOR-created output has no VTXO row here and
	// the artifact store answers for it. Treat any lookup failure as
	// "not a round leaf" rather than failing the spend outright.
	desc, err := r.server.vtxoStore.GetVTXO(ctx, outpoint)
	if err != nil {
		return nil, nil //nolint:nilerr
	}
	if desc == nil {
		return nil, nil
	}

	return bytes.Clone(desc.TaprootAssetSealedPackage), nil
}
