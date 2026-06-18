package round

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// frcOutpoint builds a deterministic VTXO outpoint for the forfeit-release
// tests.
func frcOutpoint(seed byte, index uint32) wire.OutPoint {
	op := frcSeedOutpoint(seed)
	op.Index = index

	return op
}

func frcSeedOutpoint(seed byte) wire.OutPoint {
	return regTimeoutOutpoint(seed, 0)
}

// frcForfeits builds a two-input forfeit reservation plus the outpoints it
// covers, shared across the pre-signing release table.
func frcForfeits() ([]types.ForfeitRequest, []wire.OutPoint) {
	op1 := frcOutpoint(0x11, 0)
	op2 := frcOutpoint(0x22, 3)

	return []types.ForfeitRequest{
		mkForfeit(op1, 10_000),
		mkForfeit(op2, 30_000),
	}, []wire.OutPoint{op1, op2}
}

// TestReleaseForfeitsOnFailureHelper exercises the centralized helper directly:
// it must release exactly the reserved inputs when (and only when) a transition
// lands in ClientFailedState, stay idempotent against a handler that already
// released, and never touch a non-failure transition.
func TestReleaseForfeitsOnFailureHelper(t *testing.T) {
	t.Parallel()

	forfeits, outpoints := frcForfeits()

	t.Run("nil transition is a no-op", func(t *testing.T) {
		t.Parallel()

		require.Nil(t, releaseForfeitsOnFailure(nil, forfeits))
	})

	t.Run("non-failure transition untouched", func(t *testing.T) {
		t.Parallel()

		tr := &ClientStateTransition{NextState: &RoundJoinedState{}}
		got := releaseForfeitsOnFailure(tr, forfeits)

		require.False(
			t, got.NewEvents.IsSome(),
			"a non-failure transition must not emit a release",
		)
	})

	t.Run("failure with forfeits releases them", func(t *testing.T) {
		t.Parallel()

		tr := &ClientStateTransition{NextState: &ClientFailedState{}}
		got := releaseForfeitsOnFailure(tr, forfeits)

		outbox := got.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
		release, ok := findOutbox[*ReleaseForfeitReservation](outbox)
		require.True(t, ok, "expected ReleaseForfeitReservation")
		require.ElementsMatch(t, outpoints, release.Outpoints)
	})

	t.Run("failure with no forfeits releases nothing", func(t *testing.T) {
		t.Parallel()

		tr := &ClientStateTransition{NextState: &ClientFailedState{}}
		got := releaseForfeitsOnFailure(tr, nil)

		_, ok := findOutbox[*ReleaseForfeitReservation](
			got.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox,
		)
		require.False(t, ok, "no inputs reserved, nothing to release")
	})

	t.Run("idempotent against an explicit release", func(t *testing.T) {
		t.Parallel()

		// Mirror the IntentSentState admission-timeout path, which
		// already emits a release of its own.
		tr := &ClientStateTransition{
			NextState: &ClientFailedState{},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{
					&RoundFailedNotification{},
					&ReleaseForfeitReservation{
						Outpoints: outpoints,
					},
				},
			}),
		}
		got := releaseForfeitsOnFailure(tr, forfeits)

		// Exactly one release must remain: the helper must not append a
		// duplicate.
		outbox := got.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
		var releases int
		for _, m := range outbox {
			if _, ok := m.(*ReleaseForfeitReservation); ok {
				releases++
			}
		}
		require.Equal(t, 1, releases,
			"release must not be duplicated")
	})
}

// preSigningFailure describes a pre-signing state plus a failure event it
// genuinely transitions to ClientFailedState on. Releasing forfeits in any of
// these states is safe because the client has not yet submitted VTXO forfeit
// signatures to the server.
type preSigningFailure struct {
	name  string
	state func(forfeits []types.ForfeitRequest) ClientState
	event ClientEvent
}

