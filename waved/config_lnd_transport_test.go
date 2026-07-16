package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigValidateLndTransport asserts the lnd.transport selector: an empty
// value and "grpc" both mean gRPC and are accepted, "rest" is accepted (but
// requires lnd.macaroonpath, since REST cannot resolve the per-network default
// path), and any other value is rejected with an lnd.transport-scoped error.
func TestConfigValidateLndTransport(t *testing.T) {
	t.Parallel()

	const macPath = "/tmp/admin.macaroon"

	cases := []struct {
		name            string
		transport       string
		macaroonPath    string
		wantErrContains string
	}{
		{
			name:      "empty defaults to grpc",
			transport: "",
		},
		{
			name:      "grpc",
			transport: RPCTransportGRPC,
		},
		{
			name:         "rest with macaroon",
			transport:    RPCTransportREST,
			macaroonPath: macPath,
		},
		{
			name:            "rest without macaroon",
			transport:       RPCTransportREST,
			wantErrContains: "lnd.macaroonpath",
		},
		{
			name:            "unknown",
			transport:       "http2",
			wantErrContains: "lnd.transport",
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
			cfg.Lnd.MacaroonPath = tc.macaroonPath

			err := cfg.Validate()
			if tc.wantErrContains != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErrContains,
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
