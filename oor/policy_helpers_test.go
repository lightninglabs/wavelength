package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestResolveSpendPathLeafReturnsASTNode asserts that locating a
// spend path in a policy template returns the matched semantic AST
// node so downstream callers can run AST-level checks (e.g.
// arkscript.ContainsKey) rather than byte-level scans over the
// compiled witness script. This is the shape co-sign depends on to
// reject leaves whose compiled script happens to include the
// operator key bytes as data pushes without actually CHECKSIGing
// against the key.
func TestResolveSpendPathLeafReturnsASTNode(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Build a template with a single non-operator leaf: a 1-of-1
	// multisig for the client alone. Its compiled script does not
	// contain the operator key either semantically (no Multisig
	// referencing operator) or lexically.
	clientOnlyLeaf := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				clientPriv.PubKey(),
			},
		},
	}

	template := &arkscript.PolicyTemplate{
		Leaves: []arkscript.LeafTemplate{
			clientOnlyLeaf,
		},
	}

	compiled, err := template.Compile()
	require.NoError(t, err)

	info, err := compiled.SpendInfo(0)
	require.NoError(t, err)

	spendPath := &arkscript.SpendPath{
		SpendInfo: info,
	}

	node, err := resolveSpendPathLeaf(template, spendPath)
	require.NoError(t, err)
	require.NotNil(t, node)

	// AST check on the matched node must reflect what the leaf
	// actually does: client key is present, operator key is not.
	require.True(
		t,
		arkscript.ContainsKey(
			node, clientPriv.PubKey(),
		),
		"client key should be present in AST",
	)
	require.False(
		t,
		arkscript.ContainsKey(
			node, operatorPriv.PubKey(),
		),
		"operator key must not be present in AST",
	)
}

// TestResolveSpendPathLeafRejectsUnknownSpendPath asserts that a
// spend path whose compiled script+control block do not appear in
// the policy template is rejected with an explicit error.
func TestResolveSpendPathLeafRejectsUnknownSpendPath(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	template := &arkscript.PolicyTemplate{
		Leaves: []arkscript.LeafTemplate{{
			Node: &arkscript.Multisig{
				Keys: []*btcec.PublicKey{
					clientPriv.PubKey(),
				},
			},
		}},
	}

	spendPath := &arkscript.SpendPath{
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: []byte{
				0x01,
				0x02,
				0x03,
			},
			ControlBlock: []byte{
				0x04,
				0x05,
				0x06,
			},
		},
	}

	_, err = resolveSpendPathLeaf(template, spendPath)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a leaf of vtxo policy template")
}
