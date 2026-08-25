package arkscript

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
)

// VHTLCTiming contains the four block-based constraints in a vHTLC policy.
type VHTLCTiming struct {
	// RefundLocktime is the absolute height at which the sender and
	// operator can refund without the receiver.
	RefundLocktime uint32

	// UnilateralClaimDelay is the receiver-only claim CSV delay.
	UnilateralClaimDelay uint32

	// UnilateralRefundDelay is the sender-and-receiver refund CSV delay.
	UnilateralRefundDelay uint32

	// UnilateralRefundWithoutReceiverDelay is the sender-only refund CSV
	// delay.
	UnilateralRefundWithoutReceiverDelay uint32
}

// VHTLCClaimWindow describes the chain budget a receiver needs before the
// sender-and-operator refund branch becomes available.
type VHTLCClaimWindow struct {
	// CurrentHeight is the best chain height at the admission boundary.
	CurrentHeight uint32

	// ExitAncestryDelay is the conservative number of blocks needed to
	// confirm the transactions preceding the vHTLC output.
	ExitAncestryDelay uint32

	// RecoveryMargin is the additional block margin retained after the
	// unilateral claim path first becomes spendable.
	RecoveryMargin uint32
}

// Timing returns the block constraints carried by opts.
func (opts *VHTLCOpts) Timing() VHTLCTiming {
	return VHTLCTiming{
		RefundLocktime: opts.RefundLocktime,
		UnilateralClaimDelay: opts.
			UnilateralClaimDelay,
		UnilateralRefundDelay: opts.
			UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: opts.
			UnilateralRefundWithoutReceiverDelay,
	}
}

// ValidateOrder rejects vHTLC branches whose cooperation requirements become
// weaker before the more cooperative branch is available.
func (t VHTLCTiming) ValidateOrder() error {
	if t.UnilateralClaimDelay > t.UnilateralRefundDelay {
		return fmt.Errorf("vhtlc: unilateral claim delay %d exceeds "+
			"unilateral refund delay %d", t.UnilateralClaimDelay,
			t.UnilateralRefundDelay)
	}

	if t.UnilateralRefundDelay >
		t.UnilateralRefundWithoutReceiverDelay {
		return fmt.Errorf("vhtlc: unilateral refund delay %d exceeds "+
			"unilateral refund without receiver delay %d",
			t.UnilateralRefundDelay,
			t.UnilateralRefundWithoutReceiverDelay)
	}

	return nil
}

// ValidateClaimWindow requires enough remaining absolute-locktime budget to
// publish the exit ancestry, wait out the receiver's claim CSV, and retain the
// caller-selected recovery margin.
func (t VHTLCTiming) ValidateClaimWindow(window VHTLCClaimWindow) error {
	if err := t.ValidateOrder(); err != nil {
		return err
	}

	if t.RefundLocktime <= window.CurrentHeight {
		return fmt.Errorf("vhtlc: refund locktime %d is not after "+
			"current height %d", t.RefundLocktime,
			window.CurrentHeight)
	}

	required := uint64(window.ExitAncestryDelay) +
		uint64(t.UnilateralClaimDelay) +
		uint64(window.RecoveryMargin)
	remaining := uint64(t.RefundLocktime - window.CurrentHeight)
	if remaining <= required {
		return fmt.Errorf("vhtlc: refund window %d blocks does not "+
			"exceed required claim window %d blocks (exit "+
			"ancestry %d, claim delay %d, recovery margin %d)",
			remaining, required, window.ExitAncestryDelay,
			t.UnilateralClaimDelay, window.RecoveryMargin)
	}

	return nil
}

// DecodeVHTLCTiming recognizes the canonical six-leaf vHTLC semantic shape
// and returns its four timing constraints. Other custom policies return an
// error so callers cannot accidentally apply vHTLC assumptions to them. The
// decoded tuple reflects the committed script exactly; admission callers must
// apply ValidateOrder or ValidateClaimWindow, while recovery code can still
// reconstruct an already-funded legacy policy.
func DecodeVHTLCTiming(template *PolicyTemplate) (*VHTLCTiming, error) {
	if template == nil {
		return nil, fmt.Errorf("vhtlc: policy template is required")
	}
	if len(template.Leaves) != 6 {
		return nil, fmt.Errorf("vhtlc: expected 6 leaves, got %d",
			len(template.Leaves))
	}

	var shapes vhtlcShapes
	for i := range template.Leaves {
		shape, err := decodeVHTLCNode(template.Leaves[i].Node)
		if err != nil {
			return nil, fmt.Errorf("vhtlc: leaf %d: %w", i, err)
		}
		if err := shapes.add(shape); err != nil {
			return nil, fmt.Errorf("vhtlc: leaf %d: %w", i, err)
		}
	}

	if err := shapes.validate(); err != nil {
		return nil, err
	}

	return &VHTLCTiming{
		RefundLocktime: shapes.refundWithoutReceiver.locktime,
		UnilateralClaimDelay: shapes.unilateralClaim.
			csvDelay,
		UnilateralRefundDelay: shapes.unilateralRefund.
			csvDelay,
		UnilateralRefundWithoutReceiverDelay: shapes.
			unilateralRefundWithoutReceiver.csvDelay,
	}, nil
}

