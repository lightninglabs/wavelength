package serverconn

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	TransportConnectorServerConn = "serverconn"

	defaultEgressPollInterval = time.Second
	defaultEgressLease        = time.Minute
	defaultEgressBatchSize    = 32
)

// EgressEnvelope is a durable transport effect row. The only blob is a
// mailboxpb.Envelope wire protobuf, named envelope in SQL.
type EgressEnvelope struct {
	ID              string
	Connector       string
	LocalMailboxID  string
	RemoteMailboxID string
	RPCKind         string
	Service         string
	Method          string
	CorrelationID   string
	ReplyTo         string
	MsgID           string
	IdempotencyKey  string
	Envelope        []byte
	ClaimToken      string
	Attempts        int32
}

// TransportStore persists ingress cursors and outbound mailbox envelopes.
type TransportStore interface {
	LoadIngressCursor(ctx context.Context, localMailboxID,
		remoteMailboxID string) (AckState, error)

	SaveIngressCursor(ctx context.Context, localMailboxID,
		remoteMailboxID string, state AckState) error

	InsertEgress(ctx context.Context, env EgressEnvelope) error

	ClaimDueEgress(ctx context.Context, owner string, limit int,
		lease time.Duration) ([]EgressEnvelope, error)

	MarkEgressSent(ctx context.Context, id, claimToken string) error

	ReleaseEgressForRetry(ctx context.Context, id, claimToken string,
		retryAfter time.Duration, failure error) error

	ReleaseExpiredEgressClaims(ctx context.Context) error
}

// MailboxEgressWorker drains SQL mailbox_egress rows to the remote edge.
type MailboxEgressWorker struct {
	cfg MailboxEgressWorkerConfig

	// The worker owns this run context for its background loop.
	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc
	done   chan struct{}
}

type MailboxEgressWorkerConfig struct {
	Store        TransportStore
	Edge         mailboxpb.MailboxServiceClient
	Clock        clock.Clock
	Log          fn.Option[btclog.Logger]
	Owner        string
	PollInterval time.Duration
	Lease        time.Duration
	BatchSize    int
}

func NewMailboxEgressWorker(
	cfg MailboxEgressWorkerConfig) *MailboxEgressWorker {

	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}
	if cfg.Owner == "" {
		cfg.Owner = "serverconn-egress-" + uuid.NewString()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultEgressPollInterval
	}
	if cfg.Lease <= 0 {
		cfg.Lease = defaultEgressLease
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultEgressBatchSize
	}

	return &MailboxEgressWorker{cfg: cfg}
}

func (w *MailboxEgressWorker) Start(ctx context.Context) error {
	if w.cfg.Store == nil {
		return fmt.Errorf("transport store is required")
	}
	if w.cfg.Edge == nil {
		return fmt.Errorf("mailbox edge is required")
	}
	if w.done != nil {
		return nil
	}

	if err := w.cfg.Store.ReleaseExpiredEgressClaims(ctx); err != nil {
		return err
	}

	w.ctx, w.cancel = context.WithCancel(ctx)
	w.done = make(chan struct{})
	go w.loop()

	return nil
}

func (w *MailboxEgressWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if w.done != nil {
		<-w.done
		w.done = nil
	}
}

func (w *MailboxEgressWorker) loop() {
	defer close(w.done)

	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		w.drainOnce(w.ctx)

		select {
		case <-w.ctx.Done():
			return

		case <-ticker.C:
		}
	}
}

func (w *MailboxEgressWorker) drainOnce(ctx context.Context) {
	rows, err := w.cfg.Store.ClaimDueEgress(
		ctx, w.cfg.Owner, w.cfg.BatchSize, w.cfg.Lease,
	)
	if err != nil {
		w.warn(ctx, "Claim mailbox egress failed", err)

		return
	}

	for _, row := range rows {
		if err := w.send(ctx, row); err != nil {
			w.warn(
				ctx, "Mailbox egress send failed", err,
				slog.String("id", row.ID),
				slog.String("service", row.Service),
				slog.String("method", row.Method),
			)
			_ = w.cfg.Store.ReleaseEgressForRetry(
				ctx, row.ID, row.ClaimToken,
				retryBackoff(row.Attempts), err,
			)

			continue
		}

		if err := w.cfg.Store.MarkEgressSent(
			ctx, row.ID, row.ClaimToken,
		); err != nil {

			w.warn(
				ctx, "Mark mailbox egress sent failed", err,
				slog.String("id", row.ID),
			)
		}
	}
}

func (w *MailboxEgressWorker) send(ctx context.Context,
	row EgressEnvelope) error {

	var env mailboxpb.Envelope
	if err := proto.Unmarshal(row.Envelope, &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}

	sendCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), defaultSendEventTimeout,
	)
	defer cancel()

	resp, err := w.cfg.Edge.Send(sendCtx, &mailboxpb.SendRequest{
		Envelope: &env,
	})
	if err != nil {
		return err
	}
	if resp.Status != nil && !resp.Status.Ok {
		return &statusError{
			Op:     "Send",
			Status: resp.Status,
		}
	}

	return nil
}

