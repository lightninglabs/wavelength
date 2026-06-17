package arkscript

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testVHTLCOpts returns a standard vHTLC opts for testing.
func testVHTLCOpts(t *testing.T) VHTLCOpts {
	t.Helper()

	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	server, _ := testutils.CreateKey(3)

	preimage, err := lntypes.MakePreimage(
		[]byte("test_preimage_32_bytes_exactly!!"),
	)
	require.NoError(t, err)

	return VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimage.Hash(),
		RefundLocktime:                       500000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	}
}

// TestVHTLCPolicyConstruction tests that we can construct a vHTLC policy
// using the AST closure system.
func TestVHTLCPolicyConstruction(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)
	require.NotNil(t, policy)

	// Verify we have 6 leaves.
	require.Len(t, policy.Leaves, 6, "vHTLC should have 6 leaves")

	// Verify output key is valid.
	outputKey := policy.OutputKey()
	require.NotNil(t, outputKey)

	t.Logf(
		"vHTLC output key: %s",
		hex.EncodeToString(
			outputKey.SerializeCompressed(),
		),
	)
	t.Logf("vHTLC root hash: %s",
		hex.EncodeToString(policy.RootHash))

	// Log all leaf scripts for inspection.
	for i, leaf := range policy.Leaves {
		dis, err := txscript.DisasmString(leaf.Leaf.Script)
		require.NoError(t, err)
		t.Logf("Leaf %d: %s", i, dis)
	}
}

// TestVHTLCLeafOrdering tests that leaves are sorted in canonical order
// and classifiable via ContainsKey (operator key presence).
func TestVHTLCLeafOrdering(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	// Classify leaves by operator key presence via template nodes.
	collabCount := 0
	exitCount := 0

	for _, leaf := range policy.Template.Leaves {
		if ContainsKey(leaf.Node, opts.Server) {
			collabCount++
		} else {
			exitCount++
		}
	}

	require.Equal(t, 3, collabCount, "should have 3 collab leaves")
	require.Equal(t, 3, exitCount, "should have 3 exit leaves")
}

// TestVHTLCSpendInfo tests that SpendInfo can be retrieved for each leaf.
func TestVHTLCSpendInfo(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	// Verify SpendInfo can be retrieved for each leaf.
	for i := range policy.Leaves {
		info, err := policy.SpendInfo(i)
		require.NoError(
			t, err, "failed to get SpendInfo for leaf %d", i,
		)
		require.NotEmpty(t, info.WitnessScript)
		require.NotEmpty(t, info.ControlBlock)

		// Control block should be parseable.
		ctrlBlock, err := txscript.ParseControlBlock(
			info.ControlBlock,
		)
		require.NoError(
			t, err, "failed to parse control block for leaf %d", i,
		)
		require.Equal(
			t, txscript.BaseLeafVersion, ctrlBlock.LeafVersion,
		)
	}
}

