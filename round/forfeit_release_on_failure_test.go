package round

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
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

	noID := fn.None[RoundID]()

	t.Run("nil transition is a no-op", func(t *testing.T) {
		t.Parallel()

		got, err := releaseForfeitsOnFailure(nil, nil, noID, forfeits)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("non-failure transition untouched", func(t *testing.T) {
		t.Parallel()

		tr := &ClientStateTransition{NextState: &RoundJoinedState{}}
		got, err := releaseForfeitsOnFailure(tr, nil, noID, forfeits)
		require.NoError(t, err)

		require.False(
			t, got.NewEvents.IsSome(),
			"a non-failure transition must not emit a release",
		)
	})

	t.Run("failure with forfeits releases them", func(t *testing.T) {
		t.Parallel()

		tr := &ClientStateTransition{NextState: &ClientFailedState{}}
		got, err := releaseForfeitsOnFailure(tr, nil, noID, forfeits)
		require.NoError(t, err)

		outbox := got.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
		release, ok := findOutbox[*ReleaseForfeitReservation](outbox)
		require.True(t, ok, "expected ReleaseForfeitReservation")
		require.ElementsMatch(t, outpoints, release.Outpoints)
	})

	t.Run("failure with no forfeits releases nothing", func(t *testing.T) {
		t.Parallel()

		tr := &ClientStateTransition{NextState: &ClientFailedState{}}
		got, err := releaseForfeitsOnFailure(tr, nil, noID, nil)
		require.NoError(t, err)

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
		got, err := releaseForfeitsOnFailure(tr, nil, noID, forfeits)
		require.NoError(t, err)

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

	t.Run("inner error synthesizes a failure release", func(t *testing.T) {
		t.Parallel()

		// A raw (nil, err) from the wrapped handler must be converted
		// into a clean ClientFailedState transition that releases the
		// forfeits, and the now-handled error must be dropped so the
		// FSM engine does not tear the state machine down (which would
		// skip the release entirely).
		innerErr := errors.New("populateForfeitMappingAmounts failed")
		got, err := releaseForfeitsOnFailure(
			nil, innerErr, noID, forfeits,
		)
		require.NoError(t, err, "handled error must be dropped")
		require.NotNil(t, got)

		_, ok := got.NextState.(*ClientFailedState)
		require.True(t, ok, "expected synthesized ClientFailedState")

		outbox := got.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
		release, ok := findOutbox[*ReleaseForfeitReservation](outbox)
		require.True(t, ok, "synthesized failure must release forfeits")
		require.ElementsMatch(t, outpoints, release.Outpoints)
	})

	t.Run("inner error with no forfeits propagates", func(t *testing.T) {
		t.Parallel()

		// With nothing to release, the established behavior (letting
		// the error propagate to the engine) is preserved.
		innerErr := errors.New("validation failed")
		got, err := releaseForfeitsOnFailure(nil, innerErr, noID, nil)
		require.ErrorIs(t, err, innerErr)
		require.Nil(t, got)
	})

	t.Run("release is prepended before other outbox", func(t *testing.T) {
		t.Parallel()

		// A failure transition that already carries a server Tell (e.g.
		// JoinRoundRejectOutbox) must have the local release dispatched
		// first, so a failing Tell cannot short-circuit the outbox
		// before the VTXOs are returned to LiveState.
		tr := &ClientStateTransition{
			NextState: &ClientFailedState{},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{
					&JoinRoundRejectOutbox{},
				},
			}),
		}
		got, err := releaseForfeitsOnFailure(tr, nil, noID, forfeits)
		require.NoError(t, err)

		outbox := got.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
		require.Len(t, outbox, 2)

		_, ok := outbox[0].(*ReleaseForfeitReservation)
		require.True(
			t, ok, "release must be the first outbox item, got %T",
			outbox[0],
		)
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
		{
			// A recoverable server round failure arriving while the
			// client holds the quote or is mid VTXO-tree signing
			// must still release the forfeits. These states had no
			// BoardingFailed case, so the release was silently
			// dropped and the inputs stranded in pending-forfeit.
			name: "QuoteReceivedState/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &QuoteReceivedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: boardingFailed,
		},
		{
			name: "CommitmentTxValidatedState/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &CommitmentTxValidatedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: boardingFailed,
		},
		{
			name: "NoncesAggregatedState/BoardingFailed",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &NoncesAggregatedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: boardingFailed,
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

// TestPreSigningTerminalFailureRetiresJob is the regression for the
// terminal-notification-swallowed bug. A pre-signing state that fails on a
// BoardingFailed carrying a terminal-for-job code must emit BOTH the forfeit
// release (VTXOs back to LiveState) AND a TerminalJobFailedNotification (retire
// the originating pending intent), for every wrapped pre-signing state,
// including the ones that build their own failure outbox (IntentSentState,
// CommitmentTxReceivedState). Before the fix, releaseForfeitsOnFailure's
// release-idempotency guard early-returned before the notification block for
// exactly those states, so the operator-fund-failure send replayed forever, the
// #889 bug surviving on its own fix path. The failed round in the production
// incident lands in IntentSentState (the client stays there after RoundJoined
// admission until the commitment tx arrives), which is one of the two
// failure-outbox states, so this is the reachable case, not a latent one.
func TestPreSigningTerminalFailureRetiresJob(t *testing.T) {
	t.Parallel()

	terminal := &BoardingFailed{
		Reason:      "operator cannot fund the commitment tx",
		Error:       errors.New("fund psbt: insufficient funds"),
		Recoverable: true,
		FailureCode: RoundFailureInsufficientOperatorFunds,
	}

	cases := []preSigningFailure{
		{
			name: "PendingRoundAssembly",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &PendingRoundAssembly{
					Forfeits: ff,
				}
			},
			event: terminal,
		},
		{
			// The reachable #889 path: still in IntentSentState at
			// seal time, and this handler builds its own failure
			// outbox, so the old guard swallowed the drop here.
			name: "IntentSentState",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &IntentSentState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: terminal,
		},
		{
			name: "RoundJoinedState",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &RoundJoinedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: terminal,
		},
		{
			// Second failure-outbox state, latent for the current
			// code (commitment tx is already funded here) but wired
			// so future terminal codes stay correct.
			name: "CommitmentTxReceivedState",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &CommitmentTxReceivedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: terminal,
		},
		{
			name: "NoncesSentState",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &NoncesSentState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: terminal,
		},
		{
			name: "PartialSigsSentState",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &PartialSigsSentState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: terminal,
		},
		{
			name: "ForfeitSignaturesCollectingState",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &ForfeitSignaturesCollectingState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: terminal,
		},
		{
			// These three states used to have no BoardingFailed
			// case, so a server round failure there was self-looped
			// and both the release and the drop were swallowed.
			name: "QuoteReceivedState",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &QuoteReceivedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: terminal,
		},
		{
			name: "CommitmentTxValidatedState",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &CommitmentTxValidatedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: terminal,
		},
		{
			name: "NoncesAggregatedState",
			state: func(ff []types.ForfeitRequest) ClientState {
				return &NoncesAggregatedState{
					Intents: Intents{
						Forfeits: ff,
					},
				}
			},
			event: terminal,
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

			failed, ok := tr.NextState.(*ClientFailedState)
			require.True(
				t, ok, "expected ClientFailedState, got %T",
				tr.NextState,
			)
			require.Equal(
				t, RoundFailureInsufficientOperatorFunds,
				failed.FailureCode,
			)

			outbox := tr.NewEvents.UnwrapOr(
				ClientEmittedEvent{},
			).Outbox

			// The inputs still return to the live set, exactly
			// once.
			release, ok := findOutbox[*ReleaseForfeitReservation](
				outbox,
			)
			require.True(
				t, ok, "terminal failure must release forfeits",
			)
			require.ElementsMatch(t, outpoints, release.Outpoints)

			// And, in the same breath, the originating job is
			// retired: the drop carries the forfeited outpoints so
			// the actor marks the pending intent failed instead of
			// replaying it into the broke operator.
			drop, ok := findOutbox[*TerminalJobFailedNotification](
				outbox,
			)
			require.True(
				t, ok, "terminal failure must retire the "+
					"originating job",
			)
			require.Equal(
				t, RoundFailureInsufficientOperatorFunds,
				drop.FailureCode,
			)
			require.ElementsMatch(
				t, outpoints, drop.ForfeitOutpoints,
			)
		})
	}
}

// TestPostSigningFailureDoesNotReleaseForfeits is the safety control: once the
// client has submitted its forfeit signatures (InputSigSentState onward), a
// failed round must NOT auto-release the inputs on the notification alone,
// since the server could still broadcast the forfeit if the commitment
// confirms. The release instead rides the wavelength#844 status reconcile: the
// failure parks in the state while a QueryRoundStatus probe confirms the round
// is dead at the operator, and only that authoritative answer releases (see
// status_reconcile_test.go). This test pins the notification-alone half: no
// release may ride the BoardingFailed itself.
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

// TestPreSigningInternalEventFailureReleasesForfeits is the second half of the
// strand-on-failure regression: two pre-signing states fail only on internal
// FSM events (not on a server-pushed BoardingFailed), so the table in
// TestPreSigningFailureReleasesForfeits cannot reach them. These states wrap
// their handlers in ProcessEvent so that a raw (nil, err) return from the inner
// processEvent is converted into a clean ClientFailedState that returns the
// forfeit-reserved inputs to LiveState. A refactor that dropped either wrapper,
// or let the raw error propagate to the FSM engine, would silently re-strand
// the forfeits for these cases with no other test going red.
func TestPreSigningInternalEventFailureReleasesForfeits(t *testing.T) {
	t.Parallel()

	t.Run("CommitmentTxValidated/GenerateNonces", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		forfeits, outpoints := frcForfeits()

		// Build a VTXO intent so the GenerateNonces handler enters its
		// per-VTXO session loop, but leave ClientTrees empty so the
		// signer-key lookup misses and the handler hits its raw
		// "no client tree for signer key" error path. This mirrors the
		// buildClientVTXOs_error subtest in transitions_test.go.
		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)

		state := &CommitmentTxValidatedState{
			RoundID: testRoundIDTr("frc-gen-nonces"),
			CommitmentTx: h.newTestCommitmentTx(
				[]BoardingIntent{intent},
			),
			VTXOTreePaths: map[int]*tree.Tree{
				0: h.newTestVTXOTreeForIntents(
					[]types.VTXORequest{vtxoReq},
				),
			},
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				VTXOs: []types.VTXORequest{
					vtxoReq,
				},
				Forfeits: forfeits,
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
		}

		assertReleasesForfeits(
			t, h, state, &GenerateNonces{}, outpoints,
		)
	})

	t.Run("NoncesAggregated/GeneratePartialSigs", func(t *testing.T) {
		t.Parallel()

		h := newRealSigningTestHarness(t)

		forfeits, outpoints := frcForfeits()

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxtTree := h.newTestVTXOTreeForIntents(
			[]types.VTXORequest{vtxoReq},
		)

		// Build a real signing session that never had its aggregated
		// nonces registered. Its Signatures(true) call therefore fails
		// with "not all nonces registered" inside the
		// GeneratePartialSigs handler, exercising the raw "failed to
		// generate partial signatures" error path. Using a real signer
		// (rather than the always-succeeding wallet mock) is what makes
		// Signatures(true) actually error here.
		session := h.newUnaggregatedSignerSession(vtxoReq, vtxtTree)
		signerKey := NewSignerKey(vtxoReq.SigningKey.PubKey)

		state := &NoncesAggregatedState{
			RoundID: testRoundIDTr("frc-partial-sigs"),
			CommitmentTx: h.newTestCommitmentTx(
				[]BoardingIntent{intent},
			),
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				VTXOs: []types.VTXORequest{
					vtxoReq,
				},
				Forfeits: forfeits,
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
			Musig2Sessions: map[SignerKey]*tree.SignerSession{
				signerKey: session,
			},
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
		}

		assertReleasesForfeits(
			t, h.boardingTestHarness, state, &GeneratePartialSigs{},
			outpoints,
		)
	})
}

// newUnaggregatedSignerSession builds a real tree.SignerSession for the given
// VTXO request and tree, backed by the harness's real MuSig2 signer, but
// deliberately skips registering aggregated nonces. The resulting session's
// Signatures(true) returns an error ("not all nonces registered"), which is the
// least-brittle way to force the raw error path in the GeneratePartialSigs
// handler without a large signing fixture.
func (h *realSigningTestHarness) newUnaggregatedSignerSession(
	vtxoReq types.VTXORequest, vtxtTree *tree.Tree) *tree.SignerSession {

	h.t.Helper()

	prevOutFetcher, err := vtxtTree.Root.PrevOutputFetcher(
		vtxtTree.BatchOutput,
	)
	require.NoError(h.t, err)

	session, err := tree.NewSignerSession(
		h.clientSigner, &vtxoReq.SigningKey,
		vtxtTree.SweepTapscriptRoot, prevOutFetcher, vtxtTree.Root,
	)
	require.NoError(h.t, err)

	return session
}

// assertReleasesForfeits drives the given internal event through the state's
// exported ProcessEvent (not the unexported processEvent, so the release
// wrapper is exercised) and asserts the transition lands in ClientFailedState
// while emitting a ReleaseForfeitReservation covering exactly the reserved
// outpoints.
func assertReleasesForfeits(t *testing.T, h *boardingTestHarness,
	state ClientState, event ClientEvent, outpoints []wire.OutPoint) {

	t.Helper()

	tr, err := state.ProcessEvent(h.ctx, event, h.env)
	require.NoError(t, err, "handled error must be dropped by the wrapper")

	_, ok := tr.NextState.(*ClientFailedState)
	require.True(
		t, ok, "expected ClientFailedState, got %T", tr.NextState,
	)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	release, ok := findOutbox[*ReleaseForfeitReservation](outbox)
	require.True(
		t, ok, "internal-event failure must release forfeits",
	)
	require.ElementsMatch(t, outpoints, release.Outpoints)
}
