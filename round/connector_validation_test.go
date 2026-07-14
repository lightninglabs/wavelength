package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// connectorLeafAmount is the per-leaf dust value used by the connector
// fixtures. The exact value is irrelevant to the binding logic; it only has to
// be positive and divide the root output value evenly.
const connectorLeafAmount = int64(330)

// newConnectorFixture builds a connector tree the way the operator would, plus
// a commitment tx that funds its root output at rootIdx, and returns the
// per-leaf ConnectorLeafInfo entries a client would receive over the wire. The
// returned infos are fully valid; tests mutate copies to exercise the negative
// paths.
func newConnectorFixture(t *testing.T, operatorKey *btcec.PublicKey, numLeaves,
	radix int, rootIdx uint32) (*wire.MsgTx, []*ConnectorLeafInfo) {

	t.Helper()

	// The connector root output script must be P2TR; derive one from a
	// throwaway internal key.
	internalPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	connScript, err := txscript.PayToTaprootScript(internalPriv.PubKey())
	require.NoError(t, err)

	rootOutput := &wire.TxOut{
		Value:    int64(numLeaves) * connectorLeafAmount,
		PkScript: connScript,
	}

	// Place the connector output at rootIdx, padding earlier indices with
	// throwaway outputs so the index is meaningful.
	commitmentTx := wire.NewMsgTx(3)
	commitmentTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0},
	})
	for range rootIdx {
		commitmentTx.AddTxOut(&wire.TxOut{
			Value:    1000,
			PkScript: []byte{txscript.OP_RETURN},
		})
	}
	commitmentTx.AddTxOut(rootOutput)

	rootOutpoint := wire.OutPoint{
		Hash:  commitmentTx.TxHash(),
		Index: rootIdx,
	}

	connTree, err := tree.BuildConnectorTree(
		rootOutpoint, rootOutput, tree.ConnectorDescriptor{
			PkScript:  connScript,
			NumLeaves: numLeaves,
			Amount:    btcutil.Amount(connectorLeafAmount),
		}, operatorKey, radix,
	)
	require.NoError(t, err)

	leaves := connTree.Root.GetLeafNodes()
	require.Len(t, leaves, numLeaves)

	infos := make([]*ConnectorLeafInfo, numLeaves)
	for i, leaf := range leaves {
		op, opErr := leaf.GetNonAnchorOutpoint()
		require.NoError(t, opErr)

		out, outErr := connectorLeafOutput(leaf)
		require.NoError(t, outErr)

		infos[i] = &ConnectorLeafInfo{
			LeafIndex:         i,
			ConnectorOutpoint: *op,
			ConnectorPkScript: out.PkScript,
			ConnectorAmount:   out.Value,
			RootOutputIndex:   rootIdx,
			NumLeaves:         uint32(numLeaves),
			Radix:             uint32(radix),
		}
	}

	return commitmentTx, infos
}

// mappingFrom wraps a single ConnectorLeafInfo in the forfeit-mapping shape
// validateConnectorAncestry consumes, keyed by a dummy forfeited-VTXO outpoint.
func mappingFrom(info *ConnectorLeafInfo) map[wire.OutPoint]*ConnectorLeafInfo {
	return map[wire.OutPoint]*ConnectorLeafInfo{
		{
			Index: 7,
		}: info,
	}
}

// TestValidateConnectorAncestryAccepts verifies that connector leaves genuinely
// rooted in the commitment tx pass validation, for both single-output and
// multi-output (multi-leaf) rounds.
func TestValidateConnectorAncestryAccepts(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey := operatorPriv.PubKey()

	t.Run("single leaf", func(t *testing.T) {
		t.Parallel()

		commitmentTx, infos := newConnectorFixture(
			t, operatorKey, 1, 2, 0,
		)
		require.NoError(
			t,
			validateConnectorAncestry(
				commitmentTx, operatorKey,
				mappingFrom(infos[0]),
			),
		)
	})

	t.Run("multi leaf every index", func(t *testing.T) {
		t.Parallel()

		// Use a non-zero root index to also cover the padding path.
		commitmentTx, infos := newConnectorFixture(
			t, operatorKey, 5, 2, 3,
		)

		// Every assigned leaf must validate against the same tx.
		mappings := make(map[wire.OutPoint]*ConnectorLeafInfo)
		for i, info := range infos {
			mappings[wire.OutPoint{Index: uint32(i)}] = info
		}
		require.NoError(
			t, validateConnectorAncestry(
				commitmentTx, operatorKey, mappings,
			),
		)
	})

	t.Run("no mappings is a no-op", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, validateConnectorAncestry(
			nil, nil, nil,
		))
	})
}

