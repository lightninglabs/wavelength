package round

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/require"
)

// validPSBTBytes returns a minimal valid PSBT serialized to bytes for
// use in tests that need to pass the nil PSBT check.
func validPSBTBytes(t *testing.T) []byte {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x01},
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	b, err := roundpb.PSBTToBytes(pkt)
	require.NoError(t, err)

	return b
}

// =====================================================================
// FINDING-S1: CommitmentTxBuilt.FromProto nil/empty PSBT panic vector
// Severity: HIGH (CVSS 7.5)
//
// When the server sends a ClientBatchInfo with an empty batch_psbt
// field (proto default for bytes is empty), PSBTFromBytes returns
// (nil, nil). The FromProto method sets e.Tx = nil without error.
// Any downstream code that accesses e.Tx.UnsignedTx (for sighash
// computation, input validation, etc.) will panic with a nil pointer
// dereference.
//
// This is reachable from untrusted server input via the mailbox
// ingress path. A malicious or buggy server can trigger a client
// crash by omitting batch_psbt from the ClientBatchInfo event.
// =====================================================================

// TestCommitmentTxBuiltFromProtoEmptyPSBT demonstrates that an empty
// batch_psbt field results in a nil Tx without error.
func TestCommitmentTxBuiltFromProtoEmptyPSBT(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	}

	pb := &roundpb.ClientBatchInfo{
		RoundId:   roundID[:],
		BatchPsbt: nil, // Server omits PSBT.
	}

	var got CommitmentTxBuilt
	err := got.FromProto(pb)

	// FromProto now rejects nil PSBT.
	require.Error(t, err)
	require.Contains(t, err.Error(), "required field is empty")
}

// =====================================================================
// FINDING-S2: CommitmentTxBuilt.FromProto negative tree path indices
// Severity: MEDIUM (CVSS 5.0)
//
// The proto uses map<int32, VTXOTree> for vtxo_tree_paths. Negative
// int32 keys are valid in protobuf but semantically invalid as
// commitment tx output indices. The code does int(idx) which
// preserves the sign, creating negative map keys in VTXOTreePaths.
// =====================================================================

// TestCommitmentTxBuiltNegativeTreeIndex demonstrates negative index
// acceptance.
func TestCommitmentTxBuiltNegativeTreeIndex(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	}
	hash := chainhash.Hash{0x01}

	// Minimal valid tree for the proto.
	validTree := &roundpb.VTXOTree{
		Nodes: []*roundpb.TreeNode{
			{
				Input: &roundpb.Outpoint{
					TxHash:      hash[:],
					OutputIndex: 0,
				},
				Outputs: []*roundpb.TxOut{
					{
						Value: 1000,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{},
				Amount:   1000,
			},
		},
		BatchOutpoint: &roundpb.Outpoint{
			TxHash:      hash[:],
			OutputIndex: 0,
		},
		BatchOutput: &roundpb.TxOut{
			Value: 1000,
			PkScript: []byte{
				0x51,
			},
		},
	}

	pb := &roundpb.ClientBatchInfo{
		RoundId:   roundID[:],
		BatchPsbt: validPSBTBytes(t),
		VtxoTreePaths: map[int32]*roundpb.VTXOTree{
			-1: validTree, // Negative index!
		},
	}

	// FromProto now rejects negative tree path indices.
	var got CommitmentTxBuilt
	err := got.FromProto(pb)
	require.Error(t, err)
	require.Contains(t, err.Error(), "negative")
}

// =====================================================================
// FINDING-S3: NoncesAggregated.FromProto accepts empty nonce map
// Severity: LOW (CVSS 3.0)
//
// When pb.AggNonces is nil, e.AggNonces remains nil (zero value).
// The FSM code that iterates AggNonces would silently skip all
// signing work, potentially causing a protocol stall rather than
// an explicit error.
// =====================================================================

// TestNoncesAggregatedEmptyMap demonstrates nil nonce map acceptance.
func TestNoncesAggregatedEmptyMap(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	}

	pb := &roundpb.ClientVTXOAggNonces{
		RoundId:   roundID[:],
		AggNonces: nil, // Server sends no nonces.
	}

	var got NoncesAggregated
	err := got.FromProto(pb)
	require.NoError(t, err)
	require.Nil(t, got.AggNonces,
		"nil nonce map silently accepted")
}

// =====================================================================
// FINDING-S4: OperatorSigned.FromProto accepts empty sig map
// Severity: LOW (CVSS 3.0)
//
// Same pattern as FINDING-S3. A nil AggSigs map means the client
// received "operator signed" confirmation but no actual signatures.
// The client would proceed without signatures, potentially losing
// the ability to unilaterally exit.
// =====================================================================

