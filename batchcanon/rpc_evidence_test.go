package batchcanon

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestEvidenceFromAncestryPaths proves the receive trust boundary binds all
// server-supplied fields to the serialized commitment transaction.
func TestEvidenceFromAncestryPaths(t *testing.T) {
	t.Parallel()

	valid := validRPCBatchEvidence(t)
	evidence, err := EvidenceFromAncestryPaths(
		[]*arkrpc.AncestryPath{valid},
	)
	require.NoError(t, err)
	require.Len(t, evidence, 1)
	require.Equal(
		t, valid.GetCommitmentCsvExpiryDelta(),
		evidence[0].CSVExpiryDelta,
	)
	require.Len(t, evidence[0].ConsumedInputs, 2)

	tests := []struct {
		name   string
		mutate func(*arkrpc.AncestryPath)
		want   string
	}{
		{
			name: "commitment txid mismatch",
			mutate: func(path *arkrpc.AncestryPath) {
				path.CommitmentTxid[0] ^= 1
			},
			want: "batch outpoint does not match commitment txid",
		},
		{
			name: "serialized transaction mismatch",
			mutate: func(path *arkrpc.AncestryPath) {
				path.CommitmentTx[len(path.CommitmentTx)-1] ^= 1
			},
			want: "transaction hash does not match",
		},
		{
			name: "tree output value mismatch",
			mutate: func(path *arkrpc.AncestryPath) {
				path.TreePath.BatchOutput.Value++
			},
			want: "batch output does not match",
		},
		{
			name: "missing input",
			mutate: func(path *arkrpc.AncestryPath) {
				path.CommitmentInputs =
					path.CommitmentInputs[:1]
			},
			want: "transaction has 2 inputs, evidence has 1",
		},
		{
			name: "input order mismatch",
			mutate: func(path *arkrpc.AncestryPath) {
				inputs := path.CommitmentInputs
				inputs[0], inputs[1] = inputs[1], inputs[0]
			},
			want: "input 0 outpoint does not match",
		},
		{
			name: "missing previous output script",
			mutate: func(path *arkrpc.AncestryPath) {
				path.CommitmentInputs[0].PrevOut.PkScript = nil
			},
			want: "input 0 has no previous output pkScript",
		},
		{
			name: "non-positive CSV expiry",
			mutate: func(path *arkrpc.AncestryPath) {
				path.CommitmentCsvExpiryDelta = 0
			},
			want: "CSV expiry delta must be positive",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := cloneRPCBatchEvidence(t, valid)
			test.mutate(path)
			_, err := EvidenceFromAncestryPaths(
				[]*arkrpc.AncestryPath{path},
			)
			require.ErrorContains(t, err, test.want)
		})
	}
}

// TestEvidenceFromDuplicateAncestryPaths allows same-commitment multi-leaf
// ancestry only when its shared commitment evidence agrees.
func TestEvidenceFromDuplicateAncestryPaths(t *testing.T) {
	t.Parallel()

	first := validRPCBatchEvidence(t)
	second := cloneRPCBatchEvidence(t, first)
	second.TreeDepth++

	evidence, err := EvidenceFromAncestryPaths(
		[]*arkrpc.AncestryPath{first, second},
	)
	require.NoError(t, err)
	require.Len(t, evidence, 1)

	second.CommitmentInputs[0].PrevOut.Value++
	_, err = EvidenceFromAncestryPaths(
		[]*arkrpc.AncestryPath{first, second},
	)
	require.ErrorContains(t, err, "conflicts with evidence")
}

// cloneRPCBatchEvidence makes independently mutable table-test evidence.
func cloneRPCBatchEvidence(t *testing.T,
	path *arkrpc.AncestryPath) *arkrpc.AncestryPath {

	t.Helper()

	cloned, ok := proto.Clone(path).(*arkrpc.AncestryPath)
	require.True(t, ok)

	return cloned
}

// validRPCBatchEvidence returns complete evidence for a two-input commitment
// whose second output anchors the supplied tree fragment.
func validRPCBatchEvidence(t *testing.T) *arkrpc.AncestryPath {
	t.Helper()

	inputA := testOutpoint(0x11, 1)
	inputB := testOutpoint(0x22, 2)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&inputA, nil, nil))
	tx.AddTxIn(wire.NewTxIn(&inputB, nil, nil))
	tx.AddTxOut(wire.NewTxOut(500, []byte{0x51}))
	tx.AddTxOut(wire.NewTxOut(1_000, []byte{0x51, 0x20, 0xaa}))

	var raw bytes.Buffer
	require.NoError(t, tx.Serialize(&raw))
	txid := tx.TxHash()

	return &arkrpc.AncestryPath{
		CommitmentTxid: append([]byte(nil), txid[:]...),
		CommitmentTx:   raw.Bytes(),
		CommitmentInputs: []*arkrpc.CommitmentInputEvidence{
			{
				Outpoint: rpcTestOutpoint(inputA),
				PrevOut: &arkrpc.TxOut{
					Value: 700,
					PkScript: []byte{
						0x51,
					},
				},
			},
			{
				Outpoint: rpcTestOutpoint(inputB),
				PrevOut: &arkrpc.TxOut{
					Value: 900,
					PkScript: []byte{
						0x00,
						0x14,
						0x01,
					},
				},
			},
		},
		CommitmentCsvExpiryDelta: 144,
		TreePath: &arkrpc.TreePath{
			BatchOutpoint: &arkrpc.OutPoint{
				Txid: append([]byte(nil), txid[:]...),
				Vout: 1,
			},
			BatchOutput: &arkrpc.TxOut{
				Value: tx.TxOut[1].Value,
				PkScript: append(
					[]byte(nil), tx.TxOut[1].PkScript...,
				),
			},
		},
	}
}

// rpcTestOutpoint maps one wire outpoint into the indexer transport shape.
func rpcTestOutpoint(outpoint wire.OutPoint) *arkrpc.OutPoint {
	return &arkrpc.OutPoint{
		Txid: append([]byte(nil), outpoint.Hash[:]...),
		Vout: outpoint.Index,
	}
}
