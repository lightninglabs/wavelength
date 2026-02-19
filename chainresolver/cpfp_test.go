package chainresolver

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// mockCPFPWallet implements CPFPWallet for testing. It records calls and
// returns configurable results.
type mockCPFPWallet struct {
	// fundPsbtCalled tracks whether FundPsbt was invoked.
	fundPsbtCalled bool

	// fundPsbtFeeRate records the fee rate passed to FundPsbt.
	fundPsbtFeeRate chainfee.SatPerKWeight

	// fundPsbtMinConfs records the minConfs passed to FundPsbt.
	fundPsbtMinConfs int32

	// fundPsbtErr is the error to return from FundPsbt.
	fundPsbtErr error

	// finalizePsbtCalled tracks whether FinalizePsbt was invoked.
	finalizePsbtCalled bool

	// finalizePsbtResult is the transaction to return from
	// FinalizePsbt.
	finalizePsbtResult *wire.MsgTx

	// finalizePsbtErr is the error to return from FinalizePsbt.
	finalizePsbtErr error
}

// FundPsbt records the call and returns the configured result.
func (m *mockCPFPWallet) FundPsbt(_ context.Context,
	_ *psbt.Packet, minConfs int32,
	feeRate chainfee.SatPerKWeight,
	_ string) (int32, error) {

	m.fundPsbtCalled = true
	m.fundPsbtFeeRate = feeRate
	m.fundPsbtMinConfs = minConfs

	return 0, m.fundPsbtErr
}

// FinalizePsbt records the call and returns the configured result.
func (m *mockCPFPWallet) FinalizePsbt(_ context.Context,
	_ *psbt.Packet) (*wire.MsgTx, error) {

	m.finalizePsbtCalled = true

	return m.finalizePsbtResult, m.finalizePsbtErr
}

// newTestParentTx creates a minimal transaction with a P2A anchor output
// at the specified index. Non-anchor outputs use a dummy P2TR script.
func newTestParentTx(anchorIdx int, numOutputs int) *wire.MsgTx {
	tx := wire.NewMsgTx(2)

	// Add a dummy input so the tx serializes properly.
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: testOutpoint(99),
	})

	for i := 0; i < numOutputs; i++ {
		if i == anchorIdx {
			tx.AddTxOut(scripts.AnchorOutput())
		} else {
			tx.AddTxOut(&wire.TxOut{
				Value: 100000,
				PkScript: bytes.Repeat(
					[]byte{0xab}, 34,
				),
			})
		}
	}

	return tx
}

// TestFindAnchorOutput_Found verifies that findAnchorOutput returns the
// correct index when a P2A anchor output is present.
func TestFindAnchorOutput_Found(t *testing.T) {
	t.Parallel()

	// Anchor as the last output (typical tree tx layout).
	tx := newTestParentTx(2, 3)
	require.Equal(t, 2, findAnchorOutput(tx))

	// Anchor as the first output.
	tx = newTestParentTx(0, 3)
	require.Equal(t, 0, findAnchorOutput(tx))

	// Anchor as the middle output.
	tx = newTestParentTx(1, 3)
	require.Equal(t, 1, findAnchorOutput(tx))
}

// TestFindAnchorOutput_NotFound verifies that findAnchorOutput returns
// -1 when no P2A anchor output is present.
func TestFindAnchorOutput_NotFound(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{
		Value:    100000,
		PkScript: bytes.Repeat([]byte{0xab}, 34),
	})

	require.Equal(t, -1, findAnchorOutput(tx))
}

// TestFindAnchorOutput_NoOutputs verifies that findAnchorOutput returns
// -1 for a transaction with no outputs.
func TestFindAnchorOutput_NoOutputs(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	require.Equal(t, -1, findAnchorOutput(tx))
}

// TestFindAnchorOutput_NilTx verifies that findAnchorOutput returns -1
// for a nil transaction.
func TestFindAnchorOutput_NilTx(t *testing.T) {
	t.Parallel()

	require.Equal(t, -1, findAnchorOutput(nil))
}

