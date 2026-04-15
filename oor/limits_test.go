package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// TestEnforceSubmitRequestLimits verifies the size-bounded fields of
// a SubmitOORRequest are capped before any expensive downstream work
// can run on an oversized payload.
func TestEnforceSubmitRequestLimits(t *testing.T) {
	t.Parallel()

	t.Run("nil request is a no-op", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, enforceSubmitRequestLimits(nil))
	})

	t.Run("checkpoint count at cap accepted", func(t *testing.T) {
		t.Parallel()

		msg := &SubmitOORRequest{
			CheckpointPSBTs: make(
				[]*psbt.Packet, MaxCheckpointPSBTsPerRequest,
			),
		}
		require.NoError(t, enforceSubmitRequestLimits(msg))
	})

	t.Run("checkpoint count above cap rejected", func(t *testing.T) {
		t.Parallel()

		msg := &SubmitOORRequest{
			CheckpointPSBTs: make(
				[]*psbt.Packet,
				MaxCheckpointPSBTsPerRequest+1,
			),
		}

		err := enforceSubmitRequestLimits(msg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "checkpoint PSBTs")
	})

	t.Run("descriptor count above cap rejected", func(t *testing.T) {
		t.Parallel()

		msg := &SubmitOORRequest{
			VTXOSigningDescriptors: make(
				[]VTXOSigningDescriptor,
				MaxVTXOSigningDescriptorsPerRequest+1,
			),
		}

		err := enforceSubmitRequestLimits(msg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "signing descriptors")
	})

	t.Run("recipient count above cap rejected", func(t *testing.T) {
		t.Parallel()

		msg := &SubmitOORRequest{
			Recipients: make(
				[]oorlib.RecipientOutput,
				MaxRecipientOutputsPerRequest+1,
			),
		}

		err := enforceSubmitRequestLimits(msg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "recipients")
	})
}

// TestEnforceFinalizeRequestLimits verifies the finalize payload cap
// on the number of fully-signed checkpoint PSBTs.
func TestEnforceFinalizeRequestLimits(t *testing.T) {
	t.Parallel()

	t.Run("nil request is a no-op", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, enforceFinalizeRequestLimits(nil))
	})

	t.Run("count at cap accepted", func(t *testing.T) {
		t.Parallel()

		msg := &FinalizeOORRequest{
			FinalCheckpointPSBTs: make(
				[]*psbt.Packet, MaxCheckpointPSBTsPerRequest,
			),
		}
		require.NoError(t, enforceFinalizeRequestLimits(msg))
	})

	t.Run("count above cap rejected", func(t *testing.T) {
		t.Parallel()

		msg := &FinalizeOORRequest{
			FinalCheckpointPSBTs: make(
				[]*psbt.Packet,
				MaxCheckpointPSBTsPerRequest+1,
			),
		}

		err := enforceFinalizeRequestLimits(msg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "checkpoint PSBTs")
	})
}

// TestDeserializePSBTRejectsOversizedBlob verifies deserializePSBT
// refuses to parse a PSBT blob larger than MaxPSBTBytesPerRequest.
func TestDeserializePSBTRejectsOversizedBlob(t *testing.T) {
	t.Parallel()

	// Build an oversized byte buffer. The bytes do not need to be a
	// valid PSBT: the size cap fires before psbt.NewFromRawBytes.
	blob := make([]byte, MaxPSBTBytesPerRequest+1)

	_, err := deserializePSBT(blob)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds max")
}
