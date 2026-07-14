package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/internal/testutils"
	"github.com/stretchr/testify/require"
)

// TestLeafTemplateEncodeRoundTrip verifies that a semantic leaf template can
// be encoded, decoded, and recompiled without changing the script bytes.
func TestLeafTemplateEncodeRoundTrip(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	leaf := LeafTemplate{
		Node: &Multisig{
			Keys: []*btcec.PublicKey{
				ownerKey,
				operatorKey,
			},
		},
	}

	encoded, err := leaf.Encode()
	require.NoError(t, err)

	decoded, err := DecodeLeafTemplate(encoded)
	require.NoError(t, err)

	originalScript, err := leaf.Script()
	require.NoError(t, err)

	decodedScript, err := decoded.Script()
	require.NoError(t, err)

	require.Equal(t, originalScript, decodedScript)
}

// TestPolicyTemplateEncodeRoundTrip verifies that a full semantic policy
// template can be serialized and compiled deterministically.
func TestPolicyTemplateEncodeRoundTrip(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 144)
	require.NoError(t, err)

	encoded, err := policy.Template.Encode()
	require.NoError(t, err)

	decoded, err := DecodePolicyTemplate(encoded)
	require.NoError(t, err)

	compiled, err := decoded.Compile()
	require.NoError(t, err)

	require.Equal(t, policy.RootHash, compiled.RootHash)
	require.Equal(
		t, policy.OutputKey().SerializeCompressed(),
		compiled.OutputKey().SerializeCompressed(),
	)

	err = decoded.ValidateArkPolicy(PolicyValidationOpts{
		OperatorKey:  operatorKey,
		MinExitDelay: 100,
	})
	require.NoError(t, err)

	require.Len(t, decoded.ParticipantKeys(), 2)
}

// TestConditionLeafEncodeRoundTrip verifies that a generic condition leaf keeps
// its predicate and signer structure across binary serialization.
func TestConditionLeafEncodeRoundTrip(t *testing.T) {
	t.Parallel()

	receiverKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)
	hash := make([]byte, 20)
	hash[0] = 0xaa

	predicate, err := Hash160Condition(hash)
	require.NoError(t, err)

	leaf := LeafTemplate{
		Node: &Condition{
			Predicate: predicate,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					receiverKey, operatorKey,
				},
			},
		},
	}

	encoded, err := leaf.Encode()
	require.NoError(t, err)

	decoded, err := DecodeLeafTemplate(encoded)
	require.NoError(t, err)

	originalScript, err := leaf.Script()
	require.NoError(t, err)

	decodedScript, err := decoded.Script()
	require.NoError(t, err)

	require.Equal(t, originalScript, decodedScript)
	require.Len(t, decoded.ParticipantKeys(), 2)
}

// TestVTXOSettlementPairs verifies that a standard VTXO yields one settlement
// pair for the owner consisting of the unilateral CSV path plus the operator
// collaborative leaf.
func TestVTXOSettlementPairs(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 144)
	require.NoError(t, err)

	pairs, err := policy.Template.SettlementPairsForParticipant(
		ownerKey, operatorKey,
	)
	require.NoError(t, err)
	require.Len(t, pairs, 1)

	pair := pairs[0]
	require.Equal(t, 144, int(pair.AuthPath.RequiredSequence))
	require.Zero(t, pair.AuthPath.RequiredLockTime)
	require.Equal(t, uint32(0xffffffff),
		pair.ForfeitPath.RequiredSequence)
	require.Zero(t, pair.ForfeitPath.RequiredLockTime)
}

