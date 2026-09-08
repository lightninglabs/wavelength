package vtxo

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
)

// expiryTarget binds an expiry decision to the received output. Transactions
// contains finalized OOR transactions between the round leaves and the target;
// round-direct targets need no additional transactions.
type expiryTarget struct {
	Outpoint     wire.OutPoint
	Output       *wire.TxOut
	Transactions []*wire.MsgTx
}

// validateExpiryTarget reuses tree structure and recovery graph validation,
// then executes every spend against its actual parent output. A valid unrelated
// batch or a forged child signed under an unrelated key cannot authorize the
// target.
func validateExpiryTarget(target expiryTarget, ancestry []Ancestry) error {
	if target.Output == nil {
		return fmt.Errorf("target output is required")
	}

	nodes := make(map[chainhash.Hash]*recovery.Node)
	prevouts := txscript.NewMultiPrevOutFetcher(nil)
	add := func(tx *wire.MsgTx) error {
		if tx == nil {
			return fmt.Errorf("nil proof transaction")
		}
		txid := tx.TxHash()
		if previous, ok := nodes[txid]; ok {
			var a, b bytes.Buffer
			if err := previous.Tx.Serialize(&a); err != nil {
				return err
			}
			if err := tx.Serialize(&b); err != nil {
				return err
			}
			if !bytes.Equal(a.Bytes(), b.Bytes()) {
				return fmt.Errorf("conflicting proof "+
					"transaction %s", txid)
			}

			return nil
		}
		nodes[txid] = &recovery.Node{Tx: tx}
		for i, output := range tx.TxOut {
			prevouts.AddPrevOut(
				wire.OutPoint{
					Hash:  txid,
					Index: uint32(i),
				},
				output,
			)
		}

		return nil
	}

	for _, fragment := range ancestry {
		if err := validateExpiryFragment(fragment); err != nil {
			return err
		}
		path := fragment.TreePath
		if err := path.Verify(); err != nil {
			return fmt.Errorf("tree path: %w", err)
		}
		prevouts.AddPrevOut(path.BatchOutpoint, path.BatchOutput)
		for node := range path.Root.NodesIter() {
			tx, err := node.ToSignedTx()
			if err != nil {
				return err
			}
			if err := add(tx); err != nil {
				return err
			}
		}
	}
	for _, tx := range target.Transactions {
		if err := add(tx); err != nil {
			return err
		}
	}

	proofNodes := make([]*recovery.Node, 0, len(nodes))
	for _, node := range nodes {
		proofNodes = append(proofNodes, node)
	}
	proof, err := recovery.NewProof(target.Outpoint, 0, proofNodes...)
	if err != nil {
		return err
	}
	output, err := proof.TargetOutput()
	if err != nil {
		return err
	}
	if output.Value != target.Output.Value ||
		!bytes.Equal(output.PkScript, target.Output.PkScript) {
		return fmt.Errorf("target output does not match proof")
	}

	for _, node := range proofNodes {
		tx := node.Tx
		for _, in := range tx.TxIn {
			previous := prevouts.FetchPrevOutput(
				in.PreviousOutPoint,
			)
			if previous == nil {
				return fmt.Errorf("proof input %s has no "+
					"authenticated parent",
					in.PreviousOutPoint)
			}
		}
		sigHashes := txscript.NewTxSigHashes(tx, prevouts)
		for i, in := range tx.TxIn {
			prev := prevouts.FetchPrevOutput(in.PreviousOutPoint)
			engine, err := txscript.NewEngine(
				prev.PkScript, tx, i,
				txscript.StandardVerifyFlags, nil, sigHashes,
				prev.Value, prevouts,
			)
			if err != nil {
				return err
			}
			if err := engine.Execute(); err != nil {
				return fmt.Errorf("proof transaction %s input "+
					"%d: %w", tx.TxHash(), i, err)
			}
		}
	}

	return nil
}
