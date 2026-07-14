//go:build wavewalletrpc && swapruntime && !js

package wavewalletdk

import (
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
)

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
