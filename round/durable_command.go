package round

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/timeout"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const durableClientCommandType tlv.Type = 0x5301

// Command kinds are persisted identifiers and must not be renumbered.
const (
	clientCommandIntent       uint64 = 1
	clientCommandBoarding     uint64 = 2
	clientCommandVTXOs        uint64 = 3
	clientCommandTimeout      uint64 = 4
	clientCommandConfirmation uint64 = 5
	clientCommandCancel       uint64 = 6
	clientCommandForfeit      uint64 = 7
	clientCommandGetState     uint64 = 8
	clientCommandOutputs      uint64 = 9
	clientCommandRefresh      uint64 = 10
	clientCommandCohort       uint64 = 11
	clientCommandTrigger      uint64 = 12
)

// durableClientCommand owns the data required to redeliver one local command.
// Actor references and caller contexts remain outside the durable payload.
type durableClientCommand struct {
	actor.BaseMessage
	Kind          uint64
	Payload       []byte
	Flags         uint8
	Key           []byte
	TxID          chainhash.Hash
	BlockHash     chainhash.Hash
	Height        uint32
	Confirmations uint32
}

// MessageType identifies the durable client command envelope.
func (*durableClientCommand) MessageType() string {
	return "round.ClientCommand"
}

// TLVType identifies the stable command schema in the runtime codec.
func (*durableClientCommand) TLVType() tlv.Type {
	return durableClientCommandType
}

// stream binds each command field to its stable record number.
func (m *durableClientCommand) stream() (*tlv.Stream, error) {
	return tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &m.Kind),
		tlv.MakePrimitiveRecord(3, &m.Payload),
		tlv.MakePrimitiveRecord(5, &m.Flags),
		tlv.MakePrimitiveRecord(7, &m.Key),
		tlv.MakePrimitiveRecord(
			9, (*[32]byte)(&m.TxID),
		),
		tlv.MakePrimitiveRecord(
			11, (*[32]byte)(&m.BlockHash),
		),
		tlv.MakePrimitiveRecord(13, &m.Height),
		tlv.MakePrimitiveRecord(15, &m.Confirmations),
	)
}

// validate rejects unsupported command tags and unknown presence flags.
func (m *durableClientCommand) validate() error {
	if m.Kind < clientCommandIntent || m.Kind > clientCommandTrigger {
		return fmt.Errorf("unknown durable client command: %d", m.Kind)
	}
	if m.Flags > 1 {
		return fmt.Errorf("unknown durable command flags")
	}

	return nil
}

// Encode writes an envelope suitable for the durable mailbox.
func (m *durableClientCommand) Encode(w io.Writer) error {
	if err := m.validate(); err != nil {
		return err
	}
	stream, err := m.stream()
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode requires all envelope fields before the runtime can dispatch it.
func (m *durableClientCommand) Decode(r io.Reader) error {
	*m = durableClientCommand{}
	stream, err := m.stream()
	if err != nil {
		return err
	}
	parsed, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}
	for field := tlv.Type(1); field <= 15; field += 2 {
		if _, ok := parsed[field]; !ok {
			return fmt.Errorf("missing durable command field %d",
				field)
		}
	}

	return m.validate()
}