// TestVHTLCSettlementPairs verifies that each vHTLC participant gets one
// settlement pair covering their unilateral auth path and matching
// operator-backed forfeit path.
func TestVHTLCSettlementPairs(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)
	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	senderPairs, err := policy.Template.SettlementPairsForParticipant(
		opts.Sender, opts.Server,
	)
	require.NoError(t, err)
	require.Len(t, senderPairs, 2)
	require.Equal(
		t, opts.UnilateralRefundDelay,
		senderPairs[0].AuthPath.RequiredSequence,
	)
	// The sender/receiver refund auth path (CSV + Multisig) has no CLTV.
	require.Zero(t, senderPairs[0].AuthPath.RequiredLockTime)
	// The paired forfeit path is the cooperative Refund (all-party
	// multisig) which also has no CLTV.
	require.Equal(
		t, uint32(0xffffffff),
		senderPairs[0].ForfeitPath.RequiredSequence,
	)
	require.Zero(t, senderPairs[0].ForfeitPath.RequiredLockTime)

	require.Equal(
		t, opts.UnilateralRefundWithoutReceiverDelay,
		senderPairs[1].AuthPath.RequiredSequence,
	)
	require.Equal(
		t, opts.RefundLocktime,
		senderPairs[1].AuthPath.RequiredLockTime,
	)
	// The sender-only refund auth path is both CSV-gated by Ark exit
	// safety and CLTV-gated by the invoice refund locktime.
	require.Equal(
		t, uint32(0xfffffffe),
		senderPairs[1].ForfeitPath.RequiredSequence,
	)
	require.Equal(
		t, opts.RefundLocktime,
		senderPairs[1].ForfeitPath.RequiredLockTime,
	)

	receiverPairs, err := policy.Template.SettlementPairsForParticipant(
		opts.Receiver, opts.Server,
	)
	require.NoError(t, err)
	// The receiver participates in two business branches: claim
	// (hashlock) and refund (multisig), yielding two settlement pairs.
	require.Len(t, receiverPairs, 2)

	// Verify all receiver auth paths have CSV delays and no CLTV.
	for _, pair := range receiverPairs {
		require.NotZero(t, pair.AuthPath.RequiredSequence)
		require.NotEqual(
			t, uint32(0xffffffff), pair.AuthPath.RequiredSequence,
			"receiver auth paths should have CSV",
		)
		require.Zero(t, pair.AuthPath.RequiredLockTime)
	}
}

// TestVHTLCSettlementPairsMissingParticipant verifies that pair derivation
// fails when the requested participant is not part of the policy.
func TestVHTLCSettlementPairsMissingParticipant(t *testing.T) {
	t.Parallel()

	opts := testVHTLCOpts(t)
	policy, err := NewVHTLCPolicy(opts)
	require.NoError(t, err)

	otherKey, _ := testutils.CreateKey(99)
	_, err = policy.Template.SettlementPairsForParticipant(
		otherKey, opts.Server,
	)
	require.ErrorContains(t, err, "no settlement pairs")
}

// nestedConditionNode returns a Condition AST nested to the requested depth.
// Each level wraps the child in a Condition with a trivial 1-byte predicate.
// At depth=0 the base case is a minimal single-key Multisig.
func nestedConditionNode(t *testing.T, depth int) Node {
	t.Helper()

	key, _ := testutils.CreateKey(1)

	var node Node = &Multisig{
		Keys: []*btcec.PublicKey{key},
	}

	for i := 0; i < depth; i++ {
		node = &Condition{
			Predicate: []byte{
				0x01,
			},
			Inner: node,
		}
	}

	return node
}

// TestDecodePolicyTemplateRejectsOversizeBlob verifies that a raw blob larger
// than MaxPolicyTemplateBytes is rejected before any decode work begins.
func TestDecodePolicyTemplateRejectsOversizeBlob(t *testing.T) {
	t.Parallel()

	blob := make([]byte, MaxPolicyTemplateBytes+1)
	_, err := DecodePolicyTemplate(blob)
	require.ErrorContains(t, err, "exceeds maximum")
}

// TestDecodeNodeRejectsDeepRecursion verifies that an AST nested deeper than
// MaxPolicyDepth is rejected. This is the primary decode-bomb defense: the
// security-auditor PoC nested 100_000 Conditions into a 778 KB blob.
func TestDecodeNodeRejectsDeepRecursion(t *testing.T) {
	t.Parallel()

	deep := nestedConditionNode(t, MaxPolicyDepth+1)

	encoded, err := EncodeNode(deep)
	require.NoError(t, err)

	_, err = DecodeNode(encoded)
	require.Error(t, err)
	require.Contains(t, err.Error(), "depth")
}

