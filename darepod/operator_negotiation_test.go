package darepod

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

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

// testOperatorPubKeyBytes returns a valid compressed secp256k1 public key for
// populating a fake GetInfo response.
func testOperatorPubKeyBytes(t *testing.T) []byte {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey().SerializeCompressed()
}

// TestResolveArkVersionSelection covers the pure selection decision: normal
// preference selection, an operator selecting an unsupported version, a zero
// selection (no overlap or pre-versioning server) which is always fatal, and
// a no-overlap response with a non-empty list.
func TestResolveArkVersionSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		resp         *arkrpc.GetInfoResponse
		client       []uint32
		wantSelected uint32
		wantErr      bool
	}{
		{
			name: "normal v1 selection",
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 1,
			},
			client: []uint32{
				1,
			},
			wantSelected: 1,
		},
		{
			name: "operator selects v2 client supports it",
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 2,
			},
			client: []uint32{
				1,
				2,
			},
			wantSelected: 2,
		},
		{
			name: "operator selects unsupported version",
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 2,
			},
			client: []uint32{
				1,
			},
			wantErr: true,
		},
		{
			name: "zero selection is fatal",
			resp: &arkrpc.GetInfoResponse{},
			client: []uint32{
				1,
			},
			wantErr: true,
		},
		{
			name: "no overlap is fatal",
			resp: &arkrpc.GetInfoResponse{
				SupportedArkVersions: []uint32{
					2,
					3,
				},
			},
			client: []uint32{
				1,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			selected, err := resolveArkVersionSelection(
				tc.resp, tc.client,
			)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantSelected, selected)
		})
	}
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
				SupportedArkVersions: []uint32{
					2,
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

// TestNegotiateArkBootstrapDeprecating proves a deprecating selection returns
// the advertised deadline and upgrade URL on the cached policy.
func TestNegotiateArkBootstrapDeprecating(t *testing.T) {
	t.Parallel()

	policy := &arkrpc.ArkVersionPolicy{
		Version:           1,
		State:             arkrpc.ArkVersionPolicy_STATE_DEPRECATING,
		DisableAfterUnixS: 1893456000,
		UpgradeUrl:        "https://up.example",
	}
	srv := &Server{
		arkClient: &stubArkServiceClient{
			resp: &arkrpc.GetInfoResponse{
				Pubkey:             testOperatorPubKeyBytes(t),
				SelectedArkVersion: 1,
				ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
					policy,
				},
			},
		},
	}

	neg, err := srv.negotiateArkBootstrap(
		t.Context(), []uint32{1},
	)
	require.NoError(t, err)
	require.NotNil(t, neg.selectedPolicy)
	require.Equal(
		t, arkrpc.ArkVersionPolicy_STATE_DEPRECATING,
		neg.selectedPolicy.State,
	)
	require.Equal(
		t, int64(1893456000), neg.selectedPolicy.DisableAfterUnixS,
	)
	require.Equal(
		t, "https://up.example", neg.selectedPolicy.UpgradeUrl,
	)
}

// TestValidateRefreshSelection proves a refresh cannot renegotiate: it accepts
// only a matching selection and treats any zero or different selection as a
// terminal ARK_VERSION_MISMATCH. There is no legacy fallback.
func TestValidateRefreshSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		boundArk uint32
		resp     *arkrpc.GetInfoResponse
		wantErr  bool
	}{
		{
			name:     "matching v1",
			boundArk: 1,
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 1,
			},
		},
		{
			name:     "matching v2",
			boundArk: 2,
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 2,
			},
		},
		{
			name:     "v1 rejects zero selection",
			boundArk: 1,
			resp:     &arkrpc.GetInfoResponse{},
			wantErr:  true,
		},
		{
			name:     "v1 rejects different selection",
			boundArk: 1,
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 2,
			},
			wantErr: true,
		},
		{
			name:     "v2 rejects zero selection",
			boundArk: 2,
			resp:     &arkrpc.GetInfoResponse{},
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := &Server{arkProtocolVersion: tc.boundArk}

			// validateRefreshSelection returns a typed pointer;
			// compare the pointer directly to avoid the
			// nil-interface pitfall.
			statusErr := srv.validateRefreshSelection(tc.resp)
			if tc.wantErr {
				require.NotNil(t, statusErr)
				require.Equal(
					t, mailboxconn.StatusArkVersionMismatch,
					statusErr.Code(),
				)

				return
			}

			require.Nil(t, statusErr)
		})
	}
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
	// bound v1, carrying an upgrade URL the client must surface.
	stub := &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             testOperatorPubKeyBytes(t),
			SelectedArkVersion: 2,
			ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
				{
					Version: 1,
					State: arkrpc.
						ArkVersionPolicy_STATE_DISABLED,
					UpgradeUrl: "https://up.example",
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

	// The mismatch carries actionable upgrade guidance from the bound
	// version's advertised policy.
	require.Equal(t, "https://up.example", statusErr.UpgradeURL())

	// The runtime version must be unchanged by a failed refresh.
	require.Equal(t, uint32(1), srv.arkProtocolVersion)
}

