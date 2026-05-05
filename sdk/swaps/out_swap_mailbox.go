package swaps

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	outSwapHtlcEventType    = "swap.out_htlc"
	outSwapHtlcEventService = "swaprpc.SwapService"
	outSwapHtlcEventMethod  = "OutSwapHtlcEvent"
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
}

// NewMailboxOutSwapEventReceiver creates a mailbox-backed out-swap event
// receiver. When mailboxID is empty, WaitOutSwapHtlc derives the mailbox from
// the client identity key supplied by the caller.
func NewMailboxOutSwapEventReceiver(edge mailboxpb.MailboxServiceClient,
	mailboxID string) *MailboxOutSwapEventReceiver {

	return &MailboxOutSwapEventReceiver{
		edge:             edge,
		mailboxID:        mailboxID,
		pullWaitTimeout:  5 * time.Second,
		pullMaxEnvelopes: 1,
	}
}

// WaitOutSwapHtlc waits until the matching out-swap HTLC mailbox event is
// available. The returned acknowledgement must be called only after the caller
// has validated and persisted the event.
func (r *MailboxOutSwapEventReceiver) WaitOutSwapHtlc(ctx context.Context,
	paymentHash lntypes.Hash,
	mailboxPubkey *btcec.PublicKey) (*OutSwapHtlcNotification, error) {

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
		resp, err := r.edge.Pull(ctx, &mailboxpb.PullRequest{
			MailboxId:     mailboxID,
			MaxEnvelopes:  r.pullMaxEnvelopes,
			WaitTimeoutMs: uint32(r.pullWaitTimeout.Milliseconds()),
			Cursor:        cursor,
		})
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
				Event: event,
				Ack: func(ctx context.Context) error {
					return r.ack(ctx, mailboxID, ackCursor)
				},
			}, nil
		}

		cursor = resp.GetNextCursor()
	}
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

// outSwapEventFromMailboxEnvelope unwraps one mailbox event envelope when it
// matches the out-swap HTLC event route.
func outSwapEventFromMailboxEnvelope(
	env *mailboxpb.Envelope) (*OutSwapHtlcEvent, bool, error) {

	if env == nil || env.GetRpc() == nil {
		return nil, false, nil
	}
	rpc := env.GetRpc()
	if rpc.GetKind() != mailboxpb.RpcMeta_KIND_EVENT ||
		rpc.GetService() != outSwapHtlcEventService ||
		rpc.GetMethod() != outSwapHtlcEventMethod ||
		env.GetType() != outSwapHtlcEventType {

		return nil, false, nil
	}

	body := env.GetBody()
	if body == nil {
		return nil, false, fmt.Errorf("out-swap event missing body")
	}

	var protoEvent anypb.Any
	protoEvent = *body

	msg, err := protoEvent.UnmarshalNew()
	if err != nil {
		return nil, false, fmt.Errorf(
			"unmarshal out-swap event body: %w", err,
		)
	}

	event, ok := msg.(*swaprpc.OutSwapHtlcEvent)
	if !ok {
		return nil, false, fmt.Errorf(
			"unexpected out-swap event body type %T", msg,
		)
	}

	local, err := outSwapHtlcEventFromProto(event)
	if err != nil {
		return nil, false, err
	}

	return local, true, nil
}

var _ OutSwapEventReceiver = (*MailboxOutSwapEventReceiver)(nil)
