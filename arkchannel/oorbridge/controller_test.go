package oorbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// controllerTestRef returns a fixed OOR state to the controller.
type controllerTestRef struct {
	state           oor.SessionState
	startSessionID  oor.SessionID
	startExisting   bool
	startErr        error
	startPending    bool
	startRequest    *oor.StartTransferRequest
	lookupFound     bool
	lookupPending   bool
	lookupRequests  int
	driveEvent      oor.Event
	driveErr        error
	stateAfterDrive oor.SessionState
}

// ID returns the fake actor identity.
func (*controllerTestRef) ID() string {
	return "oor-controller-test"
}

// Tell accepts messages unused by this read-only fixture.
func (*controllerTestRef) Tell(context.Context, oor.OORDurableMsg) error {
	return nil
}

// TryTell accepts messages unused by this read-only fixture.
func (*controllerTestRef) TryTell(context.Context, oor.OORDurableMsg) error {
	return nil
}

// Ask serves the current state query used by source validation.
func (r *controllerTestRef) Ask(_ context.Context,
	message oor.OORDurableMsg) actor.Future[oor.ActorResp] {

	promise := actor.NewPromise[oor.ActorResp]()
	var response oor.ActorResp
	switch message := message.(type) {
	case *oor.GetStateRequest:
		response = &oor.GetStateResponse{State: r.state}

	case *oor.StartTransferRequest:
		r.startRequest = message
		if r.startPending {
			return promise.Future()
		}
		if r.startErr != nil {
			promise.Complete(fn.Err[oor.ActorResp](r.startErr))

			return promise.Future()
		}
		response = &oor.StartTransferResponse{
			SessionID: r.startSessionID, Existing: r.startExisting,
		}

	case *oor.LookupTransferRequest:
		r.lookupRequests++
		response = &oor.LookupTransferResponse{
			SessionID: r.startSessionID, Found: r.lookupFound,
			Pending: r.lookupPending,
		}

	case *oor.DriveEventRequest:
		r.driveEvent = message.Event
		if r.stateAfterDrive != nil {
			r.state = r.stateAfterDrive
		} else {
			switch event := message.Event.(type) {
			case *oor.CommitPreparedEvent:
				r.state = &oor.Completed{}

			case *oor.AbortPreparedEvent:
				r.state = &oor.Failed{
					Reason: event.Reason, PrePONR: true,
				}
			}
		}
		if r.driveErr != nil {
			promise.Complete(fn.Err[oor.ActorResp](r.driveErr))

			return promise.Future()
		}
		response = &oor.DriveEventResponse{}

	default:
		response = &oor.DriveEventResponse{}
	}
	promise.Complete(fn.Ok(response))

	return promise.Future()
}

// TestPrepareChannelStopsBeforeOORSignatures proves the channel output is
// created through the ordinary OOR actor while its state remains Prepared.
func TestPrepareChannelStopsBeforeOORSignatures(t *testing.T) {
	t.Parallel()

	terms, expected, prepared, _, _ := preparedChannelBinding(t)
	operatorKey, err := btcec.ParsePubKey(terms.VTXO.ArkOperatorKey[:])
	require.NoError(t, err)
	ref := &controllerTestRef{
		state:          prepared,
		startSessionID: oor.SessionID(expected.OORSessionID),
	}
	controller, err := NewWithRef(ref)
	require.NoError(t, err)

	result, err := controller.PrepareChannel(t.Context(), PrepareRequest{
		Terms: terms,
		CheckpointPolicy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey,
			CSVDelay:    terms.VTXO.MinExitDelay,
		},
		Inputs:     prepared.TransferInputs,
		BackingFee: expected.Amount - terms.Capacity,
	})
	require.NoError(t, err)
	require.Equal(t, expected, result.Binding)
	require.False(t, result.Existing)
	require.NotNil(t, ref.startRequest)
	require.True(t, ref.startRequest.PrepareOnly)
	require.Equal(
		t, "ark-channel:"+hex.EncodeToString(terms.ID[:]),
		ref.startRequest.IdempotencyKey,
	)
	require.Equal(t, prepared.TransferInputs, ref.startRequest.Inputs)
	require.Len(t, ref.startRequest.Recipients, 1)
}

