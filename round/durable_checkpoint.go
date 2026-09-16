package round

import (
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableClientCheckpoint records whether an interrupted command may have used
// external signing state. Pending input remains unacknowledged in the mailbox.
type durableClientCheckpoint struct {
	Snapshot []byte
	InFlight []byte
}

// encode stores the complete actor state and optional in-flight input together.
func (v *durableClientCheckpoint) encode() ([]byte, error) {
	version := uint8(1)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &version),
		tlv.MakePrimitiveRecord(3, &v.Snapshot),
		tlv.MakePrimitiveRecord(5, &v.InFlight),
	)
}

// decodeDurableClientCheckpoint rejects unsupported checkpoint envelopes.
func decodeDurableClientCheckpoint(raw []byte,
	codec *actor.MessageCodec) (*durableClientCheckpoint, error) {

	var version uint8
	v := &durableClientCheckpoint{}
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &version),
		tlv.MakePrimitiveRecord(3, &v.Snapshot),
		tlv.MakePrimitiveRecord(5, &v.InFlight),
	)
	if err != nil {
		return nil, err
	}
	if version != 1 || len(v.Snapshot) == 0 {
		return nil, fmt.Errorf("invalid client checkpoint envelope")
	}
	if len(v.InFlight) != 0 {
		if _, err := codec.Decode(v.InFlight); err != nil {
			return nil, err
		}
	}

	return v, nil
}
