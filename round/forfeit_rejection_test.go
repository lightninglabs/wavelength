package round

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// rejectionTestState holds one outstanding input beside a signature already
// collected locally. Refusal must stop their aggregate before checkpointing.
func rejectionTestState(id RoundID) *ForfeitSignaturesCollectingState {
	missing := regTimeoutOutpoint(0x81, 0)
	signed := regTimeoutOutpoint(0x82, 0)

	return &ForfeitSignaturesCollectingState{
		RoundID: id,
		Intents: Intents{Forfeits: []types.ForfeitRequest{
			{
				VTXOOutpoint: &missing,
			}, {
				VTXOOutpoint: &signed,
			},
		}},
		ExpectedForfeits: map[wire.OutPoint]*ConnectorLeafInfo{
			missing: {}, signed: {},
		},
		CollectedForfeits: map[wire.OutPoint]*ForfeitSignatureResponse{
			signed: {
				RoundID:      id.String(),
				VTXOOutpoint: signed,
			},
		},
	}
}

// TestForfeitRejectionStopsExactCollectingAttempt verifies the real actor
// routes a refused input into safe rollback even after a peer signed locally.
// No timeout or operator message is needed to fail the attempt.
func TestForfeitRejectionStopsExactCollectingAttempt(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())
	id := testRoundID("refused-forfeit")
	state := rejectionTestState(id)
	h.injectRoundInState(id, state)
	h.roundStore.On("FailRound", mock.Anything, id).Return(nil).Once()
	rejection := &ForfeitSignatureRejected{
		RoundID: id.String(), VTXOOutpoint: regTimeoutOutpoint(0x81, 0),
		Reason: "input is exiting",
	}
	_, err := h.receive(rejection).Unpack()
	require.NoError(t, err)
	require.IsType(
		t, &ClientFailedState{}, h.queryState()[id.String()].State,
	)
	messages := h.vtxoManager.getMessages()
	require.Len(t, messages, 1)
	release, ok := messages[0].(*actormsg.ReleaseForfeitRequest)
	require.True(t, ok)
	require.Equal(t, id.String(), release.RoundID)
	require.ElementsMatch(
		t, forfeitOutpoints(state.Intents.Forfeits), release.Outpoints,
	)
	require.Empty(t, h.serverConn.snapshotMessages())

	// A duplicate rejection cannot revive the failed attempt or repeat
	// its release.
	_, err = h.receive(rejection).Unpack()
	require.NoError(t, err)
	require.Len(t, h.vtxoManager.getMessages(), 1)
}

// TestForfeitRejectionFences covers late and foreign notices at the FSM's
// owning boundary, including the checkpoint that forbids local rollback.
func TestForfeitRejectionFences(t *testing.T) {
	t.Parallel()

	id := testRoundID("refusal-fences")
	missing := regTimeoutOutpoint(0x81, 0)
	for _, tc := range []struct {
		name  string
		state ClientState
		msg   *ForfeitSignatureRejected
	}{
		{
			name: "foreign round", state: rejectionTestState(id),
			msg: &ForfeitSignatureRejected{
				RoundID:      testRoundID("other").String(),
				VTXOOutpoint: missing,
			},
		},
		{
			name: "unexpected input", state: rejectionTestState(id),
			msg: &ForfeitSignatureRejected{
				RoundID:      id.String(),
				VTXOOutpoint: wire.OutPoint{},
			},
		},
		{
			name:  "already collected",
			state: rejectionTestState(id),
			msg: &ForfeitSignatureRejected{
				RoundID:      id.String(),
				VTXOOutpoint: regTimeoutOutpoint(0x82, 0),
			},
		},
		{
			name: "checkpointed",
			state: &InputSigSentState{
				RoundID: id,
				Intents: rejectionTestState(id).Intents,
			},
			msg: &ForfeitSignatureRejected{
				RoundID: id.String(), VTXOOutpoint: missing,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := tc.state.ProcessEvent(
				t.Context(), tc.msg, &ClientEnvironment{
					Log: btclog.Disabled,
				},
			)
			require.NoError(t, err)
			require.Same(t, tc.state, tr.NextState)
			require.Empty(
				t, tr.NewEvents.UnwrapOr(
					ClientEmittedEvent{},
				).Outbox,
			)
		})
	}
}

// TestForfeitRejectionIgnoresReplacementHandoff preserves the successor's
// input ownership while predecessor cleanup is waiting to finish.
func TestForfeitRejectionIgnoresReplacementHandoff(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())
	id := testRoundID("replaced-refusal")
	state := rejectionTestState(id)
	h.injectRoundInState(id, state)
	h.actor.rounds[RoundKeyStr(id.String())].PendingReplacement =
		&BoardingFailed{
			ReplacementRoundID: fn.Some(testRoundID("successor")),
		}

	for _, roundID := range []string{
		id.String(),
		testRoundID("gone").String(),
	} {
		_, err := h.receive(&ForfeitSignatureRejected{
			RoundID: roundID, VTXOOutpoint: regTimeoutOutpoint(
				0x81, 0,
			),
		}).Unpack()
		require.NoError(t, err)
	}
	require.IsType(t, state, h.queryState()[id.String()].State)
	require.Empty(t, h.vtxoManager.getMessages())
	require.Empty(t, h.serverConn.snapshotMessages())
}
