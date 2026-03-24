package darepo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lightninglabs/lndclient"
	"github.com/stretchr/testify/require"
)

// TestDefaultConfigIsValid ensures that the default config satisfies
// its own validation rules.
func TestDefaultConfigIsValid(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.NoError(t, cfg.Validate())
}

// TestConfigValidate exercises the config validation logic across a
// range of valid and invalid configurations.
func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		modify  func(c *Config)
		wantErr string
	}{
		{
			name:   "default config is valid",
			modify: func(c *Config) {},
		},
		{
			name: "unknown network",
			modify: func(c *Config) {
				c.Network = "fakenet"
			},
			wantErr: "unknown network",
		},
		{
			name: "nil lnd config",
			modify: func(c *Config) {
				c.Lnd = nil
			},
			wantErr: "lnd config is required",
		},
		{
			name: "empty lnd host",
			modify: func(c *Config) {
				c.Lnd.Host = ""
			},
			wantErr: "lnd host is required",
		},
		{
			name: "nil db config",
			modify: func(c *Config) {
				c.DB = nil
			},
			wantErr: "db config is required",
		},
		{
			name: "nil admin rpc config",
			modify: func(c *Config) {
				c.AdminRPC = nil
			},
			wantErr: "admin rpc config is required",
		},
		{
			name: "empty admin rpc listen",
			modify: func(c *Config) {
				c.AdminRPC.ListenAddr = ""
			},
			wantErr: "admin rpc listen address is required",
		},
		{
			name: "nil rpc config",
			modify: func(c *Config) {
				c.RPC = nil
			},
			wantErr: "rpc config is required",
		},
		{
			name: "nil rounds config",
			modify: func(c *Config) {
				c.Rounds = nil
			},
			wantErr: "rounds config is required",
		},
		{
			name: "zero connector dust amount",
			modify: func(c *Config) {
				c.Rounds.ConnectorDustAmount = 0
			},
			wantErr: "rounds connector dust amount must be > 0",
		},
		{
			name: "empty rpc listen",
			modify: func(c *Config) {
				c.RPC.ListenAddr = ""
			},
			wantErr: "rpc listen address is required",
		},
		{
			name: "all supported networks",
			modify: func(c *Config) {
				// Just test one non-default valid network.
				c.Network = "mainnet"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			tc.modify(cfg)

			err := cfg.Validate()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestNetworkToLndclient verifies the mapping from our network
// strings to the lndclient network type.
func TestNetworkToLndclient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		network string
		want    lndclient.Network
		wantErr bool
	}{
		{"mainnet", lndclient.NetworkMainnet, false},
		{"testnet", lndclient.NetworkTestnet, false},
		{"regtest", lndclient.NetworkRegtest, false},
		{"simnet", lndclient.NetworkSimnet, false},
		{"signet", lndclient.NetworkSignet, false},
		{"fakenet", "", true},
		{"", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.network, func(t *testing.T) {
			t.Parallel()

			got, err := networkToLndclient(tc.network)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
		})
	}
}

// TestExpandTilde verifies tilde expansion for various path patterns.
func TestExpandTilde(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "bare tilde",
			path: "~",
			want: home,
		},
		{
			name: "tilde with path",
			path: "~/.arkd",
			want: filepath.Join(home, ".arkd"),
		},
		{
			name: "absolute path unchanged",
			path: "/tmp/arkd",
			want: "/tmp/arkd",
		},
		{
			name: "relative path unchanged",
			path: "data/arkd",
			want: "data/arkd",
		},
		{
			name: "empty string unchanged",
			path: "",
			want: "",
		},
		{
			name: "tilde nested path",
			path: "~/a/b/c",
			want: filepath.Join(home, "a", "b", "c"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := expandTilde(tc.path)
			require.Equal(t, tc.want, got)
		})
	}
}
