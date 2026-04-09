package batchsweeper

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	treepkg "github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// buildSignedSweepTx builds, signs, and returns a sweep transaction that spends
// all provided candidate outputs using the operator's unilateral CSV sweep
// leaf.
func buildSignedSweepTx(candidates []*batchwatcher.Output,
	sweepKey keychain.KeyDescriptor, sweepDelay uint32,
	sweepPkScript []byte, feeRate btcutil.Amount, signer input.Signer) (
	*wire.MsgTx, error) {

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no sweep candidates")
	}

	if signer == nil {
		return nil, fmt.Errorf("nil signer")
	}

	if len(sweepPkScript) == 0 {
		return nil, fmt.Errorf("empty sweep pkScript")
	}

	tx := wire.NewMsgTx(2)

	var totalInput btcutil.Amount
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(candidates))

	for _, output := range candidates {
		if output == nil || output.TxOut == nil {
			return nil, fmt.Errorf("nil output in candidates")
		}

		prevOuts[output.Outpoint] = output.TxOut

		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: output.Outpoint,
			Sequence:         sweepDelay,
		})

		totalInput += btcutil.Amount(output.TxOut.Value)
	}

	// Add a single sweep output. We'll set the value after fee estimation.
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(totalInput),
		PkScript: sweepPkScript,
	})

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

	// First signing pass (fee=0). This gives us accurate witness sizing.
	err := signSweepInputs(
		tx, candidates, sweepKey, sweepDelay, prevFetcher, signer,
	)
	if err != nil {
		return nil, err
	}

	vsize := mempool.GetTxVirtualSize(btcutil.NewTx(tx))
	fee, err := feeFromRate(feeRate, vsize)
	if err != nil {
		return nil, err
	}

	outputValue := totalInput - fee
	if outputValue <= 0 {
		return nil, fmt.Errorf("insufficient funds for fees: "+
			"inputs=%d, fee=%d", totalInput, fee)
	}

	tx.TxOut[0].Value = int64(outputValue)

	// Second signing pass since tapscript sighashes commit to outputs.
	err = signSweepInputs(
		tx, candidates, sweepKey, sweepDelay, prevFetcher, signer,
	)
	if err != nil {
		return nil, err
	}

	return tx, nil
}

// signSweepInputs signs each input as a Taproot script spend using the sweep
// leaf committed to in the tree outputs.
func signSweepInputs(tx *wire.MsgTx, candidates []*batchwatcher.Output,
	sweepKey keychain.KeyDescriptor, sweepDelay uint32,
	prevFetcher txscript.PrevOutputFetcher, signer input.Signer) error {

	if tx == nil {
		return fmt.Errorf("nil tx")
	}

	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)

	for i, output := range candidates {
		if output == nil || output.TxOut == nil {
			return fmt.Errorf("nil output in candidates")
		}

		if output.TreeNode == nil {
			return fmt.Errorf("candidate missing tree node")
		}

		internalKey, err := internalKeyForNode(output.TreeNode)
		if err != nil {
			return err
		}

		legacySpendInfo, err := treepkg.NewBranchSweepSpendInfo(
			internalKey, sweepKey.PubKey, sweepDelay,
		)
		if err != nil {
			return fmt.Errorf(
				"failed to derive sweep spend info: %w", err,
			)
		}

		spendInfo := &arkscript.SpendInfo{
			WitnessScript: legacySpendInfo.WitnessScript,
			ControlBlock:  legacySpendInfo.ControlBlock,
		}

		signDesc := spendInfo.BuildSignDescriptor(
			sweepKey, output.TxOut, sigHashes, prevFetcher, i,
		)

		witness, err := arkscript.VTXOTimeoutSpendWitness(
			signer, signDesc, tx,
		)
		if err != nil {
			return fmt.Errorf("failed to create sweep witness: %w",
				err)
		}

		tx.TxIn[i].Witness = witness
	}

	return nil
}

// internalKeyForNode computes the Taproot internal key for the output spent by
// the provided tree node. For multi-signer nodes, this is the MuSig2 aggregated
// key before any Taproot script root tweaks are applied.
func internalKeyForNode(node *treepkg.Node) (*btcec.PublicKey, error) {
	if node == nil {
		return nil, fmt.Errorf("nil tree node")
	}

	if len(node.CoSigners) == 0 {
		return nil, fmt.Errorf("tree node has no cosigners")
	}

	if len(node.CoSigners) == 1 {
		return node.CoSigners[0], nil
	}

	aggKey, _, _, err := musig2.AggregateKeys(node.CoSigners, true)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate keys: %w", err)
	}

	if aggKey.PreTweakedKey != nil {
		return aggKey.PreTweakedKey, nil
	}

	return aggKey.FinalKey, nil
}

// feeFromRate computes the fee in satoshis given a fee rate in sat/vbyte and a
// virtual size.
func feeFromRate(feeRate btcutil.Amount, vsize int64) (btcutil.Amount, error) {
	if vsize < 0 {
		return 0, fmt.Errorf("negative vsize")
	}

	if feeRate < 0 {
		return 0, fmt.Errorf("negative fee rate")
	}

	feeSat := int64(feeRate) * vsize
	if vsize != 0 && feeSat/vsize != int64(feeRate) {
		return 0, fmt.Errorf("fee overflow")
	}

	return btcutil.Amount(feeSat), nil
}
