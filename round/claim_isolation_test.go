package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestRegisterVTXOClaimsUsesDedicatedRounds locks the actor admission boundary:
// claims never merge with ordinary intents, claims targeting different round
// IDs never merge with each other, and the generic registration path cannot
// smuggle claims into an existing assembly.
func TestRegisterVTXOClaimsUsesDedicatedRounds(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	targetA := testRoundID("claim-isolation-a").String()
	targetB := testRoundID("claim-isolation-b").String()
	claimA := claimIsolationIntent(t, h, 2)
	claimB := claimIsolationIntent(t, h, 3)
	require.True(
		t, h.receive(&RegisterVTXOClaimsRequest{
			Claims:  []VTXOClaimIntent{claimA},
			RoundID: targetA,
		}).IsOk(),
	)
	require.True(
		t, h.receive(&RegisterVTXOClaimsRequest{
			Claims:  []VTXOClaimIntent{claimB},
			RoundID: targetB,
		}).IsOk(),
	)

	// An ordinary assembly is rejected while either claim remains pre-seal.
	// This avoids creating two registrations under the same operator
	// ClientID.
	expectOrdinaryIsolationVTXO(h, 4)
	require.True(
		t, h.receive(ordinaryIsolationRequest(4, false)).IsErr(),
	)
	require.Len(t, h.actor.rounds, 2)

	claimTargets := make(map[string]struct{})
	for _, roundFSM := range h.actor.rounds {
		state, err := roundFSM.FSM.CurrentState()
		require.NoError(t, err)
		assembly, ok := state.(*PendingRoundAssembly)
		require.True(t, ok, "unexpected assembly state %T", state)

		require.Len(t, assembly.Claims, 1)
		require.Empty(t, assembly.Boarding)
		require.Empty(t, assembly.VTXOs)
		require.Empty(t, assembly.Forfeits)
		require.Empty(t, assembly.Leaves)
		claimTargets[roundFSM.RequestedRoundID] = struct{}{}
	}
	require.Equal(t, map[string]struct{}{
		targetA: {},
		targetB: {},
	}, claimTargets)

	// Claims are accepted only through the narrow request, preventing a
	// future generic caller from bypassing the dedicated-round selection.
	result := h.receive(&RegisterIntentRequest{
		Package: &IntentPackage{Intents: Intents{
			Claims: []VTXOClaimIntent{
				claimIsolationIntent(t, h, 5),
			},
		}},
	})
	require.True(t, result.IsErr())
	require.Len(t, h.actor.rounds, 2)
}

// TestPreSealRegistrationGateSendsOnlyOneJoin proves cross-kind or same-target
// registrations cannot both reach the operator before the first round seals.
func TestPreSealRegistrationGateSendsOnlyOneJoin(t *testing.T) {
	t.Parallel()

	t.Run("claim blocks ordinary and duplicate claim", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()
		require.NoError(t, h.start())
		target := testRoundID("pre-seal-claim-first").String()
		expectOrdinaryIsolationVTXO(h, 12)

		result := h.receive(&RegisterVTXOClaimsRequest{
			Claims: []VTXOClaimIntent{
				claimIsolationIntent(t, h, 10),
			},
			RoundID:             target,
			TriggerRegistration: true,
		})
		require.True(t, result.IsOk(), result.Err())
		require.Equal(t, 1, countJoinRoundRequests(h))

		result = h.receive(&RegisterVTXOClaimsRequest{
			Claims: []VTXOClaimIntent{
				claimIsolationIntent(t, h, 11),
			},
			RoundID:             target,
			TriggerRegistration: true,
		})
		require.True(t, result.IsErr())
		require.True(
			t,
			h.receive(ordinaryIsolationRequest(12, true)).IsErr(),
		)
		require.Equal(t, 1, countJoinRoundRequests(h))
	})

	t.Run("ordinary blocks claim", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()
		require.NoError(t, h.start())
		expectOrdinaryIsolationVTXO(h, 20)

		result := h.receive(ordinaryIsolationRequest(20, true))
		require.True(t, result.IsOk(), result.Err())
		require.Equal(t, 1, countJoinRoundRequests(h))

		result = h.receive(&RegisterVTXOClaimsRequest{
			Claims: []VTXOClaimIntent{
				claimIsolationIntent(t, h, 21),
			},
			RoundID: testRoundID(
				"pre-seal-ordinary-first",
			).String(),
			TriggerRegistration: true,
		})
		require.True(t, result.IsErr())
		require.Equal(t, 1, countJoinRoundRequests(h))
	})
}

