package recovery

import (
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestNewSessionRejectsNilProof verifies the constructor's nil guard.
func TestNewSessionRejectsNilProof(t *testing.T) {
	_, err := NewSession(nil)
	require.ErrorContains(t, err, "proof cannot be nil")
}

// TestMarkConfirmedRejectsNegativeHeight guards against the overflow path
// that would otherwise turn a poisoned confirm height into a premature
// sweep-ready signal.
func TestMarkConfirmedRejectsNegativeHeight(t *testing.T) {
	session := newMergeSession(t)
	txid := session.Proof().RootTxids()[0]

	require.NoError(t, session.MarkBroadcasted(txid))
	err := session.MarkConfirmed(txid, -1)
	require.ErrorContains(t, err, "negative")
}

// TestMarkConfirmedRejectsUnbroadcastedTx verifies the state-machine guard
// that a tx must be broadcast before it can be marked confirmed.
func TestMarkConfirmedRejectsUnbroadcastedTx(t *testing.T) {
	session := newMergeSession(t)
	txid := session.Proof().RootTxids()[0]

	err := session.MarkConfirmed(txid, 100)
	require.ErrorContains(t, err, "cannot confirm before broadcast")
}

// TestMarkConfirmedIdempotentAtSameHeight re-confirming the same txid at the
// same height should be a no-op rather than an error.
func TestMarkConfirmedIdempotentAtSameHeight(t *testing.T) {
	session := newMergeSession(t)
	txid := session.Proof().RootTxids()[0]

	require.NoError(t, session.MarkBroadcasted(txid))
	require.NoError(t, session.MarkConfirmed(txid, 100))
	require.NoError(t, session.MarkConfirmed(txid, 100))

	err := session.MarkConfirmed(txid, 200)
	require.ErrorContains(t, err, "cannot reconfirm")
}

// TestMarkConfirmedRequiresParents verifies child cannot be confirmed before
// parent. Without this guard the CSV maturity view can become incoherent
// when reorgs or out-of-order chain notifications race.
func TestMarkConfirmedRequiresParents(t *testing.T) {
	session := newMergeSession(t)
	proof := session.Proof()
	mergeTxid := proof.Layers()[1][0]

	// The merge tx was not broadcast first; also its parents are still
	// pending. Both conditions should cause MarkConfirmed to refuse.
	err := session.MarkConfirmed(mergeTxid, 100)
	require.Error(t, err)
}

// TestMarkFailedRejectsOverwrite verifies the H-6 guard: subsequent failures
// cannot overwrite an existing terminal error.
func TestMarkFailedRejectsOverwrite(t *testing.T) {
	session := newMergeSession(t)
	roots := session.Proof().RootTxids()
	require.Len(t, roots, 2)

	require.NoError(t, session.MarkFailed(roots[0], fmt.Errorf("first")))

	err := session.MarkFailed(roots[1], fmt.Errorf("second"))
	require.ErrorContains(t, err, "session already failed")
}

// TestMarkFailedRejectsNilErrAndUnknownTxid exercises the remaining
// MarkFailed guards.
func TestMarkFailedRejectsNilErrAndUnknownTxid(t *testing.T) {
	session := newMergeSession(t)

	require.ErrorContains(
		t,
		session.MarkFailed(
			chainhash.Hash{0xaa}, nil,
		),
		"failure error cannot be nil",
	)

	require.ErrorContains(
		t,
		session.MarkFailed(
			chainhash.Hash{0xaa}, fmt.Errorf("x"),
		),
		"unknown txid",
	)
}

// TestSessionConcurrencySafety fires MarkBroadcasted / MarkConfirmed /
// SnapshotAt from multiple goroutines to verify the RWMutex prevents the
// concurrent-map-read/write fatal that unsynchronized access would produce.
// Run with -race to confirm.
func TestSessionConcurrencySafety(t *testing.T) {
	session := newMergeSession(t)
	roots := session.Proof().RootTxids()

	var wg sync.WaitGroup
	const iters = 200

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = session.MarkBroadcasted(roots[0])
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_, _ = session.SnapshotAt(int32(i))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_, _ = session.SnapshotAt(int32(i))
		}
	}()

	wg.Wait()
}

// TestComputeMaturityHeightOverflow asserts that csvDelay values larger than
// MaxCSVDelay are rejected even when callers bypass NewProof.
func TestComputeMaturityHeightOverflow(t *testing.T) {
	_, err := ComputeMaturityHeight(100, MaxCSVDelay+1)
	require.ErrorContains(t, err, "exceeds max")

	_, err = ComputeMaturityHeight(-1, 10)
	require.ErrorContains(t, err, "negative")

	// Near-MaxInt32 targetConfirmHeight + any csvDelay > remaining room
	// overflows int32 even though both inputs alone are in-range.
	_, err = ComputeMaturityHeight(2_147_483_000, MaxCSVDelay)
	require.ErrorContains(t, err, "overflows")
}

// TestNewProofRejectsOversizedCSV validates the NewProof entry-point guard
// rather than just the helper.
func TestNewProofRejectsOversizedCSV(t *testing.T) {
	tx := wire.NewMsgTx(1)
	tx.AddTxIn(&wire.TxIn{Sequence: wire.MaxTxInSequenceNum})
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	_, err := NewProof(
		wire.OutPoint{
			Hash: tx.TxHash(),
		},
		MaxCSVDelay+1, &Node{
			Kind: NodeKindTree,
			Tx:   tx,
		},
	)
	require.ErrorContains(t, err, "csv delay")
}

// TestNewSessionFromStateRejectsBadParents exercises the H-2 parent-confirmed
// invariant for persisted state.
func TestNewSessionFromStateRejectsBadParents(t *testing.T) {
	session := newMergeSession(t)
	proof := session.Proof()
	mergeTxid := proof.Layers()[1][0]

	// Manually craft a state that marks the merge tx confirmed without
	// confirming its parents. Before H-2 this was silently accepted.
	bad := &SessionState{
		TxStates: map[chainhash.Hash]TxState{},
		ConfirmHeights: map[chainhash.Hash]int32{
			mergeTxid: 100,
		},
	}
	for txid := range session.txStates {
		bad.TxStates[txid] = TxStatePending
	}
	bad.TxStates[mergeTxid] = TxStateConfirmed

	_, err := NewSessionFromState(proof, bad)
	require.ErrorContains(t, err, "confirmed with unconfirmed parent")
}
