package swaps

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/serverconn/mailboxpull"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	outSwapMailboxEventType    = "swap.mailbox_event"
	outSwapMailboxEventService = "swaprpc.SwapService"
	outSwapMailboxEventMethod  = "SwapMailboxEvent"
)

// OutSwapMailboxID derives the per-receive mailbox from the client's stable
// identity key and the invoice payment hash. This keeps one key per client
// while avoiding AckUpTo races between concurrent receive sessions.
func OutSwapMailboxID(mailboxPubkey *btcec.PublicKey,
	paymentHash lntypes.Hash) string {

	return serverconn.CompoundMailboxID(
		serverconn.PubKeyMailboxID(mailboxPubkey),
		hex.EncodeToString(paymentHash[:]),
	)
}

// MailboxOutSwapEventReceiver pulls out-swap HTLC events from a mailbox edge.
type MailboxOutSwapEventReceiver struct {
	edge             mailboxpb.MailboxServiceClient
	mailboxID        string
	pullWaitTimeout  time.Duration
	pullMaxEnvelopes uint32

	// pullBackoff controls the exponential backoff schedule applied when
	// edge.Pull returns a transport error. The default schedule matches
	// the serverconn ingress loop so the daemon's reconnect cadence is
	// uniform across both consumers.
	pullBackoff mailboxpull.BackoffConfig

	// log receives one WARN per failed pull attempt. nil is treated as a
	// no-op logger.
	log btclog.Logger
}

// MailboxReceiverOption tweaks a MailboxOutSwapEventReceiver at construction
// time. Defaults match the production wiring; tests use these to drive the
// retry schedule deterministically.
type MailboxReceiverOption func(*MailboxOutSwapEventReceiver)

// WithMailboxPullBackoff overrides the exponential backoff schedule used when
// edge.Pull returns a transport error.
func WithMailboxPullBackoff(
	cfg mailboxpull.BackoffConfig) MailboxReceiverOption {

	return func(r *MailboxOutSwapEventReceiver) {
		r.pullBackoff = cfg
	}
}

// WithMailboxReceiverLog wires a structured logger so retry attempts surface
// in daemon logs alongside the equivalent serverconn ingress messages.
func WithMailboxReceiverLog(log btclog.Logger) MailboxReceiverOption {
	return func(r *MailboxOutSwapEventReceiver) {
		r.log = log
	}
}

// NewMailboxOutSwapEventReceiver creates a mailbox-backed out-swap event
// receiver. When mailboxID is empty, WaitOutSwapHtlc derives the mailbox from
// the client identity key supplied by the caller.
func NewMailboxOutSwapEventReceiver(edge mailboxpb.MailboxServiceClient,
	mailboxID string,
	opts ...MailboxReceiverOption) *MailboxOutSwapEventReceiver {

	r := &MailboxOutSwapEventReceiver{
		edge:             edge,
		mailboxID:        mailboxID,
		pullWaitTimeout:  5 * time.Second,
		pullMaxEnvelopes: 1,
		pullBackoff:      mailboxpull.DefaultBackoffConfig(),
	}
	for _, opt := range opts {
		opt(r)
	}

	return r
}