// TestDecodeNodeAcceptsShallowRecursion verifies that the budget does not
// reject legitimate shallow ASTs at the boundary.
func TestDecodeNodeAcceptsShallowRecursion(t *testing.T) {
	t.Parallel()

	// Wrap the multisig leaf in MaxPolicyDepth-1 conditions so the root
	// decode consumes depth MaxPolicyDepth exactly.
	node := nestedConditionNode(t, MaxPolicyDepth-1)

	encoded, err := EncodeNode(node)
	require.NoError(t, err)

	decoded, err := DecodeNode(encoded)
	require.NoError(t, err)
	require.NotNil(t, decoded)
}

// TestDecodePolicyTemplateRejectsTooManyLeaves verifies that a policy with
// more than MaxPolicyLeaves leaves is rejected by the decoder.
func TestDecodePolicyTemplateRejectsTooManyLeaves(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)

	leaf := LeafTemplate{
		Node: &Multisig{
			Keys: []*btcec.PublicKey{
				ownerKey,
			},
		},
	}

	tooMany := make([]LeafTemplate, MaxPolicyLeaves+1)
	for i := range tooMany {
		tooMany[i] = leaf
	}

	encoded, err := (&PolicyTemplate{Leaves: tooMany}).Encode()
	require.NoError(t, err)

	_, err = DecodePolicyTemplate(encoded)
	require.ErrorContains(t, err, "leaf count")
}

// TestDecodePolicyTemplateBudgetSharedAcrossLeaves verifies that the node
// budget is shared across every leaf in a policy, so an attacker cannot
// allocate MaxPolicyNodes nodes per leaf. The test crafts a policy whose
// per-leaf node count is well under MaxPolicyNodes but whose *total* node
// count across leaves exceeds it — a per-leaf budget would accept the
// blob; only a shared budget rejects it.
func TestDecodePolicyTemplateBudgetSharedAcrossLeaves(t *testing.T) {
	t.Parallel()

	// Each leaf is (MaxPolicyDepth - 1) nested Conditions wrapping a
	// single-key Multisig, contributing exactly MaxPolicyDepth nodes
	// (one per enter() call). That keeps each leaf safely under both
	// the depth cap and the per-leaf node budget while giving us a
	// predictable contribution to the shared running count.
	perLeafNodes := MaxPolicyDepth
	leaf := LeafTemplate{
		Node: nestedConditionNode(t, MaxPolicyDepth-1),
	}

	// Positive case: leaves that safely fit under the shared cap must
	// decode cleanly. This guards against a regression where a
	// too-strict cap rejects legitimate policies.
	undercapLeaves := MaxPolicyNodes / perLeafNodes
	require.Positive(t, undercapLeaves)
	require.LessOrEqual(t, undercapLeaves, MaxPolicyLeaves)

	under := make([]LeafTemplate, undercapLeaves)
	for i := range under {
		under[i] = leaf
	}

	encodedUnder, err := (&PolicyTemplate{Leaves: under}).Encode()
	require.NoError(t, err)
	require.LessOrEqual(t, len(encodedUnder), MaxPolicyTemplateBytes)

	decodedUnder, err := DecodePolicyTemplate(encodedUnder)
	require.NoError(t, err)
	require.Len(t, decodedUnder.Leaves, undercapLeaves)

	// Negative case: add enough additional leaves that the TOTAL node
	// count strictly exceeds MaxPolicyNodes even though each individual
	// leaf stays well below the cap. A per-leaf budget would accept
	// this; a shared budget must reject it at the node-count check.
	overcapLeaves := MaxPolicyNodes/perLeafNodes + 1
	require.LessOrEqual(t, overcapLeaves, MaxPolicyLeaves)
	require.Greater(t, overcapLeaves*perLeafNodes, MaxPolicyNodes)

	over := make([]LeafTemplate, overcapLeaves)
	for i := range over {
		over[i] = leaf
	}

	encodedOver, err := (&PolicyTemplate{Leaves: over}).Encode()
	require.NoError(t, err)
	require.LessOrEqual(t, len(encodedOver), MaxPolicyTemplateBytes)

	_, err = DecodePolicyTemplate(encodedOver)
	require.ErrorContains(t, err, "node count")
}
