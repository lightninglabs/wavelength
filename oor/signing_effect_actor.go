package oor

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// SigningEffectActorID is the default durable mailbox id for OOR
	// signing side effects.
	SigningEffectActorID = "oor-signing-effect"

	signingEffectRequestTLVType tlv.Type = 0x7020
)

const (
	signingEffectKindRecordType       tlv.Type = 1
	signingEffectSessionIDRecordType  tlv.Type = 3
	signingEffectArkPSBTRecordType    tlv.Type = 5
	signingEffectCheckpointRecordType tlv.Type = 7
	signingEffectTransferInputType    tlv.Type = 9
	signingEffectRequestArk           uint64   = 1
	signingEffectRequestCheckpoint    uint64   = 2
)

// SigningEffectMsg is the sealed message set accepted by the signing effect
// actor.
type SigningEffectMsg interface {
	actor.TLVMessage
	signingEffectMsgSealed()
}

// SigningEffectRequest is a durable request to perform wallet signing for one
// OOR session and feed the resulting event back into the OOR actor.
type SigningEffectRequest struct {
	actor.BaseMessage

	Kind            uint64
	SessionID       SessionID
	ArkPSBT         *psbt.Packet
	CheckpointPSBTs []*psbt.Packet
	TransferInputs  []TransferInput
}

// MessageType returns a human-readable type name for logging.
func (m *SigningEffectRequest) MessageType() string {
	return "SigningEffectRequest"
}

// TLVType returns the durable mailbox type identifier.
func (m *SigningEffectRequest) TLVType() tlv.Type {
	return signingEffectRequestTLVType
}

// signingEffectMsgSealed marks this as a signing-effect actor message.
func (m *SigningEffectRequest) signingEffectMsgSealed() {}

