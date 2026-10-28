package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestSubmitAcceptedValidatesArkInputs checks the returned Ark witness before
// the sender advances to checkpoint signing.
func TestSubmitAcceptedValidatesArkInputs(t *testing.T) {
	t.Parallel()

	for _, mutation := range []string{"missing", "invalid"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()

			ark, checkpoints, _, _, _, operatorKey :=
				buildTestIncomingMaterialization(t)
			sessionID := SessionID(ark.UnsignedTx.TxHash())
			outpoint := ark.UnsignedTx.TxIn[0].PreviousOutPoint
			state := &AwaitingSubmitAccepted{
				ArkPSBT: ark,
				TransferInputs: []TransferInput{{
					VTXO: &vtxo.Descriptor{
						Outpoint: outpoint,
					},
					ReserveEpoch: 7,
				}},
			}
			returned := copyTestArkPSBT(t, ark)
			mutateTestArkSignature(
				t, returned, operatorKey, mutation,
			)
			require.Equal(
				t, ark.UnsignedTx.TxHash(),
				returned.UnsignedTx.TxHash(),
			)

			transition, err := state.ProcessEvent(
				t.Context(), &SubmitAcceptedEvent{
					SessionID:               sessionID,
					ArkPSBT:                 returned,
					CoSignedCheckpointPSBTs: checkpoints,
				}, nil,
			)
			require.NoError(t, err)
			failed, ok := transition.NextState.(*Failed)
			require.True(t, ok)
			require.True(t, failed.PrePONR)
			require.Contains(t, failed.Reason, "ark input 0")
			require.True(t, transition.NewEvents.IsSome())
			emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
			require.Len(t, emitted.Outbox, 1)
			release, ok := emitted.Outbox[0].(*ReleaseInputsRequest)
			require.True(t, ok)
			require.Equal(t, []wire.OutPoint{
				outpoint,
			}, release.Outpoints)
			require.Equal(
				t, uint64(7), release.ReserveEpochs[outpoint],
			)
		})
	}
}

// TestIncomingPackageValidatesArkInputs checks the Ark witness in the exact
// package supplied to the receiver before the receive event is admitted.
func TestIncomingPackageValidatesArkInputs(t *testing.T) {
	t.Parallel()

	for _, mutation := range []string{"missing", "invalid"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()

			resp, sessionID, _, eventID, operatorKey :=
				buildIncomingResolveResponse(t)
			ark, err := psbtutil.Parse(resp.Events[0].ArkPsbt)
			require.NoError(t, err)
			mutateTestArkSignature(t, ark, operatorKey, mutation)
			require.Equal(
				t, sessionID,
				SessionID(
					ark.UnsignedTx.TxHash(),
				),
			)
			resp.Events[0].ArkPsbt, err = psbtutil.Serialize(ark)
			require.NoError(t, err)

			_, err = IncomingTransferEventFromResponse(
				sessionID, eventID, resp,
			)
			require.ErrorContains(t, err, "ark input 0")
		})
	}
}

// copyTestArkPSBT makes an independent wire-format copy for event mutation.
func copyTestArkPSBT(t *testing.T, ark *psbt.Packet) *psbt.Packet {
	t.Helper()

	raw, err := psbtutil.Serialize(ark)
	require.NoError(t, err)
	copiedArk, err := psbtutil.Parse(raw)
	require.NoError(t, err)

	return copiedArk
}

// mutateTestArkSignature changes only the operator's input witness material.
func mutateTestArkSignature(t *testing.T, ark *psbt.Packet,
	operatorKey *btcec.PublicKey, mutation string) {

	t.Helper()

	input := &ark.Inputs[0]
	require.Len(t, input.TaprootScriptSpendSig, 2)
	operatorIndex := -1
	for i, sig := range input.TaprootScriptSpendSig {
		if bytes.Equal(
			sig.XOnlyPubKey, schnorr.SerializePubKey(operatorKey),
		) {

			operatorIndex = i
			break
		}
	}
	require.NotEqual(t, -1, operatorIndex)
	switch mutation {
	case "missing":
		input.TaprootScriptSpendSig = append(
			input.TaprootScriptSpendSig[:operatorIndex],
			input.TaprootScriptSpendSig[operatorIndex+1:]...,
		)

	case "invalid":
		input.TaprootScriptSpendSig[operatorIndex].Signature =
			bytes.Repeat([]byte{0x01}, 64)

	default:
		t.Fatalf("unknown mutation %q", mutation)
	}
}
