package rounds

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestMaxClients verifies the MaxClients predicate fires at exactly the
// configured threshold.
func TestMaxClients(t *testing.T) {
	t.Parallel()

	pred := MaxClients(2)

	// Empty registrations — should not seal.
	regs := make(map[clientconn.ClientID]*ClientRegistration)
	require.False(t, pred(regs))

	// One client — below threshold.
	regs["client1"] = &ClientRegistration{ClientID: "client1"}
	require.False(t, pred(regs))

	// Two clients — at threshold.
	regs["client2"] = &ClientRegistration{ClientID: "client2"}
	require.True(t, pred(regs))

	// Three clients — above threshold.
	regs["client3"] = &ClientRegistration{ClientID: "client3"}
	require.True(t, pred(regs))
}

// TestMaxClientsZeroDisabled verifies that MaxClients(0) never triggers.
func TestMaxClientsZeroDisabled(t *testing.T) {
	t.Parallel()

	pred := MaxClients(0)

	regs := map[clientconn.ClientID]*ClientRegistration{
		"c1": {ClientID: "c1"},
		"c2": {ClientID: "c2"},
	}
	require.False(t, pred(regs))
}

// TestAnySealPredicateComposition verifies OR composition of multiple
// predicates.
func TestAnySealPredicateComposition(t *testing.T) {
	t.Parallel()

	never := func(
		_ map[clientconn.ClientID]*ClientRegistration) bool {

		return false
	}
	always := func(
		_ map[clientconn.ClientID]*ClientRegistration) bool {

		return true
	}

	regs := make(map[clientconn.ClientID]*ClientRegistration)

	// All-never → false.
	require.False(t, AnySealPredicate(never, never)(regs))

	// One always → true.
	require.True(t, AnySealPredicate(never, always)(regs))

	// Empty predicates → false.
	require.False(t, AnySealPredicate()(regs))
}

// TestMaxOutputAmount verifies the MaxOutputAmount predicate sums VTXO
// amounts and leave output values and fires at exactly the threshold.
func TestMaxOutputAmount(t *testing.T) {
	t.Parallel()

	pred := MaxOutputAmount(50_000)

	// Empty registrations — should not seal.
	regs := make(map[clientconn.ClientID]*ClientRegistration)
	require.False(t, pred(regs))

	// One client with a 30k VTXO — below threshold.
	regs["c1"] = &ClientRegistration{
		ClientID: "c1",
		VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{
			route.Vertex{0x01}: {Amount: 30_000},
		},
	}
	require.False(t, pred(regs))

	// Add a second client with a 10k leave — still below (40k).
	regs["c2"] = &ClientRegistration{
		ClientID: "c2",
		LeaveOutputs: []*wire.TxOut{
			{Value: 10_000},
		},
	}
	require.False(t, pred(regs))

	// Add a third client pushing past threshold (40k + 15k = 55k).
	regs["c3"] = &ClientRegistration{
		ClientID: "c3",
		VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{
			route.Vertex{0x03}: {Amount: 15_000},
		},
	}
	require.True(t, pred(regs))
}

// TestMaxOutputAmountZeroDisabled verifies that MaxOutputAmount(0)
// never triggers.
func TestMaxOutputAmountZeroDisabled(t *testing.T) {
	t.Parallel()

	pred := MaxOutputAmount(0)

	regs := map[clientconn.ClientID]*ClientRegistration{
		"c1": {
			ClientID: "c1",
			VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{
				route.Vertex{0x01}: {Amount: 999_999_999},
			},
		},
	}
	require.False(t, pred(regs))
}

// TestMaxOutputAmountMixedOutputs verifies that both VTXOs and leaves
// are summed together.
func TestMaxOutputAmountMixedOutputs(t *testing.T) {
	t.Parallel()

	pred := MaxOutputAmount(100_000)

	regs := map[clientconn.ClientID]*ClientRegistration{
		"c1": {
			ClientID: "c1",
			VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{
				route.Vertex{0x11}: {Amount: 40_000},
				route.Vertex{0x12}: {Amount: 30_000},
			},
			LeaveOutputs: []*wire.TxOut{
				{Value: 30_000},
			},
		},
	}

	// 40k + 30k + 30k = 100k — exactly at threshold.
	require.True(t, pred(regs))
}

