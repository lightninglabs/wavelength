package round

import (
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightningnetwork/lnd/tlv"
)

const durableClientEffectType tlv.Type = 0x5303

// Effect kinds identify persisted deliveries independently of Go type names.
const (
	clientEffectServer           uint8 = 1
	clientEffectConfirmation     uint8 = 2
	clientEffectStartTimer       uint8 = 3
	clientEffectCancelTimer      uint8 = 4
	clientEffectRelease          uint8 = 5
	clientEffectDropCustom       uint8 = 6
	clientEffectCreated          uint8 = 7
	clientEffectForfeitRequest   uint8 = 8
	clientEffectForfeitConfirmed uint8 = 9
)

// durableClientEffect freezes one external effect before mailbox
// acknowledgement.
type durableClientEffect struct {
	actor.BaseMessage
	Kind    uint8
	Payload []byte
}

// MessageType identifies a committed client effect.
func (*durableClientEffect) MessageType() string { return "round.ClientEffect" }

// TLVType identifies the stable effect envelope.
func (*durableClientEffect) TLVType() tlv.Type {
	return durableClientEffectType
}

// Encode writes the already-frozen payload without consulting the current
// clock.
func (m *durableClientEffect) Encode(w io.Writer) error {
	if err := m.validate(); err != nil {
		return err
	}
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &m.Kind),
		tlv.MakePrimitiveRecord(3, &m.Payload),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode requires both fields before the runtime may deliver the effect.
func (m *durableClientEffect) Decode(r io.Reader) error {
	*m = durableClientEffect{}
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &m.Kind),
		tlv.MakePrimitiveRecord(3, &m.Payload),
	)
	if err != nil {
		return err
	}
	fields, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}
	for _, field := range []tlv.Type{1, 3} {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("missing effect field %d", field)
		}
	}

	return m.validate()
}

// validate rejects unknown effects instead of acknowledging an ignored payload.
func (m *durableClientEffect) validate() error {
	if m.Kind < clientEffectServer ||
		m.Kind > clientEffectForfeitConfirmed {
		return fmt.Errorf("unknown client effect %d", m.Kind)
	}

	return nil
}

// newDurableClientEffect fixes transport IDs and timer deadlines at capture
// time.
func newDurableClientEffect(msg ClientOutMsg,
	now time.Time) (*durableClientEffect, error) {

	if server, ok := msg.(serverconn.ServerMessage); ok {
		route := server.ServiceMethod()
		request := &serverconn.SendClientEventRequest{
			Message: server,
			Service: route.Service, Method: route.Method,
		}
		var raw bytes.Buffer
		if err := request.Encode(&raw); err != nil {
			return nil, err
		}

		return &durableClientEffect{
			Kind:    clientEffectServer,
			Payload: raw.Bytes(),
		}, nil
	}

	return newDurableLocalEffect(msg, now)
}

// serverRequest restores the native server transport envelope and dedupe IDs.
func (m *durableClientEffect) serverRequest() (
	*serverconn.SendClientEventRequest, error) {

	if m.Kind != clientEffectServer {
		return nil, fmt.Errorf("not a server effect")
	}
	request := &serverconn.SendClientEventRequest{}
	if err := request.Decode(bytes.NewReader(m.Payload)); err != nil {
		return nil, err
	}

	return request, nil
}

var _ actor.TLVMessage = (*durableClientEffect)(nil)
