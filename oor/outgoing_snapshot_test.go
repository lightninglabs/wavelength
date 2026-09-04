package oor

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// TestNewOutgoingSnapshotFinalizeSentMinimality verifies finalize-sent
// snapshots persist only the artifacts needed for deterministic retry/resume.
func TestNewOutgoingSnapshotFinalizeSentMinimality(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	const inputValue = btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	input := newTestTransferInput(
		t, clientKey, policy.OperatorKey, wire.OutPoint{
			Hash:  [32]byte{0x01},
			Index: 0,
		}, inputValue,
	)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	ark, checkpoints, err := buildSubmitPackage(
		policy, []TransferInput{input}, recipients,
	)
	require.NoError(t, err)

	state := &AwaitingFinalizeAccepted{
		SessionID:            SessionID(ark.UnsignedTx.TxHash()),
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
		TransferInputs: []TransferInput{
			input,
		},
	}

	snapshot, err := NewOutgoingSnapshot(state.SessionID, state)
	require.NoError(t, err)

	require.Equal(t, OutgoingPhaseFinalizeSent, snapshot.Phase)
	require.NotEmpty(t, snapshot.ArkPSBT)
	require.NotEmpty(t, snapshot.CheckpointPSBTs)
	require.NotNil(t, snapshot.TransferInputSnapshots)
	require.Len(t, snapshot.TransferInputSnapshots, 1)
}

// TestOutgoingRegistryRecordSuppliesNormalizedReplayProof verifies the
// registry bridge records the trusted canonical recipient list before submit
// and does not derive replacement proof from a later terminal snapshot.
func TestOutgoingRegistryRecordSuppliesNormalizedReplayProof(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipientKey1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipientKey2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		idempotencyKey = "artifact-proof-key"
		inputValue     = btcutil.Amount(10_000)
		recipientValue = inputValue / 2
	)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}
	input := newTestTransferInput(
		t, clientKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x02},
			Index: 0,
		}, inputValue,
	)

	// Start with equal-value recipients in the opposite of canonical
	// pkScript order. This pins the tie-break that determines each stored
	// positional outpoint and covers a pair the old value/script lookup
	// could not safely disambiguate by amount alone.
	canonicalRecipients := oortx.CanonicalRecipientOutputs(
		[]oortx.RecipientOutput{
			{
				Value: recipientValue,
				PkScript: newTestTaprootPkScript(
					t, recipientKey1.PubKey(),
				),
			},
			{
				Value: recipientValue,
				PkScript: newTestTaprootPkScript(
					t, recipientKey2.PubKey(),
				),
			},
		},
	)
	nonCanonicalRecipients := []oortx.RecipientOutput{
		canonicalRecipients[1], canonicalRecipients[0],
	}
	require.NotEqual(t, canonicalRecipients, nonCanonicalRecipients)

	start, err := (&Idle{}).ProcessEvent(
		t.Context(), &StartTransferEvent{
			VTXOInputs:       []TransferInput{input},
			RecipientOutputs: nonCanonicalRecipients,
			Policy:           policy,
			IdempotencyKey:   idempotencyKey,
		}, nil,
	)
	require.NoError(t, err)
	awaitingSignatures, ok := start.NextState.(*AwaitingArkSignatures)
	require.True(t, ok)
	require.Equal(
		t, canonicalRecipients, awaitingSignatures.RecipientOutputs,
	)

	submit, err := awaitingSignatures.ProcessEvent(
		t.Context(), &ArkSignedEvent{
			ArkPSBT: awaitingSignatures.ArkPSBT,
		}, nil,
	)
	require.NoError(t, err)
	awaiting, ok := submit.NextState.(*AwaitingSubmitAccepted)
	require.True(t, ok)

	sessionID, err := sessionIDFromArk(awaiting.ArkPSBT)
	require.NoError(t, err)
	arkRecipients, err := ExtractArkRecipients(awaiting.ArkPSBT)
	require.NoError(t, err)
	require.Len(t, arkRecipients, 2)

	proofRecord, err := outgoingRegistryRecord(sessionID, awaiting)
	require.NoError(t, err)
	require.NotEmpty(t, proofRecord.DispatchRequestData)

	recipients, err := OutgoingReplayRecipients(
		proofRecord.DispatchRequestData,
	)
	require.NoError(t, err)
	require.Equal(t, arkRecipients, recipients)

	terminalRecord, err := outgoingRegistryRecord(
		sessionID, &Completed{
			IdempotencyKey: idempotencyKey,
		},
	)
	require.NoError(t, err)
	require.Equal(t, idempotencyKey, terminalRecord.IdempotencyKey)
	require.Empty(t, terminalRecord.DispatchRequestData)
	require.Equal(
		t, clientdb.OORSessionStatusCompleted, terminalRecord.Status,
	)

	_, err = OutgoingReplayRecipients(terminalRecord.SnapshotData)
	require.Error(t, err)
}