// WaitOutSwapHtlc waits until the matching out-swap HTLC mailbox event is
// available. The returned acknowledgement must be called only after the caller
// has validated and persisted the event.
func (r *MailboxOutSwapEventReceiver) WaitOutSwapHtlc(ctx context.Context,
	paymentHash lntypes.Hash, mailboxPubkey *btcec.PublicKey) (
	*OutSwapHtlcNotification, error) {

	if r == nil || r.edge == nil {
		return nil, fmt.Errorf("mailbox event receiver not configured")
	}
	if mailboxPubkey == nil {
		return nil, fmt.Errorf("mailbox pubkey must be provided")
	}

	mailboxID := r.mailboxID
	if mailboxID == "" {
		mailboxID = OutSwapMailboxID(mailboxPubkey, paymentHash)
	}

	cursor := uint64(0)
	for {
		req := &mailboxpb.PullRequest{
			MailboxId:     mailboxID,
			MaxEnvelopes:  r.pullMaxEnvelopes,
			WaitTimeoutMs: uint32(r.pullWaitTimeout.Milliseconds()),
			Cursor:        cursor,
		}
		resp, err := mailboxpull.PullWithRetry(
			ctx, r.edge, req, r.pullBackoff, r.log,
		)
		if err != nil {
			return nil, fmt.Errorf("pull out-swap mailbox: %w", err)
		}
		if resp.GetStatus() != nil && !resp.GetStatus().GetOk() {
			return nil, fmt.Errorf("pull out-swap mailbox: %s (%s)",
				resp.GetStatus().GetMessage(),
				resp.GetStatus().GetCode())
		}

		if len(resp.GetEnvelopes()) == 0 {
			cursor = resp.GetNextCursor()
			continue
		}

		for _, env := range resp.GetEnvelopes() {
			event, ok, err := outSwapEventFromMailboxEnvelope(env)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if event.PaymentHash != paymentHash {
				continue
			}
			ackCursor := env.GetEventSeq() + 1

			return &OutSwapHtlcNotification{
				Event:     event,
				AckCursor: ackCursor,
				Ack: func(ctx context.Context) error {
					return r.AckOutSwapHtlc(
						ctx, paymentHash, mailboxPubkey,
						ackCursor,
					)
				},
			}, nil
		}

		cursor = resp.GetNextCursor()
	}
}

// WaitIncomingVHTLC waits until either a Lightning-backed out-swap event or a
// same-Ark vHTLC event is available for the payment hash.
func (r *MailboxOutSwapEventReceiver) WaitIncomingVHTLC(ctx context.Context,
	paymentHash lntypes.Hash, mailboxPubkey *btcec.PublicKey) (
	*IncomingVHTLCNotification, error) {

	if r == nil || r.edge == nil {
		return nil, fmt.Errorf("mailbox event receiver not configured")
	}
	if mailboxPubkey == nil {
		return nil, fmt.Errorf("mailbox pubkey must be provided")
	}

	mailboxID := r.mailboxID
	if mailboxID == "" {
		mailboxID = OutSwapMailboxID(mailboxPubkey, paymentHash)
	}

	cursor := uint64(0)
	for {
		req := &mailboxpb.PullRequest{
			MailboxId:     mailboxID,
			MaxEnvelopes:  r.pullMaxEnvelopes,
			WaitTimeoutMs: uint32(r.pullWaitTimeout.Milliseconds()),
			Cursor:        cursor,
		}
		resp, err := mailboxpull.PullWithRetry(
			ctx, r.edge, req, r.pullBackoff, r.log,
		)
		if err != nil {
			return nil, fmt.Errorf("pull incoming vHTLC "+
				"mailbox: %w", err)
		}
		if resp.GetStatus() != nil && !resp.GetStatus().GetOk() {
			return nil, fmt.Errorf("pull incoming vHTLC mailbox: "+
				"%s (%s)", resp.GetStatus().GetMessage(),
				resp.GetStatus().GetCode())
		}

		if len(resp.GetEnvelopes()) == 0 {
			cursor = resp.GetNextCursor()
			continue
		}

		for _, env := range resp.GetEnvelopes() {
			event, ok, err := incomingEventFromMailboxEnvelope(env)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}

			eventHash := lntypes.Hash{}
			switch {
			case event.OutSwap != nil:
				eventHash = event.OutSwap.PaymentHash

			case event.InArk != nil:
				eventHash = event.InArk.PaymentHash
			}
			if eventHash != paymentHash {
				continue
			}

			ackCursor := env.GetEventSeq() + 1
			event.AckCursor = ackCursor
			event.Ack = func(ctx context.Context) error {
				return r.ack(ctx, mailboxID, ackCursor)
			}

			return event, nil
		}

		cursor = resp.GetNextCursor()
	}
}

