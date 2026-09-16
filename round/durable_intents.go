package round

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableIntents preserves the positional relationship between requests and
// accepted quote amounts. Network encodings omit wallet ownership metadata.
type durableIntents struct {
	Service      []byte
	Boarding     []byte
	VTXOs        []byte
	Leaves       []byte
	Forfeits     []byte
	QuotedLeaves []byte
}

// stream assigns stable fields to the complete local intent package.
func (d *durableIntents) stream() (*tlv.Stream, error) {
	return tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &d.Service),
		tlv.MakePrimitiveRecord(3, &d.Boarding),
		tlv.MakePrimitiveRecord(5, &d.VTXOs),
		tlv.MakePrimitiveRecord(7, &d.Leaves),
		tlv.MakePrimitiveRecord(9, &d.Forfeits),
		tlv.MakePrimitiveRecord(11, &d.QuotedLeaves),
	)
}

// encodeDurableIntents records an admitted intent and its fee authorization.
func encodeDurableIntents(intents Intents) ([]byte, error) {
	var d durableIntents
	var err error
	if intents.Service != nil {
		d.Service, err = intents.Service.Encode()
		if err != nil {
			return nil, err
		}
	}
	d.Boarding, err = encodeDurableList(
		intents.Boarding, encodeDurableBoarding,
	)
	if err != nil {
		return nil, err
	}
	d.VTXOs, err = encodeDurableList(intents.VTXOs, encodeDurableVTXO)
	if err != nil {
		return nil, err
	}
	d.Leaves, err = encodeDurableList(intents.Leaves, encodeDurableLeave)
	if err != nil {
		return nil, err
	}
	d.Forfeits, err = encodeDurableList(
		intents.Forfeits, encodeDurableForfeit,
	)
	if err != nil {
		return nil, err
	}
	d.QuotedLeaves, err = encodeDurableList(
		intents.QuotedLeaveAmounts, encodeDurableAmount,
	)
	if err != nil {
		return nil, err
	}
	stream, err := d.stream()
	if err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	if err := stream.Encode(&raw); err != nil {
		return nil, err
	}

	return raw.Bytes(), nil
}

// decodeDurableIntents restores requests without deriving or normalizing keys.
func decodeDurableIntents(raw []byte,
	params *chaincfg.Params) (Intents, error) {

	var intents Intents
	var d durableIntents
	stream, err := d.stream()
	if err != nil {
		return intents, err
	}
	if err := stream.Decode(bytes.NewReader(raw)); err != nil {
		return intents, err
	}
	if len(d.Service) != 0 {
		intents.Service, err = types.DecodeServiceRequest(d.Service)
		if err != nil {
			return intents, err
		}
	}
	intents.Boarding, err = decodeDurableList(
		d.Boarding, func(raw []byte) (BoardingIntent, error) {
			return decodeDurableBoarding(raw, params)
		},
	)
	if err != nil {
		return intents, err
	}
	intents.VTXOs, err = decodeDurableList(d.VTXOs, decodeDurableVTXO)
	if err != nil {
		return intents, err
	}
	intents.Leaves, err = decodeDurableList(d.Leaves, decodeDurableLeave)
	if err != nil {
		return intents, err
	}
	intents.Forfeits, err = decodeDurableList(
		d.Forfeits, decodeDurableForfeit,
	)
	if err != nil {
		return intents, err
	}
	intents.QuotedLeaveAmounts, err = decodeDurableList(
		d.QuotedLeaves, decodeDurableAmount,
	)

	return intents, err
}

// encodeDurableList delimits every entry, preserving order and nil elements.
func encodeDurableList[T any](values []T,
	encode func(T) ([]byte, error)) ([]byte, error) {

	var raw bytes.Buffer
	for _, value := range values {
		entry, err := encode(value)
		if err != nil {
			return nil, err
		}
		if err := wire.WriteVarBytes(&raw, 0, entry); err != nil {
			return nil, err
		}
	}

	return raw.Bytes(), nil
}

// decodeDurableList bounds allocations by the remaining checkpoint bytes.
func decodeDurableList[T any](raw []byte,
	decode func([]byte) (T, error)) ([]T, error) {

	var values []T
	reader := bytes.NewReader(raw)
	for reader.Len() > 0 {
		entry, err := wire.ReadVarBytes(
			reader, 0,
			uint32(
				reader.Len(),
			),
			"durable intent entry",
		)
		if err != nil {
			return nil, err
		}
		value, err := decode(entry)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}

	return values, nil
}