func retryBackoff(attempts int32) time.Duration {
	switch {
	case attempts <= 1:
		return 5 * time.Second

	case attempts == 2:
		return 15 * time.Second

	case attempts == 3:
		return time.Minute

	case attempts == 4:
		return 5 * time.Minute

	case attempts == 5:
		return 30 * time.Minute

	default:
		return time.Hour
	}
}

func (w *MailboxEgressWorker) warn(ctx context.Context, msg string, err error,
	args ...any) {

	w.cfg.Log.WhenSome(func(log btclog.Logger) {
		log.WarnS(ctx, msg, err, args...)
	})
}

// NewTransportTellRef returns a fire-and-forget ref that durably records
// outbound envelopes in mailbox_egress.
func NewTransportTellRef(cfg ConnectorConfig) *TransportTellRef {
	return &TransportTellRef{cfg: cfg}
}

type TransportTellRef struct {
	cfg ConnectorConfig
}

func (r *TransportTellRef) ID() string {
	return RuntimeID(r.cfg.LocalMailboxID)
}

func (r *TransportTellRef) Tell(ctx context.Context, msg ServerConnMsg) error {
	row, err := egressRowFromMessage(ctx, r.cfg, msg)
	if err != nil {
		return err
	}

	return r.cfg.Transport.InsertEgress(ctx, row)
}

func (r *TransportTellRef) Ask(ctx context.Context,
	msg ServerConnMsg) actor.Future[ServerConnResp] {

	promise := actor.NewPromise[ServerConnResp]()
	if err := r.Tell(ctx, msg); err != nil {
		promise.Complete(fn.Err[ServerConnResp](err))

		return promise.Future()
	}

	switch msg.(type) {
	case *SendClientEventRequest:
		promise.Complete(
			fn.Ok[ServerConnResp](
				&SendClientEventResponse{
					Success: true,
				},
			),
		)

	case *SendUnaryRequest, DurableUnaryQuery, *SendRPCRequest:
		promise.Complete(
			fn.Ok[ServerConnResp](
				&SendRPCResponse{
					Success: true,
				},
			),
		)

	default:
		promise.Complete(
			fn.Err[ServerConnResp](
				fmt.Errorf("unknown message type: %T", msg),
			),
		)
	}

	return promise.Future()
}

func egressRowFromMessage(ctx context.Context, cfg ConnectorConfig,
	msg ServerConnMsg) (EgressEnvelope, error) {

	env, err := envelopeFromMessage(ctx, cfg, msg)
	if err != nil {
		return EgressEnvelope{}, err
	}

	raw, err := proto.Marshal(env)
	if err != nil {
		return EgressEnvelope{}, fmt.Errorf("marshal envelope: %w", err)
	}

	rpcKind := "event"
	service, method := "", ""
	correlationID := ""
	replyTo := ""
	if env.Rpc != nil {
		service = env.Rpc.Service
		method = env.Rpc.Method
		correlationID = env.Rpc.CorrelationId
		replyTo = env.Rpc.ReplyTo
		switch env.Rpc.Kind {
		case mailboxpb.RpcMeta_KIND_REQUEST:
			rpcKind = "request"

		case mailboxpb.RpcMeta_KIND_RESPONSE:
			rpcKind = "response"

		case mailboxpb.RpcMeta_KIND_EVENT:
			rpcKind = "event"

		case mailboxpb.RpcMeta_KIND_UNSPECIFIED:
		}
	}

	id := env.IdempotencyKey
	if id == "" {
		id = env.MsgId
	}
	if id == "" {
		id = uuid.NewString()
	}
	rowID := fmt.Sprintf("%s:%s:%s:%s", TransportConnectorServerConn,
		cfg.LocalMailboxID, cfg.RemoteMailboxID, id)

	return EgressEnvelope{
		ID:              rowID,
		Connector:       TransportConnectorServerConn,
		LocalMailboxID:  cfg.LocalMailboxID,
		RemoteMailboxID: cfg.RemoteMailboxID,
		RPCKind:         rpcKind,
		Service:         service,
		Method:          method,
		CorrelationID:   correlationID,
		ReplyTo:         replyTo,
		MsgID:           env.MsgId,
		IdempotencyKey:  env.IdempotencyKey,
		Envelope:        raw,
	}, nil
}

