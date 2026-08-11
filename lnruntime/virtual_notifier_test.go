package lnruntime

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// countingNotifier records confirmation registrations delegated to the real
// chain backend.
type countingNotifier struct {
	*runtimeNotifier

	confirmations atomic.Int32
}

// RegisterConfirmationsNtfn records and returns an idle real-chain event.
func (n *countingNotifier) RegisterConfirmationsNtfn(*chainhash.Hash, []byte,
	uint32, uint32, ...chainntnfs.NotifierOption) (
	*chainntnfs.ConfirmationEvent, error) {

	n.confirmations.Add(1)

	return chainntnfs.NewConfirmationEvent(1, func() {}), nil
}

// TestVirtualFundingNotifierActivatesAfterRoundConfirmation verifies lnd sees
// no funding confirmation before the Ark coordinator opens the activation
// gate.
func TestVirtualFundingNotifierActivatesAfterRoundConfirmation(t *testing.T) {
	t.Parallel()

	base := &countingNotifier{runtimeNotifier: newRuntimeNotifier(800_000)}
	notifier, err := NewVirtualFundingNotifier(base)
	require.NoError(t, err)

	fundingTx := testVirtualFundingTx()
	scid := lnwire.ShortChannelID{
		BlockHeight: 16_000_123,
		TxIndex:     42,
		TxPosition:  0,
	}
	require.NoError(
		t,
		notifier.RegisterVirtualFunding(
			VirtualFunding{
				Transaction: fundingTx,
				OutputIndex: 0,
				SCID:        scid,
			},
		),
	)

	txid := fundingTx.TxHash()
	event, err := notifier.RegisterConfirmationsNtfn(
		&txid, fundingTx.TxOut[0].PkScript, 1, 800_000,
	)
	require.NoError(t, err)
	require.Zero(t, base.confirmations.Load())

	select {
	case <-event.Confirmed:
		t.Fatal("virtual funding confirmed before Ark round")

	case <-time.After(20 * time.Millisecond):
	}

	require.NoError(t, notifier.ConfirmVirtualFunding(txid))
	update := <-event.Updates
	require.Equal(t, scid.BlockHeight, update.BlockHeight)
	confirmation := <-event.Confirmed
	require.Equal(t, scid.BlockHeight, confirmation.BlockHeight)
	require.Equal(t, scid.TxIndex, confirmation.TxIndex)
	require.Equal(t, txid, confirmation.Tx.TxHash())

	require.NoError(t, notifier.ReorgVirtualFunding(txid, 2))
	require.EqualValues(t, 2, <-event.NegativeConf)
	require.NoError(t, notifier.ConfirmVirtualFunding(txid))
	<-event.Updates
	confirmation = <-event.Confirmed
	require.Equal(t, txid, confirmation.Tx.TxHash())
}

// TestVirtualFundingNotifierDelegatesUnknownTransactions verifies normal chain
// notifications keep their existing backend semantics.
func TestVirtualFundingNotifierDelegatesUnknownTransactions(t *testing.T) {
	t.Parallel()

	base := &countingNotifier{runtimeNotifier: newRuntimeNotifier(800_000)}
	notifier, err := NewVirtualFundingNotifier(base)
	require.NoError(t, err)

	txid := chainhash.Hash{1, 2, 3}
	_, err = notifier.RegisterConfirmationsNtfn(&txid, []byte{0x51}, 1, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, base.confirmations.Load())
}

// TestVirtualFundingNotifierRejectsMismatchedFunding verifies the reserved
// SCID and channel output cannot silently diverge.
func TestVirtualFundingNotifierRejectsMismatchedFunding(t *testing.T) {
	t.Parallel()

	notifier, err := NewVirtualFundingNotifier(
		newRuntimeNotifier(800_000),
	)
	require.NoError(t, err)

	fundingTx := testVirtualFundingTx()
	err = notifier.RegisterVirtualFunding(VirtualFunding{
		Transaction: fundingTx,
		OutputIndex: 0,
		SCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_123,
			TxPosition:  1,
		},
	})
	require.ErrorContains(t, err, "does not match funding output")
}

// testVirtualFundingTx creates an immutable witness funding transaction.
func testVirtualFundingTx() *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{9}},
		Witness:          wire.TxWitness{[]byte{1}},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    100_000,
		PkScript: []byte{0x00, 0x20, 1, 2, 3},
	})

	return tx
}