// TestPrepareChannelPreservesAmbiguousAdmission proves a caller cannot release
// channel accounting after the OOR registry has returned a durable session ID
// but its exact prepared state cannot be reconstructed yet.
func TestPrepareChannelPreservesAmbiguousAdmission(t *testing.T) {
	t.Parallel()

	terms, expected, prepared, _, _ := preparedChannelBinding(t)
	operatorKey, err := btcec.ParsePubKey(terms.VTXO.ArkOperatorKey[:])
	require.NoError(t, err)
	ref := &controllerTestRef{
		state: &oor.Completed{}, startSessionID: oor.SessionID(
			expected.OORSessionID,
		),
	}
	controller, err := NewWithRef(ref)
	require.NoError(t, err)

	_, err = controller.PrepareChannel(t.Context(), PrepareRequest{
		Terms: terms,
		CheckpointPolicy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey,
			CSVDelay:    terms.VTXO.MinExitDelay,
		},
		Inputs: prepared.TransferInputs, BackingFee: expected.Amount -
			terms.Capacity,
	})
	require.ErrorIs(t, err, arkchannel.ErrOORPreparationAmbiguous)
}

// TestPrepareChannelReturnsDefinitiveAdmissionError proves an actor rejection
// observed while the caller is live is safe for the wallet to unlock.
func TestPrepareChannelReturnsDefinitiveAdmissionError(t *testing.T) {
	t.Parallel()

	terms, expected, prepared, _, _ := preparedChannelBinding(t)
	operatorKey, err := btcec.ParsePubKey(terms.VTXO.ArkOperatorKey[:])
	require.NoError(t, err)
	ref := &controllerTestRef{
		state: prepared, startSessionID: oor.SessionID(
			expected.OORSessionID,
		),
		startErr: fmt.Errorf("admission rejected"),
	}
	controller, err := NewWithRef(ref)
	require.NoError(t, err)

	_, err = controller.PrepareChannel(t.Context(), PrepareRequest{
		Terms: terms,
		CheckpointPolicy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey,
			CSVDelay:    terms.VTXO.MinExitDelay,
		},
		Inputs: prepared.TransferInputs, BackingFee: expected.Amount -
			terms.Capacity,
	})
	require.ErrorContains(t, err, "admission rejected")
	require.NotErrorIs(t, err, arkchannel.ErrOORPreparationAmbiguous)
}

// TestPrepareChannelMarksCanceledWaitAmbiguous proves caller cancellation
// cannot be interpreted as a registry admission rejection.
func TestPrepareChannelMarksCanceledWaitAmbiguous(t *testing.T) {
	t.Parallel()

	terms, expected, prepared, _, _ := preparedChannelBinding(t)
	operatorKey, err := btcec.ParsePubKey(terms.VTXO.ArkOperatorKey[:])
	require.NoError(t, err)
	controller, err := NewWithRef(&controllerTestRef{startPending: true})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = controller.PrepareChannel(ctx, PrepareRequest{
		Terms: terms,
		CheckpointPolicy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey,
			CSVDelay:    terms.VTXO.MinExitDelay,
		},
		Inputs: prepared.TransferInputs, BackingFee: expected.Amount -
			terms.Capacity,
	})
	require.ErrorIs(t, err, arkchannel.ErrOORPreparationAmbiguous)
}

// TestLookupPreparedChannelReconstructsBinding proves restart recovery uses the
// durable idempotency winner without submitting another transfer request.
func TestLookupPreparedChannelReconstructsBinding(t *testing.T) {
	t.Parallel()

	terms, expected, prepared, _, _ := preparedChannelBinding(t)
	ref := &controllerTestRef{
		state: prepared, startSessionID: oor.SessionID(
			expected.OORSessionID,
		),
		lookupFound: true,
	}
	controller, err := NewWithRef(ref)
	require.NoError(t, err)

	binding, found, err := controller.LookupPreparedChannel(
		t.Context(), terms, expected.Amount-terms.Capacity,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, expected, binding)
	require.Equal(t, 1, ref.lookupRequests)
	require.Nil(t, ref.startRequest)
}

