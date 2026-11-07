package round

import (
	"fmt"

	"github.com/lightningnetwork/lnd/tlv"
)

// durableRefresh preserves maintenance provenance and the selected wallet keys.
type durableRefresh struct {
	Outpoint []byte
	Amount   uint64
	Fee      uint64
	Flags    uint8
	Expiry   uint32
	Height   uint32
	Operator []byte
	Policy   []byte
	Owner    []byte
	Signer   []byte
}

// records defines the local refresh payload independently of the join wire.
func (v *durableRefresh) records() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(1, &v.Outpoint),
		tlv.MakePrimitiveRecord(3, &v.Amount),
		tlv.MakePrimitiveRecord(5, &v.Fee),
		tlv.MakePrimitiveRecord(7, &v.Flags),
		tlv.MakePrimitiveRecord(9, &v.Expiry),
		tlv.MakePrimitiveRecord(11, &v.Height),
		tlv.MakePrimitiveRecord(13, &v.Operator),
		tlv.MakePrimitiveRecord(15, &v.Policy),
		tlv.MakePrimitiveRecord(17, &v.Owner),
		tlv.MakePrimitiveRecord(19, &v.Signer),
	}
}

// encodeDurableRefresh freezes a refresh without deriving replacement keys.
func encodeDurableRefresh(req *RefreshVTXORequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("missing refresh request")
	}
	v := durableRefresh{
		Outpoint: durableOutpoint(req.VTXOOutpoint), Amount: uint64(
			req.Amount,
		),
		Fee: uint64(req.OperatorFee), Expiry: uint32(req.BatchExpiry),
		Height: uint32(req.TriggerHeight), Operator: durablePubKey(
			req.OperatorKey,
		),
		Policy: req.PolicyTemplate, Owner: durableKeyDescriptor(
			req.OwnerKey,
		),
		Signer: durableKeyDescriptor(req.SigningKey),
	}
	if req.Automatic {
		v.Flags |= 1
	}
	if req.ExpandCohort {
		v.Flags |= 2
	}

	return encodeDurableFields(v.records()...)
}

// decodeDurableRefresh retains signed input values for normal validation.
func decodeDurableRefresh(raw []byte) (*RefreshVTXORequest, error) {
	var v durableRefresh
	if err := decodeDurableFields(raw, v.records()...); err != nil {
		return nil, err
	}
	if v.Flags & ^uint8(3) != 0 {
		return nil, fmt.Errorf("invalid refresh flags")
	}
	point, err := parseDurableOutpoint(v.Outpoint)
	if err != nil {
		return nil, err
	}
	operator, err := parseDurablePubKey(v.Operator)
	if err != nil {
		return nil, err
	}
	owner, err := parseDurableKeyDescriptor(v.Owner)
	if err != nil {
		return nil, err
	}
	signer, err := parseDurableKeyDescriptor(v.Signer)
	if err != nil {
		return nil, err
	}

	return &RefreshVTXORequest{
		VTXOOutpoint: point,
		Amount:       int64(v.Amount), OperatorFee: int64(
			v.Fee,
		),
		BatchExpiry: int32(v.Expiry), TriggerHeight: int32(v.Height),
		Automatic: v.Flags&1 != 0, ExpandCohort: v.Flags&2 != 0,
		OperatorKey: operator, PolicyTemplate: v.Policy,
		OwnerKey: owner, SigningKey: signer,
	}, nil
}