// TestFetchOperatorTermsRefreshProcessesDeprecation proves a successful refresh
// updates the cached deprecation policy for the bound version, so a
// long-running client learns about a newly deprecating version without a
// restart.
func TestFetchOperatorTermsRefreshProcessesDeprecation(t *testing.T) {
	t.Parallel()

	policy := &arkrpc.ArkVersionPolicy{
		Version:           1,
		State:             arkrpc.ArkVersionPolicy_STATE_DEPRECATING,
		DisableAfterUnixS: 1893456000,
		UpgradeUrl:        "https://up.example",
	}
	stub := &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             testOperatorPubKeyBytes(t),
			SelectedArkVersion: 1,
			ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
				policy,
			},
		},
	}
	srv := &Server{
		arkClient:          stub,
		arkProtocolVersion: 1,
		log:                btclog.Disabled,
	}

	terms, err := srv.fetchOperatorTerms(t.Context())
	require.NoError(t, err)
	require.NotNil(t, terms)

	cached := srv.selectedArkPolicy.Load()
	require.NotNil(t, cached)
	require.Equal(
		t, arkrpc.ArkVersionPolicy_STATE_DEPRECATING, cached.State,
	)
	require.Equal(t, "https://up.example", cached.UpgradeUrl)
}

// TestNegotiateArkBootstrapSelectedButDisabled proves the client refuses to
// bootstrap a runtime when the operator selects the client's supported version
// but simultaneously advertises that version as DISABLED. The refusal is a
// permanent UPGRADE_REQUIRED status error carrying the advertised upgrade URL.
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
					UpgradeUrl: "https://up.example",
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
	require.Equal(t, "https://up.example", statusErr.UpgradeURL())
}

// TestValidateRefreshSelectionSelectedButDisabled proves a refresh that
// re-selects the bound version but advertises it as DISABLED is a terminal,
// mandatory-upgrade failure: a permanent UPGRADE_REQUIRED status error carrying
// the advertised upgrade URL, distinct from the renegotiation mismatch path.
func TestValidateRefreshSelectionSelectedButDisabled(t *testing.T) {
	t.Parallel()

	srv := &Server{arkProtocolVersion: 1}

	resp := &arkrpc.GetInfoResponse{
		SelectedArkVersion: 1,
		ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
			{
				Version: 1,
				State: arkrpc.
					ArkVersionPolicy_STATE_DISABLED,
				UpgradeUrl: "https://up.example",
			},
		},
	}

	statusErr := srv.validateRefreshSelection(resp)
	require.NotNil(t, statusErr)
	require.Equal(
		t, mailboxconn.StatusUpgradeRequired, statusErr.Code(),
	)
	require.Equal(t, "https://up.example", statusErr.UpgradeURL())
}

// TestValidateRefreshSelectionActivePolicyOk proves a matching selection with a
// non-disabled (here ACTIVE) policy for the bound version is a normal, accepted
// refresh — an advertised policy alone does not make a refresh fail.
func TestValidateRefreshSelectionActivePolicyOk(t *testing.T) {
	t.Parallel()

	srv := &Server{arkProtocolVersion: 1}

	resp := &arkrpc.GetInfoResponse{
		SelectedArkVersion: 1,
		ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
			{
				Version: 1,
				State: arkrpc.
					ArkVersionPolicy_STATE_ACTIVE,
			},
		},
	}

	require.Nil(t, srv.validateRefreshSelection(resp))
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
					UpgradeUrl: "https://up.example",
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
	require.Equal(t, "https://up.example", statusErr.UpgradeURL())

	// The refresh transitioned the runtime to INCOMPATIBLE, firing the
	// OnIncompatible callback that clears server_connected.
	require.False(t, s.isServerConnected())
}