// TestOperatorSignedEmptySigMap demonstrates nil signature map
// acceptance.
func TestOperatorSignedEmptySigMap(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	}

	pb := &roundpb.ClientVTXOAggSigs{
		RoundId: roundID[:],
		AggSigs: nil, // Server sends no signatures!
	}

	var got OperatorSigned
	err := got.FromProto(pb)
	require.NoError(t, err)
	require.Nil(
		t, got.AggSigs, "nil sig map silently accepted - client "+
			"has no signatures for unilateral exit",
	)
}

// =====================================================================
// FINDING-S5: BoardingFailed.FromProto always sets Recoverable=true
// Severity: LOW (CVSS 3.5)
//
// All server-originated failures are treated as recoverable. If the
// server sends an error indicating permanent rejection (e.g.,
// blacklisted participant, invalid boarding UTXO), the client will
// incorrectly retry, wasting resources and potentially spamming the
// server.
// =====================================================================

// TestBoardingFailedAlwaysRecoverable demonstrates unconditional
// recovery flag.
func TestBoardingFailedAlwaysRecoverable(t *testing.T) {
	t.Parallel()

	// Even a clearly permanent error is marked recoverable.
	pb := &roundpb.ClientRoundFailedResp{
		RoundId: make([]byte, 16),
		Reason:  "PERMANENT: participant banned from service",
	}

	var got BoardingFailed
	err := got.FromProto(pb)
	require.NoError(t, err)
	require.True(
		t, got.Recoverable,
		"permanent failure incorrectly marked as recoverable",
	)
}

// =====================================================================
// FINDING-S6: CommitmentTxBuilt.FromProto connector leaf with nil
// LeafOutput is correctly caught
// Severity: INFORMATIONAL (POSITIVE)
//
// The code properly checks for nil leaf_output and returns an error.
// This prevents a nil-deref on the connector info. Good practice.
// =====================================================================

// TestCommitmentTxBuiltNilConnectorLeafOutput verifies the nil check.
func TestCommitmentTxBuiltNilConnectorLeafOutput(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	}
	hash := chainhash.Hash{0xaa}

	op := wire.OutPoint{Hash: hash, Index: 0}
	key := roundpb.OutpointToMapKey(op)

	pb := &roundpb.ClientBatchInfo{
		RoundId:   roundID[:],
		BatchPsbt: validPSBTBytes(t),
		ConnectorLeafMap: map[string]*roundpb.ConnectorLeafInfo{
			key: {
				LeafOutpoint: &roundpb.Outpoint{
					TxHash:      hash[:],
					OutputIndex: 0,
				},
				LeafOutput: nil, // Missing output.
			},
		},
	}

	var got CommitmentTxBuilt
	err := got.FromProto(pb)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil leaf_output")
}

// =====================================================================
// FINDING-S7: JoinRoundRequest.FromProto optional Identifier key
// Severity: INFORMATIONAL
//
// When pb.Identifier is empty (len == 0), the code correctly skips
// parsing and leaves m.Identifier as nil. This is intentional for
// test scenarios where auth is disabled. However, if a client sends
// a join request without an identifier in production, the server
// should reject it. This is a server-side validation concern.
// =====================================================================

// TestJoinRoundRequestFromProtoEmptyIdentifier verifies nil
// identifier handling.
func TestJoinRoundRequestFromProtoEmptyIdentifier(t *testing.T) {
	t.Parallel()

	pb := &roundpb.JoinRoundRequest{
		Identifier: nil, // No identifier.
	}

	var got JoinRoundRequest
	err := got.FromProto(pb)
	require.NoError(t, err)
	require.Nil(t, got.Identifier,
		"nil identifier silently accepted")
}

// =====================================================================
// FINDING-H1-POC: Full exploit chain for nil PSBT. Demonstrates
// that a malicious server can cause a nil-deref panic in the client
// FSM by omitting batch_psbt from ClientBatchInfo.
// Severity: HIGH (CVSS 7.5)
// =====================================================================

// TestCommitmentTxBuiltNilPSBTExploitChain demonstrates the full
// exploit path: server omits PSBT -> FromProto succeeds -> accessing
// Tx.UnsignedTx panics.
func TestCommitmentTxBuiltNilPSBTExploitChain(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		0xde, 0xad, 0xbe, 0xef,
		0xde, 0xad, 0xbe, 0xef,
		0xde, 0xad, 0xbe, 0xef,
		0xde, 0xad, 0xbe, 0xef,
	}

	// Step 1: Server sends ClientBatchInfo without PSBT.
	pb := &roundpb.ClientBatchInfo{
		RoundId:   roundID[:],
		BatchPsbt: nil, // Malicious: omitted PSBT.
	}

	// Step 2: FromProto now rejects nil PSBT.
	var got CommitmentTxBuilt
	err := got.FromProto(pb)
	require.Error(t, err)
	require.Contains(t, err.Error(), "required field is empty")
}