// TestBuildCPFPChildPSBT_Valid verifies that buildCPFPChildPSBT creates
// a correct PSBT template with the anchor as an external input.
func TestBuildCPFPChildPSBT_Valid(t *testing.T) {
	t.Parallel()

	parentTx := newTestParentTx(2, 3)
	parentTxid := parentTx.TxHash()

	pkt, err := buildCPFPChildPSBT(parentTx, 2)
	require.NoError(t, err)
	require.NotNil(t, pkt)

	// The unsigned tx should have version 3 (TRUC).
	require.Equal(t, int32(cpfpTxVersion), pkt.UnsignedTx.Version)

	// Single input: the P2A anchor outpoint.
	require.Len(t, pkt.UnsignedTx.TxIn, 1)
	require.Equal(t, parentTxid,
		pkt.UnsignedTx.TxIn[0].PreviousOutPoint.Hash)
	require.Equal(t, uint32(2),
		pkt.UnsignedTx.TxIn[0].PreviousOutPoint.Index)

	// No outputs (FundPsbt will add them).
	require.Len(t, pkt.UnsignedTx.TxOut, 0)

	// The anchor input should have WitnessUtxo set to a zero-value
	// P2A output, marking it as external.
	require.Len(t, pkt.Inputs, 1)
	require.NotNil(t, pkt.Inputs[0].WitnessUtxo)
	require.Equal(t, int64(0), pkt.Inputs[0].WitnessUtxo.Value)
	require.True(t, bytes.Equal(
		scripts.AnchorPkScript,
		pkt.Inputs[0].WitnessUtxo.PkScript,
	))
}

// TestBuildCPFPChildPSBT_NilParent verifies that buildCPFPChildPSBT
// returns an error when the parent transaction is nil.
func TestBuildCPFPChildPSBT_NilParent(t *testing.T) {
	t.Parallel()

	_, err := buildCPFPChildPSBT(nil, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "parent tx is nil")
}

