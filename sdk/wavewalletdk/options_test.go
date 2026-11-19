//go:build wavewalletrpc && swapruntime && !js

package wavewalletdk

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestEmbeddedArkChannelRegistrarLifecycle boots the real swap and wallet
// registrars on the same directory across opt-out, opt-in and restart.
func TestEmbeddedArkChannelRegistrarLifecycle(t *testing.T) {
	base := waved.DefaultConfig()
	base.LogWriter = io.Discard
	registered := make(chan *waved.Config, 1)
	base.RPCServiceRegistrars = []waved.RPCServiceRegistrar{
		func(_ context.Context, _ *grpc.Server, _ *waved.RPCServer,
			cfg *waved.Config) (func(), error) {

			registered <- cfg

			return nil, nil
		},
	}
	cfg := Config{
		DaemonConfig:        base,
		DataDir:             t.TempDir(),
		Network:             "regtest",
		ServerTransport:     TransportREST,
		SwapServerTransport: TransportREST,
		WalletEsploraURL:    "http://127.0.0.1",
	}
	for _, enabled := range []bool{false, true, true} {
		cfg.ArkChannelsEnabled = enabled
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		client, err := Start(ctx, cfg)
		cancel()
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Stop() })
		running := <-registered
		require.NotNil(t, running.Swap.Backend)
		require.Equal(t, enabled, running.Swap.ArkChannelMailbox != nil)
		require.Equal(t, enabled, running.Swap.ArkChannelReceiveEnabled)
		require.NoError(t, client.Stop())
		require.Nil(t, running.Swap.ArkChannelMailbox)
		require.Nil(t, base.Swap.ArkChannelMailbox)
	}
}

// TestResolveDaemonConfigEagerRoundJoin is a table-driven test that pins the
// wavewalletdk-side override contract for waved.Config.EagerRoundJoin under the
// wavewalletrpc build tag. The matrix covers the three production paths a host
// can take through wavewalletdk.Start:
//
//  1. Pure convenience Config (no caller-owned DaemonConfig). The default
//     comes from waved.DefaultConfig, which is build-tag-aware.
//  2. Convenience Config with WithEagerRoundJoinDisabled() applied. The
//     functional option must win over the build-tag default.
//  3. Caller-owned DaemonConfig with the option applied. The option must win
//     over a true value carried on the caller's DaemonConfig too, because
//     "leave at zero" on the convenience field cannot disambiguate from
//     "explicit false" -- the functional option is the only way to force off.
func TestResolveDaemonConfigEagerRoundJoin(t *testing.T) {
	t.Parallel()

	// buildCallerDaemonCfg returns a caller-owned waved.Config with
	// EagerRoundJoin pre-set to true, mirroring a host that supplies its
	// own DaemonConfig and inherits the wavewalletrpc build-tag default.
	buildCallerDaemonCfg := func() *waved.Config {
		daemonCfg := waved.DefaultConfig()
		daemonCfg.EagerRoundJoin = true

		return daemonCfg
	}

	tests := []struct {
		name      string
		buildCfg  func() Config
		opts      []Option
		wantEager bool
		why       string
	}{
		{
			name: "convenience_config_inherits_build_tag_default",
			buildCfg: func() Config {
				return DefaultConfig()
			},
			opts:      nil,
			wantEager: true,
			why: "wavewalletrpc-tagged waved.DefaultConfig seeds " +
				"EagerRoundJoin=true; the convenience Config " +
				"inherits it",
		},
		{
			name: "option_forces_off_over_build_tag_default",
			buildCfg: func() Config {
				return DefaultConfig()
			},
			opts: []Option{
				WithEagerRoundJoinDisabled(),
			},
			wantEager: false,
			why: "WithEagerRoundJoinDisabled() must win over the " +
				"build-tag default of true",
		},
		{
			name: "option_forces_off_over_caller_daemon_config",
			buildCfg: func() Config {
				return Config{
					DaemonConfig: buildCallerDaemonCfg(),
				}
			},
			opts: []Option{
				WithEagerRoundJoinDisabled(),
			},
			wantEager: false,
			why: "WithEagerRoundJoinDisabled() must also win over " +
				"an EagerRoundJoin=true value carried on a " +
				"caller-supplied DaemonConfig",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			daemonCfg, err := resolveDaemonConfig(
				tc.buildCfg(), tc.opts...,
			)
			require.NoError(t, err)

			require.Equal(
				t, tc.wantEager, daemonCfg.EagerRoundJoin,
				tc.why,
			)
		})
	}
}

// TestResolveDaemonConfigMaxPaymentCLTV verifies the explicit disable option
// wins over both the swap-enabled default and a caller-owned daemon policy.
func TestResolveDaemonConfigMaxPaymentCLTV(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		buildCfg func() Config
		want     int32
	}{
		{
			name: "option overrides build default",
			buildCfg: func() Config {
				return DefaultConfig()
			},
			want: 0,
		},
		{
			name: "option overrides caller daemon config",
			buildCfg: func() Config {
				daemonCfg := waved.DefaultConfig()
				daemonCfg.MaxPaymentCLTV = 450

				return Config{
					DaemonConfig: daemonCfg,
				}
			},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			daemonCfg, err := resolveDaemonConfig(
				tc.buildCfg(), WithMaxPaymentCLTVDisabled(),
			)
			require.NoError(t, err)
			require.Equal(t, tc.want, daemonCfg.MaxPaymentCLTV)
		})
	}
}