// newDurableClientCommand freezes local input before a caller can mutate it.
func newDurableClientCommand(msg actormsg.RoundReceivable) (
	*durableClientCommand, error) {

	m := &durableClientCommand{}
	var err error
	switch req := msg.(type) {
	case *RegisterIntentRequest:
		if req.Package == nil {
			return nil, fmt.Errorf("missing intent package")
		}
		m.Kind = clientCommandIntent
		m.Payload, err = encodeDurableIntents(req.Package.Intents)
		if req.TriggerRegistration {
			m.Flags = 1
		}

	case *actormsg.RegisterIntentMsg:
		m.Kind = clientCommandIntent
		m.Payload, err = encodeDurableIntents(Intents{
			Service: req.Service, Forfeits: req.Forfeits,
			VTXOs: req.VTXOs, Leaves: req.Leaves,
		})
		if req.TriggerRegistration {
			m.Flags = 1
		}

	case *WalletBoardingConfirmed:
		if req.Intent == nil {
			return nil, fmt.Errorf("missing boarding intent")
		}
		m.Kind = clientCommandBoarding
		m.Payload, err = encodeDurableBoarding(BoardingIntent{
			BoardingIntent: *req.Intent,
		})

	case *VTXORequestsReceived:
		m.Kind = clientCommandVTXOs
		m.Payload, err = encodeDurableList(
			req.Requests, encodeDurableVTXO,
		)

	case *TimeoutMsg:
		m.Kind = clientCommandTimeout
		m.Key = []byte(req.TimeoutID)
		if !req.deadline.IsZero() {
			m.Payload, err = req.deadline.UTC().MarshalBinary()
		}

	case *ConfirmationEvent:
		m.Kind = clientCommandConfirmation
		m.TxID, m.BlockHash = req.Txid, req.BlockHash
		m.Height = uint32(req.BlockHeight)
		m.Confirmations = req.Confirmations
		if req.Tx != nil {
			var tx bytes.Buffer
			err = req.Tx.Serialize(&tx)
			m.Payload = tx.Bytes()
		}

	case *ForfeitSignatureResponse:
		m.Kind = clientCommandForfeit
		m.Payload, err = encodeDurableForfeitResponse(req)

	case *CancelRoundRequest:
		m.Kind = clientCommandCancel
		if req.RoundKey.IsSome() {
			m.Flags = 1
			m.Key = []byte(req.RoundKey.UnwrapOr(""))
		}

	default:
		return newDurableLocalCommand(msg)
	}
	if err != nil {
		return nil, err
	}

	return m, nil
}

// message reconstructs the ordinary actor command using fixed network config.
func (m *durableClientCommand) message(params *chaincfg.Params) (
	actormsg.RoundReceivable, error) {

	if err := m.validate(); err != nil {
		return nil, err
	}
	switch m.Kind {
	case clientCommandIntent:
		intents, err := decodeDurableIntents(m.Payload, params)
		if err != nil {
			return nil, err
		}

		return &RegisterIntentRequest{
			Package: &IntentPackage{
				Intents: intents,
			},
			TriggerRegistration: m.Flags == 1,
		}, nil

	case clientCommandBoarding:
		intent, err := decodeDurableBoarding(m.Payload, params)
		if err != nil {
			return nil, err
		}

		return &WalletBoardingConfirmed{
			Intent: &intent.BoardingIntent,
		}, nil

	case clientCommandVTXOs:
		requests, err := decodeDurableList(m.Payload, decodeDurableVTXO)
		if err != nil {
			return nil, err
		}

		return &VTXORequestsReceived{Requests: requests}, nil

	case clientCommandTimeout:
		msg := &TimeoutMsg{TimeoutID: timeout.ID(m.Key)}
		if len(m.Payload) != 0 {
			if err := msg.deadline.UnmarshalBinary(
				m.Payload,
			); err != nil {
				return nil, err
			}
		}

		return msg, nil

	case clientCommandConfirmation:
		event := &ConfirmationEvent{
			Txid: m.TxID, BlockHash: m.BlockHash,
			BlockHeight:   int32(m.Height),
			Confirmations: m.Confirmations,
		}
		if len(m.Payload) != 0 {
			event.Tx = wire.NewMsgTx(2)
			reader := bytes.NewReader(m.Payload)
			if err := event.Tx.Deserialize(reader); err != nil {
				return nil, err
			}
			if reader.Len() != 0 {
				return nil, fmt.Errorf("trailing " +
					"confirmation tx bytes")
			}
		}

		return event, nil

	case clientCommandForfeit:
		response, err := decodeDurableForfeitResponse(m.Payload)
		if err != nil {
			return nil, err
		}
		if response == nil {
			return nil, fmt.Errorf("missing forfeit response")
		}

		return response, nil

	case clientCommandCancel:
		request := &CancelRoundRequest{}
		if m.Flags == 1 {
			request.RoundKey = fn.Some(RoundKeyStr(m.Key))
		}

		return request, nil

	default:
		return m.localMessage()
	}
}

var _ actor.TLVMessage = (*durableClientCommand)(nil)

// RoundReceivable lets the existing dispatcher accept restored commands.
func (*durableClientCommand) RoundReceivable() {}