// TestLookupChannelPreparationReportsPending proves an accepted registry
// request cannot be treated as an authoritative miss during retry cleanup.
func TestLookupChannelPreparationReportsPending(t *testing.T) {
	t.Parallel()

	terms, expected, _, _, _ := preparedChannelBinding(t)
	ref := &controllerTestRef{
		startSessionID: oor.SessionID(expected.OORSessionID),
		lookupPending:  true,
	}
	controller, err := NewWithRef(ref)
	require.NoError(t, err)

	lookup, err := controller.LookupChannelPreparation(
		t.Context(), terms, expected.Amount-terms.Capacity,
	)
	require.NoError(t, err)
	require.Equal(t, PreparationPending, lookup.Status)
	require.Empty(t, lookup.InputOutpoints)
	_, found, err := controller.LookupPreparedChannel(
		t.Context(), terms, expected.Amount-terms.Capacity,
	)
	require.ErrorIs(t, err, arkchannel.ErrOORPreparationAmbiguous)
	require.False(t, found)
	require.Nil(t, ref.startRequest)
}

// TestLookupChannelPreparationTreatsPrePONRFailureAsAbsent proves a failed
// admission no longer owns wallet inputs and permits a clean retry.
func TestLookupChannelPreparationTreatsPrePONRFailureAsAbsent(t *testing.T) {
	t.Parallel()

	terms, expected, _, _, _ := preparedChannelBinding(t)
	controller, err := NewWithRef(&controllerTestRef{
		state:          &oor.Failed{Reason: "rejected", PrePONR: true},
		startSessionID: oor.SessionID(expected.OORSessionID),
		lookupFound:    true,
	})
	require.NoError(t, err)

	lookup, err := controller.LookupChannelPreparation(
		t.Context(), terms, expected.Amount-terms.Capacity,
	)
	require.NoError(t, err)
	require.Equal(t, PreparationAbsent, lookup.Status)
}

// TestValidatePreparedOORBindsExactOutput proves a caller cannot bind a
// plausible but nonexistent output from the same deterministic OOR package.
func TestValidatePreparedOORBindsExactOutput(t *testing.T) {
	t.Parallel()

	terms, binding, prepared, _, _ := preparedChannelBinding(t)
	controller, err := NewWithRef(&controllerTestRef{state: prepared})
	require.NoError(t, err)
	require.NoError(
		t,
		controller.ValidatePreparedOOR(
			t.Context(), terms, binding,
		),
	)

	wrongIndex := binding.Clone()
	wrongIndex.OutPoint.Index++
	require.ErrorContains(
		t,
		controller.ValidatePreparedOOR(
			t.Context(), terms, wrongIndex,
		),
		"does not match",
	)

	wrongAmount := binding.Clone()
	wrongAmount.Amount++
	require.ErrorContains(
		t,
		controller.ValidatePreparedOOR(
			t.Context(), terms, wrongAmount,
		),
		"does not match",
	)
}

// TestValidatePreparedOORRejectsAdvancedSession proves binding happens before
// the OOR signature and transport gates are released.
func TestValidatePreparedOORRejectsAdvancedSession(t *testing.T) {
	t.Parallel()

	terms, binding, _, _, _ := preparedChannelBinding(t)
	controller, err := NewWithRef(&controllerTestRef{
		state: &oor.AwaitingArkSignatures{},
	})
	require.NoError(t, err)
	require.ErrorContains(
		t,
		controller.ValidatePreparedOOR(
			t.Context(), terms, binding,
		),
		"expected prepared",
	)
}

