// Package expiryfixture builds signed ancestry for acceptance-boundary tests.
package expiryfixture

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// Round returns a signed round-direct inventory entry and its commitment.
// The tag makes independent fixtures use distinct, reproducible commitment IDs.
func Round(t testing.TB, value int64, script []byte, delay uint32, height int32,
	tag byte) (*arkrpc.VTXO, *wire.MsgTx) {

	t.Helper()
	_, ownerPub := btcec.PrivKeyFromBytes([]byte{tag, 1})
	_, operatorPub := btcec.PrivKeyFromBytes([]byte{tag, 2})
	keys := []*btcec.PublicKey{ownerPub, operatorPub}
	leaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(operatorPub, delay)
	require.NoError(t, err)
	sweep := leaf.TapHash()
	finalKey, err := tree.ComputeFinalKey(keys, sweep[:])
	require.NoError(t, err)
	batchScript, err := txscript.PayToTaprootScript(finalKey)
	require.NoError(t, err)
	commitment := wire.NewMsgTx(2)
	commitment.AddTxIn(
		&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{tag},
			},
		},
	)
	commitment.AddTxOut(&wire.TxOut{Value: value, PkScript: batchScript})
	commitmentID := commitment.TxHash()
	anchor := arkscript.AnchorPkScript
	node := &tree.Node{
		Input: wire.OutPoint{
			Hash: commitmentID,
		},
		Outputs: []*wire.TxOut{
			{
				Value:    value,
				PkScript: script,
			},
			{
				PkScript: anchor,
			},
		},
		CoSigners: keys,
		FinalKey:  finalKey,
	}
	path := &tree.Tree{
		Root: node, BatchOutpoint: node.Input,
		BatchOutput: commitment.TxOut[0], SweepTapscriptRoot: sweep[:],
	}
	signNode(t, node, commitment.TxOut[0], sweep[:], tag)
	require.NoError(t, path.VerifySigned())
	rpcPath, err := arkrpc.AncestryPathFromTree(path, commitmentID, nil)
	require.NoError(t, err)
	rpcPath.CommitmentSweepKey = operatorPub.SerializeCompressed()
	rpcPath.CommitmentSweepDelay = delay
	rpcPath.CommitmentHeight = height
	txid, err := node.TXID()
	require.NoError(t, err)

	return &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: txid[:],
		},
		ValueSat: uint64(value), PkScript: script,
		RoundId: "proof-round", CommitmentTxid: commitmentID[:],
		AncestryPaths: []*arkrpc.AncestryPath{
			rpcPath,
		},
		CreatedHeight: height,
		Status:        arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
	}, commitment
}

// signNode signs a tree node under the fixture's deterministic cosigner keys.
func signNode(t testing.TB, node *tree.Node, prev *wire.TxOut, sweep []byte,
	tag byte) {

	t.Helper()
	owner, _ := btcec.PrivKeyFromBytes([]byte{tag, 1})
	operator, _ := btcec.PrivKeyFromBytes([]byte{tag, 2})
	fetcher := txscript.NewCannedPrevOutputFetcher(
		prev.PkScript, prev.Value,
	)
	digest, err := node.SigHash(fetcher)
	require.NoError(t, err)
	var msg [32]byte
	copy(msg[:], digest)
	sessions := make([]*musig2.Session, 2)
	for i, key := range []*btcec.PrivateKey{owner, operator} {
		ctx, err := musig2.NewContext(
			key, true, musig2.WithKnownSigners(node.CoSigners),
			musig2.WithTaprootTweakCtx(sweep),
		)
		require.NoError(t, err)
		sessions[i], err = ctx.NewSession()
		require.NoError(t, err)
	}
	_, err = sessions[0].RegisterPubNonce(sessions[1].PublicNonce())
	require.NoError(t, err)
	_, err = sessions[1].RegisterPubNonce(sessions[0].PublicNonce())
	require.NoError(t, err)
	_, err = sessions[0].Sign(msg)
	require.NoError(t, err)
	part, err := sessions[1].Sign(msg)
	require.NoError(t, err)
	_, err = sessions[0].CombineSig(part)
	require.NoError(t, err)
	node.Signature = sessions[0].FinalSig()
}

