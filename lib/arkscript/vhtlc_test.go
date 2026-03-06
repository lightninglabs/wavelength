package arkscript

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ripemd160"
)

// VHTLCOpts contains the parameters for constructing a vHTLC policy.
// This mirrors the ark/sdk/vhtlc.Opts structure.
type VHTLCOpts struct {
	// Sender is the party initiating the HTLC (payer).
	Sender *btcec.PublicKey

	// Receiver is the party receiving the HTLC payment (payee).
	Receiver *btcec.PublicKey

	// Server is the Ark operator key.
	Server *btcec.PublicKey

	// PreimageHash is the HASH160 of the preimage (20 bytes).
	PreimageHash []byte

	// RefundLocktime is the absolute locktime for refund without receiver
	// (CLTV).
	RefundLocktime uint32

	// UnilateralClaimDelay is the CSV delay for unilateral claim path.
	UnilateralClaimDelay uint32

	// UnilateralRefundDelay is the CSV delay for unilateral refund path.
	UnilateralRefundDelay uint32

	// UnilateralRefundWithoutReceiverDelay is the CSV delay for unilateral
	// refund without receiver.
	UnilateralRefundWithoutReceiverDelay uint32
}

// VHTLCPolicy represents a compiled vHTLC taproot policy.
type VHTLCPolicy struct {
	*CompiledPolicy

	// Individual closures for easy access.
	ClaimClosure                           Node
	RefundClosure                          Node
	RefundWithoutReceiverClosure           Node
	UnilateralClaimClosure                 Node
	UnilateralRefundClosure                Node
	UnilateralRefundWithoutReceiverClosure Node
}

