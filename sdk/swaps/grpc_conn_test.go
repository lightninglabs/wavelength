package swaps

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testSwapServiceClient struct {
	swaprpc.SwapServiceClient

	authorizeErr     error
	ackErr           error
	signForfeitResp  *swaprpc.SignInSwapForfeitResponse
	signForfeitErr   error
	submitForfeitErr error
	lastAckReq       *swaprpc.AcknowledgeOutSwapHtlcRequest
	lastSignReq      *swaprpc.SignInSwapForfeitRequest
	lastSubmitSigReq *swaprpc.SubmitOutSwapForfeitSignatureRequest
}

func (c *testSwapServiceClient) AuthorizeInSwapRefund(context.Context,
	*swaprpc.AuthorizeInSwapRefundRequest, ...grpc.CallOption) (
	*swaprpc.AuthorizeInSwapRefundResponse, error) {

	return nil, c.authorizeErr
}

func (c *testSwapServiceClient) AcknowledgeOutSwapHtlc(_ context.Context,
	req *swaprpc.AcknowledgeOutSwapHtlcRequest, _ ...grpc.CallOption) (
	*swaprpc.AcknowledgeOutSwapHtlcResponse, error) {

	c.lastAckReq = req

	return nil, c.ackErr
}

func (c *testSwapServiceClient) SignInSwapForfeit(_ context.Context,
	req *swaprpc.SignInSwapForfeitRequest, _ ...grpc.CallOption) (
	*swaprpc.SignInSwapForfeitResponse, error) {

	c.lastSignReq = req
	if c.signForfeitErr != nil {
		return nil, c.signForfeitErr
	}

	return c.signForfeitResp, nil
}

func (c *testSwapServiceClient) SubmitOutSwapForfeitSignature(_ context.Context,
	req *swaprpc.SubmitOutSwapForfeitSignatureRequest,
	_ ...grpc.CallOption) (*swaprpc.SubmitOutSwapForfeitSignatureResponse,
	error) {

	c.lastSubmitSigReq = req
	if c.submitForfeitErr != nil {
		return nil, c.submitForfeitErr
	}

	return &swaprpc.SubmitOutSwapForfeitSignatureResponse{}, nil
}

func testForfeitSignaturePayload() *ForfeitSignaturePayload {
	return &ForfeitSignaturePayload{
		RequestID: []byte("request-id"),
		PaymentHash: lntypes.Hash{
			1,
			2,
			3,
		},
		VHTLCOutpoint:         "vhtlc:0",
		VHTLCAmountSat:        42_000,
		VHTLCPkScript:         []byte("vhtlc-pk-script"),
		VHTLCPolicyTemplate:   []byte("policy"),
		ForfeitSpendPath:      []byte("forfeit-path"),
		UnsignedForfeitTx:     []byte("unsigned-tx"),
		ConnectorOutpoint:     "connector:0",
		ConnectorAmountSat:    330,
		ConnectorPkScript:     []byte("connector-pk-script"),
		ServerForfeitPkScript: []byte("server-forfeit-pk-script"),
	}
}

