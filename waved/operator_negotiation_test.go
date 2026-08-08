package waved

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/types"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// TestVTXOExpiryConfigUsesLatestTerms verifies long-lived VTXO actors observe
// the latest cached free-refresh window instead of a bootstrap-only copy.
func TestVTXOExpiryConfigUsesLatestTerms(t *testing.T) {
	t.Parallel()

	server := &Server{}
	cfg := server.vtxoExpiryConfig()
	desc := &vtxo.Descriptor{
		RelativeExpiry: 24,
		Ancestry: []vtxo.Ancestry{{
			TreeDepth: 2,
		}},
	}

	require.Equal(t, int32(144), cfg.CalculateRefreshThreshold(desc))

	server.storeOperatorTerms(&types.OperatorTerms{
		FreeRefreshWindowBlocks: 120,
	})
	require.Equal(t, int32(120), cfg.CalculateRefreshThreshold(desc))

	server.storeOperatorTerms(&types.OperatorTerms{
		FreeRefreshWindowBlocks: 100,
	})
	require.Equal(t, int32(144), cfg.CalculateRefreshThreshold(desc))
}

// activeArkPolicy builds an ACTIVE policy for the given version, used to
// populate the operator's advertised policy list in fake GetInfo responses.
func activeArkPolicy(version uint32) *arkrpc.ArkVersionPolicy {
	return &arkrpc.ArkVersionPolicy{
		Version: version,
		State:   arkrpc.ArkVersionPolicy_STATE_ACTIVE,
	}
}

// stubArkServiceClient is a minimal arkrpc.ArkServiceClient that returns a
// canned GetInfo response, used to drive the bootstrap negotiation without a
// real transport.
type stubArkServiceClient struct {
	resp  *arkrpc.GetInfoResponse
	err   error
	calls int
}

// GetInfo returns the canned response.
func (s *stubArkServiceClient) GetInfo(_ context.Context,
	_ *arkrpc.GetInfoRequest, _ ...grpc.CallOption) (
	*arkrpc.GetInfoResponse, error) {

	s.calls++

	if s.err != nil {
		return nil, s.err
	}

	return s.resp, nil
}

// stubMailboxRPCClient returns a canned GetInfo response while recording the
// request sent through the generated mailbox client.
type stubMailboxRPCClient struct {
	resp   *arkrpc.GetInfoResponse
	method mailboxrpc.ServiceMethod
	req    *arkrpc.GetInfoRequest
	calls  int
	await  func(context.Context, proto.Message) error
}

// SendRPC records the request and returns a stable correlation ID.
func (s *stubMailboxRPCClient) SendRPC(_ context.Context,
	method mailboxrpc.ServiceMethod, req proto.Message,
	_ mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	typedReq, ok := req.(*arkrpc.GetInfoRequest)
	if !ok {
		return mailboxrpc.SendResult{}, fmt.Errorf("unexpected "+
			"request type: %T", req)
	}

	s.calls++
	s.method = method
	s.req = &arkrpc.GetInfoRequest{
		SupportedArkVersions: append(
			[]uint32(nil), typedReq.SupportedArkVersions...,
		),
	}

	return mailboxrpc.SendResult{CorrelationID: "get-info"}, nil
}

// AwaitRPC copies the canned response into the generated client's target.
func (s *stubMailboxRPCClient) AwaitRPC(ctx context.Context, _ string,
	resp proto.Message) error {

	if s.await != nil {
		return s.await(ctx, resp)
	}

	proto.Merge(resp, s.resp)

	return nil
}

// EstimateFee is unused by these tests.
func (s *stubArkServiceClient) EstimateFee(_ context.Context,
	_ *arkrpc.EstimateFeeRequest, _ ...grpc.CallOption) (
	*arkrpc.EstimateFeeResponse, error) {

	return &arkrpc.EstimateFeeResponse{}, nil
}

