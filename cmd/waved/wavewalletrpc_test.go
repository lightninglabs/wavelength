//go:build wavewalletrpc && swapruntime

package main

import (
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// TestWalletRPCEagerRoundJoinDefault is a table-driven test that exercises
// main.go's PreRunE wiring for the --eagerroundjoin flag under the
// wavewalletrpc build tag. It binds a viper instance against a pflag set just
// like the real binary, optionally sets the flag, then runs the unmarshal +
// configureWalletRPC path and asserts the resulting Config.EagerRoundJoin
// value.
//
// The build-tagged waved.DefaultConfig() seeds cfg.EagerRoundJoin from
// defaultEagerRoundJoin(), so the flag's default flows through viper's normal
// precedence -- no IsSet probing is required to preserve an explicit operator
// override.
func TestWalletRPCEagerRoundJoinDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setFlag     bool
		flagValue   string
		wantEager   bool
		description string
	}{
		{
			name:      "build_tag_default",
			setFlag:   false,
			wantEager: true,
			description: "wavewalletrpc build defaults " +
				"EagerRoundJoin to true when the operator " +
				"leaves the flag unset",
		},
		{
			name:      "explicit_false_wins",
			setFlag:   true,
			flagValue: "false",
			wantEager: false,
			description: "an explicit --eagerroundjoin=false " +
				"override wins over the wavewalletrpc build " +
				"default of true",
		},
		{
			name:      "explicit_true_respected",
			setFlag:   true,
			flagValue: "true",
			wantEager: true,
			description: "an explicit --eagerroundjoin=true " +
				"matches the build default and round-trips " +
				"intact",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := waved.DefaultConfig()
			v := viper.New()
			f := pflag.NewFlagSet(
				"waved-test", pflag.ContinueOnError,
			)
			f.Bool("eagerroundjoin", cfg.EagerRoundJoin, "")
			if err := v.BindPFlags(f); err != nil {
				t.Fatalf("bind pflags: %v", err)
			}

			if tc.setFlag {
				if err := f.Set(
					"eagerroundjoin", tc.flagValue,
				); err != nil {

					t.Fatalf("set eagerroundjoin flag: %v",
						err)
				}
			}

			if err := v.Unmarshal(cfg); err != nil {
				t.Fatalf("unmarshal config: %v", err)
			}
			configureWalletRPC(cfg)

			if cfg.EagerRoundJoin != tc.wantEager {
				t.Fatalf("%s: want EagerRoundJoin=%v, got %v",
					tc.description, tc.wantEager,
					cfg.EagerRoundJoin)
			}
		})
	}
}