// TestRouteHintPathsFromProto verifies alternative route-hint paths convert
// to the SDK shape with path and hop order preserved, and that empty or
// invalid paths are rejected with the offending path index.
func TestRouteHintPathsFromProto(t *testing.T) {
	t.Parallel()

	backendOnePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	backendTwoPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	backendOneID := backendOnePriv.PubKey().SerializeCompressed()
	backendTwoID := backendTwoPriv.PubKey().SerializeCompressed()

	hintToChannel := func(nodeID []byte,
		channelID uint64) *swaprpc.RouteHint {

		return &swaprpc.RouteHint{
			NodeId:             nodeID,
			ChannelId:          channelID,
			FeeBaseMsat:        1,
			FeeProportionalPpm: 2,
			CltvExpiryDelta:    40,
		}
	}

	// Two alternative paths convert in order, the first carrying two
	// ordered hops.
	hintPaths, err := routeHintPathsFromProto([]*swaprpc.RouteHintPath{{
		Hops: []*swaprpc.RouteHint{
			hintToChannel(backendOneID, 21),
			hintToChannel(backendOneID, 42),
		},
	}, {
		Hops: []*swaprpc.RouteHint{
			hintToChannel(backendTwoID, 42),
		},
	}})
	require.NoError(t, err)
	require.Len(t, hintPaths, 2)
	require.Len(t, hintPaths[0], 2)
	require.Len(t, hintPaths[1], 1)
	require.Equal(t, uint64(21), hintPaths[0][0].ChannelID)
	require.Equal(t, uint64(42), hintPaths[0][1].ChannelID)
	require.Equal(t, backendTwoID, hintPaths[1][0].NodeID)

	// A path without hops is rejected with its index.
	_, err = routeHintPathsFromProto([]*swaprpc.RouteHintPath{{
		Hops: []*swaprpc.RouteHint{
			hintToChannel(backendOneID, 42),
		},
	}, {}})
	require.ErrorContains(t, err, "route hint path 1 is empty")

	// An invalid hint inside a path wraps the path and hop index. A zero
	// channel ID never routes, so it must be rejected at conversion time.
	_, err = routeHintPathsFromProto([]*swaprpc.RouteHintPath{{
		Hops: []*swaprpc.RouteHint{
			hintToChannel(backendOneID, 0),
		},
	}})
	require.ErrorContains(t, err, "route hint path 0")
	require.ErrorContains(t, err, "hop 0")

	// No paths at all convert to an empty list; the caller decides
	// whether that is fatal.
	hintPaths, err = routeHintPathsFromProto(nil)
	require.NoError(t, err)
	require.Empty(t, hintPaths)
}

// TestAuthorizeInSwapRefundPreservesStatusCode verifies the pay session can
// still distinguish retryable "not ready" authorization responses.
func TestAuthorizeInSwapRefundPreservesStatusCode(t *testing.T) {
	t.Parallel()

	conn := &GRPCSwapServerConn{
		client: &testSwapServiceClient{
			authorizeErr: status.Error(
				codes.FailedPrecondition, "refund unavailable",
			),
		},
	}

	_, err := conn.AuthorizeInSwapRefund(
		context.Background(), lntypes.Hash{}, "txid:0", 1, nil, nil,
		nil,
	)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

// TestAcknowledgeOutSwapHTLCPreservesStatusCode verifies the receive session
// can distinguish retryable or terminal server ACK failures by their original
// gRPC status code.
func TestAcknowledgeOutSwapHTLCPreservesStatusCode(t *testing.T) {
	t.Parallel()

	client := &testSwapServiceClient{
		ackErr: status.Error(codes.FailedPrecondition, "not ready"),
	}
	conn := &GRPCSwapServerConn{
		client: client,
	}

	pubkey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hash := lntypes.Hash{1, 2, 3}
	err = conn.AcknowledgeOutSwapHTLC(
		context.Background(), hash, pubkey.PubKey(),
	)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, hash[:], client.lastAckReq.GetPaymentHash())
	require.Equal(
		t, pubkey.PubKey().SerializeCompressed(),
		client.lastAckReq.GetClientVhtlcPubkey(),
	)
}

// TestAcknowledgeOutSwapHTLCRejectsMissingPubkey verifies malformed local
// state is rejected before an invalid request can reach the swap server.
func TestAcknowledgeOutSwapHTLCRejectsMissingPubkey(t *testing.T) {
	t.Parallel()

	client := &testSwapServiceClient{}
	conn := &GRPCSwapServerConn{
		client: client,
	}

	err := conn.AcknowledgeOutSwapHTLC(
		context.Background(), lntypes.Hash{}, nil,
	)
	require.ErrorContains(t, err, "vHTLC pubkey must be provided")
	require.Nil(t, client.lastAckReq)
}