// Encode serializes the signing request.
func (m *SigningEffectRequest) Encode(w io.Writer) error {
	if m == nil {
		return fmt.Errorf("signing effect request must be provided")
	}

	sessionBytes := sessionIDBytes(m.SessionID)

	var arkBytes []byte
	if m.ArkPSBT != nil {
		var err error
		arkBytes, err = psbtutil.Serialize(m.ArkPSBT)
		if err != nil {
			return err
		}
	}

	checkpoints, err := serializePSBTSlice(m.CheckpointPSBTs)
	if err != nil {
		return err
	}

	checkpointBytes, err := encodeLengthPrefixedBlobList(checkpoints)
	if err != nil {
		return err
	}

	inputSnaps, err := snapshotTransferInputs(m.TransferInputs)
	if err != nil {
		return err
	}

	inputBytes, err := encodeTransferInputSnapshots(inputSnaps)
	if err != nil {
		return err
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			signingEffectKindRecordType, &m.Kind,
		),
		tlv.MakePrimitiveRecord(
			signingEffectSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			signingEffectArkPSBTRecordType, &arkBytes,
		),
		tlv.MakePrimitiveRecord(
			signingEffectCheckpointRecordType, &checkpointBytes,
		),
		tlv.MakePrimitiveRecord(
			signingEffectTransferInputType, &inputBytes,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the signing request.
func (m *SigningEffectRequest) Decode(r io.Reader) error {
	var (
		sessionBytes   []byte
		arkBytes       []byte
		checkpointBlob []byte
		inputBlob      []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			signingEffectKindRecordType, &m.Kind,
		),
		tlv.MakePrimitiveRecord(
			signingEffectSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			signingEffectArkPSBTRecordType, &arkBytes,
		),
		tlv.MakePrimitiveRecord(
			signingEffectCheckpointRecordType, &checkpointBlob,
		),
		tlv.MakePrimitiveRecord(
			signingEffectTransferInputType, &inputBlob,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return err
	}

	m.SessionID, err = parseSessionID(sessionBytes)
	if err != nil {
		return err
	}

	if len(arkBytes) > 0 {
		m.ArkPSBT, err = psbtutil.Parse(arkBytes)
		if err != nil {
			return err
		}
	}

	checkpointRaw, err := decodeLengthPrefixedBlobList(checkpointBlob)
	if err != nil {
		return err
	}

	m.CheckpointPSBTs, err = parsePSBTSlice(checkpointRaw)
	if err != nil {
		return err
	}

	inputSnaps, err := decodeTransferInputSnapshots(inputBlob)
	if err != nil {
		return err
	}

	m.TransferInputs = make([]TransferInput, 0, len(inputSnaps))
	for i := range inputSnaps {
		input, err := TransferInputFromSnapshot(inputSnaps[i])
		if err != nil {
			return err
		}

		m.TransferInputs = append(m.TransferInputs, input)
	}

	return nil
}

// NewSigningEffectCodec registers the signing effect message set.
func NewSigningEffectCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()
	RegisterSigningEffectMessages(codec)

	return codec
}

// RegisterSigningEffectMessages registers signing effect messages on a codec.
func RegisterSigningEffectMessages(codec *actor.MessageCodec) {
	codec.MustRegister(
		signingEffectRequestTLVType, func() actor.TLVMessage {
			return &SigningEffectRequest{}
		},
	)
}

// SigningEffectActorConfig configures the OOR signing effect actor.
type SigningEffectActorConfig struct {
	ActorID       string
	DeliveryStore actor.DeliveryStore
	Signer        input.Signer
	OORRef        actor.TellOnlyRef[OORDurableMsg]
	ActorSystem   actor.SystemContext
	Log           fn.Option[btclog.Logger]
}

// SigningEffectActor performs wallet signing outside the OOR actor turn.
type SigningEffectActor struct {
	cfg     SigningEffectActorConfig
	durable *actor.DurableActor[SigningEffectMsg, actor.Message]
}

// NewSigningEffectActor creates, starts, and optionally registers a durable
// signing effect actor.
func NewSigningEffectActor(cfg SigningEffectActorConfig) (*SigningEffectActor,
	error) {

	if cfg.ActorID == "" {
		cfg.ActorID = SigningEffectActorID
	}

	if cfg.DeliveryStore == nil {
		return nil, fmt.Errorf("delivery store must be provided")
	}

	if cfg.Signer == nil {
		return nil, fmt.Errorf("signer must be provided")
	}

	if cfg.OORRef == nil {
		return nil, fmt.Errorf("oor ref must be provided")
	}

	effect := &SigningEffectActor{cfg: cfg}

	durableCfg := actor.DefaultDurableActorConfig[
		SigningEffectMsg, actor.Message,
	](
		cfg.ActorID,
		effect,
		cfg.DeliveryStore,
		NewSigningEffectCodec(),
	)
	durableCfg.Log = cfg.Log

	effect.durable = actor.NewDurableActor(durableCfg)
	effect.durable.Start()

	if cfg.ActorSystem != nil {
		erasingRef := actor.TypeAssertingRef[
			actor.Message,
			SigningEffectMsg,
			actor.Message,
		](effect.durable.Ref())

		key := actor.NewServiceKey[actor.Message, any](cfg.ActorID)
		if err := actor.RegisterWithReceptionist(
			cfg.ActorSystem.Receptionist(), key, erasingRef,
		); err != nil {
			effect.durable.Stop()
			return nil, fmt.Errorf("register signing effect: %w",
				err)
		}
	}

	effect.logger(context.Background()).InfoS(
		context.Background(), "OOR signing effect actor started",
		slog.String("actor_id", cfg.ActorID),
	)

	return effect, nil
}

// Ref returns the durable actor ref.
func (a *SigningEffectActor) Ref() actor.ActorRef[SigningEffectMsg,
	actor.Message] {

	return a.durable.Ref()
}

// StopAndWait stops the signing effect actor and waits for shutdown.
func (a *SigningEffectActor) StopAndWait(ctx context.Context) error {
	a.durable.Stop()

	return a.durable.Wait(ctx)
}

// Receive handles one signing request and feeds the result back to OOR.
func (a *SigningEffectActor) Receive(ctx context.Context,
	msg SigningEffectMsg) fn.Result[actor.Message] {

	req, ok := msg.(*SigningEffectRequest)
	if !ok {
		return fn.Err[actor.Message](fmt.Errorf(
			"unknown signing effect message: %T", msg,
		))
	}

	events := a.handleSigningRequest(ctx, req)
	for _, event := range events {
		if err := a.cfg.OORRef.Tell(ctx, &DriveEventRequest{
			SessionID: req.SessionID,
			Event:     event,
		}); err != nil {
			return fn.Err[actor.Message](fmt.Errorf(
				"drive signing result: %w", err,
			))
		}
	}

	return fn.Ok[actor.Message](nil)
}

func (a *SigningEffectActor) handleSigningRequest(ctx context.Context,
	req *SigningEffectRequest) []Event {

	outbox, err := req.toOutboxEvent()
	if err != nil {
		return []Event{&FailEvent{Reason: err.Error()}}
	}

	handler := &SigningOutboxHandler{
		Signer: a.cfg.Signer,
	}

	followUps, err := handler.Handle(ctx, req.SessionID, outbox)
	if err != nil {
		return []Event{NewOutboxErrorEvent(outbox, err)}
	}

	if len(followUps) == 0 {
		return []Event{
			&FailEvent{
				Reason: "signing produced no follow-up event",
			},
		}
	}

	return followUps
}

func (m *SigningEffectRequest) toOutboxEvent() (OutboxEvent, error) {
	switch m.Kind {
	case signingEffectRequestArk:
		return &RequestArkSignatures{
			ArkPSBT:         m.ArkPSBT,
			CheckpointPSBTs: m.CheckpointPSBTs,
			TransferInputs:  m.TransferInputs,
		}, nil

	case signingEffectRequestCheckpoint:
		return &RequestCheckpointSignatures{
			ArkPSBT:                 m.ArkPSBT,
			CoSignedCheckpointPSBTs: m.CheckpointPSBTs,
			TransferInputs:          m.TransferInputs,
		}, nil

	default:
		return nil, fmt.Errorf("unknown signing request kind: %d",
			m.Kind)
	}
}

func (a *SigningEffectActor) logger(ctx context.Context) btclog.Logger {
	return a.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

func signingEffectRequest(sessionID SessionID,
	outbox OutboxEvent) (*SigningEffectRequest, bool) {

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		return &SigningEffectRequest{
			Kind:            signingEffectRequestArk,
			SessionID:       sessionID,
			ArkPSBT:         msg.ArkPSBT,
			CheckpointPSBTs: msg.CheckpointPSBTs,
			TransferInputs:  msg.TransferInputs,
		}, true

	case *RequestCheckpointSignatures:
		return &SigningEffectRequest{
			Kind:            signingEffectRequestCheckpoint,
			SessionID:       sessionID,
			ArkPSBT:         msg.ArkPSBT,
			CheckpointPSBTs: msg.CoSignedCheckpointPSBTs,
			TransferInputs:  msg.TransferInputs,
		}, true

	default:
		return nil, false
	}
}

type signingEffectBehavior = actor.ActorBehavior[
	SigningEffectMsg, actor.Message,
]

var _ SigningEffectMsg = (*SigningEffectRequest)(nil)
var _ signingEffectBehavior = (*SigningEffectActor)(nil)
