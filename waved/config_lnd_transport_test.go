package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigValidateLndTransport asserts the lnd.transport selector: an empty
// value and "grpc" both mean gRPC and are accepted, "rest" is accepted, and any
// other value is rejected with an lnd.transport-scoped error.
func TestConfigValidateLndTransport(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		transport string
		wantErr   bool
	}{
		{
			name:      "empty defaults to grpc",
			transport: "",
			wantErr:   false,
		},
		{
			name:      "grpc",
			transport: RPCTransportGRPC,
			wantErr:   false,
		},
		{
			name:      "rest",
			transport: RPCTransportREST,
			wantErr:   false,
		},
		{
			name:      "unknown",
			transport: "http2",
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = "regtest"
			cfg.Wallet.Type = WalletTypeLnd
			cfg.Lnd.Host = "127.0.0.1:10009"
			cfg.Lnd.Transport = tc.transport

			err := cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(
					t, err.Error(),
					"lnd.transport",
				)

				return
			}

			require.NoError(t, err)
		})
	}
}

// TestDefaultConfigLndTransportGRPC locks in the default: DefaultConfig selects
// the gRPC transport so an operator building from defaults keeps the historical
// behavior.
func TestDefaultConfigLndTransportGRPC(t *testing.T) {
	t.Parallel()

	require.Equal(t, RPCTransportGRPC, DefaultConfig().Lnd.Transport)
}
