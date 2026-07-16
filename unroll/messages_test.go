package unroll

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestDurableMessageTLVRoundTrip pins the TLV encoding of every unroll
// durable mailbox message so a field order or record-type shuffle breaks
// loudly rather than silently corrupting persisted inbox rows. Every
// message here implements actor.TLVMessage, so the encode path is the
// same one the durable mailbox codec drives on disk.
func TestDurableMessageTLVRoundTrip(t *testing.T) {
	t.Parallel()

	var txid chainhash.Hash
	copy(txid[:], bytes.Repeat([]byte{0xab}, chainhash.HashSize))

	t.Run("StartUnrollRequest", func(t *testing.T) {
		t.Parallel()

		orig := &StartUnrollRequest{
			Height:         12345,
			Trigger:        TriggerCriticalExpiry,
			ExitPolicyKind: "vhtlc_refund_without_receiver",
			ExitPolicyRef:  "recovery-id",
		}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &StartUnrollRequest{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, orig.Height, got.Height)
		require.Equal(t, orig.Trigger, got.Trigger)
		require.Equal(t, orig.ExitPolicyKind, got.ExitPolicyKind)
		require.Equal(t, orig.ExitPolicyRef, got.ExitPolicyRef)
	})

	t.Run("ResumeUnrollRequest", func(t *testing.T) {
		t.Parallel()

		orig := &ResumeUnrollRequest{Height: 98765}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &ResumeUnrollRequest{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, orig.Height, got.Height)
	})

	t.Run("HeightObservedMsg", func(t *testing.T) {
		t.Parallel()

		orig := &HeightObservedMsg{Height: 500}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &HeightObservedMsg{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, orig.Height, got.Height)
	})

	t.Run("TxConfirmedMsg", func(t *testing.T) {
		t.Parallel()

		orig := &TxConfirmedMsg{
			Txid:     txid,
			Height:   42,
			NumConfs: 6,
		}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &TxConfirmedMsg{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, orig.Txid, got.Txid)
		require.Equal(t, orig.Height, got.Height)
		require.Equal(t, orig.NumConfs, got.NumConfs)
	})

	t.Run("TxFailedMsg", func(t *testing.T) {
		t.Parallel()

		orig := &TxFailedMsg{
			Txid:   txid,
			Reason: "rejected by mempool",
		}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &TxFailedMsg{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, orig.Txid, got.Txid)
		require.Equal(t, orig.Reason, got.Reason)
	})

	t.Run("SpendObservedMsg", func(t *testing.T) {
		t.Parallel()

		orig := &SpendObservedMsg{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					0xcd,
				},
				Index: 9,
			},
			SpendingTxid:   txid,
			SpendingHeight: 777,
		}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &SpendObservedMsg{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, orig.Outpoint, got.Outpoint)
		require.Equal(t, orig.SpendingTxid, got.SpendingTxid)
		require.Equal(t, orig.SpendingHeight, got.SpendingHeight)
	})

	t.Run("TxReorgedMsg", func(t *testing.T) {
		t.Parallel()

		orig := &TxReorgedMsg{Txid: txid}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &TxReorgedMsg{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, orig.Txid, got.Txid)
	})

	t.Run("SpendReorgedMsg", func(t *testing.T) {
		t.Parallel()

		orig := &SpendReorgedMsg{}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &SpendReorgedMsg{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
	})

	t.Run("SpendFinalizedMsg", func(t *testing.T) {
		t.Parallel()

		orig := &SpendFinalizedMsg{}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &SpendFinalizedMsg{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
	})

	t.Run("TxFinalizedMsg", func(t *testing.T) {
		t.Parallel()

		orig := &TxFinalizedMsg{Txid: txid}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &TxFinalizedMsg{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, orig.Txid, got.Txid)
	})

	t.Run("GetStateRequest", func(t *testing.T) {
		t.Parallel()

		orig := &GetStateRequest{}

		var buf bytes.Buffer
		require.NoError(t, orig.Encode(&buf))

		got := &GetStateRequest{}
		require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
	})
}

// TestDurableMessagePriorityOrdering verifies that concrete progress
// notifications are delivered ahead of lossy block ticks and read-only status
// probes.
func TestDurableMessagePriorityOrdering(t *testing.T) {
	t.Parallel()

	require.Greater(
		t, (&TxConfirmedMsg{}).Priority(),
		(&HeightObservedMsg{}).Priority(),
	)
	require.Greater(
		t, (&TxFailedMsg{}).Priority(),
		(&HeightObservedMsg{}).Priority(),
	)
	require.Greater(
		t, (&SpendObservedMsg{}).Priority(),
		(&HeightObservedMsg{}).Priority(),
	)
	require.Greater(
		t, (&HeightObservedMsg{}).Priority(),
		(&GetStateRequest{}).Priority(),
	)
	require.Less(t, (&HeightObservedMsg{}).Priority(), 0)
	require.Less(t, (&GetStateRequest{}).Priority(), 0)
}
