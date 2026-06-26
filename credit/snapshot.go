package credit

import (
	"bytes"

	"github.com/lightningnetwork/lnd/tlv"
)

// snapshotVersion is the current encoding version of the per-operation resume
// snapshot blob persisted in credit_operations.snapshot_data. Bump it when the
// snapshot layout changes incompatibly.
const snapshotVersion int32 = 1

// tlv record types for the opSnapshot blob. Local to the snapshot stream.
const (
	snapRecRedeemPkScript tlv.Type = 1
	snapRecMemo           tlv.Type = 2
	snapRecCreditOnly     tlv.Type = 3
	snapRecAwaitPolls     tlv.Type = 5
)

// opSnapshot is the irreducible per-operation resume material that does not fit
// the queryable control-plane columns. It is small by design: most state lives
// in first-class columns; this blob only round-trips, nothing queries it.
type opSnapshot struct {
	// RedeemPkScript is the wallet-owned receive pkScript a redemption
	// watches for the redeemed VTXO to land. Empty for pay/receive ops.
	RedeemPkScript []byte

	// Memo is the receive memo embedded in the server-owned invoice, kept
	// here so a receive operation pre-written by the registry retains the
	// memo across its first ReceiveCreating turn. Empty for pay/redeem ops.
	Memo string

	// CreditOnly marks a pay that settles entirely from credit with no
	// Lightning swap leg, so the wallet projector owns its terminal
	// transition. Persisted here (rather than in a queryable column)
	// because only the wallet projector reads it. False for receive/redeem
	// ops.
	CreditOnly bool

	// AwaitPolls counts how many reconciliation polls the current awaiting
	// state has taken, so a configured MaxAwaitingPolls cap can terminal-
	// fail an operation that the server never resolves. Reset to zero each
	// time the FSM advances into a new awaiting state.
	AwaitPolls uint32
}

// encode serializes the snapshot to a TLV blob.
func (s *opSnapshot) encode() ([]byte, error) {
	if s == nil {
		return nil, nil
	}

	records := make([]tlv.Record, 0, 5)
	if len(s.RedeemPkScript) > 0 {
		pkScript := s.RedeemPkScript
		records = append(
			records, tlv.MakePrimitiveRecord(
				snapRecRedeemPkScript, &pkScript,
			),
		)
	}
	if s.Memo != "" {
		memo := []byte(s.Memo)
		records = append(
			records, tlv.MakePrimitiveRecord(snapRecMemo, &memo),
		)
	}
	if s.CreditOnly {
		creditOnly := uint8(1)
		records = append(
			records, tlv.MakePrimitiveRecord(
				snapRecCreditOnly, &creditOnly,
			),
		)
	}
	if s.AwaitPolls > 0 {
		awaitPolls := s.AwaitPolls
		records = append(
			records, tlv.MakePrimitiveRecord(
				snapRecAwaitPolls, &awaitPolls,
			),
		)
	}
	if len(records) == 0 {
		return nil, nil
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodeOpSnapshot deserializes a snapshot blob. An empty blob decodes to a
// zero-value snapshot so legacy or partially-written rows are tolerated.
func decodeOpSnapshot(raw []byte) (*opSnapshot, error) {
	snap := &opSnapshot{}
	if len(raw) == 0 {
		return snap, nil
	}

	var (
		pkScript   []byte
		memo       []byte
		creditOnly uint8
		awaitPolls uint32
	)
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(snapRecRedeemPkScript, &pkScript),
		tlv.MakePrimitiveRecord(snapRecMemo, &memo),
		tlv.MakePrimitiveRecord(snapRecCreditOnly, &creditOnly),
		tlv.MakePrimitiveRecord(snapRecAwaitPolls, &awaitPolls),
	)
	if err != nil {
		return nil, err
	}

	if err := stream.Decode(bytes.NewReader(raw)); err != nil {
		return nil, err
	}

	snap.RedeemPkScript = pkScript
	snap.Memo = string(memo)
	snap.CreditOnly = creditOnly != 0
	snap.AwaitPolls = awaitPolls

	return snap, nil
}