// testOperatorPubKeyBytes returns a valid compressed secp256k1 public key for
// populating a fake GetInfo response.
func testOperatorPubKeyBytes(t *testing.T) []byte {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey().SerializeCompressed()
}

// TestOperatorTermsFreeRefreshWindow verifies GetInfo policy plumbing keeps
// the late-refresh hint available to downstream wallet and RPC surfaces.
func TestOperatorTermsFreeRefreshWindow(t *testing.T) {
	t.Parallel()

	terms, err := operatorTermsFromResponse(&arkrpc.GetInfoResponse{
		Pubkey:                  testOperatorPubKeyBytes(t),
		FreeRefreshWindowBlocks: 72,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(72), terms.FreeRefreshWindowBlocks)
}

// TestNegotiateArkBootstrapZeroSelection proves the client refuses to bootstrap
// when the operator returns a zero selection (no common version, or a
// pre-versioning server). There is no legacy fallback.
func TestNegotiateArkBootstrapZeroSelection(t *testing.T) {
	t.Parallel()

	srv := &Server{
		arkClient: &stubArkServiceClient{
			resp: &arkrpc.GetInfoResponse{
				Pubkey: testOperatorPubKeyBytes(t),
			},
		},
	}

	neg, err := srv.negotiateArkBootstrap(
		t.Context(), []uint32{arkrpc.ArkProtocolVersionV1},
	)
	require.Error(t, err)
	require.Nil(t, neg)
}

// TestNegotiateArkBootstrapNoOverlap proves a no-overlap response yields an
// error so connectAndBootstrapMailbox refuses to create the runtime.
func TestNegotiateArkBootstrapNoOverlap(t *testing.T) {
	t.Parallel()

	srv := &Server{
		arkClient: &stubArkServiceClient{
			resp: &arkrpc.GetInfoResponse{
				Pubkey: testOperatorPubKeyBytes(t),
				ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
					activeArkPolicy(2),
				},
			},
		},
	}

	neg, err := srv.negotiateArkBootstrap(
		t.Context(), []uint32{arkrpc.ArkProtocolVersionV1},
	)
	require.Error(t, err)
	require.Nil(t, neg)
}

// TestFetchOperatorTermsRefreshPinsVersion proves fetchOperatorTerms is
// refresh-only: it sends the runtime-bound version as the singleton supported
// list and leaves the bound version unchanged on success.
func TestFetchOperatorTermsRefreshPinsVersion(t *testing.T) {
	t.Parallel()

	stub := &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             testOperatorPubKeyBytes(t),
			SelectedArkVersion: 1,
		},
	}
	srv := &Server{arkClient: stub, arkProtocolVersion: 1}

	terms, err := srv.fetchOperatorTerms(t.Context())
	require.NoError(t, err)
	require.NotNil(t, terms)

	// The runtime version must be unchanged by a refresh.
	require.Equal(t, uint32(1), srv.arkProtocolVersion)
}

// TestRefreshAuthenticatedOperatorTermsUsesMailbox verifies that the
// post-bootstrap refresh replaces anonymous policy with the terms resolved for
// the daemon's authenticated mailbox identity.
func TestRefreshAuthenticatedOperatorTermsUsesMailbox(t *testing.T) {
	t.Parallel()

	pubKey := testOperatorPubKeyBytes(t)
	direct := &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             pubKey,
			SelectedArkVersion: 1,
			MaxVtxoAmount:      200_000,
		},
	}
	mailbox := &stubMailboxRPCClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             pubKey,
			SelectedArkVersion: 1,
			MaxVtxoAmount:      5_000_000,
		},
	}

	srv := &Server{
		arkClient:          direct,
		ark:                arkrpc.NewArkServiceMailboxClient(mailbox),
		arkProtocolVersion: 1,
	}
	srv.storeOperatorTerms(&types.OperatorTerms{
		MaxVTXOAmount: 200_000,
	})
	srv.setServerConnected(true)

	err := srv.refreshAuthenticatedOperatorTerms(t.Context())
	require.NoError(t, err)
	require.Equal(t, 0, direct.calls)
	require.Equal(t, 1, mailbox.calls)
	require.Equal(t, "arkrpc.ArkService", mailbox.method.Service)
	require.Equal(t, "GetInfo", mailbox.method.Method)
	require.Equal(t, []uint32{1}, mailbox.req.SupportedArkVersions)
	require.NotNil(t, srv.loadOperatorTerms().PubKey)
	require.True(t, srv.hasPersonalizedLimits.Load())
	require.EqualValues(
		t, 5_000_000, srv.loadOperatorTerms().MaxVTXOAmount,
	)
}

