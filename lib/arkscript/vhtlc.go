package arkscript

import (
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"golang.org/x/crypto/ripemd160" //nolint:gosec
)

// VHTLCOpts contains the parameters for constructing a vHTLC policy.
type VHTLCOpts struct {
	// Sender is the party initiating the HTLC (payer).
	Sender *btcec.PublicKey

	// Receiver is the party receiving the HTLC payment (payee).
	Receiver *btcec.PublicKey

	// Server is the Ark operator key.
	Server *btcec.PublicKey

	// PreimageHash is the SHA256 of the preimage (32 bytes).
	// This matches the Lightning Network payment hash format:
	// paymentHash = SHA256(preimage).
	PreimageHash []byte

	// RefundLocktime is the absolute locktime for refund without
	// receiver (CLTV).
	RefundLocktime uint32

	// UnilateralClaimDelay is the CSV delay for unilateral claim path.
	UnilateralClaimDelay uint32

	// UnilateralRefundDelay is the CSV delay for unilateral refund
	// path.
	UnilateralRefundDelay uint32

	// UnilateralRefundWithoutReceiverDelay is the CSV delay for
	// unilateral refund without receiver.
	UnilateralRefundWithoutReceiverDelay uint32
}

// VHTLCPolicy represents a compiled vHTLC taproot policy.
type VHTLCPolicy struct {
	// Template is the semantic policy template for this vHTLC.
	Template *PolicyTemplate

	*CompiledPolicy

	// Individual closures for easy access.
	ClaimClosure                           Node
	RefundClosure                          Node
	RefundWithoutReceiverClosure           Node
	UnilateralClaimClosure                 Node
	UnilateralRefundClosure                Node
	UnilateralRefundWithoutReceiverClosure Node

	// claimLeafIndex is the canonical index of the Claim leaf after
	// sorting.
	claimLeafIndex int

	// refundLeafIndex is the canonical index of the Refund leaf.
	refundLeafIndex int

	// refundWithoutReceiverLeafIndex is the canonical index of the
	// RefundWithoutReceiver leaf.
	refundWithoutReceiverLeafIndex int

	// unilateralClaimLeafIndex is the canonical index of the
	// UnilateralClaim leaf.
	unilateralClaimLeafIndex int

	// unilateralRefundLeafIndex is the canonical index of the
	// UnilateralRefund leaf.
	unilateralRefundLeafIndex int

	// unilateralRefundWithoutReceiverLeafIndex is the canonical index
	// of the UnilateralRefundWithoutReceiver leaf.
	unilateralRefundWithoutReceiverLeafIndex int

	// orderedNodes maps leaf index → AST Node in canonical order.
	orderedNodes []Node
}

