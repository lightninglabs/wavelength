package waved

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
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
