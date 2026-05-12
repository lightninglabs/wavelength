package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	clientdarepod "github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/txconfirm"
)

// ForceBroadcastLineageTx builds a v3 CPFP child against the
// ephemeral anchor of entry.Tx, signed by the harness bitcoind
// regtest wallet, and submits {entry.Tx, child} as a package via
// bitcoind's submitpackage RPC. Returns the child txid on success.
//
// Test-harness only. Used by fraud-response itests to confirm an
// arbitrary tx in a VTXO's recovery lineage outside the legitimate
// unroll path so the operator's chain watcher and (eventually) the
// FraudDetector actor can be exercised end-to-end.
//
// feeSat is the total fee the package contributes — the parent has
// zero fee per the v3 ephemeral-anchor convention, so the child must
// pay for both. ~1000 sat is comfortable for a small tree node on
// regtest; callers should set higher if the parent is large.
func (h *ArkHarness) ForceBroadcastLineageTx(ctx context.Context,
	entry *clientdarepod.VTXOLineageEntry, feeSat btcutil.Amount) (
	chainhash.Hash, error) {

	if entry == nil || entry.Tx == nil {
		return chainhash.Hash{}, fmt.Errorf("entry has no " +
			"transaction to broadcast")
	}

	parentTxid := entry.Tx.TxHash()

	// Locate the ephemeral anchor output. Every recovery-DAG tx that
	// the planner expects to CPFP carries one; reject up front if
	// not found so the caller knows this lineage step isn't
	// CPFP-bumpable.
	anchorIdx := -1
	for i, out := range entry.Tx.TxOut {
		if arktx.IsAnchorOutput(out) {
			anchorIdx = i

			break
		}
	}
	if anchorIdx == -1 {
		return chainhash.Hash{}, fmt.Errorf("entry tx %s has no "+
			"ephemeral anchor output", parentTxid)
	}

	anchorOutpoint := wire.OutPoint{
		Hash:  parentTxid,
		Index: uint32(anchorIdx),
	}
	anchorOutput := entry.Tx.TxOut[anchorIdx]

	btc, err := h.Bitcoind()
	if err != nil {
		return chainhash.Hash{}, err
	}

	// Pull a confirmed wallet UTXO + change address from bitcoind
	// to fund the CPFP child.
	feeInput, err := btc.SelectFeeInput(feeSat)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("select fee input: %w", err)
	}

	changePkScript, err := btc.NewChangePkScript("force-broadcast-change")
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("change pkScript: %w", err)
	}

	// Build the unsigned child. Match the parent's tx version (always
	// v3 / TRUC for valid recovery txs) so the package passes the
	// TRUC gate.
	child, err := txconfirm.BuildCPFPChild(
		entry.Tx.Version, anchorOutpoint, anchorOutput, feeInput,
		changePkScript, feeSat,
	)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("build CPFP child: %w", err)
	}

	// Have bitcoind sign the wallet input. The anchor input is
	// anyone-can-spend (P2A) so it needs no signature.
	signed, err := btc.SignWalletInputs(child)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("sign CPFP child: %w", err)
	}

	// Submit the package directly via the bitcoind submitter — same
	// path the daemon uses, just driven from the test harness.
	childTxid, err := btc.SubmitPackage(
		ctx, []*wire.MsgTx{entry.Tx}, signed,
	)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("submit package "+
			"(parent=%s child=%s): %w", parentTxid, signed.TxHash(),
			err)
	}

	return childTxid, nil
}

// WaitForTxConfirmed polls bitcoind for the given txid and returns
// nil once the transaction has at least one confirmation. Used by
// fraud-response itests after mining a block to assert a
// force-broadcast lineage tx actually landed on chain.
//
// Returns an error if the deadline is reached without a confirmation
// or if the bitcoind RPC fails. Test-harness only.
func (h *ArkHarness) WaitForTxConfirmed(ctx context.Context,
	txid chainhash.Hash, timeout time.Duration) error {

	btc, err := h.Bitcoind()
	if err != nil {
		return err
	}

	return btc.WaitTxConfirmed(ctx, txid, timeout)
}