// TestSignInSwapForfeitMapsPayloadAndSignature verifies the in-swap refresh
// signing RPC preserves every field in the exact forfeit transcript and maps
// the participant signature back into the SDK shape.
func TestSignInSwapForfeitMapsPayloadAndSignature(t *testing.T) {
	t.Parallel()

	client := &testSwapServiceClient{
		signForfeitResp: &swaprpc.SignInSwapForfeitResponse{
			Signature: &swaprpc.ForfeitParticipantSignature{
				Pubkey:    []byte("server-key"),
				Signature: []byte("server-sig"),
			},
		},
	}
	conn := &GRPCSwapServerConn{
		client: client,
	}

	payload := testForfeitSignaturePayload()
	sig, err := conn.SignInSwapForfeit(t.Context(), payload)
	require.NoError(t, err)
	require.Equal(t, []byte("server-key"), sig.PubKey)
	require.Equal(t, []byte("server-sig"), sig.Signature)

	require.NotNil(t, client.lastSignReq)
	got := client.lastSignReq.GetPayload()
	require.Equal(t, payload.RequestID, got.GetRequestId())
	require.Equal(t, payload.PaymentHash[:], got.GetPaymentHash())
	require.Equal(t, payload.VHTLCOutpoint, got.GetVhtlcOutpoint())
	require.EqualValues(t, payload.VHTLCAmountSat, got.GetVhtlcAmountSat())
	require.Equal(t, payload.VHTLCPkScript, got.GetVhtlcPkScript())
	require.Equal(
		t, payload.VHTLCPolicyTemplate, got.GetVhtlcPolicyTemplate(),
	)
	require.Equal(t, payload.ForfeitSpendPath, got.GetForfeitSpendPath())
	require.Equal(t, payload.UnsignedForfeitTx, got.GetUnsignedForfeitTx())
	require.Equal(t, payload.ConnectorOutpoint, got.GetConnectorOutpoint())
	require.EqualValues(
		t, payload.ConnectorAmountSat, got.GetConnectorAmountSat(),
	)
	require.Equal(t, payload.ConnectorPkScript, got.GetConnectorPkScript())
	require.Equal(
		t, payload.ServerForfeitPkScript,
		got.GetServerForfeitPkScript(),
	)
}

// TestSubmitOutSwapForfeitSignatureMapsPayloadAndSignature verifies the
// receive-side refresh signature submission keeps the original forfeit
// transcript and participant signature intact.
func TestSubmitOutSwapForfeitSignatureMapsPayloadAndSignature(t *testing.T) {
	t.Parallel()

	client := &testSwapServiceClient{}
	conn := &GRPCSwapServerConn{
		client: client,
	}

	payload := testForfeitSignaturePayload()
	sig := &ForfeitParticipantSignature{
		PubKey:    []byte("receiver-key"),
		Signature: []byte("receiver-sig"),
	}

	err := conn.SubmitOutSwapForfeitSignature(t.Context(), payload, sig)
	require.NoError(t, err)

	require.NotNil(t, client.lastSubmitSigReq)
	gotPayload := client.lastSubmitSigReq.GetPayload()
	require.Equal(t, payload.RequestID, gotPayload.GetRequestId())
	require.Equal(t, payload.PaymentHash[:], gotPayload.GetPaymentHash())
	require.Equal(t, payload.VHTLCOutpoint, gotPayload.GetVhtlcOutpoint())

	gotSig := client.lastSubmitSigReq.GetSignature()
	require.Equal(t, sig.PubKey, gotSig.GetPubkey())
	require.Equal(t, sig.Signature, gotSig.GetSignature())
}

// TestSignInSwapForfeitPreservesStatusCode verifies retry decisions can inspect
// server-side forfeit signing errors.
func TestSignInSwapForfeitPreservesStatusCode(t *testing.T) {
	t.Parallel()

	conn := &GRPCSwapServerConn{
		client: &testSwapServiceClient{
			signForfeitErr: status.Error(
				codes.FailedPrecondition, "not ready",
			),
		},
	}

	_, err := conn.SignInSwapForfeit(
		t.Context(), testForfeitSignaturePayload(),
	)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}