// TestFetchCurrentOperatorPubKeyPreservesPersonalizedLimits verifies that a
// later anonymous key refresh cannot replace the policy learned through the
// authenticated startup refresh.
func TestFetchCurrentOperatorPubKeyPreservesPersonalizedLimits(t *testing.T) {
	t.Parallel()

	freshPubKey := testOperatorPubKeyBytes(t)
	direct := &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             freshPubKey,
			SelectedArkVersion: 1,
			MaxVtxoAmount:      200_000,
			MaxUserBalance:     100_000_000,
		},
	}
	srv := &Server{
		arkClient:          direct,
		arkProtocolVersion: 1,
	}
	srv.hasPersonalizedLimits.Store(true)
	srv.storeOperatorTerms(&types.OperatorTerms{
		MaxVTXOAmount:  5_000_000,
		MaxUserBalance: 150_000_000,
	})

	pubKey, err := srv.fetchCurrentOperatorPubKey(t.Context())
	require.NoError(t, err)
	require.Equal(t, freshPubKey, pubKey.SerializeCompressed())
	require.Equal(t, 1, direct.calls)
	require.EqualValues(
		t, 5_000_000, srv.loadOperatorTerms().MaxVTXOAmount,
	)
	require.EqualValues(
		t, 150_000_000, srv.loadOperatorTerms().MaxUserBalance,
	)
}

// TestFetchCurrentOperatorPubKeyUpdatesGlobalLimits verifies an ordinary
// client can learn global policy changes from a later direct GetInfo.
func TestFetchCurrentOperatorPubKeyUpdatesGlobalLimits(t *testing.T) {
	t.Parallel()

	direct := &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             testOperatorPubKeyBytes(t),
			SelectedArkVersion: 1,
			MaxVtxoAmount:      500_000,
			MaxUserBalance:     200_000_000,
		},
	}
	srv := &Server{
		arkClient:          direct,
		arkProtocolVersion: 1,
	}
	srv.storeOperatorTerms(&types.OperatorTerms{
		MaxVTXOAmount:  200_000,
		MaxUserBalance: 100_000_000,
	})

	_, err := srv.fetchCurrentOperatorPubKey(t.Context())
	require.NoError(t, err)
	require.False(t, srv.hasPersonalizedLimits.Load())
	require.EqualValues(
		t, 500_000, srv.loadOperatorTerms().MaxVTXOAmount,
	)
	require.EqualValues(
		t, 200_000_000, srv.loadOperatorTerms().MaxUserBalance,
	)
}

