package waved

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/lightninglabs/lndclient"
	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/tapassets"
)

const (
	// AssetBackendNone disables Taproot Assets. Rounds that would create
	// an asset VTXO for this daemon fail before signing.
	AssetBackendNone = "none"

	// AssetBackendTapd verifies Taproot Assets proofs through a tapd node.
	AssetBackendTapd = "tapd"

	// DefaultAssetBackend is the asset backend used when none is set.
	DefaultAssetBackend = AssetBackendNone

	// assetBackendStartupTimeout bounds the startup check of the asset
	// backend connection.
	assetBackendStartupTimeout = 30 * time.Second
)

// AssetsConfig selects the backend that verifies Taproot Assets.
type AssetsConfig struct {
	// Backend selects the asset backend: "none" or "tapd".
	Backend string `mapstructure:"backend"`

	// Tapd configures the tapd node used by the "tapd" backend.
	Tapd *TapdConfig `mapstructure:"tapd"`

	// Lnd configures the lnd node behind tapd. It signs the asset deposits
	// tapd funds, so DepositAsset requires it.
	Lnd *AssetLndConfig `mapstructure:"lnd"`
}

// AssetLndConfig configures the connection to the lnd node behind tapd.
type AssetLndConfig struct {
	// Host is the network address of the lnd gRPC interface.
	Host string `mapstructure:"host"`

	// TLSPath is the path to lnd's TLS certificate file. If empty, the
	// default lnd TLS cert location is used.
	TLSPath string `mapstructure:"tlspath"`

	// MacaroonPath is the full path to the lnd admin macaroon. If empty,
	// the default lnd macaroon location for the active network is used.
	MacaroonPath string `mapstructure:"macaroonpath"`

	// RPCTimeout is the maximum duration for individual RPC calls to
	// lnd. If zero, DefaultRPCTimeout is used.
	RPCTimeout time.Duration `mapstructure:"rpctimeout"`
}

// TapdConfig configures the connection to a tapd node.
type TapdConfig struct {
	// Host is the network address of the tapd gRPC interface.
	Host string `mapstructure:"host"`

	// TLSPath is the path to tapd's TLS certificate. If empty, tapd's
	// default certificate location is used.
	TLSPath string `mapstructure:"tlspath"`

	// MacaroonPath is the path to the tapd admin macaroon. If empty, the
	// default macaroon directory for the active network is used.
	MacaroonPath string `mapstructure:"macaroonpath"`

	// RPCTimeout bounds individual tapd calls. If zero, DefaultRPCTimeout
	// is used.
	RPCTimeout time.Duration `mapstructure:"rpctimeout"`
}

// defaultAssetsConfig returns the asset configuration with no backend.
func defaultAssetsConfig() *AssetsConfig {
	return &AssetsConfig{
		Backend: DefaultAssetBackend,
		Tapd: &TapdConfig{
			RPCTimeout: DefaultRPCTimeout,
		},
		Lnd: &AssetLndConfig{
			RPCTimeout: DefaultRPCTimeout,
		},
	}
}

// validateAssetsConfig checks the asset backend selection.
func (c *Config) validateAssetsConfig() error {
	if c.Assets == nil {
		c.Assets = defaultAssetsConfig()
	}
	if c.Assets.Backend == "" {
		c.Assets.Backend = DefaultAssetBackend
	}

	depositLnd := c.Assets.Lnd != nil && c.Assets.Lnd.Host != ""
	if depositLnd && c.Assets.Backend != AssetBackendTapd {
		return fmt.Errorf("assets.lnd.host requires assets.backend " +
			"tapd")
	}
	if depositLnd && c.Assets.Lnd.RPCTimeout < 0 {
		return fmt.Errorf("assets.lnd.rpctimeout must not be "+
			"negative: got %s", c.Assets.Lnd.RPCTimeout)
	}

	switch c.Assets.Backend {
	case AssetBackendNone:
		return nil

	case AssetBackendTapd:
		if c.Assets.Tapd == nil || c.Assets.Tapd.Host == "" {
			return fmt.Errorf("assets.tapd.host is required with " +
				"assets.backend tapd")
		}
		if c.Assets.Tapd.RPCTimeout < 0 {
			return fmt.Errorf("assets.tapd.rpctimeout must not be "+
				"negative: got %s", c.Assets.Tapd.RPCTimeout)
		}

		return nil

	default:
		return fmt.Errorf("unknown assets.backend %q (want %s or %s)",
			c.Assets.Backend, AssetBackendNone, AssetBackendTapd)
	}
}

// connectAssetBackend connects the configured asset backend and returns its
// asset VTXO verifier, or nil when assets are disabled. The returned cleanup
// closes the backend connection.
func (s *Server) connectAssetBackend(ctx context.Context) (
	round.AssetVTXOVerifier, func(), error) {

	if s.cfg.Assets == nil || s.cfg.Assets.Backend != AssetBackendTapd {
		return nil, func() {}, nil
	}

	client, err := s.connectTapd(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("connect tapd asset backend: %w",
			err)
	}
	s.tapdClient = client
	cleanup := func() {
		if err := client.Close(); err != nil {
			s.log.DebugS(ctx, "Unable to close tapd connection",
				slog.String("error", err.Error()),
			)
		}
	}

	chain, err := tapassets.NewTapdProofChainVerifier(client)
	if err != nil {
		cleanup()

		return nil, nil, err
	}
	verifier, err := tapassets.NewVTXOVerifier(chain)
	if err != nil {
		cleanup()

		return nil, nil, err
	}

	return verifier, cleanup, nil
}

