package waved

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/unrollbridge"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

// validateRecoveryPackageRoots accepts package traversal endpoints only when
// the transported round ancestry supplies the transaction they spend.
func validateRecoveryPackageRoots(ancestry []vtxo.Ancestry,
	unresolved []wire.OutPoint) error {

	if len(unresolved) == 0 {
		return nil
	}
	treeOutputs := make(map[wire.OutPoint]struct{})
	for i := range ancestry {
		fragment := ancestry[i]
		if fragment.TreePath == nil || fragment.TreePath.Root == nil {
			return fmt.Errorf("Ark channel ancestry %d has no tree",
				i)
		}
		for node := range fragment.TreePath.Root.NodesIter() {
			tx, err := node.ToTx()
			if err != nil {
				return fmt.Errorf("build Ark channel ancestry "+
					"%d: %w", i, err)
			}
			txID := tx.TxHash()
			for outputIndex := range tx.TxOut {
				treeOutputs[wire.OutPoint{
					Hash: txID, Index: uint32(outputIndex),
				}] = struct{}{}
			}
		}
	}
	for _, outpoint := range unresolved {
		if _, ok := treeOutputs[outpoint]; !ok {
			return fmt.Errorf("Ark channel OOR lineage is "+
				"missing root %s", outpoint)
		}
	}

	return nil
}

// channelOperatorKey verifies that the channel and all roots share one Ark
// operator under BIP-340 x-only key semantics.
func channelOperatorKey(expectedOperator [33]byte,
	roots []*vtxo.Descriptor) (*btcec.PublicKey, error) {

	operatorKey, err := btcec.ParsePubKey(expectedOperator[:])
	if err != nil {
		return nil, fmt.Errorf("parse Ark channel operator key: %w",
			err)
	}
	rootOperatorKey, err := recoveryOperatorKey(roots)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(
		schnorr.SerializePubKey(operatorKey),
		schnorr.SerializePubKey(rootOperatorKey),
	) {
		return nil, fmt.Errorf("Ark channel operator does not match " +
			"roots")
	}

	return operatorKey, nil
}

// arkChannelRecoveryDescriptor reconstructs a channel recovery-only VTXO.
func arkChannelRecoveryDescriptor(terms arkchannel.Terms,
	source arkchannel.VTXOBinding,
	recovery arkchannel.RecoveryDescriptor) (*vtxo.Descriptor, error) {

	clientKey, err := btcec.ParsePubKey(terms.VTXO.ClientArkKey[:])
	if err != nil {
		return nil, err
	}
	operatorKey, err := btcec.ParsePubKey(terms.VTXO.ArkOperatorKey[:])
	if err != nil {
		return nil, err
	}
	ancestry := make([]vtxo.Ancestry, len(recovery.Ancestry))
	for i := range recovery.Ancestry {
		treePath, err := db.DeserializeTree(
			recovery.Ancestry[i].TreePath,
		)
		if err != nil {
			return nil, fmt.Errorf("decode Ark channel ancestry "+
				"%d: %w", i, err)
		}
		ancestry[i] = vtxo.Ancestry{
			TreePath:       treePath,
			CommitmentTxID: recovery.Ancestry[i].CommitmentTxID,
			InputIndices: append(
				[]uint32(nil),
				recovery.Ancestry[i].InputIndices...,
			),
			TreeDepth:        recovery.Ancestry[i].TreeDepth,
			CommitmentHeight: recovery.Ancestry[i].CommitmentHeight,
		}
	}

	return &vtxo.Descriptor{
		Outpoint: source.OutPoint, Amount: source.Amount,
		PolicyTemplate: append([]byte(nil), source.PolicyTemplate...),
		PkScript:       append([]byte(nil), source.PkScript...),
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientKey,
		},
		OperatorKey: operatorKey, Ancestry: ancestry,
		RoundID:        recovery.RoundID,
		CommitmentTxID: recovery.CommitmentTxID,
		BatchExpiry:    recovery.BatchExpiry,
		// The non-interactive fallback is the pre-signed client+hub
		// channel path. Persist its delay before watches are armed so a
		// confirmed ancestor cannot race proof assembly against a later
		// mutable update. In-Ark refresh uses the ordinary OOR actor
		// and never enters the unroller.
		RelativeExpiry: terms.VTXO.ChannelDelay,
		ChainDepth:     recovery.ChainDepth,
		CreatedHeight:  recovery.CreatedHeight,
		Status:         vtxo.VTXOStatusRecoveryOnly,
		ConstructionVersion: arkrpc.ConstructionVersion(
			recovery.ConstructionVersion,
		),
	}, nil
}

// channelSourceCSVDelay selects the delay of the exact final spend policy.
func channelSourceCSVDelay(terms arkchannel.Terms,
	kind unroll.ExitPolicyKind) (uint32, error) {

	if kind != unrollbridge.ExitPolicyKind {
		return 0, fmt.Errorf("unsupported Ark channel exit policy %q",
			kind)
	}

	return terms.VTXO.ChannelDelay, nil
}