// TestRefreshAuthenticatedOperatorTermsHonorsDeadline verifies a stalled
// mailbox response returns when the caller's startup deadline expires.
func TestRefreshAuthenticatedOperatorTermsHonorsDeadline(t *testing.T) {
	t.Parallel()

	mailbox := &stubMailboxRPCClient{
		await: func(ctx context.Context, _ proto.Message) error {
			<-ctx.Done()

			return ctx.Err()
		},
	}
	srv := &Server{
		ark: arkrpc.NewArkServiceMailboxClient(mailbox),
	}
	srv.setServerConnected(true)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := srv.refreshAuthenticatedOperatorTerms(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestFetchOperatorTermsRefreshRejectsRenegotiation proves a refresh that
// selects a different version fails with a terminal ARK_VERSION_MISMATCH and
// does not change the bound version.
func TestFetchOperatorTermsRefreshRejectsRenegotiation(t *testing.T) {
	t.Parallel()

	// The operator now prefers v2 and advertises a disabled policy for the
	// bound v1.
	stub := &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             testOperatorPubKeyBytes(t),
			SelectedArkVersion: 2,
			ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
				{
					Version: 1,
					State: arkrpc.
						ArkVersionPolicy_STATE_DISABLED,
				},
			},
		},
	}
	srv := &Server{arkClient: stub, arkProtocolVersion: 1}

	_, err := srv.fetchOperatorTerms(t.Context())
	require.Error(t, err)
	require.True(t, mailboxconn.IsPermanentVersionError(err))

	var statusErr *mailboxconn.StatusError
	require.ErrorAs(t, err, &statusErr)
	require.Equal(
		t, mailboxconn.StatusArkVersionMismatch, statusErr.Code(),
	)

	// The runtime version must be unchanged by a failed refresh.
	require.Equal(t, uint32(1), srv.arkProtocolVersion)
}

// TestNegotiateArkBootstrapSelectedButDisabled proves the client refuses to
// bootstrap a runtime when the operator selects the client's supported version
// but simultaneously advertises that version as DISABLED. The refusal is a
// permanent UPGRADE_REQUIRED status error.
func TestNegotiateArkBootstrapSelectedButDisabled(t *testing.T) {
	t.Parallel()

	stub := &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             testOperatorPubKeyBytes(t),
			SelectedArkVersion: 1,
			ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
				{
					Version: 1,
					State: arkrpc.
						ArkVersionPolicy_STATE_DISABLED,
				},
			},
		},
	}
	srv := &Server{arkClient: stub}

	neg, err := srv.negotiateArkBootstrap(t.Context(), []uint32{1})
	require.Error(t, err)
	require.Nil(t, neg)

	require.True(t, mailboxconn.IsPermanentVersionError(err))

	var statusErr *mailboxconn.StatusError
	require.ErrorAs(t, err, &statusErr)
	require.Equal(
		t, mailboxconn.StatusUpgradeRequired, statusErr.Code(),
	)
}

// TestFetchOperatorTermsRefreshSelectedButDisabledMarksIncompatible proves the
// refresh path drives an existing runtime to INCOMPATIBLE when the operator
// re-selects the bound version but advertises it as DISABLED.
// fetchOperatorTerms returns the permanent UPGRADE_REQUIRED error and calls
// MarkIncompatible, whose OnIncompatible callback clears server_connected.
func TestFetchOperatorTermsRefreshSelectedButDisabledMarksIncompatible(
	t *testing.T) {

	t.Parallel()

	s := newCompatTestServer(t, okPullEdge{})

	// Model a healthy, connected client bound to v1 before the refresh.
	s.serverConnected.Store(true)
	s.arkProtocolVersion = 1
	s.arkClient = &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             testOperatorPubKeyBytes(t),
			SelectedArkVersion: 1,
			ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
				{
					Version: 1,
					State: arkrpc.
						ArkVersionPolicy_STATE_DISABLED,
				},
			},
		},
	}

	_, err := s.fetchOperatorTerms(t.Context())
	require.Error(t, err)
	require.True(t, mailboxconn.IsPermanentVersionError(err))

	var statusErr *mailboxconn.StatusError
	require.ErrorAs(t, err, &statusErr)
	require.Equal(
		t, mailboxconn.StatusUpgradeRequired, statusErr.Code(),
	)

	// The refresh transitioned the runtime to INCOMPATIBLE, firing the
	// OnIncompatible callback that clears server_connected.
	require.False(t, s.isServerConnected())
}
