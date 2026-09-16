package round

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableCreated retains wallet outputs and independently keyed ledger entries.
type durableCreated struct {
	VTXOs      []byte
	Outflows   []byte
	RoundID    []byte
	Commitment [32]byte
	Expiry     uint32
	Height     uint32
	Fee        uint64
	FeeType    []byte
}

// records defines the complete output-notification checkpoint.
func (v *durableCreated) records() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(1, &v.VTXOs),
		tlv.MakePrimitiveRecord(3, &v.Outflows),
		tlv.MakePrimitiveRecord(5, &v.RoundID),
		tlv.MakePrimitiveRecord(7, &v.Commitment),
		tlv.MakePrimitiveRecord(9, &v.Expiry),
		tlv.MakePrimitiveRecord(11, &v.Height),
		tlv.MakePrimitiveRecord(13, &v.Fee),
		tlv.MakePrimitiveRecord(15, &v.FeeType),
	}
}

// encodeDurableCreated preserves ledger provenance alongside the wallet data.
func encodeDurableCreated(msg *VTXOCreatedNotification) ([]byte, error) {
	v := durableCreated{
		RoundID: []byte(msg.RoundID), Commitment: msg.CommitmentTxID,
		Expiry: uint32(msg.BatchExpiry), Height: uint32(
			msg.CreatedHeight,
		),
		Fee: uint64(msg.OperatorFeeSat), FeeType: []byte(
			msg.OperatorFeeType,
		),
	}
	var err error
	v.VTXOs, err = encodeDurableList(msg.VTXOs, encodeDurableClientVTXO)
	if err != nil {
		return nil, err
	}
	v.Outflows, err = encodeDurableList(msg.Outflows, encodeDurableOutflow)
	if err != nil {
		return nil, err
	}

	return encodeDurableFields(v.records()...)
}

// decodeDurableCreated restores all idempotency keys before ledger delivery.
func decodeDurableCreated(raw []byte) (ClientOutMsg, error) {
	var v durableCreated
	if err := decodeDurableFields(raw, v.records()...); err != nil {
		return nil, err
	}
	outputs, err := decodeDurableList(v.VTXOs, decodeDurableClientVTXO)
	if err != nil {
		return nil, err
	}
	outflows, err := decodeDurableList(v.Outflows, decodeDurableOutflow)
	if err != nil {
		return nil, err
	}

	return &VTXOCreatedNotification{
		VTXOs: outputs, Outflows: outflows, RoundID: string(v.RoundID),
		CommitmentTxID: chainhash.Hash(v.Commitment),
		BatchExpiry: int32(
			v.Expiry,
		),
		CreatedHeight: int32(v.Height), OperatorFeeSat: int64(v.Fee),
		OperatorFeeType: string(v.FeeType),
	}, nil
}

// encodeDurableOutflow preserves the recipient entry's independent dedupe key.
func encodeDurableOutflow(value RoundLedgerOutflow) ([]byte, error) {
	amount := uint64(value.AmountSat)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &amount),
		tlv.MakePrimitiveRecord(3, &value.IdempotencyKey),
	)
}

// decodeDurableOutflow restores the original signed amount without
// normalization.
func decodeDurableOutflow(raw []byte) (RoundLedgerOutflow, error) {
	var amount uint64
	var key []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &amount),
		tlv.MakePrimitiveRecord(3, &key),
	)

	return RoundLedgerOutflow{
		AmountSat:      int64(amount),
		IdempotencyKey: key,
	}, err
}