// connectTapd dials tapd and checks that it runs on the daemon's network.
func (s *Server) connectTapd(ctx context.Context) (*tapgrpc.Client, error) {
	tapdCfg := s.cfg.Assets.Tapd
	network := tapsdk.Network(s.cfg.Network)

	rpcTimeout := tapdCfg.RPCTimeout
	if rpcTimeout == 0 {
		rpcTimeout = DefaultRPCTimeout
	}
	clientCfg := &tapgrpc.Config{
		Host:       tapdCfg.Host,
		Network:    network,
		RPCTimeout: rpcTimeout,
	}
	if tapdCfg.TLSPath != "" {
		clientCfg.TLS = tapgrpc.TLSFromPath(tapdCfg.TLSPath)
	}
	if tapdCfg.MacaroonPath != "" {
		clientCfg.Macaroon = macaroon.FromPath(tapdCfg.MacaroonPath)
	}

	client, err := tapgrpc.NewClient(clientCfg)
	if err != nil {
		return nil, err
	}

	infoCtx, cancel := context.WithTimeout(ctx, assetBackendStartupTimeout)
	defer cancel()
	info, err := client.GetInfo(infoCtx)
	if err != nil {
		_ = client.Close()

		return nil, fmt.Errorf("query tapd info: %w", err)
	}
	if info.Network != string(network) {
		_ = client.Close()

		return nil, fmt.Errorf("tapd runs on %s, daemon on %s",
			info.Network, network)
	}

	return client, nil
}

// startAssetDeposits starts the asset deposit service when the tapd backend
// and the lnd node behind tapd are configured. The returned cleanup stops the
// deposit watcher and closes the lnd connection.
func (s *Server) startAssetDeposits(ctx context.Context, store *db.Store) (
	func(), error) {

	lndCfg := s.cfg.Assets.Lnd
	if s.tapdClient == nil || lndCfg == nil || lndCfg.Host == "" {
		return func() {}, nil
	}

	network, err := networkToLndclient(s.cfg.Network)
	if err != nil {
		return nil, err
	}
	rpcTimeout := lndCfg.RPCTimeout
	if rpcTimeout == 0 {
		rpcTimeout = DefaultRPCTimeout
	}
	lnd, err := lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:            lndCfg.Host,
		Network:               network,
		CustomMacaroonPath:    lndCfg.MacaroonPath,
		TLSPath:               lndCfg.TLSPath,
		BlockUntilChainSynced: true,
		BlockUntilUnlocked:    true,
		CallerCtx:             ctx,
		RPCTimeout:            rpcTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("connect asset deposit lnd: %w", err)
	}

	journal, err := tapassets.NewFileStore(
		filepath.Join(
			s.cfg.NetworkDir(),
			"assets", "onboarding",
		),
	)
	if err != nil {
		lnd.Close()

		return nil, err
	}
	onboarder, err := tapassets.NewOnboarder(tapassets.OnboarderConfig{
		Wallet: tapsdk.NewWallet(
			s.tapdClient, tapsdk.Network(s.cfg.Network),
		),
		Store:          journal,
		Signer:         lndAnchorSigner(lnd.WalletKit),
		DeriveOwnerKey: s.depositOwnerKeyDeriver(),
	})
	if err != nil {
		lnd.Close()

		return nil, err
	}

	claimJournal, err := tapassets.NewFileStore(
		filepath.Join(
			s.cfg.NetworkDir(),
			"assets", "claims",
		),
	)
	if err != nil {
		lnd.Close()

		return nil, err
	}
	claimer, err := tapassets.NewClaimer(tapassets.ClaimerConfig{
		Wallet: tapsdk.NewWallet(
			s.tapdClient, tapsdk.Network(s.cfg.Network),
		),
		Store:  claimJournal,
		Signer: claimSigner{server: s},
	})
	if err != nil {
		lnd.Close()

		return nil, err
	}

	depositStore := db.NewAssetDepositStore(store)
	s.assetDeposits = newAssetDepositService(&assetDepositConfig{
		log:              s.log,
		store:            depositStore,
		tapd:             s.tapdClient,
		onboarder:        onboarder,
		terms:            s.loadOperatorTerms,
		quoteBoardingFee: s.quoteBoardingFee,
		chainParams:      s.chainParams,
		registerBoarding: s.registerAssetBoarding,
		claims:           depositStore,
		claimer:          claimer,
		broadcastClaim:   s.broadcastAssetClaim,
		reserveClaim:     s.reserveAssetDepositClaim,
	})

	// The subscriber lives as long as the actor system, not this call.
	//nolint:contextcheck
	s.assetClaimSubscriber = s.registerAssetClaimSubscriber(
		s.assetDeposits,
	)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.assetDeposits.run(runCtx)
	}()

	return func() {
		cancel()
		<-done
		lnd.Close()
	}, nil
}