// preparedChannelBinding creates a real deterministic OOR package containing
// one channel-policy recipient.
func preparedChannelBinding(t *testing.T) (arkchannel.Terms,
	arkchannel.VTXOBinding, *oor.Prepared, *btcec.PrivateKey,
	*btcec.PrivateKey) {

	t.Helper()
	keys := make([]*btcec.PrivateKey, 8)
	for i := range keys {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		keys[i] = key
	}
	compressed := func(key *btcec.PublicKey) [33]byte {
		var raw [33]byte
		copy(raw[:], key.SerializeCompressed())

		return raw
	}
	terms := arkchannel.Terms{
		ID: arkchannel.ID{
			1,
		},
		Kind:   arkchannel.KindPromotion,
		Funder: arkchannel.PartyClient,
		PendingChannelID: [32]byte{
			2,
		},
		ReservedSCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_001,
		}.ToUint64(),
		Capacity:      100_000,
		ClientNodeKey: compressed(keys[0].PubKey()),
		HubNodeKey:    compressed(keys[1].PubKey()),
		VTXO: arkchannel.VTXOTerms{
			ClientArkKey:     compressed(keys[2].PubKey()),
			HubArkKey:        compressed(keys[3].PubKey()),
			ArkOperatorKey:   compressed(keys[4].PubKey()),
			ClientChannelKey: compressed(keys[5].PubKey()),
			HubChannelKey:    compressed(keys[6].PubKey()),
			FunderKey:        compressed(keys[7].PubKey()),
			ChannelDelay:     10,
			FunderDelay: 10 +
				arkscript.DefaultChannelReactionWindow,
			MinExitDelay: 10,
		},
	}
	policyTemplate, channelScript, err := terms.VTXO.Artifacts()
	require.NoError(t, err)
	amount := terms.Capacity + 1_000
	recipients := []oortx.RecipientOutput{{
		PkScript:           channelScript,
		Value:              amount,
		VTXOPolicyTemplate: policyTemplate,
	}}

	owner := keys[0]
	operator := keys[4]
	exitDelay := uint32(10)
	tapScript, err := arkscript.VTXOTapScript(
		owner.PubKey(), operator.PubKey(), exitDelay,
	)
	require.NoError(t, err)
	tapKey, err := arkscript.VTXOTapKey(
		owner.PubKey(), operator.PubKey(), exitDelay,
	)
	require.NoError(t, err)
	inputScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)
	ownerLeaf, err := arkscript.MultiSigCollabTapLeaf(
		owner.PubKey(), operator.PubKey(),
	)
	require.NoError(t, err)
	inputs := []oor.TransferInput{{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					9,
				},
			},
			Amount:   amount,
			PkScript: inputScript,
			ClientKey: keychain.KeyDescriptor{
				PubKey: owner.PubKey(),
			},
			OperatorKey:    operator.PubKey(),
			TapScript:      tapScript,
			RelativeExpiry: exitDelay,
		},
		OwnerLeafScript: ownerLeaf.Script,
	}}
	checkpointPolicy := arkscript.CheckpointPolicy{
		OperatorKey: operator.PubKey(),
		CSVDelay:    exitDelay,
	}
	session, outbox, err := oor.NewPreparedSessionWithIdempotencyKey(
		t.Context(), checkpointPolicy, inputs, recipients,
		"ark-channel:1", oor.EnvConfig{},
	)
	require.NoError(t, err)
	require.Empty(t, outbox)
	t.Cleanup(session.FSM.Stop)
	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	prepared, ok := state.(*oor.Prepared)
	require.True(t, ok)
	outpoint, err := oortx.RecipientOutPoint(
		chainhash.Hash(session.ID), recipients, recipients[0],
	)
	require.NoError(t, err)
	var arkTransaction bytes.Buffer
	require.NoError(
		t, prepared.ArkPSBT.UnsignedTx.Serialize(&arkTransaction),
	)

	return terms, arkchannel.VTXOBinding{
		OORSessionID:   [32]byte(session.ID),
		OutPoint:       outpoint,
		Amount:         amount,
		ArkTransaction: arkTransaction.Bytes(),
		PolicyTemplate: policyTemplate,
		PkScript:       channelScript,
	}, prepared, keys[2], keys[3]
}

var _ actor.ActorRef[
	oor.OORDurableMsg, oor.ActorResp,
] = (*controllerTestRef)(nil)