// NewVHTLCPolicy constructs a vHTLC policy using the AST closure system.
//
// The vHTLC has 6 leaves:
//  1. Claim (collab): HashLock(preimage) + Multisig([receiver, server])
//  2. Refund (collab): Multisig([sender, receiver, server])
//  3. RefundWithoutReceiver (collab): CLTV(locktime) +
//     Multisig([sender, server])
//  4. UnilateralClaim (exit): CSV(delay) + HashLock(preimage) +
//     Checksig(receiver)
//  5. UnilateralRefund (exit): CSV(delay) + Multisig([sender, receiver])
//  6. UnilateralRefundWithoutReceiver (exit): CSV(delay) +
//     Checksig(sender)
func NewVHTLCPolicy(opts VHTLCOpts) (*VHTLCPolicy, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	claimPredicate, err := sha256Condition(opts.PreimageHash)
	if err != nil {
		return nil, err
	}

	refundPredicate, err := AbsoluteLockTimeCondition(opts.RefundLocktime)
	if err != nil {
		return nil, err
	}

	// 1. Claim: SHA256(preimage) + Multisig([receiver, server]).
	claimClosure := &Condition{
		Predicate: claimPredicate,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				opts.Receiver, opts.Server,
			},
		},
	}

	// 2. Refund: Multisig([sender, receiver, server]).
	refundClosure := &Multisig{
		Keys: []*btcec.PublicKey{
			opts.Sender, opts.Receiver, opts.Server,
		},
	}

	// 3. RefundWithoutReceiver: CLTV(locktime) +
	// Multisig([sender, server]).
	refundWithoutReceiverClosure := &Condition{
		Predicate: refundPredicate,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				opts.Sender, opts.Server,
			},
		},
	}

	// 4. UnilateralClaim: CSV(delay) + SHA256(preimage) +
	// Multisig([receiver]).
	unilateralClaimClosure := &CSV{
		Lock: opts.UnilateralClaimDelay,
		Inner: &Condition{
			Predicate: claimPredicate,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{opts.Receiver},
			},
		},
	}

	// 5. UnilateralRefund: CSV(delay) + Multisig([sender, receiver]).
	unilateralRefundClosure := &CSV{
		Lock: opts.UnilateralRefundDelay,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				opts.Sender, opts.Receiver,
			},
		},
	}

	// 6. UnilateralRefundWithoutReceiver: CSV(delay) +
	// Multisig([sender]).
	unilateralRefundWithoutReceiverClosure := &CSV{
		Lock: opts.UnilateralRefundWithoutReceiverDelay,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{opts.Sender},
		},
	}

	// Compile all closures and build leaf set. The first three are
	// collaborative (require operator), the last three are unilateral
	// exit paths.
	type closureEntry struct {
		node Node
		role LeafRole
	}

	closures := []closureEntry{
		{claimClosure, LeafRoleCollab},
		{refundClosure, LeafRoleCollab},
		{refundWithoutReceiverClosure, LeafRoleCollab},
		{unilateralClaimClosure, LeafRoleExit},
		{unilateralRefundClosure, LeafRoleExit},
		{unilateralRefundWithoutReceiverClosure, LeafRoleExit},
	}

	leaves := make([]PolicyLeaf, len(closures))
	leafTemplates := make([]LeafTemplate, len(closures))
	for i, entry := range closures {
		script, err := entry.node.Script()
		if err != nil {
			return nil, fmt.Errorf("compile closure %d: %w",
				i, err)
		}

		leaves[i] = PolicyLeaf{
			Leaf: txscript.NewBaseTapLeaf(script),
			Role: entry.role,
		}
		leafTemplates[i] = LeafTemplate{Node: entry.node}
	}

	template := &PolicyTemplate{Leaves: leafTemplates}

	// Sort leaves canonically and track where each closure ended up.
	SortLeaves(leaves)

	// Build index mapping by matching scripts.
	claimScript, _ := claimClosure.Script()
	refundScript, _ := refundClosure.Script()
	refundWithoutReceiverScript, _ := refundWithoutReceiverClosure.Script()
	unilateralClaimScript, _ := unilateralClaimClosure.Script()
	unilateralRefundScript, _ := unilateralRefundClosure.Script()
	unilateralRefundWithoutReceiverScript, _ :=
		unilateralRefundWithoutReceiverClosure.Script()

	scriptToIndex := make(map[string]int, len(leaves))
	for i, leaf := range leaves {
		scriptToIndex[string(leaf.Leaf.Script)] = i
	}

	// Build ordered nodes slice (leaf index → AST Node).
	stn := make(map[string]Node, 6)
	stn[string(claimScript)] = claimClosure
	stn[string(refundScript)] = refundClosure
	stn[string(refundWithoutReceiverScript)] =
		refundWithoutReceiverClosure
	stn[string(unilateralClaimScript)] =
		unilateralClaimClosure
	stn[string(unilateralRefundScript)] =
		unilateralRefundClosure
	stn[string(unilateralRefundWithoutReceiverScript)] =
		unilateralRefundWithoutReceiverClosure

	ordered := make([]Node, len(leaves))
	for i, leaf := range leaves {
		ordered[i] = stn[string(leaf.Leaf.Script)]
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	if err != nil {
		return nil, fmt.Errorf("build vhtlc tree: %w", err)
	}

	si := scriptToIndex

	return &VHTLCPolicy{
		Template:       template,
		CompiledPolicy: policy,

		ClaimClosure:                 claimClosure,
		RefundClosure:                refundClosure,
		RefundWithoutReceiverClosure: refundWithoutReceiverClosure,
		UnilateralClaimClosure:       unilateralClaimClosure,
		UnilateralRefundClosure:      unilateralRefundClosure,

		UnilateralRefundWithoutReceiverClosure: unilateralRefundWithoutReceiverClosure, //nolint:ll

		claimLeafIndex:  si[string(claimScript)],
		refundLeafIndex: si[string(refundScript)],
		refundWithoutReceiverLeafIndex: si[string(
			refundWithoutReceiverScript,
		)],
		unilateralClaimLeafIndex: si[string(
			unilateralClaimScript,
		)],
		unilateralRefundLeafIndex: si[string(
			unilateralRefundScript,
		)],
		unilateralRefundWithoutReceiverLeafIndex: si[string(
			unilateralRefundWithoutReceiverScript,
		)],
		orderedNodes: ordered,
	}, nil
}

// ClaimSpendInfo returns the spend information for the Claim path.
func (p *VHTLCPolicy) ClaimSpendInfo() (*SpendInfo, error) {
	return p.SpendInfo(p.claimLeafIndex)
}

// RefundSpendInfo returns the spend information for the Refund path.
func (p *VHTLCPolicy) RefundSpendInfo() (*SpendInfo, error) {
	return p.SpendInfo(p.refundLeafIndex)
}

