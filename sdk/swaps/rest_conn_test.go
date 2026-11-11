package swaps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestRESTSwapServerConnRequestChannelID verifies the REST swap server
// connection uses the swaprpc gateway routes through the shared REST client.
func TestRESTSwapServerConnRequestChannelID(t *testing.T) {
	nodePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nodeID := nodePriv.PubKey().SerializeCompressed()
	routeHint := &swaprpc.RouteHint{
		NodeId:             nodeID,
		ChannelId:          42,
		FeeBaseMsat:        1,
		FeeProportionalPpm: 2,
		CltvExpiryDelta:    40,
	}

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				require.Equal(
					t, "/v1/swap/request-channel-id",
					r.URL.Path,
				)
				requestBody, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				var req swaprpc.RequestChannelIdRequest
				err = protojson.Unmarshal(requestBody, &req)
				require.NoError(t, err)
				require.NotNil(t, req.GetAccountAuthorization())

				routeHintPaths := []*swaprpc.RouteHintPath{{
					Hops: []*swaprpc.RouteHint{
						routeHint,
					},
				}}
				body, err := protojson.Marshal(
					&swaprpc.RequestChannelIdResponse{
						RouteHintPaths: routeHintPaths,
						PayerFeeMsat:   123,
					},
				)
				require.NoError(t, err)

				w.Header().Set(
					"Content-Type", "application/json",
				)
				_, err = w.Write(body)
				require.NoError(t, err)
			},
		),
	)
	defer server.Close()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	signer := func(_ context.Context, accountKey []byte,
		requestDigest [32]byte, expiresAtUnix int64,
		nonce [swaprpc.CreditAccountNonceSize]byte) (*schnorr.Signature,
		error) {

		require.Equal(
			t, clientPriv.PubKey().SerializeCompressed(),
			accountKey,
		)
		digest := swaprpc.CreditAccountAuthDigest(
			clientPriv.PubKey().SerializeCompressed(),
			requestDigest, expiresAtUnix, nonce[:],
		)

		return schnorr.Sign(clientPriv, digest[:])
	}
	client := NewAuthenticatedRESTSwapServerConn(server.URL, signer)
	quote, err := client.RequestChannelID(
		t.Context(), clientPriv.PubKey(), lntypes.Hash{1},
		btcutil.Amount(42_000), 30, true, nil, nil,
	)
	require.NoError(t, err)
	require.Len(t, quote.RouteHintPaths, 1)
	require.Len(t, quote.RouteHintPaths[0], 1)
	hint := quote.RouteHintPaths[0][0]
	require.Equal(t, uint64(42), hint.ChannelID)
	require.Equal(t, nodeID, hint.NodeID)
	require.EqualValues(t, 42_000, quote.ReceiveAmountSat)
	require.EqualValues(t, 123, quote.PayerFeeMsat)
}

// TestRESTSwapRecoveryCompatibility verifies legacy gateways receive requests
// signed without the fields they discard, while upgraded gateways retain them.
func TestRESTSwapRecoveryCompatibility(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		infoStatus int
		wantErr    codes.Code
	}{
		{
			name:       "legacy",
			infoStatus: http.StatusNotFound,
		},
		{
			name:       "unimplemented",
			infoStatus: http.StatusNotImplemented,
		},
		{
			name:       "upgraded",
			infoStatus: http.StatusOK,
		},
		{
			name:       "unavailable",
			infoStatus: http.StatusServiceUnavailable,
			wantErr:    codes.Unavailable,
		},
		{
			name:       "unauthorized",
			infoStatus: http.StatusUnauthorized,
			wantErr:    codes.Unauthenticated,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			key, err := btcec.NewPrivateKey()
			require.NoError(t, err)
			owner := &swaprpc.SwapOwnerProof{
				ClientIdentityPubkey: key.
					PubKey().
					SerializeCompressed(),
				Signature: []byte{
					1,
				},
			}
			params := []byte{1, 2, 3}
			server, creates := newRESTRecoveryTestServer(
				t, test.infoStatus, key.PubKey(), owner, params,
			)
			defer server.Close()

			var signatures int
			signer := func(_ context.Context, account []byte,
				digest [32]byte, expiry int64,
				nonce [swaprpc.CreditAccountNonceSize]byte) (
				*schnorr.Signature, error) {

				signatures++
				authDigest := swaprpc.CreditAccountAuthDigest(
					account, digest, expiry, nonce[:],
				)

				return schnorr.Sign(key, authDigest[:])
			}
			conn := NewAuthenticatedRESTSwapServerConn(
				server.URL, signer,
			)
			_, err = conn.RequestChannelID(
				t.Context(), key.PubKey(), lntypes.Hash{1},
				42_000, 3600, false, owner, params,
			)
			require.Equal(
				t, test.wantErr, status.Code(err),
				"%v", err,
			)
			_, err = conn.CreateInSwapWithCredits(
				t.Context(),
				"invoice", 10, key.PubKey(),
				key.PubKey().SerializeCompressed(), 0, owner,
			)
			require.Equal(
				t, test.wantErr, status.Code(err),
				"%v", err,
			)
			if test.wantErr != codes.OK {
				require.Zero(t, creates.Load())
				require.Zero(t, signatures)

				return
			}
			require.EqualValues(t, 2, creates.Load())
			require.Equal(t, 2, signatures)
			if test.infoStatus != http.StatusOK {
				client, _, _, _ := newRefundRecoveryFixture(t)
				client.server = conn
				result, err := client.RecoverSwapserverVHTLCs(
					t.Context(),
				)
				require.NoError(t, err)
				require.Zero(t, result.RecoveredVHTLCs)
			}
		})
	}
}