// TestSealPredicateTriggersEarlySeal exercises the full FSM path: two clients
// join with MaxClients(2), and the round seals immediately after the second
// join — without the registration timeout firing.
func TestSealPredicateTriggersEarlySeal(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.setupPermissiveMocks()

	// Wire a MaxClients(2) predicate into the environment.
	h.env.ShouldSeal = MaxClients(2)

	// Use a very long registration timeout so we can be sure the seal
	// is triggered by the predicate and not the timer.
	h.env.Terms.RegistrationTimeout = 10 * 60 * 1_000_000_000 // 10 min

	// --- First client joins ---
	outpoint1 := wire.OutPoint{
		Hash: chainhash.HashH([]byte("boarding1")), Index: 0,
	}
	_, joinEvt1 := quickClient(h, "client1", 10, &outpoint1)
	feedJoinSuccess(h, joinEvt1)

	// Should be in IntentCollectingState (predicate threshold not yet met).
	assertStateType[*IntentCollectingState](h)

	// Outbox: ClientSuccessResp + StartTimeoutReq (first client).
	h.assertOutboxLen(2)
	assertOutboxMessageType[*ClientSuccessResp](h, 0)
	assertOutboxMessageType[*StartTimeoutReq](h, 1)

	// --- Second client joins (triggers seal predicate) ---
	h.outboxMessages = nil

	outpoint2 := wire.OutPoint{
		Hash: chainhash.HashH([]byte("boarding2")), Index: 0,
	}
	_, joinEvt2 := quickClient(h, "client2", 20, &outpoint2)
	feedJoinSuccess(h, joinEvt2)

	// The predicate should have fired, so the FSM emits a SealEvent
	// internally and transitions through batch building to
	// AwaitingInputSigsState.
	assertStateType[*AwaitingInputSigsState](h)

	// Outbox should contain: ClientSuccessResp + CancelTimeoutReq
	// (cancels the registration timeout) + RoundSealedReq.
	assertOutboxContains[*ClientSuccessResp](h)
	assertOutboxContains[*RoundSealedReq](h)

	cancel := assertOutboxContains[*CancelTimeoutReq](h)
	require.Equal(t, h.env.RoundID, cancel.RoundID)
	require.Equal(
		t, TimeoutPhaseRegistration, cancel.Phase,
	)
}

// TestSealPredicateTriggersOnFirstClient verifies that MaxClients(1) seals
// immediately when the very first client joins in CreatedState.
func TestSealPredicateTriggersOnFirstClient(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.setupPermissiveMocks()

	// Wire MaxClients(1) — should seal on the very first join.
	h.env.ShouldSeal = MaxClients(1)
	h.env.Terms.RegistrationTimeout = 10 * 60 * 1_000_000_000

	outpoint := wire.OutPoint{
		Hash: chainhash.HashH([]byte("solo")), Index: 0,
	}
	_, joinEvt := quickClient(h, "client1", 10, &outpoint)
	feedJoinSuccess(h, joinEvt)

	// The predicate fires on the first client, so the FSM should
	// seal and transition through batch building.
	assertStateType[*AwaitingInputSigsState](h)

	assertOutboxContains[*ClientSuccessResp](h)
	assertOutboxContains[*StartTimeoutReq](h)
	assertOutboxContains[*RoundSealedReq](h)

	// The registration timeout started just before the predicate
	// fired, so it must be cancelled.
	cancel := assertOutboxContains[*CancelTimeoutReq](h)
	require.Equal(t, h.env.RoundID, cancel.RoundID)
	require.Equal(
		t, TimeoutPhaseRegistration, cancel.Phase,
	)
}