// TestOutgoingRegistryRecordDoesNotDeriveProofAfterSubmit verifies a later
// checkpoint never parses the Ark PSBT to rebuild replay proof. A malformed
// artifact can still be durably checkpointed and replay remains fail closed.
func TestOutgoingRegistryRecordDoesNotDeriveProofAfterSubmit(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)

	// Move the anchor before the recipient to violate canonical output
	// ordering without making the PSBT unserializable.
	ark.UnsignedTx.TxOut[0], ark.UnsignedTx.TxOut[1] =
		ark.UnsignedTx.TxOut[1], ark.UnsignedTx.TxOut[0]
	ark.Outputs[0], ark.Outputs[1] = ark.Outputs[1], ark.Outputs[0]

	sessionID := SessionID(ark.UnsignedTx.TxHash())
	record, err := outgoingRegistryRecord(
		sessionID, &AwaitingFinalizeAccepted{
			SessionID:            sessionID,
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
			TransferInputs:       testRetryTransferInputs(t),
			IdempotencyKey:       "invalid-proof-key",
		},
	)
	require.NoError(t, err)
	require.Empty(t, record.DispatchRequestData)

	_, err = OutgoingReplayRecipients(record.DispatchRequestData)
	require.Error(t, err)
}

// TestOutgoingReplayRecipientsRejectsMalformedProof verifies replay accepts
// only well-formed versioned positional records.
func TestOutgoingReplayRecipientsRejectsMalformedProof(t *testing.T) {
	t.Parallel()

	recipients := []oortx.RecipientOutput{{
		Value: 10_000,
		PkScript: []byte{
			0x51,
			0x20,
			0x01,
		},
	}}
	proof, err := newOutgoingReplayData(recipients)
	require.NoError(t, err)

	decoded, err := OutgoingReplayRecipients(proof)
	require.NoError(t, err)
	require.Equal(t, uint32(0), decoded[0].OutputIndex)
	require.Equal(t, recipients[0].Value, decoded[0].Value)
	require.Equal(t, recipients[0].PkScript, decoded[0].PkScript)

	unknownVersion := append([]byte(nil), proof...)
	unknownVersion[0] = 0xff
	_, err = OutgoingReplayRecipients(unknownVersion)
	require.ErrorContains(t, err, "unknown outgoing replay proof version")

	trailing := append(append([]byte(nil), proof...), 0x00)
	_, err = OutgoingReplayRecipients(trailing)
	require.ErrorContains(t, err, "trailing bytes")

	truncated := proof[:len(proof)-1]
	_, err = OutgoingReplayRecipients(truncated)
	require.ErrorContains(t, err, "pkScript is truncated")
}

