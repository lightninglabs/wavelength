package waved

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

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

	// AssetBackendEmbedded runs the asset engine inside the daemon, on
	// the daemon wallet's seed, chain backend and database.
	AssetBackendEmbedded = "embedded"

	// DefaultAssetBackend is the asset backend used when none is set.
	DefaultAssetBackend = AssetBackendNone

	// assetBackendStartupTimeout bounds the startup check of the asset
	// backend connection.
	assetBackendStartupTimeout = 30 * time.Second
)

// AssetsConfig selects the backend that verifies Taproot Assets.
type AssetsConfig struct {
	// Backend selects the asset backend: "none", "tapd" or "embedded".
	Backend string `mapstructure:"backend"`

	// Tapd configures the tapd node used by the "tapd" backend.
	Tapd *TapdConfig `mapstructure:"tapd"`

	// GroupKeys lists, as hex compressed public keys, the asset groups
	// the "embedded" backend trusts beyond those of the assets a request
	// names. A proof of a grouped asset verifies only for a trusted
	// group.
	GroupKeys []string `mapstructure:"groupkeys"`
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

	case AssetBackendEmbedded:
		// The engine derives its keys from the daemon wallet's own
		// seed, which an lnd wallet does not expose.
		switch c.Wallet.Type {
		case WalletTypeLwwallet, WalletTypeBtcwallet:
		default:
			return fmt.Errorf("assets.backend embedded requires "+
				"wallet.type %s or %s", WalletTypeLwwallet,
				WalletTypeBtcwallet)
		}
		if c.Network != "regtest" {
			return fmt.Errorf("assets.backend embedded is "+
				"available on regtest only: network is %s",
				c.Network)
		}
		if _, err := c.Assets.groupKeys(); err != nil {
			return err
		}

		return nil

	default:
		return fmt.Errorf("unknown assets.backend %q (want %s, "+
			"%s or %s)", c.Assets.Backend, AssetBackendNone,
			AssetBackendTapd, AssetBackendEmbedded)
	}
}

// groupKeys parses the configured trusted asset group keys.
func (c *AssetsConfig) groupKeys() ([]tapsdk.PubKey, error) {
	keys := make([]tapsdk.PubKey, 0, len(c.GroupKeys))
	for _, encoded := range c.GroupKeys {
		raw, err := hex.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("assets.groupkeys: %q is "+
				"not hex: %w", encoded, err)
		}
		key, err := tapsdk.ParsePubKey(raw)
		if err != nil {
			return nil, fmt.Errorf("assets.groupkeys: %q is not a "+
				"compressed public key: %w", encoded, err)
		}
		keys = append(keys, key)
	}

	return keys, nil
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
	s.assetClient = client
	cleanup := func() {
		if err := client.Close(); err != nil {
			s.log.DebugS(ctx, "Unable to close tapd connection",
				slog.String("error", err.Error()),
			)
		}
	}

	claimer, err := s.newAssetClaimer(client)
	if err != nil {
		cleanup()

		return nil, nil, err
	}
	s.assetClaimer = claimer

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

// newAssetClaimer returns the claimer of asset deposits and asset VTXOs. Its
// journal pins each claim, so a retry or a restart rebuilds the same one.
func (s *Server) newAssetClaimer(client *tapgrpc.Client) (*tapassets.Claimer,
	error) {

	journal, err := tapassets.NewFileStore(
		filepath.Join(
			s.cfg.NetworkDir(),
			"assets", "claims",
		),
	)
	if err != nil {
		return nil, err
	}

	return tapassets.NewClaimer(tapassets.ClaimerConfig{
		Wallet: tapsdk.NewWallet(client, tapsdk.Network(s.cfg.Network)),
		Store:  journal,
		Signer: claimSigner{server: s},
	})
}

// startAssetDeposits starts the asset deposit service on the connected asset
// backend. The returned cleanup stops the deposit watcher.
func (s *Server) startAssetDeposits(ctx context.Context, store *db.Store) (
	func(), error) {

	if s.assetClient == nil {
		return func() {}, nil
	}

	journal, err := s.assetDepositJournal(store)
	if err != nil {
		return nil, err
	}
	onboarder, err := tapassets.NewOnboarder(tapassets.OnboarderConfig{
		Wallet: tapsdk.NewWallet(
			s.assetClient, tapsdk.Network(s.cfg.Network),
		),
		Store: journal,
		Funder: &depositFunder{
			wallet:      s.depositWallet,
			estimateFee: s.estimateDepositFee,
		},
		DeriveOwnerKey: s.depositOwnerKeyDeriver(),
	})
	if err != nil {
		return nil, err
	}

	depositStore := db.NewAssetDepositStore(store)
	s.assetDeposits = newAssetDepositService(&assetDepositConfig{
		log:              s.log,
		store:            depositStore,
		backend:          s.assetClient,
		signsAnchors:     s.embeddedAssets != nil,
		onboarder:        onboarder,
		terms:            s.loadOperatorTerms,
		quoteBoardingFee: s.quoteBoardingFee,
		chainParams:      s.chainParams,
		registerBoarding: s.registerAssetBoarding,
		claims:           depositStore,
		claimer:          s.assetClaimer,
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
	}, nil
}

// assetDepositJournal returns the journal that makes deposits idempotent.
// The embedded backend keeps it in the database, since a browser has no
// files; the tapd backend keeps its files.
func (s *Server) assetDepositJournal(store *db.Store) (tapassets.Store, error) {
	if s.embeddedAssets != nil {
		return &assetJournal{store: db.NewAssetEngineStore(store)}, nil
	}

	return tapassets.NewFileStore(
		filepath.Join(
			s.cfg.NetworkDir(),
			"assets", "onboarding",
		),
	)
}
