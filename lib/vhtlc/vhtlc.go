// Package vhtlc implements the virtual HTLC (vHTLC) tapscript policy used
// for off-chain HTLC payments in the Ark protocol.
//
// A vHTLC is a 6-leaf taproot output that enables atomic payment exchange
// using the standard Ark NUMS internal key (no key-path spend):
//
//   - Claim (collab):                      HashLock + Multisig([receiver, server])
//   - Refund (collab):                     Multisig([sender, receiver, server])
//   - RefundWithoutReceiver (collab):      CLTV + Multisig([sender, server])
//   - UnilateralClaim (exit):              CSV + HashLock + Checksig(receiver)
//   - UnilateralRefund (exit):             CSV + Multisig([sender, receiver])
//   - UnilateralRefundWithoutReceiver (exit): CSV + Checksig(sender)
//
// Leaves are sorted in canonical order (collab before exit, then
// lexicographic) using arkscript.SortLeaves before the tree is built.
// Clients should treat the sorted leaf indices as opaque and use the
// named accessors (ClaimSpendInfo, RefundSpendInfo, etc.) to retrieve
// spend information for a specific path.
package vhtlc

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	arkscript "github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/scripts"
)

// Opts contains the parameters for constructing a vHTLC policy.
type Opts struct {
	// Sender is the party initiating the HTLC (payer).
	Sender *btcec.PublicKey

	// Receiver is the party receiving the HTLC payment (payee).
	Receiver *btcec.PublicKey

	// Server is the Ark operator/cosigner key.
	Server *btcec.PublicKey

	// PreimageHash is the HASH160 (RIPEMD160(SHA256(preimage))) of the
	// payment preimage. Must be exactly 20 bytes.
	PreimageHash []byte

	// RefundLocktime is the absolute locktime (CLTV) after which the
	// RefundWithoutReceiver path becomes valid.
	RefundLocktime uint32

	// UnilateralClaimDelay is the CSV delay (in blocks) required before
	// the receiver can unilaterally claim the HTLC.
	UnilateralClaimDelay uint32

	// UnilateralRefundDelay is the CSV delay (in blocks) required before
	// sender and receiver can unilaterally refund.
	UnilateralRefundDelay uint32

	// UnilateralRefundWithoutReceiverDelay is the CSV delay (in blocks)
	// required before the sender can unilaterally refund alone.
	UnilateralRefundWithoutReceiverDelay uint32
}

// Policy represents a compiled vHTLC taproot policy. All six spending
// leaves are compiled and arranged in canonical order. Use the named
// accessor methods to retrieve spend information for each path.
type Policy struct {
	// CompiledPolicy is the underlying canonical taproot tree.
	*arkscript.CompiledPolicy

	// claimIdx is the sorted leaf index for the collaborative claim path.
	claimIdx int

	// refundIdx is the sorted leaf index for the collaborative refund
	// path.
	refundIdx int

	// refundWithoutReceiverIdx is the sorted leaf index for the
	// collaborative refund-without-receiver path.
	refundWithoutReceiverIdx int

	// unilateralClaimIdx is the sorted leaf index for the unilateral
	// claim path.
	unilateralClaimIdx int

	// unilateralRefundIdx is the sorted leaf index for the unilateral
	// refund path.
	unilateralRefundIdx int

	// unilateralRefundWithoutReceiverIdx is the sorted leaf index for
	// the unilateral refund-without-receiver path.
	unilateralRefundWithoutReceiverIdx int
}

// ClaimSpendInfo returns the spend information for the collaborative claim
// path: HashLock(preimage) + Multisig([receiver, server]).
func (p *Policy) ClaimSpendInfo() (*arkscript.SpendInfo, error) {
	return p.SpendInfo(p.claimIdx)
}

// RefundSpendInfo returns the spend information for the collaborative refund
// path: Multisig([sender, receiver, server]).
func (p *Policy) RefundSpendInfo() (*arkscript.SpendInfo, error) {
	return p.SpendInfo(p.refundIdx)
}

// RefundWithoutReceiverSpendInfo returns the spend information for the
// collaborative refund-without-receiver path:
// CLTV(locktime) + Multisig([sender, server]).
func (p *Policy) RefundWithoutReceiverSpendInfo() (*arkscript.SpendInfo,
	error) {

	return p.SpendInfo(p.refundWithoutReceiverIdx)
}

// UnilateralClaimSpendInfo returns the spend information for the unilateral
// claim exit path: CSV(delay) + HashLock(preimage) + Checksig(receiver).
func (p *Policy) UnilateralClaimSpendInfo() (*arkscript.SpendInfo, error) {
	return p.SpendInfo(p.unilateralClaimIdx)
}

// UnilateralRefundSpendInfo returns the spend information for the unilateral
// refund exit path: CSV(delay) + Multisig([sender, receiver]).
func (p *Policy) UnilateralRefundSpendInfo() (*arkscript.SpendInfo, error) {
	return p.SpendInfo(p.unilateralRefundIdx)
}

