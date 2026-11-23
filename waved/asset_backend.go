package waved

import (
	"context"
	"encoding/hex"
	"fmt"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/tapassets"
)

const (
	// AssetBackendNone disables Taproot Assets. Rounds that would create
	// an asset VTXO for this daemon fail before signing.
	AssetBackendNone = "none"

	// AssetBackendEmbedded runs the asset engine inside the daemon, on
	// the daemon wallet's keys, chain backend and database.
	AssetBackendEmbedded = "embedded"

	// DefaultAssetBackend is the asset backend used when none is set.
	DefaultAssetBackend = AssetBackendNone
)

// AssetsConfig selects the backend that verifies Taproot Assets.
type AssetsConfig struct {
	// Backend selects the asset backend: "none" or "embedded".
	Backend string `mapstructure:"backend"`

	// GroupKeys lists, as hex compressed public keys, the asset groups
	// the "embedded" backend trusts beyond those of the assets a request
	// names. A proof of a grouped asset verifies only for a trusted
	// group.
	GroupKeys []string `mapstructure:"groupkeys"`
}

// defaultAssetsConfig returns the asset configuration with no backend.
func defaultAssetsConfig() *AssetsConfig {
	return &AssetsConfig{Backend: DefaultAssetBackend}
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

	case AssetBackendEmbedded:
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
		return fmt.Errorf("unknown assets.backend %q (want %s or %s)",
			c.Assets.Backend, AssetBackendNone,
			AssetBackendEmbedded)
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

// newAssetClaimer returns the claimer of asset deposits and asset VTXOs on
// an asset backend. The journal pins each claim, so a retry or a restart
// rebuilds the same one. The daemon wallet signs the exit leaf.
func (s *Server) newAssetClaimer(client tapsdk.Client,
	journal tapassets.Store) (*tapassets.Claimer, error) {

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

	// The deposit journal lives in the database beside the claim journal,
	// since a browser has no files.
	onboarder, err := tapassets.NewOnboarder(tapassets.OnboarderConfig{
		Wallet: tapsdk.NewWallet(
			s.assetClient, tapsdk.Network(s.cfg.Network),
		),
		Store: &assetJournal{store: db.NewAssetEngineStore(store)},
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
		onboarder:        onboarder,
		terms:            s.loadOperatorTerms,
		quoteBoardingFee: s.quoteBoardingFee,
		chainParams:      s.chainParams,
		registerBoarding: s.registerAssetBoarding,
		claims:           depositStore,
		claimer:          s.assetClaimer,
		broadcastClaim:   s.broadcastAssetClaim,
		reserveClaim:     s.reserveAssetDepositClaim,

		// The engine's record of a claim's block is checked against
		// the daemon's chain backend, which serves the engine its
		// headers.
		bestBlockHash: (&assetChain{server: s}).BlockHash,
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
