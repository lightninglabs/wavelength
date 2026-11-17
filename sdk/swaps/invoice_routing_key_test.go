package swaps

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestInvoiceRoutingKeyRestoresFromInvoice verifies that a fresh SDK can reopen
// invoices in both modes and that the delegated key is payment-scoped.
func TestInvoiceRoutingKeyRestoresFromInvoice(t *testing.T) {
	t.Parallel()

	walletKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	daemon := &testDaemonConn{identityKey: walletKey.PubKey()}
	preimage := lntypes.Preimage{1}
	hash := preimage.Hash()
	net := &chaincfg.RegressionNetParams
	generator := NewEphemeralInvoiceGenerator(walletKey, nil, net)
	client := NewSwapClient(nil, daemon, nil, generator)
	legacy, err := client.receiveAuthKey(t.Context(), hash)
	require.NoError(t, err)
	routing, err := client.deriveInvoiceRoutingKey(t.Context(), hash)
	require.NoError(t, err)
	other, err := client.deriveInvoiceRoutingKey(
		t.Context(), lntypes.Hash{2},
	)
	require.NoError(t, err)
	require.False(t, routing.PubKey().IsEqual(legacy.PubKey()))
	require.False(t, routing.PubKey().IsEqual(other.PubKey()))

	for _, key := range []ReceiveAuthKey{legacy, routing} {
		hints := [][]*RouteHint{{{
			NodeID:    walletKey.PubKey().SerializeCompressed(),
			ChannelID: 42, CltvExpiryDelta: 318,
		}}}
		inv, _, err := generator.CreateInvoiceWithKeyRouteHintPaths(
			t.Context(), 1_000, "receive", hints, 0, key, &preimage,
		)
		require.NoError(t, err)

		session := &ReceiveSession{
			client:      NewSwapClient(nil, daemon, nil, generator),
			Invoice:     string(inv.PaymentRequest),
			PaymentHash: hash,
		}
		restored, err := session.receiveOnionKey(t.Context())
		require.NoError(t, err)
		require.True(t, key.PubKey().IsEqual(restored.PubKey()))

		session.Invoice += "?"
		_, err = session.receiveOnionKey(t.Context())
		require.ErrorContains(t, err, "decode stored receive invoice")
	}
}

// invoiceRouteTestClient scripts capability and registration responses.
type invoiceRouteTestClient struct {
	swaprpc.SwapServiceClient
	discoveryCode codes.Code
	supportsKey   bool
	acknowledge   bool
	discoveries   int
	requests      int
	request       *swaprpc.RequestChannelIdRequest
}

func (c *invoiceRouteTestClient) GetSwapRecoveryInfo(_ context.Context,
	_ *swaprpc.GetSwapRecoveryInfoRequest, _ ...grpc.CallOption) (
	*swaprpc.GetSwapRecoveryInfoResponse, error) {

	c.discoveries++
	if c.discoveryCode != codes.OK {
		return nil, status.Error(c.discoveryCode, "discovery failed")
	}

	return &swaprpc.GetSwapRecoveryInfoResponse{
		SupportsInvoiceRoutingKey: c.supportsKey,
	}, nil
}

func (c *invoiceRouteTestClient) RequestChannelId(_ context.Context,
	req *swaprpc.RequestChannelIdRequest, _ ...grpc.CallOption) (
	*swaprpc.RequestChannelIdResponse, error) {

	c.requests++
	c.request = req
	hop := &swaprpc.RouteHint{
		NodeId: req.GetClientVhtlcPubkey(), ChannelId: 42,
		CltvExpiryDelta: 318,
	}
	resp := &swaprpc.RequestChannelIdResponse{
		RouteHintPaths: []*swaprpc.RouteHintPath{{
			Hops: []*swaprpc.RouteHint{
				hop,
			},
		}},
	}
	if c.acknowledge {
		_, pub := btcec.PrivKeyFromBytes(req.GetInvoiceRoutingKey())
		resp.InvoiceRoutingPubkey = pub.SerializeCompressed()
	}

	return resp, nil
}

