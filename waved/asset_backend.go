package waved

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/tapassets"
)

// AssetsConfig configures Taproot Assets.
type AssetsConfig struct {
	// Enabled turns on Taproot Assets. The daemon then holds and verifies
	// assets itself, on its wallet's keys, chain backend and database.
	// When it is off, a round that would create an asset VTXO for this
	// daemon fails before signing.
	Enabled bool `mapstructure:"enabled"`

	// GroupKeys lists, as hex compressed public keys, the asset groups
	// the daemon trusts beyond those of the assets a request names. A
	// proof of a grouped asset verifies only for a trusted group.
	GroupKeys []string `mapstructure:"groupkeys"`

	// Courier configures the proof courier of the daemon's Taproot Asset
	// addresses.
	Courier AssetCourierConfig `mapstructure:"courier"`
}

// assetCourierScheme is the scheme of a proof courier that runs both a
// universe and an auth mailbox, the only kind a version 2 address can name.
const assetCourierScheme = "authmailbox+universerpc"

// AssetCourierConfig configures the proof courier through which the daemon
// receives at its Taproot Asset addresses. A sender uploads the proofs of a
// transfer to the courier's universe and announces the transfer in the
// courier's mailbox, which the daemon reads.
type AssetCourierConfig struct {
	// Addr is the proof courier written into new addresses, as
	// authmailbox+universerpc://host:port. Empty leaves the daemon
	// unable to create an address.
	Addr string `mapstructure:"addr"`

	// Endpoints lists the couriers the daemon does not reach as their
	// address says. An entry is host:port=target or
	// host:port=target=tlscertpath: host:port is the courier as an address
	// names it, target is the host and port to dial in its place, and
	// tlscertpath is a PEM file of the certificates to trust for it in
	// place of the system's. An empty target dials the address.
	Endpoints []string `mapstructure:"endpoints"`
}

// assetCourierEndpoint is how the daemon reaches one proof courier.
type assetCourierEndpoint struct {
	// target is the host and port to dial, empty to dial the address.
	target string

	// tlsCertPath is a PEM file of the certificates to trust, empty to
	// trust the system's.
	tlsCertPath string
}

// addr parses the proof courier of new addresses, nil when none is
// configured. The asset engine takes the address as it is written: it
// rejects another scheme and does not complete a missing part.
func (c *AssetCourierConfig) addr() (*url.URL, error) {
	if c.Addr == "" {
		return nil, nil
	}

	addr, err := url.Parse(c.Addr)
	if err != nil {
		return nil, fmt.Errorf("assets.courier.addr: %w", err)
	}

	bare := addr.Opaque == "" && addr.User == nil && addr.Path == "" &&
		addr.RawQuery == "" && addr.Fragment == ""
	if addr.Scheme != assetCourierScheme || addr.Hostname() == "" ||
		addr.Port() == "" || !bare {
		return nil, fmt.Errorf("assets.courier.addr: %q is not of the "+
			"form %s://host:port", c.Addr, assetCourierScheme)
	}

	return addr, nil
}

// endpoints parses the dial overrides, keyed by the host and port of the
// courier address they apply to.
func (c *AssetCourierConfig) endpoints() (map[string]assetCourierEndpoint,
	error) {

	endpoints := make(map[string]assetCourierEndpoint, len(c.Endpoints))
	for _, entry := range c.Endpoints {
		parts := strings.SplitN(entry, "=", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("assets.courier.endpoints: %q "+
				"is not host:port=target or "+
				"host:port=target=tlscertpath", entry)
		}

		host := parts[0]
		if err := validateHostPort(host); err != nil {
			return nil, fmt.Errorf("assets.courier.endpoints: "+
				"courier %q: %w", host, err)
		}
		if _, ok := endpoints[host]; ok {
			return nil, fmt.Errorf("assets.courier.endpoints: "+
				"courier %q is listed twice", host)
		}

		endpoint := assetCourierEndpoint{target: parts[1]}
		if len(parts) == 3 {
			endpoint.tlsCertPath = parts[2]
		}
		if endpoint.target == "" && endpoint.tlsCertPath == "" {
			return nil, fmt.Errorf("assets.courier.endpoints: %q "+
				"names no target and no certificate", entry)
		}
		if endpoint.target != "" {
			err := validateHostPort(endpoint.target)
			if err != nil {
				return nil, fmt.Errorf(
					"assets.courier.endpoints: target "+
						"%q: %w", endpoint.target, err)
			}
		}
		endpoints[host] = endpoint
	}

	return endpoints, nil
}

// validateHostPort checks that an address names a host and a port.
func validateHostPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || port == "" {
		return fmt.Errorf("host and port are required")
	}

	return nil
}

// defaultAssetsConfig returns the asset configuration with assets off.
func defaultAssetsConfig() *AssetsConfig {
	return &AssetsConfig{}
}

// validateAssetsConfig checks the asset configuration.
func (c *Config) validateAssetsConfig() error {
	if c.Assets == nil {
		c.Assets = defaultAssetsConfig()
	}
	if !c.Assets.Enabled {
		return nil
	}

	if c.Network != "regtest" {
		return fmt.Errorf("assets.enabled is available on regtest "+
			"only: network is %s", c.Network)
	}
	if _, err := c.Assets.groupKeys(); err != nil {
		return err
	}
	if _, err := c.Assets.Courier.addr(); err != nil {
		return err
	}
	_, err := c.Assets.Courier.endpoints()

	return err
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