// newRESTRecoveryTestServer emulates the optional capability and creation
// routes, checking the authorization the gateway would pass to the service.
func newRESTRecoveryTestServer(t *testing.T, infoStatus int,
	key *btcec.PublicKey, owner *swaprpc.SwapOwnerProof,
	params []byte) (*httptest.Server, *atomic.Int32) {

	t.Helper()
	var creates atomic.Int32
	legacy := infoStatus != http.StatusOK
	pubkey := key.SerializeCompressed()
	cfg := &swaprpc.VHTLCConfig{
		RefundLocktime:                       1000,
		UnilateralClaimDelay:                 1,
		UnilateralRefundDelay:                1,
		UnilateralRefundWithoutReceiverDelay: 1,
		SwapserverPubkey:                     pubkey,
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/swap/recovery-info" {
			if legacy {
				w.WriteHeader(infoStatus)

				return
			}
			_, err := w.Write([]byte("{}"))
			require.NoError(t, err)

			return
		}
		creates.Add(1)
		var req, resp proto.Message
		switch r.URL.Path {
		case "/v1/swap/request-channel-id":
			req = &swaprpc.RequestChannelIdRequest{}
			resp = &swaprpc.RequestChannelIdResponse{
				RouteHintPaths: []*swaprpc.RouteHintPath{{
					Hops: []*swaprpc.RouteHint{{
						NodeId:          pubkey,
						ChannelId:       42,
						CltvExpiryDelta: 40,
					}},
				}},
			}

		case "/v1/swap/create-in-swap":
			req = &swaprpc.CreateInSwapRequest{}
			resp = &swaprpc.CreateInSwapResponse{
				PaymentHash:  make([]byte, lntypes.HashSize),
				AmountSat:    42_000,
				ServerPubkey: pubkey,
				VhtlcConfig:  cfg,
				Expiry: timestamppb.New(
					time.Now().Add(time.Hour),
				),
			}

		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)

			return
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, protojson.Unmarshal(body, req))
		assertRESTRecoveryAuthorization(
			t, req, key, owner, params, legacy,
		)
		body, err = protojson.Marshal(resp)
		require.NoError(t, err)
		_, err = w.Write(body)
		require.NoError(t, err)
	}

	return httptest.NewServer(http.HandlerFunc(handler)), &creates
}

// assertRESTRecoveryAuthorization models an old gateway dropping recovery
// fields before checking the account signature, or an upgraded one keeping
// them.
func assertRESTRecoveryAuthorization(t *testing.T, req proto.Message,
	key *btcec.PublicKey, owner *swaprpc.SwapOwnerProof, params []byte,
	legacy bool) {

	t.Helper()
	var gotOwner *swaprpc.SwapOwnerProof
	switch typed := req.(type) {
	case *swaprpc.RequestChannelIdRequest:
		gotOwner = typed.GetOwnerProof()
		if legacy {
			require.Empty(t, typed.GetPreimageRecoveryParams())
			typed.OwnerProof = nil
			typed.PreimageRecoveryParams = nil
		} else {
			require.Equal(
				t, params, typed.GetPreimageRecoveryParams(),
			)
		}

	case *swaprpc.CreateInSwapRequest:
		gotOwner = typed.GetOwnerProof()
		if legacy {
			typed.OwnerProof = nil
		}

	default:
		t.Fatalf("unexpected request type %T", req)
	}
	if legacy {
		require.Nil(t, gotOwner)
	} else {
		require.True(t, proto.Equal(owner, gotOwner))
	}
	digest, account, err := swaprpc.CreditAccountRequestDigest(req)
	require.NoError(t, err)
	auth, err := swaprpc.CreditAccountAuthorizationForRequest(req)
	require.NoError(t, err)
	require.NotNil(t, auth)
	authDigest := swaprpc.CreditAccountAuthDigest(
		account, digest, auth.GetExpiresAtUnix(), auth.GetNonce(),
	)
	sig, err := schnorr.ParseSignature(auth.GetSignature())
	require.NoError(t, err)
	require.True(t, sig.Verify(authDigest[:], key))
}
