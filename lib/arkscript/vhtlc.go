package arkscript

import (
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightningnetwork/lnd/lntypes"
)

// VHTLCOpts contains the parameters for constructing a vHTLC policy.
type VHTLCOpts struct {
	// Sender is the party initiating the HTLC (payer).
	Sender *btcec.PublicKey

	// Receiver is the party receiving the HTLC payment (payee).
	Receiver *btcec.PublicKey

	// Server is the Ark operator key.
	Server *btcec.PublicKey

	// PreimageHash is the SHA256 of the preimage.
	// This matches the Lightning Network payment hash format:
	// paymentHash = SHA256(preimage).
	PreimageHash lntypes.Hash

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

// VHTLCPolicy represents a compiled vHTLC taproot policy with 6 leaves:
// 3 collaborative (operator-cosigned) and 3 unilateral exit paths.
// Named accessors provide typed spend info with correct tx-context
// requirements derived from each closure's AST.
type VHTLCPolicy struct {
	// Template is the semantic policy template for this vHTLC.
	Template *PolicyTemplate

	// CompiledPolicy is the underlying compiled taproot tree.
	*CompiledPolicy

	// PreimageHash is the SHA256 hash the claim paths are locked to.
	PreimageHash lntypes.Hash

	// ClaimClosure through UnilateralRefundWithoutReceiverClosure
	// are the semantic AST nodes for each of the 6 leaves, used for
	// programmatic access and tx-context derivation. Canonical leaf
	// indices are derived on the fly via ScriptIndex.
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
//  3. RefundWithoutReceiver (collab): CLTV(locktime) +
//     Multisig([sender, server])
//  4. UnilateralClaim (exit): CSV(delay) + HashLock(preimage) +
//     Checksig(receiver)
//  5. UnilateralRefund (exit): CSV(delay) + Multisig([sender, receiver])
//  6. UnilateralRefundWithoutReceiver (exit): CSV(delay) + CLTV(locktime)
//     + Checksig(sender)
func NewVHTLCPolicy(opts VHTLCOpts) (*VHTLCPolicy, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	claimPredicate, err := sha256Condition(opts.PreimageHash[:])
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

	// The delay fields on VHTLCOpts are raw block counts; wrap them with
	// blockchain.LockTimeToSequence so the CSV leaf explicitly stores the
	// BIP-68 block-mode encoding rather than relying on the identity
	// collapse of "raw blocks == encoded sequence" that only holds for
	// small values in block mode.
	claimSeq := blockchain.LockTimeToSequence(
		false, opts.UnilateralClaimDelay,
	)
	refundSeq := blockchain.LockTimeToSequence(
		false, opts.UnilateralRefundDelay,
	)
	refundNoRecvSeq := blockchain.LockTimeToSequence(
		false, opts.UnilateralRefundWithoutReceiverDelay,
	)

	// 4. UnilateralClaim: CSV(delay) + SHA256(preimage) +
	// Multisig([receiver]).
	unilateralClaimClosure := &CSV{
		Lock: claimSeq,
		Inner: &Condition{
			Predicate: claimPredicate,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					opts.Receiver,
				},
			},
		},
	}

	// 5. UnilateralRefund: CSV(delay) + Multisig([sender, receiver]).
	unilateralRefundClosure := &CSV{
		Lock: refundSeq,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				opts.Sender, opts.Receiver,
			},
		},
	}

	// 6. UnilateralRefundWithoutReceiver: CSV(delay) +
	// CLTV(locktime) + Multisig([sender]).
	unilateralRefundWithoutReceiverClosure := &CSV{
		Lock: refundNoRecvSeq,
		Inner: &Condition{
			Predicate: refundPredicate,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					opts.Sender,
				},
			},
		},
	}

	// Build the template and compile. Compile() handles leaf
	// compilation, canonical sorting, and tree construction.
	closures := []Node{
		claimClosure,
		refundClosure,
		refundWithoutReceiverClosure,
		unilateralClaimClosure,
		unilateralRefundClosure,
		unilateralRefundWithoutReceiverClosure,
	}

	leafTemplates := make([]LeafTemplate, len(closures))
	for i, node := range closures {
		leafTemplates[i] = LeafTemplate{Node: node}
	}

	template := &PolicyTemplate{Leaves: leafTemplates}

	policy, err := template.Compile()
	if err != nil {
		return nil, fmt.Errorf("compile vhtlc: %w", err)
	}

	return &VHTLCPolicy{
		Template:       template,
		CompiledPolicy: policy,
		PreimageHash:   opts.PreimageHash,

		ClaimClosure:                 claimClosure,
		RefundClosure:                refundClosure,
		RefundWithoutReceiverClosure: refundWithoutReceiverClosure,
		UnilateralClaimClosure:       unilateralClaimClosure,
		UnilateralRefundClosure:      unilateralRefundClosure,

		UnilateralRefundWithoutReceiverClosure: unilateralRefundWithoutReceiverClosure, //nolint:ll
	}, nil
}

