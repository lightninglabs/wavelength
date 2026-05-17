package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

// TestValidateSubmitAcceptedRejectsSessionMismatch verifies that
// validateSubmitAcceptedIdentity rejects a SubmitAcceptedEvent whose inner
// session ID differs from the outer request's session ID.
func TestValidateSubmitAcceptedRejectsSessionMismatch(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	otherSessionID := SessionID(chainhash.Hash{4, 4, 4})

	err = validateSubmitAcceptedIdentity(sessionID, &SubmitAcceptedEvent{
		SessionID:               otherSessionID,
		ArkPSBT:                 ark,
		CoSignedCheckpointPSBTs: checkpoints,
	})
	require.ErrorContains(
		t, err, "submit accepted event session id mismatch",
	)
}

// TestValidateSubmitAcceptedRejectsArkMismatch verifies that
// validateSubmitAcceptedIdentity rejects a SubmitAcceptedEvent whose Ark
// PSBT txid doesn't match the session ID.
func TestValidateSubmitAcceptedRejectsArkMismatch(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	tamperedArk := clonePSBTForDriveEventTest(t, ark)
	tamperedArk.UnsignedTx.TxOut[0].Value++

	err = validateSubmitAcceptedIdentity(sessionID, &SubmitAcceptedEvent{
		SessionID:               sessionID,
		ArkPSBT:                 tamperedArk,
		CoSignedCheckpointPSBTs: checkpoints,
	})
	require.ErrorContains(
		t, err, "submit accepted event ark txid mismatch",
	)
}

// TestDriveEventPayloadEncodesNilArkPSBT verifies that the domain payload
// codec accepts a SubmitAcceptedEvent with nil ArkPSBT. This supports the
// server-push EventRouter path where the oorpb proto does not echo the Ark
// PSBT back.
func TestDriveEventPayloadEncodesNilArkPSBT(t *testing.T) {
	t.Parallel()

	_, checkpoints := testOutboxPSBTPair(t)
	sessionID := SessionID(chainhash.Hash{1, 2, 3})

	raw, err := encodeDriveEventRequestPayload(
		sessionID, &SubmitAcceptedEvent{
			SessionID:               sessionID,
			ArkPSBT:                 nil,
			CoSignedCheckpointPSBTs: checkpoints,
		},
	)
	require.NoError(t, err)

	decodedSessionID, decodedEvent, err :=
		decodeDriveEventRequestPayloadWithLimits(raw, ReceiveLimits{})
	require.NoError(t, err)
	require.Equal(t, sessionID, decodedSessionID)

	submitEvt, ok := decodedEvent.(*SubmitAcceptedEvent)
	require.True(t, ok)
	require.Nil(t, submitEvt.ArkPSBT)
	require.Equal(t, sessionID, submitEvt.SessionID)
	require.Len(t, submitEvt.CoSignedCheckpointPSBTs, len(checkpoints))
}

func clonePSBTForDriveEventTest(t *testing.T, pkt *psbt.Packet) *psbt.Packet {
	t.Helper()

	raw, err := psbtutil.Serialize(pkt)
	require.NoError(t, err)

	cloned, err := psbtutil.Parse(raw)
	require.NoError(t, err)

	return cloned
}
