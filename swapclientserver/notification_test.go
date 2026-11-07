//go:build swapruntime

package swapclientserver

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestNotificationPinFailsBeforeSetup proves malformed gRPC and REST pins fail
// before setup can touch the RPCServer, its signer hooks or a wallet store.
func TestNotificationPinFailsBeforeSetup(t *testing.T) {
	for _, transport := range []string{
		waved.RPCTransportGRPC,
		waved.RPCTransportREST,
	} {
		t.Run(transport, func(t *testing.T) {
			cfg := waved.DefaultConfig()
			cfg.Swap.ServerTransport = transport
			cfg.Swap.NotificationIssuer = "not-a-compressed-key"
			cleanup, err := Register(
				t.Context(), grpc.NewServer(), nil, cfg,
			)
			require.ErrorIs(t, err, errInvalidNotificationIssuer)
			require.Nil(t, cleanup)
		})
	}
}

// TestNotificationPinCanonicalIdentity accepts exactly a compressed public key
// and keeps unsupported transports without a registrar, even with a valid pin.
func TestNotificationPinCanonicalIdentity(t *testing.T) {
	_, key := btcec.PrivKeyFromBytes([]byte{7})
	issuer, err := parseSwapNotificationIssuer(&waved.SwapConfig{
		NotificationIssuer: hex.EncodeToString(
			key.SerializeCompressed(),
		),
	})
	require.NoError(t, err)
	require.True(t, key.IsEqual(issuer))
	service := &swapClientService{}
	require.Nil(t, service.swapNotificationRegistrar(issuer, nil))
}