// Merge builds a signed transaction consuming two round leaves. sameBatch tests
// distinct rooted paths within one confirmed commitment; otherwise the parents
// expire at different heights. The returned package is sufficient to test the
// inventory proof graph without invoking the OOR state machine.
func Merge(t testing.TB, sameBatch bool) (*arkrpc.VTXO, []*wire.MsgTx) {
	t.Helper()
	key, pub := btcec.PrivKeyFromBytes([]byte{99})
	script, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(pub),
	)
	require.NoError(t, err)
	first, firstCommit := Round(t, 10000, script, 50, 100, 10)
	second, secondCommit := Round(t, 10000, script, 20, 120, 11)
	parents := []*arkrpc.VTXO{first, second}
	commits := []*wire.MsgTx{firstCommit, secondCommit}
	if sameBatch {
		combined := firstCommit.Copy()
		combined.AddTxOut(secondCommit.TxOut[0])
		hash := combined.TxHash()
		for i, parent := range parents {
			path, err := arkrpc.AncestryPathToTree(
				parent.AncestryPaths[0],
			)
			require.NoError(t, err)
			path.BatchOutpoint = wire.OutPoint{
				Hash:  hash,
				Index: uint32(i),
			}
			path.Root.Input = path.BatchOutpoint
			signNode(
				t, path.Root, path.BatchOutput,
				path.SweepTapscriptRoot, byte(10+i),
			)
			rpcPath, err := arkrpc.AncestryPathFromTree(
				path, hash, []uint32{uint32(i)},
			)
			require.NoError(t, err)
			previous := parent.AncestryPaths[0]
			rpcPath.CommitmentSweepKey = previous.CommitmentSweepKey
			delay := previous.CommitmentSweepDelay
			rpcPath.CommitmentSweepDelay = delay
			rpcPath.CommitmentHeight = 100
			parent.AncestryPaths = []*arkrpc.AncestryPath{rpcPath}
			parent.CommitmentTxid = hash[:]
			leafHash, err := path.Root.TXID()
			require.NoError(t, err)
			parent.Outpoint.Txid = leafHash[:]
		}
		commits = []*wire.MsgTx{combined}
	}
	tx := wire.NewMsgTx(3)
	prevouts := txscript.NewMultiPrevOutFetcher(nil)
	for _, parent := range parents {
		var hash chainhash.Hash
		copy(hash[:], parent.Outpoint.Txid)
		op := wire.OutPoint{Hash: hash}
		tx.AddTxIn(
			&wire.TxIn{
				PreviousOutPoint: op,
				Sequence:         wire.MaxTxInSequenceNum,
			},
		)
		prevouts.AddPrevOut(
			op, &wire.TxOut{
				Value:    int64(parent.ValueSat),
				PkScript: parent.PkScript,
			},
		)
	}
	tx.AddTxOut(&wire.TxOut{Value: 20000, PkScript: script})
	tx.AddTxOut(&wire.TxOut{PkScript: arkscript.AnchorPkScript})
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	hashes := txscript.NewTxSigHashes(tx, prevouts)
	for i := range tx.TxIn {
		sig, err := txscript.RawTxInTaprootSignature(
			tx, hashes, i, 10000, script, nil,
			txscript.SigHashDefault, key,
		)
		require.NoError(t, err)
		packet.Inputs[i].TaprootKeySpendSig = sig
	}
	raw, err := psbtutil.Serialize(packet)
	require.NoError(t, err)
	hash := tx.TxHash()
	result, ok := proto.Clone(first).(*arkrpc.VTXO)
	require.True(t, ok)
	result.Outpoint = &arkrpc.OutPoint{Txid: hash[:]}
	result.ValueSat = 20000
	result.ChainDepth = 1
	result.AncestryPaths = append(
		result.AncestryPaths, second.AncestryPaths...,
	)
	for i, path := range result.AncestryPaths {
		path.InputIndices = []uint32{uint32(i)}
	}
	result.AncestryPackages = []*arkrpc.OORSessionPackage{
		{
			SessionId: hash[:],
			ArkPsbt:   raw,
		},
	}

	return result, commits
}