// NewVHTLCPolicy constructs a vHTLC policy using the AST closure system.
//
// The vHTLC has 6 leaves:
//  1. Claim (collab): HashLock(preimage) + Multisig([receiver, server])
//  2. Refund (collab): Multisig([sender, receiver, server])
//  3. RefundWithoutReceiver (collab): CLTV(locktime) + Multisig([sender, server])
//  4. UnilateralClaim (exit): CSV(delay) + HashLock(preimage) + Checksig(receiver)
//  5. UnilateralRefund (exit): CSV(delay) + Multisig([sender, receiver])
//  6. UnilateralRefundWithoutReceiver (exit): CSV(delay) + Checksig(sender)
func NewVHTLCPolicy(opts VHTLCOpts) (*VHTLCPolicy, error) {
	// Build the 6 closures using AST composition.

	// 1. Claim: HashLock(HASH160, preimageHash, Multisig([receiver, server]))
	// Collaborative path - receiver claims with preimage + server cosign.
	claimClosure := &HashLock{
		Algorithm: HashAlgoHash160,
		Hash:      opts.PreimageHash,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{opts.Receiver, opts.Server},
			Type: MultisigTypeChecksig,
		},
	}

	// 2. Refund: Multisig([sender, receiver, server])
	// Collaborative refund - all parties agree.
	refundClosure := &Multisig{
		Keys: []*btcec.PublicKey{opts.Sender, opts.Receiver, opts.Server},
		Type: MultisigTypeChecksig,
	}

	// 3. RefundWithoutReceiver: CLTV(locktime, Multisig([sender, server]))
	// Collaborative refund without receiver after absolute timeout.
	refundWithoutReceiverClosure := &CLTV{
		Lock: opts.RefundLocktime,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{opts.Sender, opts.Server},
			Type: MultisigTypeChecksig,
		},
	}

	// 4. UnilateralClaim: CSV(delay, HashLock(preimageHash, Checksig(receiver)))
	// Exit path - receiver can claim unilaterally with preimage after delay.
	unilateralClaimClosure := &CSV{
		Lock: opts.UnilateralClaimDelay,
		Inner: &HashLock{
			Algorithm: HashAlgoHash160,
			Hash:      opts.PreimageHash,
			Inner:     &Checksig{Key: opts.Receiver},
		},
	}

	// 5. UnilateralRefund: CSV(delay, Multisig([sender, receiver]))
	// Exit path - sender and receiver can refund unilaterally after delay.
	unilateralRefundClosure := &CSV{
		Lock: opts.UnilateralRefundDelay,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{opts.Sender, opts.Receiver},
			Type: MultisigTypeChecksig,
		},
	}

	// 6. UnilateralRefundWithoutReceiver: CSV(delay, Checksig(sender))
	// Exit path - sender can refund alone after longest delay.
	unilateralRefundWithoutReceiverClosure := &CSV{
		Lock: opts.UnilateralRefundWithoutReceiverDelay,
		Inner: &Checksig{
			Key: opts.Sender,
		},
	}

	// Compile all closures to scripts.
	claimScript, err := claimClosure.Script()
	if err != nil {
		return nil, err
	}

	refundScript, err := refundClosure.Script()
	if err != nil {
		return nil, err
	}

	refundWithoutReceiverScript, err := refundWithoutReceiverClosure.Script()
	if err != nil {
		return nil, err
	}

	unilateralClaimScript, err := unilateralClaimClosure.Script()
	if err != nil {
		return nil, err
	}

	unilateralRefundScript, err := unilateralRefundClosure.Script()
	if err != nil {
		return nil, err
	}

	unilateralRefundWithoutReceiverScript, err := unilateralRefundWithoutReceiverClosure.Script()
	if err != nil {
		return nil, err
	}

	// Build leaves with roles. Collaborative paths are "collab" role,
	// unilateral paths are "exit" role.
	leaves := []PolicyLeaf{
		// Collaborative paths (with server).
		{
			Role: LeafRoleCollab,
			Leaf: txscript.NewBaseTapLeaf(claimScript),
		},
		{
			Role: LeafRoleCollab,
			Leaf: txscript.NewBaseTapLeaf(refundScript),
		},
		{
			Role: LeafRoleCollab,
			Leaf: txscript.NewBaseTapLeaf(refundWithoutReceiverScript),
		},
		// Exit paths (without server).
		{
			Role: LeafRoleExit,
			Leaf: txscript.NewBaseTapLeaf(unilateralClaimScript),
		},
		{
			Role: LeafRoleExit,
			Leaf: txscript.NewBaseTapLeaf(unilateralRefundScript),
		},
		{
			Role: LeafRoleExit,
			Leaf: txscript.NewBaseTapLeaf(unilateralRefundWithoutReceiverScript),
		},
	}

	// Sort leaves canonically.
	SortLeaves(leaves)

	// Build the taproot tree.
	policy, err := BuildTree(leaves, &scripts.ARKNUMSKey)
	if err != nil {
		return nil, err
	}

	return &VHTLCPolicy{
		CompiledPolicy:                         policy,
		ClaimClosure:                           claimClosure,
		RefundClosure:                          refundClosure,
		RefundWithoutReceiverClosure:           refundWithoutReceiverClosure,
		UnilateralClaimClosure:                 unilateralClaimClosure,
		UnilateralRefundClosure:                unilateralRefundClosure,
		UnilateralRefundWithoutReceiverClosure: unilateralRefundWithoutReceiverClosure,
	}, nil
}

// hash160 computes RIPEMD160(SHA256(data)).
func hash160(data []byte) []byte {
	sha := sha256.Sum256(data)
	ripemd := ripemd160.New()
	ripemd.Write(sha[:])

	return ripemd.Sum(nil)
}

