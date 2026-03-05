package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

// TestDriveEventEncodeRejectsSubmitAcceptedSessionMismatch verifies that
// encoding a DriveEventRequest fails when the SubmitAcceptedEvent's inner
// session ID differs from the outer request's session ID.
func TestDriveEventEncodeRejectsSubmitAcceptedSessionMismatch(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	otherSessionID := SessionID(chainhash.Hash{4, 4, 4})
	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event: &SubmitAcceptedEvent{
			SessionID:               otherSessionID,
			ArkPSBT:                 ark,
			CoSignedCheckpointPSBTs: checkpoints,
		},
	}

	var buf bytes.Buffer
	err = msg.Encode(&buf)
	require.ErrorContains(
		t, err, "submit accepted event session id mismatch",
	)
}

// TestDriveEventEncodeRejectsSubmitAcceptedArkMismatch verifies that
// encoding a DriveEventRequest fails when the SubmitAcceptedEvent's Ark
// PSBT txid doesn't match the session ID.
func TestDriveEventEncodeRejectsSubmitAcceptedArkMismatch(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	tamperedArk := clonePSBTForDriveEventTest(t, ark)
	tamperedArk.UnsignedTx.TxOut[0].Value++

	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event: &SubmitAcceptedEvent{
			SessionID:               sessionID,
			ArkPSBT:                 tamperedArk,
			CoSignedCheckpointPSBTs: checkpoints,
		},
	}

	var buf bytes.Buffer
	err = msg.Encode(&buf)
	require.ErrorContains(
		t, err, "submit accepted event ark txid mismatch",
	)
}

func clonePSBTForDriveEventTest(t *testing.T, pkt *psbt.Packet) *psbt.Packet {
	t.Helper()

	raw, err := psbtutil.Serialize(pkt)
	require.NoError(t, err)

	cloned, err := psbtutil.Parse(raw)
	require.NoError(t, err)

	return cloned
}