// ClaimSpendInfo returns the spend information for the Claim path.
func (p *VHTLCPolicy) ClaimSpendInfo() (*SpendInfo, error) {
	return p.CompiledPolicy.SpendInfoForNode(p.ClaimClosure)
}

// RefundSpendInfo returns the spend information for the Refund path.
func (p *VHTLCPolicy) RefundSpendInfo() (*SpendInfo, error) {
	return p.CompiledPolicy.SpendInfoForNode(p.RefundClosure)
}

// RefundWithoutReceiverSpendInfo returns the spend information for the
// RefundWithoutReceiver path.
func (p *VHTLCPolicy) RefundWithoutReceiverSpendInfo() (*SpendInfo, error) {
	return p.CompiledPolicy.SpendInfoForNode(
		p.RefundWithoutReceiverClosure,
	)
}

// UnilateralClaimSpendInfo returns the spend information for the
// UnilateralClaim path.
func (p *VHTLCPolicy) UnilateralClaimSpendInfo() (*SpendInfo, error) {
	return p.CompiledPolicy.SpendInfoForNode(
		p.UnilateralClaimClosure,
	)
}

// UnilateralRefundSpendInfo returns the spend information for the
// UnilateralRefund path.
func (p *VHTLCPolicy) UnilateralRefundSpendInfo() (*SpendInfo, error) {
	return p.CompiledPolicy.SpendInfoForNode(
		p.UnilateralRefundClosure,
	)
}

// UnilateralRefundWithoutReceiverSpendInfo returns the spend information
// for the UnilateralRefundWithoutReceiver path.
func (p *VHTLCPolicy) UnilateralRefundWithoutReceiverSpendInfo() (*SpendInfo,
	error) {

	return p.CompiledPolicy.SpendInfoForNode(
		p.UnilateralRefundWithoutReceiverClosure,
	)
}

// ClaimPath returns a SpendPath for claiming via the hashlock leaf.
// The preimage's SHA256 must match the policy's PreimageHash.
func (p *VHTLCPolicy) ClaimPath(preimage lntypes.Preimage) (*SpendPath, error) {
	if !preimage.Matches(p.PreimageHash) {
		return nil, fmt.Errorf("preimage does not match policy hash")
	}

	return p.CompiledPolicy.SpendPathForNode(
		p.ClaimClosure, [][]byte{
			preimage[:],
		},
	)
}

// UnilateralClaimPath returns a SpendPath for claiming via the receiver-only
// CSV hashlock leaf. The preimage's SHA256 must match the policy's
// PreimageHash.
func (p *VHTLCPolicy) UnilateralClaimPath(preimage lntypes.Preimage) (
	*SpendPath, error) {

	if !preimage.Matches(p.PreimageHash) {
		return nil, fmt.Errorf("preimage does not match policy hash")
	}

	return p.CompiledPolicy.SpendPathForNode(
		p.UnilateralClaimClosure, [][]byte{
			preimage[:],
		},
	)
}

// RefundPath returns a SpendPath for the cooperative refund.
func (p *VHTLCPolicy) RefundPath() (*SpendPath, error) {
	return p.CompiledPolicy.SpendPathForNode(
		p.RefundClosure, nil,
	)
}

// RefundWithoutReceiverPath returns a SpendPath for the CLTV-gated
// refund without receiver.
func (p *VHTLCPolicy) RefundWithoutReceiverPath() (*SpendPath, error) {
	return p.CompiledPolicy.SpendPathForNode(
		p.RefundWithoutReceiverClosure, nil,
	)
}

// UnilateralRefundWithoutReceiverPath returns a SpendPath for the sender-only
// CSV+CLTV refund leaf.
func (p *VHTLCPolicy) UnilateralRefundWithoutReceiverPath() (*SpendPath,
	error) {

	return p.CompiledPolicy.SpendPathForNode(
		p.UnilateralRefundWithoutReceiverClosure, nil,
	)
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

	case opts.PreimageHash == lntypes.Hash{}:
		return fmt.Errorf("vhtlc: preimage hash must not be zero")

	case opts.RefundLocktime == 0:
		return fmt.Errorf("vhtlc: refund locktime must be non-zero")

	case opts.UnilateralClaimDelay == 0:
		return fmt.Errorf("vhtlc: unilateral claim delay must be " +
			"non-zero")

	case opts.UnilateralRefundDelay == 0:
		return fmt.Errorf("vhtlc: unilateral refund delay must be " +
			"non-zero")

	case opts.UnilateralRefundWithoutReceiverDelay == 0:
		return fmt.Errorf("vhtlc: unilateral refund without receiver " +
			"delay must be non-zero")
	}

	return nil
}

// sha256Condition builds the canonical script prefix for
// SHA256(<witness_item>) == hash with a fixed 32-byte witness item.
func sha256Condition(hash []byte) ([]byte, error) {
	if len(hash) != 32 {
		return nil, fmt.Errorf("sha256 condition requires 32-byte "+
			"hash, got %d", len(hash))
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
