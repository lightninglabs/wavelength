package round

import (
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
)

// newDurableClientCodec includes the runtime restart message and every ingress.
func newDurableClientCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()
	RegisterDurableClientMessages(codec)
	codec.MustRegister(actor.RestartTLVType, func() actor.TLVMessage {
		return &actor.RestartMessage{}
	})

	return codec
}

// durableClientIngress freezes ordinary requests before the mailbox accepts
// them.
func durableClientIngress(msg actormsg.RoundReceivable) (actor.TLVMessage,
	error) {

	switch message := msg.(type) {
	case *durableClientCommand:
		return message, nil

	case *durableClientEffect:
		return message, nil

	case *ServerMessageNotification:
		return message, nil

	default:
		return newDurableClientCommand(msg)
	}
}

// RegisterDurableClientMessages adds the client envelopes to a shared outbox
// codec. Register generic runtime messages, including restart, separately.
func RegisterDurableClientMessages(codec *actor.MessageCodec) {
	codec.MustRegister(durableClientCommandType, func() actor.TLVMessage {
		return &durableClientCommand{}
	})
	codec.MustRegister(durableServerMessageType, func() actor.TLVMessage {
		return &ServerMessageNotification{}
	})
	codec.MustRegister(durableClientEffectType, func() actor.TLVMessage {
		return &durableClientEffect{}
	})
}