// RefundWithoutReceiverSpendInfo returns the spend information for the
// RefundWithoutReceiver path.
func (p *VHTLCPolicy) RefundWithoutReceiverSpendInfo() (*SpendInfo,
	error) {

	return p.SpendInfo(p.refundWithoutReceiverLeafIndex)
}

// UnilateralClaimSpendInfo returns the spend information for the
// UnilateralClaim path.
func (p *VHTLCPolicy) UnilateralClaimSpendInfo() (*SpendInfo, error) {
	return p.SpendInfo(p.unilateralClaimLeafIndex)
}

// UnilateralRefundSpendInfo returns the spend information for the
// UnilateralRefund path.
func (p *VHTLCPolicy) UnilateralRefundSpendInfo() (*SpendInfo, error) {
	return p.SpendInfo(p.unilateralRefundLeafIndex)
}

// UnilateralRefundWithoutReceiverSpendInfo returns the spend information
// for the UnilateralRefundWithoutReceiver path.
func (p *VHTLCPolicy) UnilateralRefundWithoutReceiverSpendInfo() (
	*SpendInfo, error) {

	return p.SpendInfo(
		p.unilateralRefundWithoutReceiverLeafIndex,
	)
}

// ClaimPath returns a SpendPath for claiming via the hashlock leaf.
// The preimage is included as a condition witness element.
func (p *VHTLCPolicy) ClaimPath(
	preimage []byte) (*SpendPath, error) {

	info, err := p.ClaimSpendInfo()
	if err != nil {
		return nil, err
	}

	return &SpendPath{
		SpendInfo:  info,
		Conditions: [][]byte{preimage},
	}, nil
}

// RefundPath returns a SpendPath for the cooperative refund
// (all parties sign, no conditions).
func (p *VHTLCPolicy) RefundPath() (*SpendPath, error) {
	info, err := p.RefundSpendInfo()
	if err != nil {
		return nil, err
	}

	return &SpendPath{SpendInfo: info}, nil
}

// RefundWithoutReceiverPath returns a SpendPath for the CLTV-gated
// refund without receiver.
func (p *VHTLCPolicy) RefundWithoutReceiverPath() (*SpendPath,
	error) {

	info, err := p.RefundWithoutReceiverSpendInfo()
	if err != nil {
		return nil, err
	}

	return &SpendPath{SpendInfo: info}, nil
}

// OrderedNodes returns the AST nodes in canonical leaf order,
// matching the Leaves slice indices.
func (p *VHTLCPolicy) OrderedNodes() []Node {
	return p.orderedNodes
}

// PkScript returns the P2TR pkScript for the vHTLC output.
func (p *VHTLCPolicy) PkScript() ([]byte, error) {
	return txscript.PayToTaprootScript(p.OutputKey())
}

// validate checks that all required fields are populated.
func (opts *VHTLCOpts) validate() error {
	switch {
	case opts.Sender == nil:
		return fmt.Errorf("vhtlc: sender key is nil")

	case opts.Receiver == nil:
		return fmt.Errorf("vhtlc: receiver key is nil")

	case opts.Server == nil:
		return fmt.Errorf("vhtlc: server key is nil")

	case len(opts.PreimageHash) != 32:
		return fmt.Errorf("vhtlc: preimage hash must be 32 bytes "+
			"(SHA256), got %d", len(opts.PreimageHash))

	case opts.UnilateralClaimDelay == 0:
		return fmt.Errorf("vhtlc: unilateral claim delay must " +
			"be non-zero")

	case opts.UnilateralRefundDelay == 0:
		return fmt.Errorf("vhtlc: unilateral refund delay must " +
			"be non-zero")

	case opts.UnilateralRefundWithoutReceiverDelay == 0:
		return fmt.Errorf("vhtlc: unilateral refund without " +
			"receiver delay must be non-zero")
	}

	return nil
}

// Hash160 computes RIPEMD160(SHA256(data)).
func Hash160(data []byte) []byte {
	sha := sha256.Sum256(data)
	ripemd := ripemd160.New() //nolint:gosec
	ripemd.Write(sha[:])

	return ripemd.Sum(nil)
}

// sha256Condition builds the canonical script prefix for
// SHA256(<witness_item>) == hash with a fixed 32-byte witness item.
func sha256Condition(hash []byte) ([]byte, error) {
	if len(hash) != 32 {
		return nil, fmt.Errorf("sha256 condition "+
			"requires 32-byte hash, got %d",
			len(hash))
	}

	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_SHA256)
	builder.AddData(hash)
	builder.AddOp(txscript.OP_EQUALVERIFY)

	return builder.Script()
}