func envelopeFromMessage(ctx context.Context, cfg ConnectorConfig,
	msg ServerConnMsg) (*mailboxpb.Envelope, error) {

	now := time.Now()

	switch m := msg.(type) {
	case *SendClientEventRequest:
		protoMsg, err := m.Message.ToProto().Unpack()
		if err != nil {
			return nil, fmt.Errorf("convert to proto: %w", err)
		}
		body, err := anypb.New(protoMsg)
		if err != nil {
			return nil, fmt.Errorf("wrap proto in Any: %w", err)
		}

		msgID := m.MsgID
		idempotencyKey := m.IdempotencyKey
		if msgID == "" || idempotencyKey == "" {
			bodyBytes, err := (proto.MarshalOptions{
				Deterministic: true,
			}).Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("marshal event body: %w",
					err)
			}
			if msgID == "" {
				msgID = mailboxconn.StableEventMsgID(bodyBytes)
			}
			if idempotencyKey == "" {
				idempotencyKey = mailboxconn.
					StableEventIdempotencyKey(
						bodyBytes,
					)
			}
		}

		service, method := eventRoutingMetadata(m)

		return &mailboxpb.Envelope{
			ProtocolVersion: cfg.ProtocolVersion,
			MsgId:           msgID,
			IdempotencyKey:  idempotencyKey,
			Sender:          cfg.LocalMailboxID,
			Recipient:       cfg.RemoteMailboxID,
			CreatedAtUnixMs: now.UnixMilli(),
			Headers:         cfg.mergeAuthHeaders(nil),
			Body:            body,
			Rpc: &mailboxpb.RpcMeta{
				Kind:    mailboxpb.RpcMeta_KIND_EVENT,
				Service: service,
				Method:  method,
				ReplyTo: cfg.LocalMailboxID,
			},
		}, nil

	case *SendUnaryRequest:
		return envelopeFromUnary(cfg, m, now)

	case DurableUnaryQuery:
		unary, err := buildDurableUnaryMessage(ctx, cfg, m)
		if err != nil {
			return nil, err
		}

		return envelopeFromUnary(cfg, unary, now)

	case *SendRPCRequest:
		if m.Envelope == nil {
			return nil, fmt.Errorf("rpc envelope must be provided")
		}

		return m.Envelope, nil

	default:
		return nil, fmt.Errorf("unknown message type: %T", msg)
	}
}

func envelopeFromUnary(cfg ConnectorConfig, req *SendUnaryRequest,
	now time.Time) (*mailboxpb.Envelope, error) {

	if req == nil || req.Body == nil {
		return nil, fmt.Errorf("unary request body must be provided")
	}
	if req.Service == "" || req.Method == "" {
		return nil, fmt.Errorf("unary request service and method " +
			"must be provided")
	}
	if req.CorrelationID == "" {
		return nil, fmt.Errorf("unary request correlation id must be " +
			"provided")
	}

	msgID := req.MsgID
	idempotencyKey := req.IdempotencyKey
	if msgID == "" || idempotencyKey == "" {
		bodyBytes, err := (proto.MarshalOptions{
			Deterministic: true,
		}).Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("marshal unary body: %w", err)
		}
		if msgID == "" {
			msgID = mailboxconn.StableEventMsgID(bodyBytes)
		}
		if idempotencyKey == "" {
			idempotencyKey = mailboxconn.
				StableEventIdempotencyKey(
					bodyBytes,
				)
		}
	}

	return &mailboxpb.Envelope{
		ProtocolVersion: cfg.ProtocolVersion,
		MsgId:           msgID,
		IdempotencyKey:  idempotencyKey,
		Sender:          cfg.LocalMailboxID,
		Recipient:       cfg.RemoteMailboxID,
		CreatedAtUnixMs: now.UnixMilli(),
		Headers:         cfg.mergeAuthHeaders(nil),
		Body:            req.Body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
			Service:       req.Service,
			Method:        req.Method,
			CorrelationId: req.CorrelationID,
			ReplyTo:       cfg.LocalMailboxID,
		},
	}, nil
}

func buildDurableUnaryMessage(ctx context.Context, cfg ConnectorConfig,
	q DurableUnaryQuery) (*SendUnaryRequest, error) {

	if cfg.DurableUnaryBuilder == nil {
		return nil, fmt.Errorf("durable unary builder must be provided")
	}
	if q.QueryCorrelationID() == "" {
		return nil, fmt.Errorf("durable unary query requires a " +
			"correlation ID")
	}

	body, stableBytes, err := q.BuildBody(ctx, cfg.DurableUnaryBuilder)
	if err != nil {
		return nil, err
	}

	msgID := q.QueryMsgID()
	if msgID == "" {
		msgID = mailboxconn.StableEventMsgID(stableBytes)
	}

	idempotencyKey := q.QueryIdempotencyKey()
	if idempotencyKey == "" {
		idempotencyKey = mailboxconn.StableEventIdempotencyKey(
			stableBytes,
		)
	}

	method := q.ServiceMethod()

	return &SendUnaryRequest{
		Body:           body,
		Service:        method.Service,
		Method:         method.Method,
		CorrelationID:  q.QueryCorrelationID(),
		MsgID:          msgID,
		IdempotencyKey: idempotencyKey,
	}, nil
}
