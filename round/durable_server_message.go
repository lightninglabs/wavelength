package round

import (
	"fmt"
	"io"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/proto"
)

const durableServerMessageType tlv.Type = 0x5302

// wireRoundEvent uses the same domain validation for live and replayed input.
type wireRoundEvent interface {
	ClientEvent

	FromProto(proto.Message) error
}

// serverMessageTypes binds each push method to its protobuf and domain types.
func serverMessageTypes(method string, opts ...roundpb.TreeFromProtoOption) (
	proto.Message, wireRoundEvent, error) {

	switch method {
	case roundpb.MethodJoinAck:
		return &roundpb.ClientSuccessResp{}, &RoundJoined{}, nil

	case roundpb.MethodJoinRoundQuote:
		return &roundpb.JoinRoundQuote{}, &JoinRoundQuoteReceived{}, nil

	case roundpb.MethodBatchInfo:
		return &roundpb.ClientBatchInfo{}, &CommitmentTxBuilt{
			TreeOpts: opts,
		}, nil

	case roundpb.MethodAwaitingInputSigs:
		payload := &roundpb.ClientAwaitingInputSigsResp{}

		return payload, &AwaitingBoardingSigs{}, nil

	case roundpb.MethodAggNonces:
		return &roundpb.ClientVTXOAggNonces{}, &NoncesAggregated{}, nil

	case roundpb.MethodAggSigs:
		return &roundpb.ClientVTXOAggSigs{}, &OperatorSigned{}, nil

	case roundpb.MethodRoundFailed:
		return &roundpb.ClientRoundFailedResp{}, &BoardingFailed{}, nil

	case roundpb.MethodError:
		return &roundpb.ClientErrorResp{}, &BoardingFailed{}, nil

	case roundpb.MethodRoundStatusReport:
		payload := &roundpb.ClientRoundStatusReport{}

		return payload, &RoundStatusReported{}, nil

	default:
		return nil, nil, fmt.Errorf("unsupported round push method %q",
			method)
	}
}

// NewServerMessageNotification validates a push and retains its protobuf for
// durable delivery. Decoding a stored notification applies the same validation
// with the receiving actor's configured tree limits.
func NewServerMessageNotification(method string, payload proto.Message,
	opts ...roundpb.TreeFromProtoOption) (*ServerMessageNotification,
	error) {

	expected, event, err := serverMessageTypes(method, opts...)
	if err != nil {
		return nil, err
	}
	if payload == nil || !payload.ProtoReflect().IsValid() ||
		payload.ProtoReflect().Descriptor().FullName() !=
			expected.ProtoReflect().Descriptor().FullName() {
		return nil, fmt.Errorf("unexpected round push payload for %s",
			method)
	}
	payload = proto.Clone(payload)
	if err := event.FromProto(payload); err != nil {
		return nil, err
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return &ServerMessageNotification{
		Message: event, wireMethod: []byte(method), wirePayload: raw,
	}, nil
}

// TLVType identifies a server push in the durable actor mailbox.
func (*ServerMessageNotification) TLVType() tlv.Type {
	return durableServerMessageType
}

// wireStream retains the method identity independently of protobuf contents.
func (m *ServerMessageNotification) wireStream() (*tlv.Stream, error) {
	return tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &m.wireMethod),
		tlv.MakePrimitiveRecord(3, &m.wirePayload),
	)
}

// Encode requires the original wire envelope; reconstructing it from a domain
// event could discard protocol fields needed when replaying after an upgrade.
func (m *ServerMessageNotification) Encode(w io.Writer) error {
	if _, _, err := serverMessageTypes(string(m.wireMethod)); err != nil {
		return err
	}
	stream, err := m.wireStream()
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode restores wire data only. Domain validation uses actor configuration
// when the restored notification reaches Receive.
func (m *ServerMessageNotification) Decode(r io.Reader) error {
	*m = ServerMessageNotification{}
	stream, err := m.wireStream()
	if err != nil {
		return err
	}
	fields, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}
	for _, field := range []tlv.Type{1, 3} {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("missing round push field %d", field)
		}
	}
	_, _, err = serverMessageTypes(string(m.wireMethod))

	return err
}

// restoreWireEvent reconstructs a fresh event from the persisted protobuf.
func (m *ServerMessageNotification) restoreWireEvent(maxTreeNodes int) error {
	var opts []roundpb.TreeFromProtoOption
	if maxTreeNodes > 0 {
		opts = append(opts, roundpb.WithMaxTreeNodes(maxTreeNodes))
	}
	payload, event, err := serverMessageTypes(string(m.wireMethod), opts...)
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(m.wirePayload, payload); err != nil {
		return err
	}
	if err := event.FromProto(payload); err != nil {
		return err
	}
	m.Message = event

	return nil
}

var _ actor.TLVMessage = (*ServerMessageNotification)(nil)