// TestVHTLCNamedAccessors tests the named SpendInfo accessors including
// tx-context requirements derived from the AST.
func TestVHTLCNamedAccessors(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	// Each SpendInfo accessor should return valid witness data.
	infoAccessors := []struct {
		name string
		fn   func() (*SpendInfo, error)
	}{
		{
			"Claim",
			policy.ClaimSpendInfo,
		},
		{
			"Refund",
			policy.RefundSpendInfo,
		},
		{
			"RefundWithoutReceiver",
			policy.RefundWithoutReceiverSpendInfo,
		},
		{
			"UnilateralClaim",
			policy.UnilateralClaimSpendInfo,
		},
		{
			"UnilateralRefund",
			policy.UnilateralRefundSpendInfo,
		},
		{
			"UnilateralRefundWithoutReceiver",
			policy.UnilateralRefundWithoutReceiverSpendInfo,
		},
	}

	for _, a := range infoAccessors {
		t.Run(a.name, func(t *testing.T) {
			info, err := a.fn()
			require.NoError(t, err)
			require.NotEmpty(t, info.WitnessScript)
			require.NotEmpty(t, info.ControlBlock)
		})
	}

	// Tx-context lives on SpendPath, derived via SpendPathForNode.
	pathAccessors := []struct {
		name     string
		node     Node
		wantSeq  uint32
		wantLock uint32
	}{
		{
			"Claim", policy.ClaimClosure,
			0xffffffff, 0,
		},
		{
			"Refund", policy.RefundClosure,
			0xffffffff, 0,
		},
		{
			"RefundWithoutReceiver",
			policy.RefundWithoutReceiverClosure,
			0xfffffffe, opts.RefundLocktime,
		},
		{
			"UnilateralClaim",
			policy.UnilateralClaimClosure,
			opts.UnilateralClaimDelay, 0,
		},
		{
			"UnilateralRefund",
			policy.UnilateralRefundClosure,
			opts.UnilateralRefundDelay, 0,
		},
		{
			"UnilateralRefundWithoutReceiver",
			policy.UnilateralRefundWithoutReceiverClosure,
			opts.UnilateralRefundWithoutReceiverDelay,
			opts.RefundLocktime,
		},
	}

	for _, a := range pathAccessors {
		t.Run(a.name+"_tx_context", func(t *testing.T) {
			path, err := policy.CompiledPolicy.SpendPathForNode(
				a.node, nil,
			)
			require.NoError(t, err)
			require.Equal(
				t, a.wantSeq, path.RequiredSequence,
				"RequiredSequence mismatch",
			)
			require.Equal(
				t, a.wantLock, path.RequiredLockTime,
				"RequiredLockTime mismatch",
			)
		})
	}
}

// TestVHTLCPkScript tests the PkScript convenience method.
func TestVHTLCPkScript(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)
	require.Len(t, pkScript, 34, "P2TR pkScript should be 34 bytes")
	require.Equal(t, byte(txscript.OP_1), pkScript[0])
}

// TestVHTLCDeterminism tests that vHTLC construction is deterministic.
func TestVHTLCDeterminism(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)

	policy1, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	policy2, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	require.Equal(
		t, policy1.OutputKey().SerializeCompressed(),
		policy2.OutputKey().SerializeCompressed(),
		"output keys should be deterministic",
	)

	require.Equal(
		t, policy1.RootHash, policy2.RootHash,
		"root hashes should be deterministic",
	)

	for i := range policy1.Leaves {
		require.Equal(
			t, policy1.Leaves[i].Leaf.Script,
			policy2.Leaves[i].Leaf.Script, "leaf %d script "+
				"should be deterministic", i,
		)
	}
}

// TestVHTLCComposition tests composing a vHTLC with an external root.
func TestVHTLCComposition(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	var externalRoot [32]byte
	copy(externalRoot[:], []byte("taproot_assets_commitment_root!"))

	composed, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, externalRoot,
	)
	require.NoError(t, err)

	require.NotEqual(
		t, policy.OutputKey().SerializeCompressed(),
		composed.OutputKey().SerializeCompressed(),
		"composed output key should differ",
	)

	for i := range policy.Leaves {
		info, err := composed.SpendInfo(i)
		require.NoError(t, err)

		originalInfo, err := policy.SpendInfo(i)
		require.NoError(t, err)
		require.Equal(
			t, len(originalInfo.ControlBlock)+32,
			len(info.ControlBlock),
			"composed control block should include external root",
		)
	}
}

// TestVHTLCValidation tests parameter validation.
func TestVHTLCValidation(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)

	t.Run("nil sender", func(t *testing.T) {
		bad := opts
		bad.Sender = nil
		_, err := NewVHTLCPolicy(bad)
		require.Error(t, err)
		require.Contains(t, err.Error(), "sender")
	})

	t.Run("zero preimage hash", func(t *testing.T) {
		bad := opts
		bad.PreimageHash = lntypes.Hash{}
		_, err := NewVHTLCPolicy(bad)
		require.Error(t, err)
		require.Contains(t, err.Error(), "zero")
	})

	t.Run("zero refund locktime", func(t *testing.T) {
		bad := opts
		bad.RefundLocktime = 0
		_, err := NewVHTLCPolicy(bad)
		require.Error(t, err)
		require.Contains(t, err.Error(), "refund locktime")
	})
}
