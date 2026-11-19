package oorbridge

import (
	"context"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/stretchr/testify/require"
)

// TestSourceLifetimeCommitGate validates the durable selected inputs at the
// signing gate and turns rejection into a proven, replayable pre-PONR abort.
func TestSourceLifetimeCommitGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		offset      int32
		expiry      int32
		backendFail bool
		aborted     bool
	}{
		{
			name:   "fresh",
			expiry: 2000,
			offset: -1,
		},
		{
			name:    "deadline",
			expiry:  2000,
			aborted: true,
		},
		{
			name: "stale", expiry: 2000, offset: 1,
			aborted: true,
		},
		{
			name:    "missing expiry",
			aborted: true,
		},
		{
			name: "backend unavailable", expiry: 2000, offset: -1,
			backendFail: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			terms, source, prepared, _, _ := preparedChannelBinding(
				t,
			)
			input := prepared.TransferInputs[0].VTXO
			input.BatchExpiry = test.expiry
			deadline, err := arkchannel.SourceMaterializationHeight(
				2000, terms.VTXO.FunderDelay,
			)
			require.NoError(t, err)
			ref := &controllerTestRef{state: prepared}
			controller, err := NewWithRef(ref)
			require.NoError(t, err)
			checks := 0
			controller.SetPreparedSourceValidator(func(
				ctx context.Context, gotTerms arkchannel.Terms,
				gotSource arkchannel.VTXOBinding,
				state *oor.Prepared) error {

				checks++
				require.Equal(t, t.Context(), ctx)
				require.Equal(t, terms, gotTerms)
				require.Equal(
					t, prepared, state,
				)
				require.Equal(t, source, gotSource)
				if test.backendFail && checks == 1 {
					return fmt.Errorf("best block " +
						"unavailable")
				}

				vtxo := state.TransferInputs[0].VTXO

				return arkchannel.ValidateSourceLifetime(
					vtxo.BatchExpiry, deadline+test.offset,
					gotTerms.VTXO.FunderDelay,
				)
			})

			commit := controller.CommitPreparedOORResult
			result, err := commit(
				t.Context(), terms.ID, terms, source,
			)
			if test.backendFail {
				require.ErrorContains(t, err, "best block")
				require.Nil(t, ref.driveEvent)
				require.Same(t, prepared, ref.state)
				result, err = commit(
					t.Context(), terms.ID, terms, source,
				)
			}
			require.NoError(t, err)
			require.NoError(t, result.Validate())
			require.Equal(t, test.aborted, result.Aborted)
			require.Equal(t, !test.aborted, result.Finalized)
			if test.aborted {
				event := ref.driveEvent
				abort, ok := event.(*oor.AbortPreparedEvent)
				require.True(t, ok)
				require.Equal(t, abort.Reason, result.Reason)
				require.Contains(
					t, result.Reason, "source lifetime",
				)
			} else {
				require.IsType(
					t, &oor.CommitPreparedEvent{},
					ref.driveEvent,
				)
			}

			replayed, err := controller.CommitPreparedOORResult(
				t.Context(), terms.ID, terms, source,
			)
			require.NoError(t, err)
			require.Equal(t, result, replayed)
			if test.backendFail {
				require.Equal(t, 2, checks)
			} else {
				require.Equal(t, 1, checks)
			}
		})
	}
}

// TestSourceLifetimeAbortReconciliation refuses to report an abort without
// durable proof and preserves a concurrent commit's authoritative outcome.
func TestSourceLifetimeAbortReconciliation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		next      oor.SessionState
		finalized bool
		failed    bool
	}{
		{
			name: "concurrent commit", next: &oor.Completed{},
			finalized: true,
		},
		{
			name: "lost abort response",
			next: &oor.Failed{
				PrePONR: true,
				Reason:  "source expired",
			},
		},
		{
			name:   "abort not persisted",
			failed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			terms, source, prepared, _, _ := preparedChannelBinding(
				t,
			)
			next := test.next
			if next == nil {
				next = prepared
			}
			ref := &controllerTestRef{
				state: prepared, stateAfterDrive: next,
				driveErr: fmt.Errorf("lost control response"),
			}
			controller, err := NewWithRef(ref)
			require.NoError(t, err)
			controller.SetPreparedSourceValidator(
				func(context.Context, arkchannel.Terms,
					arkchannel.VTXOBinding,
					*oor.Prepared) error {

					return arkchannel.ErrSourceInadmissible
				},
			)

			result, err := controller.CommitPreparedOORResult(
				t.Context(), terms.ID, terms, source,
			)
			require.IsType(
				t, &oor.AbortPreparedEvent{}, ref.driveEvent,
			)
			if test.failed {
				require.ErrorContains(
					t, err, "lost control response",
				)
				require.Zero(t, result)

				return
			}

			require.NoError(t, err)
			require.NoError(t, result.Validate())
			require.Equal(t, test.finalized, result.Finalized)
			require.Equal(t, !test.finalized, result.Aborted)
		})
	}
}

