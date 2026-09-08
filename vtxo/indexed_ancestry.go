package vtxo

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// IndexedAncestryFromRPC converts indexed ancestry only after proving that its
// signed paths fund the indexed target outpoint, value, and script. This is the
// shared ingress boundary for incoming, recovered, and custom-refresh VTXOs.
// Sweep expiry still requires the separate local confirmation check.
func IndexedAncestryFromRPC(candidate *arkrpc.VTXO) ([]Ancestry, error) {
	ancestry, err := AncestryFromRPC(candidate.GetAncestryPaths())
	if err != nil {
		return nil, err
	}
	op := candidate.GetOutpoint()
	if len(op.GetTxid()) != chainhash.HashSize ||
		candidate.GetValueSat() > math.MaxInt64 {
		return nil, invalidBatchExpiry(
			"invalid target outpoint or value",
		)
	}
	var txid chainhash.Hash
	copy(txid[:], op.GetTxid())
	target := expiryTarget{
		Outpoint: wire.OutPoint{
			Hash:  txid,
			Index: op.GetVout(),
		},
		Output: &wire.TxOut{
			Value:    int64(candidate.GetValueSat()),
			PkScript: candidate.GetPkScript(),
		},
	}
	for i, pkg := range candidate.GetAncestryPackages() {
		if pkg == nil {
			return nil, invalidBatchExpiry(
				"nil ancestry package %d", i,
			)
		}
		rawPackets := append(
			[][]byte{pkg.GetArkPsbt()}, pkg.GetCheckpointPsbts()...,
		)
		for j, raw := range rawPackets {
			tx, err := expiryPackageTransaction(raw)
			if err != nil {
				return nil, invalidBatchExpiry(
					"package %d transaction %d: %v", i, j,
					err,
				)
			}
			target.Transactions = append(target.Transactions, tx)
		}
	}
	if err := validateExpiryTarget(target, ancestry); err != nil {
		return nil, invalidBatchExpiry("target proof: %v", err)
	}

	return ancestry, nil
}

// expiryPackageTransaction reconstructs the signed wire transaction without
// trusting PSBT prevout metadata. The target proof supplies actual parent
// outputs to the script engine, so a forged WitnessUtxo cannot authenticate a
// spend.
func expiryPackageTransaction(raw []byte) (*wire.MsgTx, error) {
	pkt, err := psbtutil.Parse(raw)
	if err != nil {
		return nil, err
	}
	if len(pkt.Inputs) != len(pkt.UnsignedTx.TxIn) {
		return nil, fmt.Errorf("PSBT input count mismatch")
	}
	tx := pkt.UnsignedTx.Copy()
	for i, in := range pkt.Inputs {
		witness, err := oortx.BuildTaprootWitness(in)
		if err != nil {
			return nil, err
		}
		tx.TxIn[i].Witness = witness
	}

	return tx, nil
}