// TestReceiveInvoiceRoutingNegotiation checks which key signs the invoice and
// which fields are sent in the single signed registration.
func TestReceiveInvoiceRoutingNegotiation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		isREST        bool
		discoveryCode codes.Code
		supportsKey   bool
		acknowledge   bool
		dropMetadata  bool
		wantErr       string
	}{
		{
			name: "delegated invoice", isREST: true,
			supportsKey: true, acknowledge: true,
		},
		{
			name: "missing acknowledgement", supportsKey: true,
			wantErr: "invoice routing key not acknowledged",
		},
		{
			name:          "RPC unavailable",
			discoveryCode: codes.Unavailable,
		},
		{
			name:          "RPC unimplemented",
			discoveryCode: codes.Unimplemented,
		},
		{
			name: "REST without routing key support", isREST: true,
		},
		{
			name: "REST without recovery endpoint", isREST: true,
			discoveryCode: codes.NotFound, dropMetadata: true,
		},
		{
			name: "REST discovery unavailable", isREST: true,
			discoveryCode: codes.Unavailable,
			wantErr:       "discovery failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wallet, err := btcec.NewPrivateKey()
			require.NoError(t, err)
			daemon := &testDaemonConn{
				identityKey:  wallet.PubKey(),
				identityPriv: wallet,
				operatorKey:  wallet.PubKey(),
			}
			wire := &invoiceRouteTestClient{
				discoveryCode: tc.discoveryCode,
				supportsKey:   tc.supportsKey,
				acknowledge:   tc.acknowledge,
			}
			conn := newSwapServerConn(wire)
			conn.isREST = tc.isREST
			conn.creditAccountSigner = func(_ context.Context,
				account []byte, digest [32]byte, expiry int64,
				nonce [creditAccountNonceSize]byte) (
				*schnorr.Signature, error) {

				msg := swaprpc.CreditAccountAuthDigest(
					account, digest, expiry, nonce[:],
				)

				return schnorr.Sign(wallet, msg[:])
			}
			net := &chaincfg.RegressionNetParams
			generator := NewEphemeralInvoiceGenerator(
				wallet, nil, net,
			)
			client := NewSwapClient(conn, daemon, nil, generator)
			session := &ReceiveSession{
				client: client, amountSat: 1_000,
				state: ReceiveStateCreated,
			}

			err = session.prepareInvoice(t.Context())
			require.Equal(t, 1, wire.discoveries)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Empty(t, session.Invoice)
				if tc.supportsKey {
					require.Equal(t, 1, wire.requests)
					require.NotEmpty(
						t, wire.request.
							GetInvoiceRoutingKey(),
					)
				} else {
					require.Zero(t, wire.requests)
				}

				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, wire.requests)
			require.NotNil(
				t, wire.request.GetAccountAuthorization(),
			)
			require.Equal(
				t, tc.dropMetadata,
				wire.request.GetOwnerProof() == nil,
			)
			params := wire.request.GetPreimageRecoveryParams()
			require.Equal(
				t, tc.dropMetadata, len(params) == 0,
			)

			invoice, err := zpay32.Decode(session.Invoice, net)
			require.NoError(t, err)
			hash := session.PaymentHash
			if tc.supportsKey {
				key, err := client.deriveInvoiceRoutingKey(
					t.Context(), hash,
				)
				require.NoError(t, err)
				require.Equal(
					t, key.PrivKey.Serialize(),
					wire.request.GetInvoiceRoutingKey(),
				)
				require.True(
					t,
					invoice.Destination.IsEqual(
						key.PubKey(),
					),
				)

				return
			}

			legacy, err := client.receiveAuthKey(
				t.Context(), hash,
			)
			require.NoError(t, err)
			require.Empty(t, wire.request.GetInvoiceRoutingKey())
			require.True(
				t,
				invoice.Destination.IsEqual(
					legacy.PubKey(),
				),
			)
		})
	}
}