// =====================================================================
// FINDING-L2-POC: OperatorSigned with empty sigs means the client
// has no signatures for unilateral exit. Demonstrates the protocol
// stall / fund loss vector.
// Severity: LOW (CVSS 3.0)
// =====================================================================

// TestOperatorSignedEmptySigsUnilateralExitBlocked demonstrates that
// accepting empty agg_sigs means the client cannot perform unilateral
// exit. The ValidateAndSubmitSignatures call would fail with "no
// signatures provided".
func TestOperatorSignedEmptySigsUnilateralExitBlocked(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		0x01, 0x02, 0x03, 0x04,
		0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c,
		0x0d, 0x0e, 0x0f, 0x10,
	}

	// Server sends OperatorSigned with empty agg_sigs.
	pb := &roundpb.ClientVTXOAggSigs{
		RoundId: roundID[:],
		AggSigs: nil, // No signatures!
	}

	var got OperatorSigned
	err := got.FromProto(pb)

	// FromProto succeeds.
	require.NoError(t, err)

	// AggSigs is nil. The FSM would pass this to
	// tree.ValidateAndSubmitSignatures which rejects nil sigs,
	// but only after the state machine has already transitioned.
	require.Nil(
		t, got.AggSigs, "EXPLOIT: nil sig map accepted - client "+
			"cannot perform unilateral exit without signatures",
	)
}

// =====================================================================
// FINDING-L1-POC: Negative tree path index flows through to domain
// map key, creating semantically invalid output index references.
// Severity: LOW (CVSS 3.1)
// =====================================================================

// TestCommitmentTxBuiltNegativeTreePathExploit demonstrates that
// negative proto map keys create negative domain map keys that are
// semantically invalid as commitment tx output indices.
func TestCommitmentTxBuiltNegativeTreePathExploit(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		0xaa, 0xbb, 0xcc, 0xdd,
		0xaa, 0xbb, 0xcc, 0xdd,
		0xaa, 0xbb, 0xcc, 0xdd,
		0xaa, 0xbb, 0xcc, 0xdd,
	}
	hash := chainhash.Hash{0x01}

	validTree := &roundpb.VTXOTree{
		Nodes: []*roundpb.TreeNode{
			{
				Input: &roundpb.Outpoint{
					TxHash:      hash[:],
					OutputIndex: 0,
				},
				Outputs: []*roundpb.TxOut{
					{
						Value: 1000,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{},
				Amount:   1000,
			},
		},
		BatchOutpoint: &roundpb.Outpoint{
			TxHash:      hash[:],
			OutputIndex: 0,
		},
		BatchOutput: &roundpb.TxOut{
			Value: 1000,
			PkScript: []byte{
				0x51,
			},
		},
	}

	pb := &roundpb.ClientBatchInfo{
		RoundId:   roundID[:],
		BatchPsbt: validPSBTBytes(t),
		VtxoTreePaths: map[int32]*roundpb.VTXOTree{
			-1:          validTree,
			-2147483648: validTree, // int32 min.
		},
	}

	// FromProto now rejects negative tree path indices.
	var got CommitmentTxBuilt
	err := got.FromProto(pb)
	require.Error(t, err)
	require.Contains(t, err.Error(), "negative")
}

// =====================================================================
// FINDING-CONNECTOR-POC: ConnectorLeafInfo with negative
// ConnectorAmount demonstrates that negative values flow through to
// forfeit transaction construction.
// Severity: HIGH (related to H2)
// =====================================================================

// TestCommitmentTxBuiltNegativeConnectorAmount demonstrates that
// a malicious server can send negative connector amounts, which
// would corrupt forfeit transaction fee calculations.
func TestCommitmentTxBuiltNegativeConnectorAmount(t *testing.T) {
	t.Parallel()

	roundID := [16]byte{
		0xfe, 0xed, 0xfa, 0xce,
		0xfe, 0xed, 0xfa, 0xce,
		0xfe, 0xed, 0xfa, 0xce,
		0xfe, 0xed, 0xfa, 0xce,
	}
	hash := chainhash.Hash{0xbb}

	op := wire.OutPoint{Hash: hash, Index: 0}
	key := roundpb.OutpointToMapKey(op)

	pb := &roundpb.ClientBatchInfo{
		RoundId:   roundID[:],
		BatchPsbt: validPSBTBytes(t),
		ConnectorLeafMap: map[string]*roundpb.ConnectorLeafInfo{
			key: {
				LeafOutpoint: &roundpb.Outpoint{
					TxHash:      hash[:],
					OutputIndex: 0,
				},
				LeafOutput: &roundpb.TxOut{
					// Negative value!
					Value: -1_000_000,
					PkScript: []byte{
						0x51,
						0x20,
					},
				},
			},
		},
	}

	// TxOutFromProto now rejects negative output values.
	var got CommitmentTxBuilt
	err := got.FromProto(pb)
	require.Error(t, err)
	require.Contains(t, err.Error(), "negative output value")
}
