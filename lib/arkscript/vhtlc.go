package arkscript

import (
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"golang.org/x/crypto/ripemd160"
)

// VHTLCOpts contains the parameters for constructing a vHTLC policy.
type VHTLCOpts struct {
	// Sender is the party initiating the HTLC (payer).
	Sender *btcec.PublicKey

	// Receiver is the party receiving the HTLC payment (payee).
	Receiver *btcec.PublicKey

	// Server is the Ark operator key.
	Server *btcec.PublicKey

	// PreimageHash is the HASH160 of the preimage (20 bytes).
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

	// 1. Claim: HashLock(HASH160, preimageHash,
	//    Multisig([receiver, server]))
	claimClosure := &HashLock{
		Algorithm: HashAlgoHash160,
		Hash:      opts.PreimageHash,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				opts.Receiver, opts.Server,
			},
			Type: MultisigTypeChecksig,
		},
	}

	// 2. Refund: Multisig([sender, receiver, server])
	refundClosure := &Multisig{
		Keys: []*btcec.PublicKey{
			opts.Sender, opts.Receiver, opts.Server,
		},
		Type: MultisigTypeChecksig,
	}

	// 3. RefundWithoutReceiver: CLTV(locktime,
	//    Multisig([sender, server]))
	refundWithoutReceiverClosure := &CLTV{
		Lock: opts.RefundLocktime,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				opts.Sender, opts.Server,
			},
			Type: MultisigTypeChecksig,
		},
	}

	// 4. UnilateralClaim: CSV(delay,
	//    HashLock(preimageHash, Checksig(receiver)))
	unilateralClaimClosure := &CSV{
		Lock: opts.UnilateralClaimDelay,
		Inner: &HashLock{
			Algorithm: HashAlgoHash160,
			Hash:      opts.PreimageHash,
			Inner:     &Checksig{Key: opts.Receiver},
		},
	}

	// 5. UnilateralRefund: CSV(delay,
	//    Multisig([sender, receiver]))
	unilateralRefundClosure := &CSV{
		Lock: opts.UnilateralRefundDelay,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				opts.Sender, opts.Receiver,
			},
			Type: MultisigTypeChecksig,
		},
	}

	// 6. UnilateralRefundWithoutReceiver: CSV(delay,
	//    Checksig(sender))
	unilateralRefundWithoutReceiverClosure := &CSV{
		Lock: opts.UnilateralRefundWithoutReceiverDelay,
		Inner: &Checksig{
			Key: opts.Sender,
		},
	}

	// Compile all closures and build leaf set.
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
	for i, c := range closures {
		script, err := c.node.Script()
		if err != nil {
			return nil, fmt.Errorf("compile closure %d: %w",
				i, err)
		}

		leaves[i] = PolicyLeaf{
			Role: c.role,
			Leaf: txscript.NewBaseTapLeaf(script),
		}
	}

	// Sort leaves canonically and track where each closure ended up.
	SortLeaves(leaves)

	// Build index mapping by matching scripts.
	claimScript, _ := claimClosure.Script()
	refundScript, _ := refundClosure.Script()
	refundWithoutReceiverScript, _ := refundWithoutReceiverClosure.Script()
	unilateralClaimScript, _ := unilateralClaimClosure.Script()
	unilateralRefundScript, _ := unilateralRefundClosure.Script()
	unilateralRefundWithoutReceiverScript, _ := unilateralRefundWithoutReceiverClosure.Script()

	scriptToIndex := make(map[string]int, len(leaves))
	for i, leaf := range leaves {
		scriptToIndex[string(leaf.Leaf.Script)] = i
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	if err != nil {
		return nil, fmt.Errorf("build vhtlc tree: %w", err)
	}

	return &VHTLCPolicy{
		CompiledPolicy:                 policy,
		ClaimClosure:                   claimClosure,
		RefundClosure:                  refundClosure,
		RefundWithoutReceiverClosure:   refundWithoutReceiverClosure,
		UnilateralClaimClosure:         unilateralClaimClosure,
		UnilateralRefundClosure:        unilateralRefundClosure,
		UnilateralRefundWithoutReceiverClosure: unilateralRefundWithoutReceiverClosure,

		claimLeafIndex:                          scriptToIndex[string(claimScript)],
		refundLeafIndex:                         scriptToIndex[string(refundScript)],
		refundWithoutReceiverLeafIndex:          scriptToIndex[string(refundWithoutReceiverScript)],
		unilateralClaimLeafIndex:                scriptToIndex[string(unilateralClaimScript)],
		unilateralRefundLeafIndex:               scriptToIndex[string(unilateralRefundScript)],
		unilateralRefundWithoutReceiverLeafIndex: scriptToIndex[string(unilateralRefundWithoutReceiverScript)],
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

	case len(opts.PreimageHash) != 20:
		return fmt.Errorf("vhtlc: preimage hash must be 20 bytes "+
			"(HASH160), got %d", len(opts.PreimageHash))

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
	ripemd := ripemd160.New()
	ripemd.Write(sha[:])

	return ripemd.Sum(nil)
}