// TestBuildCPFPChildPSBT_InvalidIndex verifies that buildCPFPChildPSBT
// returns an error for an out-of-range anchor index.
func TestBuildCPFPChildPSBT_InvalidIndex(t *testing.T) {
	t.Parallel()

	parentTx := newTestParentTx(0, 2)

	// Index too high.
	_, err := buildCPFPChildPSBT(parentTx, 5)
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of range")

	// Negative index.
	_, err = buildCPFPChildPSBT(parentTx, -1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of range")
}

// TestFinalizeAnchorInput_Valid verifies that finalizeAnchorInput sets
// the empty witness on the correct input.
func TestFinalizeAnchorInput_Valid(t *testing.T) {
	t.Parallel()

	parentTx := newTestParentTx(0, 1)

	pkt, err := buildCPFPChildPSBT(parentTx, 0)
	require.NoError(t, err)

	err = finalizeAnchorInput(pkt, 0)
	require.NoError(t, err)

	// The FinalScriptWitness should be a single 0x00 byte
	// (empty witness stack).
	require.Equal(t, []byte{0x00},
		pkt.Inputs[0].FinalScriptWitness)
}

// TestFinalizeAnchorInput_NilPSBT verifies that finalizeAnchorInput
// returns an error when the PSBT is nil.
func TestFinalizeAnchorInput_NilPSBT(t *testing.T) {
	t.Parallel()

	err := finalizeAnchorInput(nil, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "PSBT is nil")
}

// TestFinalizeAnchorInput_InvalidIndex verifies that
// finalizeAnchorInput returns an error for an out-of-range index.
func TestFinalizeAnchorInput_InvalidIndex(t *testing.T) {
	t.Parallel()

	parentTx := newTestParentTx(0, 1)

	pkt, err := buildCPFPChildPSBT(parentTx, 0)
	require.NoError(t, err)

	// Index too high.
	err = finalizeAnchorInput(pkt, 5)
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of range")

	// Negative index.
	err = finalizeAnchorInput(pkt, -1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of range")
}

// TestComputeAdjustedFeeRate verifies the fee rate inflation logic
// covers the parent weight.
func TestComputeAdjustedFeeRate(t *testing.T) {
	t.Parallel()

	parentTx := newTestParentTx(1, 2)
	parentWeight := parentTx.SerializeSize()
	parentVsize := (parentWeight + 3) / 4

	targetRate := btcutil.Amount(10) // 10 sat/vB

	result := computeAdjustedFeeRate(parentTx, targetRate)

	// Expected: target * (parentVsize + 200) / 200, converted to
	// sat/kW.
	expectedSatPerVByte := targetRate *
		btcutil.Amount(parentVsize+estimatedChildVsize) /
		btcutil.Amount(estimatedChildVsize)

	expectedSatPerKW := chainfee.SatPerKVByte(
		expectedSatPerVByte * 1000,
	).FeePerKWeight()

	require.Equal(t, expectedSatPerKW, result)

	// The adjusted rate should be strictly higher than the target
	// rate to cover the parent weight.
	targetSatPerKW := chainfee.SatPerKVByte(
		targetRate * 1000,
	).FeePerKWeight()
	require.Greater(t, int64(result), int64(targetSatPerKW))
}

// TestComputeAdjustedFeeRate_MinimumOneRate verifies that the adjusted
// fee rate is at least 1 sat/vB even for a zero target rate.
func TestComputeAdjustedFeeRate_MinimumOneRate(t *testing.T) {
	t.Parallel()

	parentTx := newTestParentTx(0, 1)

	result := computeAdjustedFeeRate(parentTx, 0)

	// With 0 target, the minimum 1 sat/vB floor applies.
	minSatPerKW := chainfee.SatPerKVByte(1000).FeePerKWeight()
	require.Equal(t, minSatPerKW, result)
}

// TestBuildCPFPChild_Success verifies the end-to-end CPFP child build
// flow with a mock wallet.
func TestBuildCPFPChild_Success(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	parentTx := newTestParentTx(2, 3)

	// The mock wallet returns a dummy signed child tx.
	childResult := wire.NewMsgTx(cpfpTxVersion)
	childResult.AddTxIn(&wire.TxIn{
		PreviousOutPoint: testOutpoint(50),
	})
	childResult.AddTxOut(&wire.TxOut{
		Value:    50000,
		PkScript: bytes.Repeat([]byte{0xcd}, 34),
	})

	wallet := &mockCPFPWallet{
		finalizePsbtResult: childResult,
	}

	result, err := buildCPFPChild(ctx, wallet, parentTx, 10)
	require.NoError(t, err)
	require.Equal(t, childResult, result)

	// Verify the wallet was called with correct parameters.
	require.True(t, wallet.fundPsbtCalled)
	require.True(t, wallet.finalizePsbtCalled)
	require.Equal(t, int32(cpfpMinConfs), wallet.fundPsbtMinConfs)

	// The fee rate should be inflated above the target.
	targetSatPerKW := chainfee.SatPerKVByte(
		10 * 1000,
	).FeePerKWeight()
	require.Greater(t,
		int64(wallet.fundPsbtFeeRate), int64(targetSatPerKW))
}

// TestBuildCPFPChild_NoAnchor verifies that buildCPFPChild returns an
// error when the parent has no P2A anchor output.
func TestBuildCPFPChild_NoAnchor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// No anchor output in the parent.
	parentTx := wire.NewMsgTx(2)
	parentTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: testOutpoint(99),
	})
	parentTx.AddTxOut(&wire.TxOut{
		Value:    100000,
		PkScript: bytes.Repeat([]byte{0xab}, 34),
	})

	wallet := &mockCPFPWallet{}

	_, err := buildCPFPChild(ctx, wallet, parentTx, 10)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no P2A anchor output")

	// Wallet should not have been called.
	require.False(t, wallet.fundPsbtCalled)
	require.False(t, wallet.finalizePsbtCalled)
}

// TestBuildCPFPChild_FundPsbtError verifies that buildCPFPChild
// propagates FundPsbt errors.
func TestBuildCPFPChild_FundPsbtError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	parentTx := newTestParentTx(0, 1)

	wallet := &mockCPFPWallet{
		fundPsbtErr: fmt.Errorf("insufficient funds"),
	}

	_, err := buildCPFPChild(ctx, wallet, parentTx, 10)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fund CPFP child")
	require.Contains(t, err.Error(), "insufficient funds")
}

// TestBuildCPFPChild_FinalizePsbtError verifies that buildCPFPChild
// propagates FinalizePsbt errors.
func TestBuildCPFPChild_FinalizePsbtError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	parentTx := newTestParentTx(0, 1)

	wallet := &mockCPFPWallet{
		finalizePsbtErr: fmt.Errorf("signing failed"),
	}

	_, err := buildCPFPChild(ctx, wallet, parentTx, 10)
	require.Error(t, err)
	require.Contains(t, err.Error(), "finalize CPFP child")
	require.Contains(t, err.Error(), "signing failed")
}
