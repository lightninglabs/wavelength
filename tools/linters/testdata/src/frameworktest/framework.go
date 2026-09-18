package frameworktest

import (
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightningnetwork/lnd/tlv"
)

const badMessageType tlv.Type = 7

type badMessage struct{}

func (*badMessage) TLVType() tlv.Type {
	return badMessageType
}

func (*badMessage) Encode(w io.Writer) error {
	payload := struct{ Value uint64 }{}

	return binary.Write( // want "ATL001: durable actor message uses fixed-layout binary encoding"
		w, binary.BigEndian, payload,
	)
}

func (*badMessage) Decode(r io.Reader) error {
	payload := struct{ Value uint64 }{}

	return binary.Read( // want "ATL001: durable actor message uses fixed-layout binary encoding"
		r, binary.BigEndian, &payload,
	)
}

func badCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()
	codec.MustRegister(
		8, // want "ATL002: message codec registration ID does not match"
		func() actor.TLVMessage { return &badMessage{} },
	)
	codec.MustRegister(
		8, // want "ATL003: message codec registers the same TLV type" "ATL002: message codec registration ID does not match"
		func() actor.TLVMessage { return &badMessage{} },
	)

	return codec
}

type stateA struct{}
type stateB struct{}
type stateTerminal struct{}
type eventA struct{}
type eventB struct{}

var badTable = protofsm.TransitionTable[any, any, any]{
	MachineName: "", // want "PFS004: protofsm transition table must have a non-empty MachineName"
	States: []protofsm.StateTransitions[any, any, any]{
		{
			FromState: &stateA{},
			Transitions: []protofsm.TransitionEntry[any, any, any]{
				{
					Event:       &eventA{},
					ToState:     &stateTerminal{},
					Description: "", // want "PFS003: protofsm transition must have a non-empty Description"
					IsTerminal:  true,
				},
				{
					Event:       &eventA{}, // want "PFS002: protofsm state contains duplicate event-to-state"
					ToState:     &stateTerminal{},
					Description: "duplicate event",
					IsTerminal:  true,
				},
			},
		},
		{
			FromState: &stateA{}, // want "PFS001: protofsm transition table contains duplicate rows"
			Transitions: []protofsm.TransitionEntry[any, any, any]{
				{
					Event:       &eventB{},
					ToState:     &stateA{},
					Description: "duplicate row",
				},
			},
		},
		{
			FromState: &stateTerminal{},
			Transitions: []protofsm.TransitionEntry[any, any, any]{
				{
					Event:       &eventB{},
					ToState:     &stateTerminal{},
					Description: "terminal self-loop",
					IsTerminal:  true,
				},
			},
		},
		{
			FromState: &stateB{},
			Transitions: []protofsm.TransitionEntry[any, any, any]{
				{
					Event:       &eventB{},
					ToState:     &stateTerminal{}, // want "PFS005: transition to a terminal self-loop state must set IsTerminal"
					Description: "forgot terminal marker",
				},
			},
		},
	},
}

func unboundedDetachedAsk(future actor.Future[int],
	detached actor.DetachedAsk[int]) {

	future.OnComplete(
		detached.CallerCtx, // want "ALC001: DetachedAsk.CallerCtx must be wrapped"
		func(error) {},
	)

	boundedCtx, cancel := context.WithTimeout(
		detached.CallerCtx, time.Second,
	)
	defer cancel()
	future.OnComplete(boundedCtx, func(error) {})
}

func forbiddenClassicPool(cfg actor.DurableActorConfig[int, error]) {
	_ = cfg.AllowConcurrentClassicBehavior() // want "ALC002: AllowConcurrentClassicBehavior is a test-only escape hatch"
}

type store struct{}

type stagedBehavior struct{}

func (*stagedBehavior) Receive( // want "ALC003: TxBehavior.Receive stages durable state but never commits" Receive:"&\\{8\\}"
	ctx context.Context, _ int, ax actor.Exec[store]) error {

	return ax.Stage(ctx, func(context.Context, store) error {
		return nil
	})
}

func writerDeadlock(ctx context.Context, ax actor.Exec[store], // want writerDeadlock:"&\\{16\\}"
	ref actor.ActorRef[int, int], db *sql.DB) error {

	return ax.Commit( // want "ATX001: writer transaction callback waits on an actor future" "ATX003: writer transaction callback sends with actor.WithoutTx" "ATX004: writer transaction callback opens an independent sql.DB transaction" "ATX005: transaction context escapes its callback lifetime"
		ctx, func(txCtx context.Context, _ store) error {
			_ = ref.Ask(txCtx, 1).Await(txCtx)
			_ = ref.Tell(actor.WithoutTx(txCtx), 1)
			_, _ = db.BeginTx(txCtx, nil)
			ref.Ask(txCtx, 1).OnComplete(txCtx, func(error) {})

			return nil
		},
	)
}

func awaitHelper(ctx context.Context, // want awaitHelper:"&\\{1\\}"
	ref actor.ActorRef[int, int]) error {

	return ref.Ask(ctx, 1).Await(ctx)
}

func indirectWriterDeadlock(ctx context.Context, // want indirectWriterDeadlock:"&\\{16\\}"
	ax actor.Exec[store], ref actor.ActorRef[int, int]) error {

	return ax.Commit( // want "ATX001: writer transaction callback waits on an actor future"
		ctx, func(txCtx context.Context, _ store) error {
			return awaitHelper(txCtx, ref)
		},
	)
}

func safeTransactionPhases(ctx context.Context, ax actor.Exec[store], // want safeTransactionPhases:"&\\{25\\}"
	ref actor.ActorRef[int, int]) error {

	if err := ax.Stage(ctx, func(txCtx context.Context, _ store) error {
		return ref.Tell(txCtx, 1)
	}); err != nil {
		return err
	}

	_ = ref.Ask(ctx, 1).Await(ctx)

	return ax.Commit(ctx, func(context.Context, store) error {
		return nil
	})
}

type classicBehavior struct {
	ref actor.ActorRef[int, error]
}

func (b *classicBehavior) Receive(ctx context.Context, msg int) error { // want Receive:"&\\{1\\}"
	return b.ref.Ask(ctx, msg).Await(ctx)
}

func badClassicConfig(codec *actor.MessageCodec) {
	_ = actor.DefaultDurableActorConfig(
		"classic", &classicBehavior{}, // want "ATX002: classic durable behavior may await an actor"
		nil, codec,
	)
}