// lifetimeReplayRef advances an already committed session on the next read
// without sending a new signing or abort control event.
type lifetimeReplayRef struct {
	controllerTestRef
	reads int
}

// Ask lets the controller observe an in-flight replay followed by completion.
func (r *lifetimeReplayRef) Ask(ctx context.Context,
	message oor.OORDurableMsg) actor.Future[oor.ActorResp] {

	if _, ok := message.(*oor.GetStateRequest); ok {
		r.reads++
		if r.reads > 1 {
			r.state = &oor.Completed{}
		}
	}

	return r.controllerTestRef.Ask(ctx, message)
}

// TestSourceLifetimeAdvancedReplay never applies a new admission rejection
// after prepared commit, including post-PONR and already terminal replays.
func TestSourceLifetimeAdvancedReplay(t *testing.T) {
	t.Parallel()

	states := []oor.SessionState{
		&oor.AwaitingArkSignatures{},
		&oor.AwaitingSubmitAccepted{},
		&oor.AwaitingCheckpointSignatures{},
		&oor.AwaitingFinalizeAccepted{},
		&oor.AwaitingLocalVTXOUpdate{},
		&oor.Completed{},
		&oor.ReceiveCompleted{},
	}
	for _, state := range states {
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()

			terms, source, _, _, _ := preparedChannelBinding(t)
			ref := &lifetimeReplayRef{
				controllerTestRef: controllerTestRef{
					state: state,
				},
			}
			controller, err := NewWithRef(ref)
			require.NoError(t, err)
			controller.SetPreparedSourceValidator(
				func(context.Context, arkchannel.Terms,
					arkchannel.VTXOBinding,
					*oor.Prepared) error {

					t.Fatal(
						"replay must not " +
							"revalidate source " +
							"lifetime",
					)

					return fmt.Errorf("expired after " +
						"signing")
				},
			)
			result, err := controller.CommitPreparedOORResult(
				t.Context(), terms.ID, terms, source,
			)
			require.NoError(t, err)
			require.Equal(
				t, TerminalResult{
					Finalized: true,
				},
				result,
			)
			require.Nil(t, ref.driveEvent)
		})
	}
}

// TestSourceLifetimeExplicitAbort skips admission so a backend outage cannot
// stop an explicit pre-PONR cancellation from releasing its selected inputs.
func TestSourceLifetimeExplicitAbort(t *testing.T) {
	t.Parallel()

	terms, source, prepared, _, _ := preparedChannelBinding(t)
	controller, err := NewWithRef(&controllerTestRef{state: prepared})
	require.NoError(t, err)
	controller.SetPreparedSourceValidator(func(context.Context,
		arkchannel.Terms, arkchannel.VTXOBinding, *oor.Prepared) error {

		t.Fatal("explicit abort must not require source admission")

		return fmt.Errorf("backend unavailable")
	})
	result, err := controller.AbortPreparedOORResult(
		t.Context(), terms.ID, terms, source, "caller canceled",
	)
	require.NoError(t, err)
	require.Equal(t, TerminalResult{
		Aborted: true, Reason: "caller canceled",
	}, result)
}

// TestSourceLifetimeAbortAppliesTerminalFact advances the channel cancellation
// gate with the durable abort rather than leaving it to retry rejected signing.
func TestSourceLifetimeAbortAppliesTerminalFact(t *testing.T) {
	t.Parallel()

	terms, source, prepared, _, _ := preparedChannelBinding(t)
	controller, err := NewWithRef(&controllerTestRef{state: prepared})
	require.NoError(t, err)
	sink := &controllerTestSink{}
	require.NoError(t, controller.BindChannelEventSink(sink))
	controller.SetPreparedSourceValidator(func(context.Context,
		arkchannel.Terms, arkchannel.VTXOBinding, *oor.Prepared) error {

		return fmt.Errorf("%w: source expired",
			arkchannel.ErrSourceInadmissible)
	})
	require.NoError(
		t,
		controller.CommitPreparedOOR(
			t.Context(), terms.ID, terms, source,
		),
	)
	event, ok := sink.event.(*arkchannel.OORAborted)
	require.True(t, ok)
	require.Equal(t, source.OORSessionID, event.SessionID)
	require.Contains(t, event.Reason, "source expired")
}
