package round

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/batchcanon"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// bcRef aliases the canonicality manager tell-ref to keep the test helper
// signatures within the line limit.
type bcRef = actor.TellOnlyRef[batchcanon.ManagerMsg]

// bcTestOutpoint builds a deterministic outpoint from a single seed byte.
func bcTestOutpoint(seed byte) wire.OutPoint {
	var h chainhash.Hash
	h[0] = seed

	return wire.OutPoint{Hash: h, Index: uint32(seed)}
}

// newBatchCanonActor builds a minimal RoundClientActor wired with the given
// canonicality ref option. Only the fields registerBatchCanonicality touches
// are populated.
func newBatchCanonActor(ref fn.Option[bcRef]) *RoundClientActor {
	return &RoundClientActor{
		cfg: &RoundClientConfig{
			BatchCanonicality: ref,
		},
		log: btclog.Disabled,
	}
}

// TestRegisterBatchCanonicalityEmitsRequest verifies the round actor forwards a
// RegisterBatchRequest carrying the batch txid, consumed inputs (boarding +
// forfeited), dependent VTXO outpoints, confirmation pkScript and CSV delta
// when a canonicality manager ref is wired.
func TestRegisterBatchCanonicalityEmitsRequest(t *testing.T) {
	t.Parallel()

	ref := actor.NewChannelTellOnlyRef[batchcanon.ManagerMsg](
		"batchcanon-test", 2,
	)
	a := newBatchCanonActor(
		fn.Some[bcRef](ref),
	)

	var commitment chainhash.Hash
	commitment[0] = 0xaa
	board := bcTestOutpoint(1)
	forfeit := bcTestOutpoint(2)
	vtxoOut := bcTestOutpoint(3)
	pkScript := []byte{0x51, 0x20, 0x01}
	consumed := []batchcanon.ConsumedInput{
		{
			Outpoint: board,
			PkScript: []byte{
				0x51,
				0x20,
				0x0b,
			},
		},
		{
			Outpoint: forfeit,
			PkScript: []byte{
				0x51,
				0x20,
				0x0f,
			},
		},
	}

	a.registerBatchCanonicality(t.Context(), &VTXOCreatedNotification{
		VTXOs:                []*ClientVTXO{{Outpoint: vtxoOut}},
		CommitmentTxID:       commitment,
		ConsumedInputs:       consumed,
		ConfirmationPkScript: pkScript,
		CSVExpiryDelta:       144,
	})

	msg, ok := ref.AwaitMessage(time.Second)
	require.True(t, ok, "expected a RegisterBatchRequest")

	req, ok := msg.(*batchcanon.RegisterBatchRequest)
	require.True(t, ok)
	require.Equal(t, commitment, req.BatchTxID)
	require.Equal(t, consumed, req.ConsumedInputs)
	require.Equal(t, []wire.OutPoint{vtxoOut}, req.DependentVTXOs)
	require.Equal(t, pkScript, req.ConfirmationPkScript)
	require.Equal(t, int32(144), req.CSVExpiryDelta)
}

// TestRegisterBatchCanonicalityNoopWhenUnwired verifies registration is a
// no-op when no manager ref is configured (the gate stays dormant), preserving
// pre-C6 behavior.
func TestRegisterBatchCanonicalityNoopWhenUnwired(t *testing.T) {
	t.Parallel()

	a := newBatchCanonActor(
		fn.None[bcRef](),
	)

	// Must not panic and must not attempt any delivery.
	a.registerBatchCanonicality(t.Context(), &VTXOCreatedNotification{
		VTXOs: []*ClientVTXO{{Outpoint: bcTestOutpoint(3)}},
	})
}

// TestRegisterBatchCanonicalitySkipsEmptyBatch verifies nothing is emitted when
// the round produced no owned VTXOs and consumed no client inputs (nothing for
// the gate to govern).
func TestRegisterBatchCanonicalitySkipsEmptyBatch(t *testing.T) {
	t.Parallel()

	ref := actor.NewChannelTellOnlyRef[batchcanon.ManagerMsg](
		"batchcanon-empty", 1,
	)
	a := newBatchCanonActor(
		fn.Some[bcRef](ref),
	)

	a.registerBatchCanonicality(t.Context(), &VTXOCreatedNotification{})

	_, ok := ref.AwaitMessage(100 * time.Millisecond)
	require.False(t, ok, "no registration expected for an empty batch")
}