// TestVHTLCPolicyConstruction tests that we can construct a vHTLC policy
// using the AST closure system.
func TestVHTLCPolicyConstruction(t *testing.T) {
	t.Parallel()

	// Create test keys.
	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	server, _ := testutils.CreateKey(3)

	// Create a test preimage and hash.
	preimage := []byte("test_preimage_32_bytes_exactly!!")
	preimageHash := hash160(preimage)

	opts := VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimageHash,
		RefundLocktime:                       500000, // Absolute locktime
		UnilateralClaimDelay:                 144,    // ~1 day in blocks
		UnilateralRefundDelay:                288,    // ~2 days in blocks
		UnilateralRefundWithoutReceiverDelay: 1008,   // ~1 week in blocks
	}

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)
	require.NotNil(t, policy)

	// Verify we have 6 leaves.
	require.Len(t, policy.Leaves, 6, "vHTLC should have 6 leaves")

	// Verify output key is valid.
	outputKey := policy.OutputKey()
	require.NotNil(t, outputKey)

	t.Logf("vHTLC output key: %s",
		hex.EncodeToString(outputKey.SerializeCompressed()))
	t.Logf("vHTLC root hash: %s",
		hex.EncodeToString(policy.RootHash))

	// Log all leaf scripts for inspection.
	for i, leaf := range policy.Leaves {
		dis, err := txscript.DisasmString(leaf.Leaf.Script)
		require.NoError(t, err)
		t.Logf("Leaf %d (%s): %s", i, leaf.Role, dis)
	}
}

// TestVHTLCLeafOrdering tests that leaves are sorted in canonical order.
func TestVHTLCLeafOrdering(t *testing.T) {
	t.Parallel()

	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	server, _ := testutils.CreateKey(3)

	preimage := []byte("test_preimage_32_bytes_exactly!!")
	preimageHash := hash160(preimage)

	opts := VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimageHash,
		RefundLocktime:                       500000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	}

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	// Verify collab leaves come before exit leaves.
	collabCount := 0
	exitCount := 0
	seenExit := false

	for _, leaf := range policy.Leaves {
		if leaf.Role == LeafRoleCollab {
			require.False(t, seenExit,
				"collab leaves must come before exit leaves")
			collabCount++
		} else if leaf.Role == LeafRoleExit {
			seenExit = true
			exitCount++
		}
	}

	require.Equal(t, 3, collabCount, "should have 3 collab leaves")
	require.Equal(t, 3, exitCount, "should have 3 exit leaves")
}

// TestVHTLCSpendInfo tests that SpendInfo can be retrieved for each leaf.
func TestVHTLCSpendInfo(t *testing.T) {
	t.Parallel()

	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	server, _ := testutils.CreateKey(3)

	preimage := []byte("test_preimage_32_bytes_exactly!!")
	preimageHash := hash160(preimage)

	opts := VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimageHash,
		RefundLocktime:                       500000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	}

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	// Verify SpendInfo can be retrieved for each leaf.
	for i := range policy.Leaves {
		info, err := policy.SpendInfo(i)
		require.NoError(t, err, "failed to get SpendInfo for leaf %d", i)
		require.NotEmpty(t, info.WitnessScript)
		require.NotEmpty(t, info.ControlBlock)

		// Control block should be parseable.
		ctrlBlock, err := txscript.ParseControlBlock(info.ControlBlock)
		require.NoError(t, err, "failed to parse control block for leaf %d", i)
		require.Equal(t, txscript.BaseLeafVersion, ctrlBlock.LeafVersion)
	}
}

