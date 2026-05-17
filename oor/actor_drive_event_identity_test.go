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

func clonePSBTForDriveEventTest(t *testing.T, pkt *psbt.Packet) *psbt.Packet {
	t.Helper()

	raw, err := psbtutil.Serialize(pkt)
	require.NoError(t, err)

	cloned, err := psbtutil.Parse(raw)
	require.NoError(t, err)

	return cloned
}