// WaitOutSwapForfeitSignature waits until the matching out-swap vHTLC refresh
// signature request is available. The returned acknowledgement must be called
// only after the caller has validated and durably handled the request.
func (r *MailboxOutSwapEventReceiver) WaitOutSwapForfeitSignature(
	ctx context.Context, paymentHash lntypes.Hash,
	mailboxPubkey *btcec.PublicKey) (*OutSwapForfeitSignatureNotification,
	error) {

	if r == nil || r.edge == nil {
		return nil, fmt.Errorf("mailbox event receiver not configured")
	}
	if mailboxPubkey == nil {
		return nil, fmt.Errorf("mailbox pubkey must be provided")
	}

	mailboxID := r.mailboxID
	if mailboxID == "" {
		mailboxID = OutSwapMailboxID(mailboxPubkey, paymentHash)
	}

	cursor := uint64(0)
	for {
		req := &mailboxpb.PullRequest{
			MailboxId:     mailboxID,
			MaxEnvelopes:  r.pullMaxEnvelopes,
			WaitTimeoutMs: uint32(r.pullWaitTimeout.Milliseconds()),
			Cursor:        cursor,
		}
		resp, err := mailboxpull.PullWithRetry(
			ctx, r.edge, req, r.pullBackoff, r.log,
		)
		if err != nil {
			return nil, fmt.Errorf("pull out-swap forfeit "+
				"mailbox: %w", err)
		}
		if resp.GetStatus() != nil && !resp.GetStatus().GetOk() {
			return nil, fmt.Errorf("pull out-swap forfeit "+
				"mailbox: %s (%s)",
				resp.GetStatus().GetMessage(),
				resp.GetStatus().GetCode())
		}

		if len(resp.GetEnvelopes()) == 0 {
			cursor = resp.GetNextCursor()
			continue
		}

		for _, env := range resp.GetEnvelopes() {
			payload, ok, err := outSwapForfeitSignatureFromMailbox(
				env,
			)
			if err != nil {
				return nil, err
			}
			if !ok || payload.PaymentHash != paymentHash {
				continue
			}

			ackCursor := env.GetEventSeq() + 1

			return &OutSwapForfeitSignatureNotification{
				Payload:   payload,
				AckCursor: ackCursor,
				Ack: func(ctx context.Context) error {
					return r.ack(ctx, mailboxID, ackCursor)
				},
			}, nil
		}

		cursor = resp.GetNextCursor()
	}
}

// AckOutSwapHtlc advances the remote mailbox cursor after the caller has
// durably accepted the matching notification.
func (r *MailboxOutSwapEventReceiver) AckOutSwapHtlc(ctx context.Context,
	paymentHash lntypes.Hash, mailboxPubkey *btcec.PublicKey,
	cursor uint64) error {

	if r == nil || r.edge == nil {
		return fmt.Errorf("mailbox event receiver not configured")
	}
	if mailboxPubkey == nil {
		return fmt.Errorf("mailbox pubkey must be provided")
	}

	mailboxID := r.mailboxID
	if mailboxID == "" {
		mailboxID = OutSwapMailboxID(mailboxPubkey, paymentHash)
	}

	return r.ack(ctx, mailboxID, cursor)
}

// ack advances the remote mailbox cursor after the caller has durably accepted
// the matching notification.
func (r *MailboxOutSwapEventReceiver) ack(ctx context.Context, mailboxID string,
	cursor uint64) error {

	resp, err := r.edge.AckUpTo(ctx, &mailboxpb.AckUpToRequest{
		MailboxId: mailboxID,
		Cursor:    cursor,
	})
	if err != nil {
		return err
	}
	if resp.GetStatus() != nil && !resp.GetStatus().GetOk() {
		return fmt.Errorf("ack out-swap mailbox: %s (%s)",
			resp.GetStatus().GetMessage(),
			resp.GetStatus().GetCode())
	}

	return nil
}