// TestClaimPreSealDefersAutoRefresh verifies a claim registration cannot
// strand an automatic refresh that already moved its source VTXO into
// PendingForfeit. The refresh Tell is retained without emitting a competing
// join, then drained into an ordinary assembly immediately after the claim
// crosses the quote seal boundary.
func TestClaimPreSealDefersAutoRefresh(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	claimRoundID := testRoundID("deferred-refresh-claim")
	h.injectRoundInState(claimRoundID, &IntentSentState{})
	claimFSM := h.actor.rounds[RoundKeyStr(claimRoundID.KeyString())]
	claimFSM.RequestedRoundID = claimRoundID.String()
	claimFSM.ClaimIntents = []VTXOClaimIntent{
		claimIsolationIntent(t, h, 30),
	}

	refreshOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("deferred-auto-refresh")),
		Index: 7,
	}
	const refreshAmount = btcutil.Amount(80_000)
	h.vtxoStore.On(
		"GetVTXO", mock.Anything, refreshOutpoint,
	).Return(&ClientVTXO{
		Outpoint: refreshOutpoint,
		Amount:   refreshAmount,
	}, nil).Maybe()

	refresh := &RefreshVTXORequest{
		VTXOOutpoint: refreshOutpoint,
		Amount:       int64(refreshAmount),
		PolicyTemplate: stdTpl(
			t, h.clientPubKey, h.operatorPubKey, 144,
		),
		OwnerKey: keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
		},
		SigningKey: keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
		},
	}
	result := h.receive(refresh)
	require.True(t, result.IsOk(), result.Err())
	require.Len(t, h.actor.deferredRefreshes, 1)
	require.Len(t, h.actor.rounds, 1)
	require.Zero(t, countJoinRoundRequests(h))

	// Replace the staged claim state with the first post-seal state. The
	// next actor turn observes that the gate is clear and drains the queue.
	h.injectRoundInState(
		claimRoundID, newQuoteReceivedTestState(0, 0),
	)
	result = h.receive(&GetClientStateRequest{})
	require.True(t, result.IsOk(), result.Err())
	require.Empty(t, h.actor.deferredRefreshes)
	require.Len(t, h.actor.rounds, 2)

	ordinary := h.actor.findAssemblingRound()
	require.NotNil(t, ordinary)
	state, err := ordinary.FSM.CurrentState()
	require.NoError(t, err)
	assembly, ok := state.(*PendingRoundAssembly)
	require.True(t, ok, "unexpected deferred state %T", state)
	require.Len(t, assembly.Forfeits, 1)
	require.Equal(t, refreshOutpoint,
		*assembly.Forfeits[0].VTXOOutpoint)
}

// expectOrdinaryIsolationVTXO makes the canonical amount lookup performed
// while registering the ordinary forfeit deterministic.
func expectOrdinaryIsolationVTXO(h *actorTestHarness, index byte) {
	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte{index}),
		Index: uint32(index),
	}
	amount := btcutil.Amount(20_000 + int(index))

	h.vtxoStore.On(
		"GetVTXO", mock.Anything, outpoint,
	).Return(&ClientVTXO{
		Outpoint: outpoint,
		Amount:   amount,
	}, nil).Maybe()
}

// claimIsolationIntent returns a structurally complete claim intent for actor
// admission tests.
func claimIsolationIntent(t *testing.T, h *actorTestHarness,
	index byte) VTXOClaimIntent {

	t.Helper()
	claimTemplate := stdTpl(
		t, h.clientPubKey, h.operatorPubKey, 145,
	)

	return VTXOClaimIntent{
		Input: types.VTXOClaimInput{
			SourceOutpoint: wire.OutPoint{
				Hash:  chainhash.HashH([]byte{index}),
				Index: uint32(index),
			},
			ParticipantPubKey: h.clientPubKey,
			ReplacementSigningKey: keychain.KeyDescriptor{
				PubKey: h.clientPubKey,
			},
		},
		ExpectedOutput: types.VTXORequest{
			Amount:         btcutil.Amount(10_000 + int(index)),
			FixedAmount:    true,
			PolicyTemplate: claimTemplate,
			ClientKey:      h.clientPubKey,
			OperatorKey:    h.operatorPubKey,
			Expiry:         145,
			SigningKey: keychain.KeyDescriptor{
				PubKey: h.clientPubKey,
			},
			Origin: types.VTXOOriginClaimReissue,
		},
	}
}

// ordinaryIsolationRequest returns a balanced leave intent that requires no
// VTXO signing-key derivation in the actor harness.
func ordinaryIsolationRequest(index byte, trigger bool) *RegisterIntentRequest {
	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte{index}),
		Index: uint32(index),
	}
	amount := btcutil.Amount(20_000 + int(index))

	return &RegisterIntentRequest{
		Package: &IntentPackage{Intents: Intents{
			Forfeits: []types.ForfeitRequest{{
				VTXOOutpoint: &outpoint,
				Amount:       amount,
			}},
			Leaves: []*types.LeaveRequest{{
				Output: &wire.TxOut{
					Value: int64(amount),
					PkScript: []byte{
						0x51,
					},
				},
			}},
		}},
		TriggerRegistration: trigger,
	}
}

// countJoinRoundRequests counts only actual join messages handed to the
// server-connection actor.
func countJoinRoundRequests(h *actorTestHarness) int {
	count := 0
	for _, message := range h.serverConn.snapshotMessages() {
		send, ok := message.(*serverconn.SendClientEventRequest)
		if !ok {
			continue
		}
		if _, ok := send.Message.(*JoinRoundRequest); ok {
			count++
		}
	}

	return count
}
