package round

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/proto"
)

// encodeDurableFailure preserves typed failure classification and ownership
// metadata. The error string remains diagnostic; FailureCode controls behavior.
func encodeDurableFailure(v *BoardingFailed) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	var flags uint8
	if v.Recoverable {
		flags |= 1
	}
	var errorText []byte
	if v.Error != nil {
		flags |= 2
		errorText = []byte(v.Error.Error())
	}
	var admission []byte
	if v.Admission != nil {
		flags |= 4
		var err error
		admission, err = proto.MarshalOptions{
			Deterministic: true,
		}.Marshal(
			v.Admission,
		)
		if err != nil {
			return nil, err
		}
	}
	var id []byte
	if v.RoundID.IsSome() {
		value := v.RoundID.UnwrapOr(RoundID{})
		id = value[:]
	}
	reason := []byte(v.Reason)
	code := uint32(v.FailureCode)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &flags),
		tlv.MakePrimitiveRecord(3, &errorText),
		tlv.MakePrimitiveRecord(5, &admission),
		tlv.MakePrimitiveRecord(7, &id),
		tlv.MakePrimitiveRecord(9, &reason),
		tlv.MakePrimitiveRecord(11, &code),
	)
}

// decodeDurableFailure restores presence independently of empty field values.
func decodeDurableFailure(raw []byte) (*BoardingFailed, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var flags uint8
	var code uint32
	var errorText, admission, id, reason []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &flags),
		tlv.MakePrimitiveRecord(3, &errorText),
		tlv.MakePrimitiveRecord(5, &admission),
		tlv.MakePrimitiveRecord(7, &id),
		tlv.MakePrimitiveRecord(9, &reason),
		tlv.MakePrimitiveRecord(11, &code),
	)
	if err != nil {
		return nil, err
	}
	if flags & ^uint8(7) != 0 {
		return nil, fmt.Errorf("unknown failure flags")
	}
	v := &BoardingFailed{
		Reason: string(reason), Recoverable: flags&1 != 0,
		FailureCode: RoundFailureCode(code),
	}
	if flags&2 != 0 {
		v.Error = fmt.Errorf("%s", errorText)
	}
	if flags&4 != 0 {
		v.Admission = &roundpb.ServiceAdmission{}
		if err := proto.Unmarshal(admission, v.Admission); err != nil {
			return nil, err
		}
	}
	if len(id) != 0 {
		roundID, err := parseDurableRoundID(id)
		if err != nil {
			return nil, err
		}
		v.RoundID = fn.Some(roundID)
	}

	return v, nil
}