// TestValidateConnectorAncestryRejects verifies that connector leaves that do
// not provably descend from the commitment tx — whether from an operator bug,
// misconfiguration, or compromise — are rejected before the forfeit is signed.
func TestValidateConnectorAncestryRejects(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey := operatorPriv.PubKey()

	// mutate returns a copy of a valid leaf info with fn applied.
	mutate := func(info *ConnectorLeafInfo,
		fn func(*ConnectorLeafInfo)) *ConnectorLeafInfo {

		cp := *info
		fn(&cp)

		return &cp
	}

	tests := []struct {
		name   string
		mutate func(info *ConnectorLeafInfo)
	}{
		{
			name: "root output index out of range",
			mutate: func(i *ConnectorLeafInfo) {
				i.RootOutputIndex = 99
			},
		},
		{
			name: "leaf not reachable by tree",
			mutate: func(i *ConnectorLeafInfo) {
				i.ConnectorOutpoint.Hash[0] ^= 0xff
			},
		},
		{
			name: "wrong radix reshapes the tree",
			mutate: func(i *ConnectorLeafInfo) {
				i.Radix = 3
			},
		},
		{
			name: "num leaves mismatch",
			mutate: func(i *ConnectorLeafInfo) {
				i.NumLeaves = 4
			},
		},
		{
			name: "num leaves exceeds maximum",
			mutate: func(i *ConnectorLeafInfo) {
				i.NumLeaves = maxConnectorTreeLeaves + 1
			},
		},
		{
			name: "radix exceeds maximum",
			mutate: func(i *ConnectorLeafInfo) {
				i.Radix = maxConnectorTreeRadix + 1
			},
		},
		{
			name: "radix below minimum",
			mutate: func(i *ConnectorLeafInfo) {
				i.Radix = 1
			},
		},
		{
			name: "leaf index out of range",
			mutate: func(i *ConnectorLeafInfo) {
				i.LeafIndex = 10
			},
		},
		{
			name: "tampered connector script",
			mutate: func(i *ConnectorLeafInfo) {
				i.ConnectorPkScript = []byte{
					0x51,
					0x20,
				}
			},
		},
		{
			name: "tampered connector amount",
			mutate: func(i *ConnectorLeafInfo) {
				i.ConnectorAmount = connectorLeafAmount + 1
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Rebuild against a multi-leaf tree so index/shape
			// mutations are meaningful.
			commitmentTx, infos := newConnectorFixture(
				t, operatorKey, 5, 2, 0,
			)
			bad := mutate(infos[2], tc.mutate)

			require.Error(
				t,
				validateConnectorAncestry(
					commitmentTx, operatorKey,
					mappingFrom(bad),
				),
			)
		})
	}

	t.Run("leaf from a different commitment tx", func(t *testing.T) {
		t.Parallel()

		// A leaf info built against commitment A must not validate
		// against a structurally identical commitment B with a
		// different txid.
		commitmentA, infosA := newConnectorFixture(
			t, operatorKey, 3, 2, 0,
		)
		require.NoError(
			t,
			validateConnectorAncestry(
				commitmentA, operatorKey,
				mappingFrom(infosA[1]),
			),
		)

		commitmentB := commitmentA.Copy()
		commitmentB.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Index: 1},
		})
		require.NotEqual(t, commitmentA.TxHash(), commitmentB.TxHash())

		require.Error(
			t,
			validateConnectorAncestry(
				commitmentB, operatorKey,
				mappingFrom(infosA[1]),
			),
		)
	})

	t.Run("wrong operator key reshapes the tree", func(t *testing.T) {
		t.Parallel()

		commitmentTx, infos := newConnectorFixture(
			t, operatorKey, 3, 2, 0,
		)

		otherPriv, otherErr := btcec.NewPrivateKey()
		require.NoError(t, otherErr)

		require.Error(
			t,
			validateConnectorAncestry(
				commitmentTx, otherPriv.PubKey(),
				mappingFrom(infos[1]),
			),
		)
	})

	t.Run("nil connector info", func(t *testing.T) {
		t.Parallel()

		commitmentTx, _ := newConnectorFixture(
			t, operatorKey, 1, 2, 0,
		)
		require.Error(
			t,
			validateConnectorAncestry(
				commitmentTx, operatorKey,
				map[wire.OutPoint]*ConnectorLeafInfo{
					{Index: 0}: nil,
				},
			),
		)
	})
}
