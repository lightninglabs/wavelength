package waved

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/batchschedule"
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

// TestVTXOExpiryConfigReservesMaxPaymentCLTV verifies the daemon carries its
// configured Lightning payment target into every VTXO actor's dynamic expiry
// policy without letting the operator's fee waiver shorten the reserve.
func TestVTXOExpiryConfigReservesMaxPaymentCLTV(t *testing.T) {
	t.Parallel()

	server := &Server{cfg: &Config{MaxPaymentCLTV: 300}}
	server.storeOperatorTerms(&types.OperatorTerms{
		FreeRefreshWindowBlocks: 144,
	})
	cfg := server.vtxoExpiryConfig()
	desc := &vtxo.Descriptor{
		RelativeExpiry: 144,
		Ancestry: []vtxo.Ancestry{{
			TreeDepth: 7,
		}},
	}

	require.Equal(t, int32(558), cfg.CalculateRefreshThreshold(desc))
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
	await func(context.Context) error
}

// GetInfo returns the canned response.
func (s *stubArkServiceClient) GetInfo(ctx context.Context,
	_ *arkrpc.GetInfoRequest, _ ...grpc.CallOption) (
	*arkrpc.GetInfoResponse, error) {

	s.calls++
	if s.await != nil {
		if err := s.await(ctx); err != nil {
			return nil, err
		}
	}

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

// TestOperatorTermsVTXOConfirmations verifies new operators can advertise a
// shallow VTXO activation depth without weakening boarding-input maturity.
func TestOperatorTermsVTXOConfirmations(t *testing.T) {
	t.Parallel()

	terms, err := operatorTermsFromResponse(&arkrpc.GetInfoResponse{
		Pubkey:            testOperatorPubKeyBytes(t),
		MinConfirmations:  6,
		VtxoConfirmations: 1,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(6), terms.MinConfirmations)
	require.Equal(t, uint32(1), terms.VTXOConfirmations)
}

// TestOperatorTermsVTXOConfirmationsLegacyFallback verifies a new client
// preserves the old coupled policy when the additive field is absent.
func TestOperatorTermsVTXOConfirmationsLegacyFallback(t *testing.T) {
	t.Parallel()

	terms, err := operatorTermsFromResponse(&arkrpc.GetInfoResponse{
		Pubkey:           testOperatorPubKeyBytes(t),
		MinConfirmations: 3,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(3), terms.VTXOConfirmations)
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

// TestOperatorVTXOFloorRefreshesEffectiveMinimum verifies credit policy reads
// the current operator terms and uses the VTXO minimum when it exceeds dust.
func TestOperatorVTXOFloorRefreshesEffectiveMinimum(t *testing.T) {
	t.Parallel()

	stub := &stubArkServiceClient{
		resp: &arkrpc.GetInfoResponse{
			Pubkey:             testOperatorPubKeyBytes(t),
			SelectedArkVersion: 1,
			DustLimit:          546,
			MinVtxoAmountSat:   1_000,
		},
	}
	rpc := &RPCServer{server: &Server{
		arkClient:          stub,
		arkProtocolVersion: 1,
	}}

	floor, err := rpc.OperatorVTXOFloor(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(1_000), floor)

	stub.resp.MinVtxoAmountSat = 1_200
	floor, err = rpc.OperatorVTXOFloor(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(1_200), floor)
	require.Equal(t, 2, stub.calls)
}

// TestOperatorVTXOFloorFailsClosed verifies credit policy cannot fall back to
// a baked-in threshold while operator terms are unavailable.
func TestOperatorVTXOFloorFailsClosed(t *testing.T) {
	t.Parallel()

	rpc := &RPCServer{server: &Server{
		arkClient: &stubArkServiceClient{
			err: fmt.Errorf("operator unavailable"),
		},
		arkProtocolVersion: 1,
	}}

	_, err := rpc.OperatorVTXOFloor(t.Context())
	require.ErrorContains(t, err, "operator unavailable")
}

// TestOperatorVTXOFloorRejectsZero verifies an otherwise successful terms
// refresh cannot authorize a zero-satoshi credit materialization threshold.
func TestOperatorVTXOFloorRejectsZero(t *testing.T) {
	t.Parallel()

	rpc := &RPCServer{server: &Server{
		arkClient: &stubArkServiceClient{
			resp: &arkrpc.GetInfoResponse{
				Pubkey:             testOperatorPubKeyBytes(t),
				SelectedArkVersion: 1,
			},
		},
		arkProtocolVersion: 1,
	}}

	_, err := rpc.OperatorVTXOFloor(t.Context())
	require.ErrorContains(t, err, "operator VTXO floor is unavailable")
}

// TestOperatorVTXOFloorBoundsRefresh verifies a daemon-lifetime caller cannot
// leave the policy turn parked forever on an operator that never responds.
func TestOperatorVTXOFloorBoundsRefresh(t *testing.T) {
	t.Parallel()

	var observedDeadline time.Time
	rpc := &RPCServer{server: &Server{
		arkClient: &stubArkServiceClient{
			await: func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				observedDeadline = deadline

				return context.DeadlineExceeded
			},
		},
		arkProtocolVersion: 1,
	}}

	started := time.Now()
	_, err := rpc.OperatorVTXOFloor(t.Context())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.WithinDuration(
		t, started.Add(operatorTermsRefreshTimeout), observedDeadline,
		time.Second,
	)
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

// testScheduledGetInfoResponse builds a GetInfo response from a scheduled
// operator that publishes a single slot opening ten minutes after now and
// closing two minutes later, stamped with the given server clock reading. A
// non-positive serverTimeUnix models an operator that reports no clock.
func testScheduledGetInfoResponse(t *testing.T, now time.Time,
	serverTimeUnix int64) *arkrpc.GetInfoResponse {

	t.Helper()

	return &arkrpc.GetInfoResponse{
		Pubkey:             testOperatorPubKeyBytes(t),
		SelectedArkVersion: 1,
		MaxVtxoAmount:      200_000,
		BatchSchedule: &arkrpc.BatchSchedule{
			Version: 1,
			ScheduleId: append(
				[]byte{2}, make([]byte, 31)...,
			),
			ServerTimeUnix: serverTimeUnix,
			Slots: []*arkrpc.BatchSlot{{
				RegistrationOpensUnix: now.Unix() + 600,
				CutoffUnix:            now.Unix() + 720,
			}},
		},
	}
}

// TestRoundOperatorTermsRefreshesPublishedHorizon pins that each scheduled
// registration attempt fetches fresh discovery. The published slot list is
// finite and rolls forward, so the cached copy may already be exhausted by
// the time a new attempt starts. The fresh terms must keep the client's
// personalized limits, which the generic GetInfo response does not carry,
// and must stay private to the attempt so they never overwrite the shared
// cache. An event-driven operator, whose terms never roll forward, must not
// pay for an extra discovery call.
func TestRoundOperatorTermsRefreshesPublishedHorizon(t *testing.T) {
	t.Parallel()

	// Cache a published list whose only slot closes at now, so it has
	// nothing left to offer an attempt that starts at now.
	now := time.Unix(1800000000, 0).UTC()
	old, err := batchschedule.NewPublished(
		batchschedule.ID{1}, []batchschedule.Slot{{
			Opens:  now.Add(-time.Minute),
			Cutoff: now,
		}},
	)
	require.NoError(t, err)

	// The operator now publishes a later slot with generic limits that
	// differ from the personalized ones in the cache.
	direct := &stubArkServiceClient{
		resp: testScheduledGetInfoResponse(t, now, 0),
	}
	srv := &Server{
		arkClient:          direct,
		arkProtocolVersion: 1,
	}
	cached := &types.OperatorTerms{
		BatchSchedule:  old,
		MaxVTXOAmount:  5_000_000,
		MaxUserBalance: 150_000_000,
	}
	srv.storeOperatorTerms(cached)
	srv.hasPersonalizedLimits.Store(true)

	// A scheduled attempt makes exactly one discovery call and keeps the
	// personalized limits rather than the response's generic ones.
	fresh, err := srv.roundOperatorTerms(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, direct.calls)
	require.EqualValues(t, 5_000_000, fresh.MaxVTXOAmount)
	require.EqualValues(t, 150_000_000, fresh.MaxUserBalance)

	// The fresh list offers the newly published slot, and the shared
	// cache still holds the original snapshot.
	slot, err := fresh.BatchSchedule.Next(now)
	require.NoError(t, err)
	require.True(t, slot.Cutoff.Equal(now.Add(720*time.Second)))
	require.Same(t, cached, srv.loadOperatorTerms())

	// Event-driven operators get their cached terms back unchanged,
	// without another discovery call.
	cached = &types.OperatorTerms{}
	srv.storeOperatorTerms(cached)

	fresh, err = srv.roundOperatorTerms(t.Context())
	require.NoError(t, err)
	require.Same(t, cached, fresh)
	require.Equal(t, 1, direct.calls)
}

// TestRoundOperatorTermsEstimatesClockOffset pins that a scheduled attempt
// attaches an estimate of the operator's clock offset to its private copy of
// the published list. A client aims its join at a window measured on the
// operator's clock, so a skewed local clock would otherwise miss short
// windows entirely. An operator that reports no clock reading must yield no
// correction, and the shared cache must never receive the estimate.
func TestRoundOperatorTermsEstimatesClockOffset(t *testing.T) {
	t.Parallel()

	now := time.Unix(1800000000, 0).UTC()

	// The operator skew is two hours, far enough to also take the
	// diagnostic log path. The estimate may differ from the true offset
	// by the half-second truncation plus half of the stub's negligible
	// round trip, so a one-second tolerance is ample.
	testCases := []struct {
		name         string
		reportsClock bool
		skew         time.Duration
		minOffset    time.Duration
		maxOffset    time.Duration
	}{
		{
			name:         "operator clock ahead",
			reportsClock: true,
			skew:         2 * time.Hour,
			minOffset:    2*time.Hour - time.Second,
			maxOffset:    2*time.Hour + time.Second,
		},
		{
			name:         "operator clock behind",
			reportsClock: true,
			skew:         -2 * time.Hour,
			minOffset:    -2*time.Hour - time.Second,
			maxOffset:    -2*time.Hour + time.Second,
		},
		{
			name:         "operator clock unreported",
			reportsClock: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Cache terms from an earlier discovery that carried
			// no offset, so any offset on the result must come
			// from this attempt's round trip.
			cachedSchedule, err := arkrpc.ParseBatchSchedule(
				testScheduledGetInfoResponse(
					t, now, 0,
				).BatchSchedule,
			)
			require.NoError(t, err)

			// The stub stamps its skewed clock while serving the
			// call, just as the operator stamps the response
			// between the client's send and receive.
			stub := &stubArkServiceClient{
				resp: testScheduledGetInfoResponse(t, now, 0),
			}
			if tc.reportsClock {
				stub.await = func(context.Context) error {
					serverTime := time.Now().Add(tc.skew)
					stub.resp.BatchSchedule.ServerTimeUnix =
						serverTime.Unix()

					return nil
				}
			}

			srv := &Server{
				arkClient:          stub,
				arkProtocolVersion: 1,
				log:                btclog.Disabled,
			}
			cached := &types.OperatorTerms{
				BatchSchedule: cachedSchedule,
			}
			srv.storeOperatorTerms(cached)

			fresh, err := srv.roundOperatorTerms(t.Context())
			require.NoError(t, err)

			// The attempt's copy carries an estimate within the
			// expected error of the true offset.
			offset := fresh.BatchSchedule.ClockOffset()
			require.GreaterOrEqual(t, offset, tc.minOffset)
			require.LessOrEqual(t, offset, tc.maxOffset)

			// The shared cache keeps its original schedule with no
			// offset attached.
			require.Same(t, cached, srv.loadOperatorTerms())
			require.Zero(t, cachedSchedule.ClockOffset())
		})
	}
}