// TestPreSigningFailureReleasesForfeits is the core regression for the
// strand-on-failure bug: every pre-signing state that fails the round (whether
// from a server-pushed BoardingFailed, a local rejection, or a collection
// timeout) must return its forfeit-reserved inputs to LiveState instead of
// leaving them wedged in pending-forfeit.
func TestPreSigningFailureReleasesForfeits(t *testing.T) {
	t.Parallel()

	boardingFailed := &BoardingFailed{
		Reason:      "server failed the round",
		Error:       errors.New("commitment tx build failed"),
		Recoverable: true,
	}

	cases := []preSigningFailure{
		{
			name: "PendingRoundAssembly/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &PendingRoundAssembly{
					Forfeits: ff,
				}
			},
			event: boardingFailed,
		},
		{
			name: "IntentSentState/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &IntentSentState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: boardingFailed,
		},
		{
			// The production incident: the server admits the
			// client, then fails the round while the client waits
			// in RoundJoinedState for the commitment tx.
			name: "RoundJoinedState/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &RoundJoinedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: boardingFailed,
		},
		{
			name: "CommitmentTxReceivedState/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &CommitmentTxReceivedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: boardingFailed,
		},
		{
			name: "NoncesSentState/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &NoncesSentState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: boardingFailed,
		},
		{
			name: "PartialSigsSentState/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &PartialSigsSentState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: boardingFailed,
		},
		{
			// Boundary state: failures here are still pre-signing
			// because VTXO forfeit sigs only cross to the server on
			// the success transition out of this state.
			name: "ForfeitSignaturesCollecting/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &ForfeitSignaturesCollectingState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: boardingFailed,
		},
		{
			name: "ForfeitSignaturesCollecting/Timeout",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &ForfeitSignaturesCollectingState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: &ForfeitCollectionTimedOut{},
		},
		{
			// A non-BoardingFailed failure that also emits its own
			// outbox (JoinRoundRejectOutbox): the release must
			// compose with, not replace, the existing message.
			name: "QuoteReceivedState/QuoteRejected",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &QuoteReceivedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: &QuoteRejected{
				Reason: "fee exceeds cap",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			forfeits, outpoints := frcForfeits()
			env := &ClientEnvironment{Log: btclog.Disabled}

			tr, err := tc.state(forfeits).ProcessEvent(
				context.Background(), tc.event, env,
			)
			require.NoError(t, err)

			_, ok := tr.NextState.(*ClientFailedState)
			require.True(
				t, ok, "expected ClientFailedState, got %T",
				tr.NextState,
			)

			outbox := tr.NewEvents.UnwrapOr(
				ClientEmittedEvent{},
			).Outbox
			release, ok := findOutbox[*ReleaseForfeitReservation](
				outbox,
			)
			require.True(
				t, ok,
				"pre-signing failure must release forfeits",
			)
			require.ElementsMatch(t, outpoints, release.Outpoints)
		})
	}
}

// TestPostSigningFailureDoesNotReleaseForfeits is the safety control: once the
// client has submitted its forfeit signatures (InputSigSentState onward), a
// failed round must NOT auto-release the inputs, since the server could still
// broadcast the forfeit if the commitment confirms. Releasing here would risk a
// double-spend, so the post-signing states are deliberately not wired to the
// release helper.
func TestPostSigningFailureDoesNotReleaseForfeits(t *testing.T) {
	t.Parallel()

	forfeits, _ := frcForfeits()
	env := &ClientEnvironment{Log: btclog.Disabled}

	s := &InputSigSentState{Intents: Intents{Forfeits: forfeits}}
	tr, err := s.ProcessEvent(context.Background(), &BoardingFailed{
		Reason:      "post-signing failure",
		Recoverable: false,
	}, env)
	require.NoError(t, err)

	failed, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)
	require.False(t, failed.Recoverable)

	_, ok = findOutbox[*ReleaseForfeitReservation](
		tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox,
	)
	require.False(
		t, ok, "post-signing failure must not auto-release forfeits",
	)
}