// encodeDurableFields serializes a nested record using the same TLV contract.
func encodeDurableFields(records ...tlv.Record) ([]byte, error) {
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	if err := stream.Encode(&raw); err != nil {
		return nil, err
	}

	return raw.Bytes(), nil
}

// decodeDurableFields decodes a nested record without interpreting its values.
func decodeDurableFields(raw []byte, records ...tlv.Record) error {
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Decode(bytes.NewReader(raw))
}

// encodeDurableAmount retains signed values until normal admission validation.
func encodeDurableAmount(amount int64) ([]byte, error) {
	value := uint64(amount)

	return encodeDurableFields(tlv.MakePrimitiveRecord(1, &value))
}

// decodeDurableAmount restores the original signed bit pattern.
func decodeDurableAmount(raw []byte) (int64, error) {
	var value uint64
	err := decodeDurableFields(raw, tlv.MakePrimitiveRecord(1, &value))

	return int64(value), err
}

// encodeDurableLeave distinguishes an absent request from an absent output.
func encodeDurableLeave(leave *types.LeaveRequest) ([]byte, error) {
	if leave == nil {
		return nil, nil
	}
	var flags uint8
	var amount uint64
	var script []byte
	if leave.IsChange {
		flags |= 1
	}
	if leave.Output != nil {
		flags |= 2
		amount = uint64(leave.Output.Value)
		script = leave.Output.PkScript
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &flags),
		tlv.MakePrimitiveRecord(3, &amount),
		tlv.MakePrimitiveRecord(5, &script),
	)
}

// decodeDurableLeave preserves placeholder positions in assembling intents.
func decodeDurableLeave(raw []byte) (*types.LeaveRequest, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var flags uint8
	var amount uint64
	var script []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &flags),
		tlv.MakePrimitiveRecord(3, &amount),
		tlv.MakePrimitiveRecord(5, &script),
	)
	if err != nil {
		return nil, err
	}
	if flags & ^uint8(3) != 0 {
		return nil, fmt.Errorf("unknown leave flags")
	}
	leave := &types.LeaveRequest{IsChange: flags&1 != 0}
	if flags&2 != 0 {
		leave.Output = wire.NewTxOut(int64(amount), script)
	}

	return leave, nil
}

// encodeDurableForfeit retains the local amount omitted by the join wire.
func encodeDurableForfeit(req types.ForfeitRequest) ([]byte, error) {
	var point []byte
	if req.VTXOOutpoint != nil {
		point = durableOutpoint(*req.VTXOOutpoint)
	}
	amount := uint64(req.Amount)
	auth, err := encodeDurableSpend(req.AuthSpend)
	if err != nil {
		return nil, err
	}
	spend, err := encodeDurableSpend(req.ForfeitSpend)
	if err != nil {
		return nil, err
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &point),
		tlv.MakePrimitiveRecord(3, &amount),
		tlv.MakePrimitiveRecord(5, &auth),
		tlv.MakePrimitiveRecord(7, &spend),
	)
}

// decodeDurableForfeit restores both authorization and collaborative paths.
func decodeDurableForfeit(raw []byte) (types.ForfeitRequest, error) {
	var req types.ForfeitRequest
	var point, auth, spend []byte
	var amount uint64
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &point),
		tlv.MakePrimitiveRecord(3, &amount),
		tlv.MakePrimitiveRecord(5, &auth),
		tlv.MakePrimitiveRecord(7, &spend),
	)
	if err != nil {
		return req, err
	}
	req.Amount = btcutil.Amount(int64(amount))
	if len(point) != 0 {
		outpoint, err := parseDurableOutpoint(point)
		if err != nil {
			return req, err
		}
		req.VTXOOutpoint = &outpoint
	}
	if len(auth) != 0 {
		req.AuthSpend, err = arkscript.DecodeSpendPath(auth)
		if err != nil {
			return req, err
		}
	}
	if len(spend) != 0 {
		req.ForfeitSpend, err = arkscript.DecodeSpendPath(spend)
		if err != nil {
			return req, err
		}
	}

	return req, nil
}

// encodeDurableSpend leaves standard-wallet paths absent until resolved by
// the normal transition from the canonical VTXO descriptor.
func encodeDurableSpend(path *arkscript.SpendPath) ([]byte, error) {
	if path == nil {
		return nil, nil
	}

	return path.Encode()
}