// TestOutgoingReplayDataBindsMaxVTXOAgeBlocks verifies version-two request
// identity round-trips the freshness limit and canonical exact input set while
// legacy proofs decode as backward-compatible unconstrained requests.
func TestOutgoingReplayDataBindsMaxVTXOAgeBlocks(t *testing.T) {
	t.Parallel()

	recipients := []oortx.RecipientOutput{
		{
			Value: 10_000,
			PkScript: []byte{
				0x51, 0x20, 0x01,
			},
		},
	}
	exactInputs := []wire.OutPoint{
		{
			Hash: [32]byte{
				0x02,
			},
			Index: 2,
		},
		{
			Hash: [32]byte{
				0x01,
			},
			Index: 3,
		},
	}

	proof, err := NewOutgoingReplayDataWithMaxVTXOAgeBlocks(
		recipients, recipients, 144, exactInputs,
	)
	require.NoError(t, err)
	reorderedProof, err := NewOutgoingReplayDataWithMaxVTXOAgeBlocks(
		recipients, recipients, 144,
		[]wire.OutPoint{exactInputs[1], exactInputs[0]},
	)
	require.NoError(t, err)
	require.Equal(t, proof, reorderedProof)

	decoded, err := DecodeOutgoingReplayData(proof)
	require.NoError(t, err)
	require.Equal(t, uint32(144), decoded.MaxVTXOAgeBlocks)
	require.Equal(t, []wire.OutPoint{
		exactInputs[1], exactInputs[0],
	}, decoded.ExactInputOutpoints)
	require.Len(t, decoded.Recipients, 1)
	require.Equal(t, recipients[0].Value, decoded.Recipients[0].Value)
	require.Equal(
		t, recipients[0].PkScript, decoded.Recipients[0].PkScript,
	)

	legacyProof, err := NewOutgoingReplayData(recipients, recipients)
	require.NoError(t, err)
	legacy, err := DecodeOutgoingReplayData(legacyProof)
	require.NoError(t, err)
	require.Zero(t, legacy.MaxVTXOAgeBlocks)
	require.Empty(t, legacy.ExactInputOutpoints)

	_, err = DecodeOutgoingReplayData(proof[:outgoingReplayV2HeaderSize-1])
	require.ErrorContains(t, err, "truncated")

	nonCanonical := append([]byte(nil), proof...)
	inputAStart := outgoingReplayV2HeaderSize
	inputAEnd := inputAStart + outgoingReplayInputSize
	inputA := append(
		[]byte(nil),
		nonCanonical[inputAStart:inputAEnd]...,
	)
	inputBOffset := outgoingReplayV2HeaderSize + outgoingReplayInputSize
	copy(
		nonCanonical[outgoingReplayV2HeaderSize:],
		nonCanonical[inputBOffset:inputBOffset+outgoingReplayInputSize],
	)
	copy(nonCanonical[inputBOffset:], inputA)
	_, err = DecodeOutgoingReplayData(nonCanonical)
	require.ErrorContains(t, err, "not in canonical order")

	_, err = NewOutgoingReplayDataWithMaxVTXOAgeBlocks(
		recipients, recipients, 144,
		[]wire.OutPoint{exactInputs[0], exactInputs[0]},
	)
	require.ErrorContains(t, err, "duplicate outgoing replay input")
}

// TestSnapshotRetryMetadataRoundTrip verifies that RetryAfter and retry
// reason survive TLV encode/decode. This is essential for restart-safe
// retry scheduling: the actor persists retry metadata alongside the real
// protocol state so a restarted actor can schedule a timer instead of
// immediately re-driving the outbox.
func TestSnapshotRetryMetadataRoundTrip(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	const inputValue = btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	input := newTestTransferInput(
		t, clientKey, policy.OperatorKey, wire.OutPoint{
			Hash:  [32]byte{0x02},
			Index: 0,
		}, inputValue,
	)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(
				t, clientKey.PubKey(),
			),
			Value: inputValue,
		},
	}

	ark, checkpoints, err := buildSubmitPackage(
		policy, []TransferInput{input}, recipients,
	)
	require.NoError(t, err)

	sessionID := SessionID(ark.UnsignedTx.TxHash())

	state := &AwaitingSubmitAccepted{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
		TransferInputs: []TransferInput{
			input,
		},
		IdempotencyKey: "funding-key-1",
	}

	// Create a snapshot and apply retry metadata (simulating what the
	// actor does when a retryable outbox error occurs).
	snapshot, err := NewOutgoingSnapshot(sessionID, state)
	require.NoError(t, err)
	require.Equal(t, OutgoingPhaseSubmitSent, snapshot.Phase)

	snapshot.RetryAfter = 3 * time.Second
	snapshot.FailReason = "temporary transport error"

	// Encode and decode the snapshot to simulate checkpoint
	// persistence.
	raw, err := encodeOutgoingSnapshot(snapshot)
	require.NoError(t, err)

	decoded, err := decodeOutgoingSnapshot(raw)
	require.NoError(t, err)

	require.Equal(t, OutgoingPhaseSubmitSent, decoded.Phase)
	require.Equal(t, 3*time.Second, decoded.RetryAfter)
	require.Equal(t, "temporary transport error", decoded.FailReason)
	require.Equal(t, "funding-key-1", decoded.IdempotencyKey)

	// Verify the decoded snapshot can restore the original state.
	restored, err := OutgoingStateFromSnapshot(decoded)
	require.NoError(t, err)
	restoredSubmit, ok := restored.(*AwaitingSubmitAccepted)
	require.True(t, ok)
	require.Equal(t, "funding-key-1",
		restoredSubmit.IdempotencyKey)
}