type vhtlcNodeShape struct {
	csvDelay  uint32
	locktime  uint32
	predicate []byte
	keys      [][]byte
}

type vhtlcShapes struct {
	claim                           *vhtlcNodeShape
	refund                          *vhtlcNodeShape
	refundWithoutReceiver           *vhtlcNodeShape
	unilateralClaim                 *vhtlcNodeShape
	unilateralRefund                *vhtlcNodeShape
	unilateralRefundWithoutReceiver *vhtlcNodeShape
}

// decodeVHTLCNode accepts only the wrapper shapes emitted by NewVHTLCPolicy.
func decodeVHTLCNode(node Node) (*vhtlcNodeShape, error) {
	shape := &vhtlcNodeShape{}

	if csv, ok := node.(*CSV); ok {
		if err := validateCSVLock(csv.Lock); err != nil {
			return nil, fmt.Errorf("invalid CSV delay: %w", err)
		}

		// CSV.Lock stores a BIP-68 sequence. Decode the relative block
		// count explicitly after rejecting disable, time-mode and
		// reserved flag bits above.
		shape.csvDelay = csv.Lock & uint32(wire.SequenceLockTimeMask)
		node = csv.Inner
	}
	if condition, ok := node.(*Condition); ok {
		shape.predicate = bytes.Clone(condition.Predicate)
		shape.locktime = ExtractAbsoluteLockTime(condition)
		node = condition.Inner
	}

	multisig, ok := node.(*Multisig)
	if !ok {
		return nil, fmt.Errorf("unsupported closure shape %T", node)
	}
	shape.keys = make([][]byte, len(multisig.Keys))
	for i, key := range multisig.Keys {
		if key == nil {
			return nil, fmt.Errorf("multisig key %d is nil", i)
		}

		shape.keys[i] = key.SerializeCompressed()
	}

	return shape, nil
}

// add assigns one structurally unique vHTLC branch.
func (s *vhtlcShapes) add(shape *vhtlcNodeShape) error {
	var target **vhtlcNodeShape
	switch {
	case shape.csvDelay == 0 && shape.locktime == 0 &&
		len(shape.predicate) > 0 && len(shape.keys) == 2:

		target = &s.claim

	case shape.csvDelay == 0 && shape.locktime == 0 &&
		len(shape.predicate) == 0 && len(shape.keys) == 3:

		target = &s.refund

	case shape.csvDelay == 0 && shape.locktime > 0 &&
		len(shape.keys) == 2:

		target = &s.refundWithoutReceiver

	case shape.csvDelay > 0 && shape.locktime == 0 &&
		len(shape.predicate) > 0 && len(shape.keys) == 1:

		target = &s.unilateralClaim

	case shape.csvDelay > 0 && shape.locktime == 0 &&
		len(shape.predicate) == 0 && len(shape.keys) == 2:

		target = &s.unilateralRefund

	case shape.csvDelay > 0 && shape.locktime > 0 &&
		len(shape.keys) == 1:

		target = &s.unilateralRefundWithoutReceiver

	default:
		return fmt.Errorf("unrecognized branch shape")
	}

	if *target != nil {
		return fmt.Errorf("duplicate branch shape")
	}
	*target = shape

	return nil
}

// validate binds the six structural branches to one sender, receiver,
// operator, payment hash, and refund locktime.
func (s *vhtlcShapes) validate() error {
	branches := []*vhtlcNodeShape{
		s.claim, s.refund, s.refundWithoutReceiver,
		s.unilateralClaim, s.unilateralRefund,
		s.unilateralRefundWithoutReceiver,
	}
	for _, branch := range branches {
		if branch == nil {
			return fmt.Errorf("vhtlc: missing branch")
		}
	}

	sender := s.unilateralRefundWithoutReceiver.keys[0]
	receiver := s.unilateralClaim.keys[0]
	server := s.claim.keys[1]

	checks := []struct {
		name string
		got  [][]byte
		want [][]byte
	}{
		{
			"claim",
			s.claim.keys,
			[][]byte{
				receiver,
				server,
			},
		},
		{
			"refund",
			s.refund.keys,
			[][]byte{
				sender,
				receiver,
				server,
			},
		},
		{"refund without receiver", s.refundWithoutReceiver.keys,
			[][]byte{
				sender,
				server,
			}},
		{"unilateral refund", s.unilateralRefund.keys,
			[][]byte{
				sender,
				receiver,
			}},
	}
	for _, check := range checks {
		if !equalVHTLCKeys(check.got, check.want) {
			return fmt.Errorf("vhtlc: %s keys do not match",
				check.name)
		}
	}

	if !bytes.Equal(s.claim.predicate,
		s.unilateralClaim.predicate) {
		return fmt.Errorf("vhtlc: claim predicates do not match")
	}
	if !bytes.Equal(
		s.refundWithoutReceiver.predicate,
		s.unilateralRefundWithoutReceiver.predicate,
	) ||
		s.refundWithoutReceiver.locktime !=
			s.unilateralRefundWithoutReceiver.locktime {
		return fmt.Errorf("vhtlc: refund locktimes do not match")
	}

	return nil
}

// equalVHTLCKeys compares ordered compressed public-key encodings.
func equalVHTLCKeys(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !bytes.Equal(left[i], right[i]) {
			return false
		}
	}

	return true
}