// TestVHTLCTxContextDerivation tests that tx-context requirements are derived
// correctly for each leaf.
func TestVHTLCTxContextDerivation(t *testing.T) {
	t.Parallel()

	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	server, _ := testutils.CreateKey(3)

	preimage := []byte("test_preimage_32_bytes_exactly!!")
	preimageHash := hash160(preimage)

	opts := VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimageHash,
		RefundLocktime:                       500000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	}

	_, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	// Test tx-context derivation for each closure type.

	// ClaimClosure: HashLock + Multisig (no timelock).
	claimSeq := DeriveSequence(opts.claimClosure(opts))
	claimLocktime := DeriveLockTime(opts.claimClosure(opts))
	require.Equal(t, uint32(0xffffffff), claimSeq,
		"claim should have no sequence requirement")
	require.Equal(t, uint32(0), claimLocktime,
		"claim should have no locktime requirement")

	// RefundClosure: Multisig (no timelock).
	refundSeq := DeriveSequence(opts.refundClosure())
	refundLocktime := DeriveLockTime(opts.refundClosure())
	require.Equal(t, uint32(0xffffffff), refundSeq)
	require.Equal(t, uint32(0), refundLocktime)

	// RefundWithoutReceiverClosure: CLTV + Multisig.
	refundNoReceiverSeq := DeriveSequence(opts.refundWithoutReceiverClosure(opts))
	refundNoReceiverLocktime := DeriveLockTime(opts.refundWithoutReceiverClosure(opts))
	require.Equal(t, uint32(0xfffffffe), refundNoReceiverSeq,
		"CLTV requires non-final sequence")
	require.Equal(t, opts.RefundLocktime, refundNoReceiverLocktime,
		"CLTV locktime should match")

	// UnilateralClaimClosure: CSV + HashLock + Checksig.
	unilateralClaimSeq := DeriveSequence(opts.unilateralClaimClosure(opts))
	unilateralClaimLocktime := DeriveLockTime(opts.unilateralClaimClosure(opts))
	require.Equal(t, opts.UnilateralClaimDelay, unilateralClaimSeq,
		"CSV delay should be returned as sequence")
	require.Equal(t, uint32(0), unilateralClaimLocktime,
		"CSV has no locktime requirement")

	// UnilateralRefundClosure: CSV + Multisig.
	unilateralRefundSeq := DeriveSequence(opts.unilateralRefundClosure(opts))
	unilateralRefundLocktime := DeriveLockTime(opts.unilateralRefundClosure(opts))
	require.Equal(t, opts.UnilateralRefundDelay, unilateralRefundSeq)
	require.Equal(t, uint32(0), unilateralRefundLocktime)

	// UnilateralRefundWithoutReceiverClosure: CSV + Checksig.
	unilateralRefundNoReceiverSeq := DeriveSequence(
		opts.unilateralRefundWithoutReceiverClosure(opts),
	)
	unilateralRefundNoReceiverLocktime := DeriveLockTime(
		opts.unilateralRefundWithoutReceiverClosure(opts),
	)
	require.Equal(t, opts.UnilateralRefundWithoutReceiverDelay,
		unilateralRefundNoReceiverSeq)
	require.Equal(t, uint32(0), unilateralRefundNoReceiverLocktime)
}

// Helper methods to construct closures for tx-context testing.
func (opts VHTLCOpts) claimClosure(o VHTLCOpts) Node {
	return &HashLock{
		Algorithm: HashAlgoHash160,
		Hash:      o.PreimageHash,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{o.Receiver, o.Server},
			Type: MultisigTypeChecksig,
		},
	}
}

func (opts VHTLCOpts) refundClosure() Node {
	return &Multisig{
		Keys: []*btcec.PublicKey{opts.Sender, opts.Receiver, opts.Server},
		Type: MultisigTypeChecksig,
	}
}

func (opts VHTLCOpts) refundWithoutReceiverClosure(o VHTLCOpts) Node {
	return &CLTV{
		Lock: o.RefundLocktime,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{o.Sender, o.Server},
			Type: MultisigTypeChecksig,
		},
	}
}

func (opts VHTLCOpts) unilateralClaimClosure(o VHTLCOpts) Node {
	return &CSV{
		Lock: o.UnilateralClaimDelay,
		Inner: &HashLock{
			Algorithm: HashAlgoHash160,
			Hash:      o.PreimageHash,
			Inner:     &Checksig{Key: o.Receiver},
		},
	}
}

func (opts VHTLCOpts) unilateralRefundClosure(o VHTLCOpts) Node {
	return &CSV{
		Lock: o.UnilateralRefundDelay,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{o.Sender, o.Receiver},
			Type: MultisigTypeChecksig,
		},
	}
}

func (opts VHTLCOpts) unilateralRefundWithoutReceiverClosure(o VHTLCOpts) Node {
	return &CSV{
		Lock:  o.UnilateralRefundWithoutReceiverDelay,
		Inner: &Checksig{Key: o.Sender},
	}
}

