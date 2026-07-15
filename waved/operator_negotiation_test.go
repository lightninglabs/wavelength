package waved

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/types"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
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
	resp *arkrpc.GetInfoResponse
	err  error
}

// GetInfo returns the canned response.
func (s *stubArkServiceClient) GetInfo(_ context.Context,
	_ *arkrpc.GetInfoRequest, _ ...grpc.CallOption) (
	*arkrpc.GetInfoResponse, error) {

	if s.err != nil {
		return nil, s.err
	}

	return s.resp, nil
}

// EstimateFee is unused by these tests.
func (s *stubArkServiceClient) EstimateFee(_ context.Context,
	_ *arkrpc.EstimateFeeRequest, _ ...grpc.CallOption) (
	*arkrpc.EstimateFeeResponse, error) {

	return &arkrpc.EstimateFeeResponse{}, nil
}

// RegisterTaprootAssetVTXO is unused by these negotiation tests.
func (s *stubArkServiceClient) RegisterTaprootAssetVTXO(_ context.Context,
	_ *arkrpc.RegisterTaprootAssetVTXORequest, _ ...grpc.CallOption) (
	*arkrpc.RegisterTaprootAssetVTXOResponse, error) {

	return &arkrpc.RegisterTaprootAssetVTXOResponse{}, nil
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