// outSwapForfeitSignatureFromMailbox unwraps one mailbox event envelope when it
// matches the out-swap forfeit signature request route.
func outSwapForfeitSignatureFromMailbox(env *mailboxpb.Envelope) (
	*ForfeitSignaturePayload, bool, error) {

	wrapped, ok, err := swapMailboxEventFromEnvelope(env)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}

	req := wrapped.GetOutSwapForfeitSignatureRequest()
	if req == nil {
		return nil, false, nil
	}

	payload, err := forfeitSignaturePayloadFromProto(req.GetPayload())
	if err != nil {
		return nil, false, err
	}

	return payload, true, nil
}

// outSwapEventFromMailboxEnvelope unwraps one mailbox event envelope when it
// matches the out-swap HTLC event route.
func outSwapEventFromMailboxEnvelope(env *mailboxpb.Envelope) (
	*OutSwapHtlcEvent, bool, error) {

	wrapped, ok, err := swapMailboxEventFromEnvelope(env)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}

	event := wrapped.GetOutSwapHtlc()
	if event == nil {
		return nil, false, nil
	}

	local, err := outSwapHtlcEventFromProto(event)
	if err != nil {
		return nil, false, err
	}

	return local, true, nil
}

// incomingEventFromMailboxEnvelope unwraps either incoming vHTLC event type.
func incomingEventFromMailboxEnvelope(env *mailboxpb.Envelope) (
	*IncomingVHTLCNotification, bool, error) {

	wrapped, ok, err := swapMailboxEventFromEnvelope(env)
	if err != nil || !ok {
		return nil, ok, err
	}

	if outSwap := wrapped.GetOutSwapHtlc(); outSwap != nil {
		local, err := outSwapHtlcEventFromProto(outSwap)
		if err != nil {
			return nil, false, err
		}

		return &IncomingVHTLCNotification{
			OutSwap: local,
		}, true, nil
	}

	if inArk := wrapped.GetInArkHtlc(); inArk != nil {
		local, err := inArkHtlcEventFromProto(inArk)
		if err != nil {
			return nil, false, err
		}

		return &IncomingVHTLCNotification{
			InArk: local,
		}, true, nil
	}

	return nil, false, nil
}

// swapMailboxEventFromEnvelope unwraps one shared swap mailbox event envelope.
func swapMailboxEventFromEnvelope(env *mailboxpb.Envelope) (
	*swaprpc.SwapMailboxEvent, bool, error) {

	if env == nil || env.GetRpc() == nil {
		return nil, false, nil
	}
	rpc := env.GetRpc()
	if rpc.GetKind() != mailboxpb.RpcMeta_KIND_EVENT ||
		rpc.GetService() != outSwapMailboxEventService ||
		rpc.GetMethod() != outSwapMailboxEventMethod ||
		env.GetType() != outSwapMailboxEventType {
		return nil, false, nil
	}

	body := env.GetBody()
	if body == nil {
		return nil, false, fmt.Errorf("swap mailbox event missing body")
	}

	msg, err := body.UnmarshalNew()
	if err != nil {
		return nil, false, fmt.Errorf("unmarshal swap mailbox event "+
			"body: %w", err)
	}

	wrapped, ok := msg.(*swaprpc.SwapMailboxEvent)
	if !ok {
		return nil, false, fmt.Errorf("unexpected swap mailbox event "+
			"body type %T", msg)
	}

	return wrapped, true, nil
}

var _ OutSwapEventReceiver = (*MailboxOutSwapEventReceiver)(nil)

var _ OutSwapForfeitSignatureReceiver = (*MailboxOutSwapEventReceiver)(nil)

var _ IncomingVHTLCEventReceiver = (*MailboxOutSwapEventReceiver)(nil)
