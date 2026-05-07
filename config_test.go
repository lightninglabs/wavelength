package darepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightninglabs/darepo-client/chainbackends/bitcoindrpc"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo/mailbox"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

const mailboxCountLimitErr = "mailbox max envelopes per mailbox must be >= 0"

// TestDefaultConfigIsValid ensures that the default config satisfies
// its own validation rules.
func TestDefaultConfigIsValid(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.NoError(t, cfg.Validate())
}

// TestDefaultConfigDisablesMetrics ensures metrics remain opt-in in the
// default daemon configuration.
func TestDefaultConfigDisablesMetrics(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.NotNil(t, cfg.Metrics)
	require.Empty(t, cfg.Metrics.ListenAddr)
}

// TestDefaultConfigIncludesMailboxConfig ensures mailbox limits are
// configurable even when left disabled by default.
func TestDefaultConfigIncludesMailboxConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.NotNil(t, cfg.Mailbox)
	require.Zero(t, cfg.Mailbox.MaxEnvelopeBytes)
	require.Zero(t, cfg.Mailbox.MaxEnvelopesPerMailbox)
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
			name: "nil mailbox config",
			modify: func(c *Config) {
				c.Mailbox = nil
			},
			wantErr: "mailbox config is required",
		},
		{
			name: "negative mailbox size limit",
			modify: func(c *Config) {
				c.Mailbox.MaxEnvelopeBytes = -1
			},
			wantErr: "mailbox max envelope bytes must be >= 0",
		},
		{
			name: "negative mailbox count limit",
			modify: func(c *Config) {
				c.Mailbox.MaxEnvelopesPerMailbox = -1
			},
			wantErr: mailboxCountLimitErr,
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
		{
			name: "negative static fee rate",
			modify: func(c *Config) {
				c.Fees.StaticFeeRateSatKW = -1
			},
			wantErr: "fees.staticfeeratesatkw must be non-negative",
		},
		{
			name: "sub-floor static fee rate",
			modify: func(c *Config) {
				// Positive but below FeePerKwFloor (253).
				c.Fees.StaticFeeRateSatKW = 100
			},
			wantErr: "below the bitcoin relay fee floor",
		},
		{
			name: "at-floor static fee rate is accepted",
			modify: func(c *Config) {
				c.Fees.StaticFeeRateSatKW = int64(
					chainfee.FeePerKwFloor,
				)
			},
		},
		{
			name: "static fee rate above sanity ceiling",
			modify: func(c *Config) {
				// 10_000_001 sat/kW > the 10_000_000
				// sanity ceiling.
				c.Fees.StaticFeeRateSatKW = 10_000_001
			},
			wantErr: "exceeds sanity ceiling",
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

// TestConfigValidatePackageRelay ensures the non-serialized package relay
// dependency is validated separately from file-backed config fields.
func TestConfigValidatePackageRelay(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.ErrorContains(
		t, cfg.ValidatePackageRelay(), "bitcoind package relay",
	)

	cfg.PackageSubmitter = bitcoindrpc.New(
		"127.0.0.1:18443", "user", "pass",
	)
	require.NoError(t, cfg.ValidatePackageRelay())
}

// testMailboxEnvelope builds a minimal mailbox envelope for quota
// wiring tests.
func testMailboxEnvelope(recipient, msgID, payload string) *mailbox.Envelope {
	return &mailboxpb.Envelope{
		Recipient: recipient,
		MsgId:     msgID,
		Body: &anypb.Any{
			TypeUrl: "type.googleapis.com/test.Payload",
			Value:   []byte(payload),
		},
	}
}

// TestMailboxStoreOptionsApplyEnvelopeSizeLimit verifies that the
// config-derived store options enforce the mailbox envelope size cap.
func TestMailboxStoreOptionsApplyEnvelopeSizeLimit(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Mailbox.MaxEnvelopeBytes = 64

	store := mailbox.NewMemoryStore(cfg.mailboxStoreOptions()...)

	_, err := store.Append(
		t.Context(),
		testMailboxEnvelope(
			"alice", "msg-1", strings.Repeat("x", 256),
		),
	)
	require.Error(t, err)

	var tooLarge *mailbox.ErrEnvelopeTooLarge
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, 64, tooLarge.Max)
}

// TestMailboxStoreOptionsApplyPerMailboxLimit verifies that the
// config-derived store options enforce the per-mailbox envelope cap.
func TestMailboxStoreOptionsApplyPerMailboxLimit(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Mailbox.MaxEnvelopesPerMailbox = 1

	store := mailbox.NewMemoryStore(cfg.mailboxStoreOptions()...)

	_, err := store.Append(
		t.Context(),
		testMailboxEnvelope("alice", "msg-1", "ok"),
	)
	require.NoError(t, err)

	_, err = store.Append(
		t.Context(),
		testMailboxEnvelope("alice", "msg-2", "ok"),
	)
	require.Error(t, err)

	var full *mailbox.ErrMailboxFull
	require.ErrorAs(t, err, &full)
	require.Equal(t, "alice", full.Recipient)
	require.Equal(t, 1, full.Max)
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