// TestVHTLCDeterminism tests that vHTLC construction is deterministic.
func TestVHTLCDeterminism(t *testing.T) {
	t.Parallel()

	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	server, _ := testutils.CreateKey(3)

	preimage := []byte("test_preimage_32_bytes_exactly!!")
	preimageHash := hash160(preimage)

	opts := VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimageHash,
		RefundLocktime:                       500000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	}

	policy1, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	policy2, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	// Output keys should be identical.
	require.Equal(t,
		policy1.OutputKey().SerializeCompressed(),
		policy2.OutputKey().SerializeCompressed(),
		"output keys should be deterministic")

	// Root hashes should be identical.
	require.Equal(t, policy1.RootHash, policy2.RootHash,
		"root hashes should be deterministic")

	// All leaf scripts should be identical.
	for i := range policy1.Leaves {
		require.Equal(t,
			policy1.Leaves[i].Leaf.Script,
			policy2.Leaves[i].Leaf.Script,
			"leaf %d script should be deterministic", i)
	}
}

// TestVHTLCComposition tests composing a vHTLC with an external root
// (for Taproot Assets integration).
func TestVHTLCComposition(t *testing.T) {
	t.Parallel()

	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	server, _ := testutils.CreateKey(3)

	preimage := []byte("test_preimage_32_bytes_exactly!!")
	preimageHash := hash160(preimage)

	opts := VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimageHash,
		RefundLocktime:                       500000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	}

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	// Simulate an external root (e.g., from Taproot Assets).
	var externalRoot [32]byte
	copy(externalRoot[:], []byte("taproot_assets_commitment_root!"))

	// Compose with external root.
	composed, err := ComposeWithSiblingRoot(policy.CompiledPolicy, externalRoot)
	require.NoError(t, err)

	// Composed output key should differ from original.
	require.NotEqual(t,
		policy.OutputKey().SerializeCompressed(),
		composed.OutputKey().SerializeCompressed(),
		"composed output key should differ")

	// SpendInfo should work for composed policy.
	for i := range policy.Leaves {
		info, err := composed.SpendInfo(i)
		require.NoError(t, err)

		// Control block should be 32 bytes longer (external root added).
		originalInfo, err := policy.SpendInfo(i)
		require.NoError(t, err)
		require.Equal(t,
			len(originalInfo.ControlBlock)+32,
			len(info.ControlBlock),
			"composed control block should include external root")
	}
}

// TestVHTLCScriptDisassembly provides a visual inspection of the compiled
// scripts for documentation purposes.
func TestVHTLCScriptDisassembly(t *testing.T) {
	t.Parallel()

	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	server, _ := testutils.CreateKey(3)

	preimage := []byte("test_preimage_32_bytes_exactly!!")
	preimageHash := hash160(preimage)

	opts := VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimageHash,
		RefundLocktime:                       500000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	}

	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	// Document expected script structures.
	t.Log("vHTLC Script Structures:")
	t.Log("========================")
	t.Log("")

	// Disassemble each closure.
	closures := []struct {
		name string
		node Node
	}{
		{"Claim (HashLock+Multisig)", policy.ClaimClosure},
		{"Refund (Multisig)", policy.RefundClosure},
		{"RefundWithoutReceiver (CLTV+Multisig)", policy.RefundWithoutReceiverClosure},
		{"UnilateralClaim (CSV+HashLock+Checksig)", policy.UnilateralClaimClosure},
		{"UnilateralRefund (CSV+Multisig)", policy.UnilateralRefundClosure},
		{"UnilateralRefundWithoutReceiver (CSV+Checksig)", policy.UnilateralRefundWithoutReceiverClosure},
	}

	for _, c := range closures {
		script, err := c.node.Script()
		require.NoError(t, err)

		dis, err := txscript.DisasmString(script)
		require.NoError(t, err)

		t.Logf("%s:", c.name)
		t.Logf("  Script (%d bytes): %s", len(script), dis)
		t.Logf("  Hex: %s", hex.EncodeToString(script))
		t.Log("")
	}
}