// UnilateralRefundWithoutReceiverSpendInfo returns the spend information for
// the unilateral refund-without-receiver exit path: CSV(delay) + Checksig(sender).
func (p *Policy) UnilateralRefundWithoutReceiverSpendInfo() (
	*arkscript.SpendInfo, error) {

	return p.SpendInfo(p.unilateralRefundWithoutReceiverIdx)
}

// PkScript returns the P2TR pkScript for this vHTLC output, suitable for
// use in a transaction output.
func (p *Policy) PkScript() ([]byte, error) {
	return txscript.PayToTaprootScript(p.OutputKey())
}

// NewPolicy creates and validates a vHTLC policy from the given parameters.
// All six leaves are compiled, sorted canonically, and assembled into a
// balanced binary taproot tree using the ARK NUMS internal key.
func NewPolicy(opts Opts) (*Policy, error) {
	switch {
	case opts.Sender == nil:
		return nil, fmt.Errorf("vhtlc: sender key is nil")

	case opts.Receiver == nil:
		return nil, fmt.Errorf("vhtlc: receiver key is nil")

	case opts.Server == nil:
		return nil, fmt.Errorf("vhtlc: server key is nil")

	case len(opts.PreimageHash) != 20:
		return nil, fmt.Errorf("vhtlc: preimage hash must be 20 bytes, "+
			"got %d", len(opts.PreimageHash))

	case opts.ExitDelaysZero():
		return nil, fmt.Errorf("vhtlc: at least one CSV delay must " +
			"be non-zero")
	}

	// Build the 6 AST closures and compile each to a script. We collect
	// all scripts before sorting so we can recover each leaf's sorted
	// index by matching script bytes.

	// 1. Claim: HashLock(preimageHash, Multisig([receiver, server]))
	// Collaborative path — receiver presents the preimage and both
	// receiver and server must co-sign.
	claimNode := &arkscript.HashLock{
		Algorithm: arkscript.HashAlgoHash160,
		Hash:      opts.PreimageHash,
		Inner: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{opts.Receiver, opts.Server},
			Type: arkscript.MultisigTypeChecksig,
		},
	}
	claimScript, err := claimNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vhtlc: compile claim leaf: %w", err)
	}

	// 2. Refund: Multisig([sender, receiver, server])
	// Collaborative refund — all three parties agree.
	refundNode := &arkscript.Multisig{
		Keys: []*btcec.PublicKey{opts.Sender, opts.Receiver, opts.Server},
		Type: arkscript.MultisigTypeChecksig,
	}
	refundScript, err := refundNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vhtlc: compile refund leaf: %w", err)
	}

	// 3. RefundWithoutReceiver: CLTV(locktime, Multisig([sender, server]))
	// Collaborative refund path available after an absolute timeout,
	// without requiring the receiver's cooperation.
	refundNoReceiverNode := &arkscript.CLTV{
		Lock: opts.RefundLocktime,
		Inner: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{opts.Sender, opts.Server},
			Type: arkscript.MultisigTypeChecksig,
		},
	}
	refundNoReceiverScript, err := refundNoReceiverNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vhtlc: compile refund-without-receiver "+
			"leaf: %w", err)
	}

	// 4. UnilateralClaim: CSV(delay, HashLock(preimageHash, Checksig(receiver)))
	// Exit path — receiver can claim unilaterally after the CSV delay
	// by presenting the preimage.
	unilateralClaimNode := &arkscript.CSV{
		Lock: opts.UnilateralClaimDelay,
		Inner: &arkscript.HashLock{
			Algorithm: arkscript.HashAlgoHash160,
			Hash:      opts.PreimageHash,
			Inner:     &arkscript.Checksig{Key: opts.Receiver},
		},
	}
	unilateralClaimScript, err := unilateralClaimNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vhtlc: compile unilateral-claim leaf: "+
			"%w", err)
	}

	// 5. UnilateralRefund: CSV(delay, Multisig([sender, receiver]))
	// Exit path — sender and receiver can jointly refund after the delay.
	unilateralRefundNode := &arkscript.CSV{
		Lock: opts.UnilateralRefundDelay,
		Inner: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{opts.Sender, opts.Receiver},
			Type: arkscript.MultisigTypeChecksig,
		},
	}
	unilateralRefundScript, err := unilateralRefundNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vhtlc: compile unilateral-refund leaf: "+
			"%w", err)
	}

	// 6. UnilateralRefundWithoutReceiver: CSV(delay, Checksig(sender))
	// Exit path — sender can refund alone after the longest delay.
	unilateralRefundNoReceiverNode := &arkscript.CSV{
		Lock:  opts.UnilateralRefundWithoutReceiverDelay,
		Inner: &arkscript.Checksig{Key: opts.Sender},
	}
	unilateralRefundNoReceiverScript, err :=
		unilateralRefundNoReceiverNode.Script()
	if err != nil {
		return nil, fmt.Errorf("vhtlc: compile unilateral-refund-"+
			"without-receiver leaf: %w", err)
	}

	// Assign roles: collaborative paths are LeafRoleCollab, exit paths
	// are LeafRoleExit. This controls canonical leaf ordering.
	leaves := []arkscript.PolicyLeaf{
		{
			Role: arkscript.LeafRoleCollab,
			Leaf: txscript.NewBaseTapLeaf(claimScript),
		},
		{
			Role: arkscript.LeafRoleCollab,
			Leaf: txscript.NewBaseTapLeaf(refundScript),
		},
		{
			Role: arkscript.LeafRoleCollab,
			Leaf: txscript.NewBaseTapLeaf(refundNoReceiverScript),
		},
		{
			Role: arkscript.LeafRoleExit,
			Leaf: txscript.NewBaseTapLeaf(unilateralClaimScript),
		},
		{
			Role: arkscript.LeafRoleExit,
			Leaf: txscript.NewBaseTapLeaf(unilateralRefundScript),
		},
		{
			Role: arkscript.LeafRoleExit,
			Leaf: txscript.NewBaseTapLeaf(unilateralRefundNoReceiverScript),
		},
	}

	// Sort leaves into canonical order before building the tree. After
	// sorting, the original script bytes are still intact so we can
	// recover each leaf's new index by script-byte comparison.
	arkscript.SortLeaves(leaves)

	// Build the taproot tree using the ARK NUMS key as the unspendable
	// internal key.
	policy, err := arkscript.BuildTree(leaves, &scripts.ARKNUMSKey)
	if err != nil {
		return nil, fmt.Errorf("vhtlc: build tree: %w", err)
	}

	// Recover the sorted index for each named path by matching compiled
	// script bytes against the canonical leaf list.
	claimIdx, err := leafIndexForScript(policy.Leaves, claimScript)
	if err != nil {
		return nil, fmt.Errorf("vhtlc: find claim leaf: %w", err)
	}

	refundIdx, err := leafIndexForScript(policy.Leaves, refundScript)
	if err != nil {
		return nil, fmt.Errorf("vhtlc: find refund leaf: %w", err)
	}

	refundNoReceiverIdx, err := leafIndexForScript(
		policy.Leaves, refundNoReceiverScript,
	)
	if err != nil {
		return nil, fmt.Errorf("vhtlc: find refund-without-receiver "+
			"leaf: %w", err)
	}

	unilateralClaimIdx, err := leafIndexForScript(
		policy.Leaves, unilateralClaimScript,
	)
	if err != nil {
		return nil, fmt.Errorf("vhtlc: find unilateral-claim leaf: %w",
			err)
	}

	unilateralRefundIdx, err := leafIndexForScript(
		policy.Leaves, unilateralRefundScript,
	)
	if err != nil {
		return nil, fmt.Errorf("vhtlc: find unilateral-refund leaf: %w",
			err)
	}

	unilateralRefundNoReceiverIdx, err := leafIndexForScript(
		policy.Leaves, unilateralRefundNoReceiverScript,
	)
	if err != nil {
		return nil, fmt.Errorf("vhtlc: find unilateral-refund-without-"+
			"receiver leaf: %w", err)
	}

	return &Policy{
		CompiledPolicy:                     policy,
		claimIdx:                           claimIdx,
		refundIdx:                          refundIdx,
		refundWithoutReceiverIdx:           refundNoReceiverIdx,
		unilateralClaimIdx:                 unilateralClaimIdx,
		unilateralRefundIdx:                unilateralRefundIdx,
		unilateralRefundWithoutReceiverIdx: unilateralRefundNoReceiverIdx,
	}, nil
}

// Hash160 computes RIPEMD160(SHA256(data)), the standard Bitcoin HASH160
// used as the vHTLC preimage hash. Callers should store the original preimage
// securely and provide its Hash160 in Opts.PreimageHash.
func Hash160(data []byte) []byte {
	return btcutil.Hash160(data)
}

// ExitDelaysZero returns true if all three CSV delays are zero, which is
// invalid for a vHTLC policy.
func (o *Opts) ExitDelaysZero() bool {
	return o.UnilateralClaimDelay == 0 &&
		o.UnilateralRefundDelay == 0 &&
		o.UnilateralRefundWithoutReceiverDelay == 0
}

// leafIndexForScript searches the sorted policy leaves for the leaf whose
// script bytes exactly match script, returning its index. An error is returned
// if no matching leaf is found.
func leafIndexForScript(leaves []arkscript.PolicyLeaf,
	script []byte) (int, error) {

	for i := range leaves {
		if bytes.Equal(leaves[i].Leaf.Script, script) {
			return i, nil
		}
	}

	return 0, fmt.Errorf("leaf not found in policy tree")
}
