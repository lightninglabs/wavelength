package mailboxclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Client implements mailboxrpc.RPCClient by sending and receiving mailbox
// envelopes through a mailboxpb.MailboxServiceClient.
type Client struct {
	cfg Config

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex

	cursor uint64

	pending map[string][]byte
	waiters map[string][]chan struct{}
}

// New constructs and starts a mailboxclient.Client.
func New(cfg Config) (*Client, error) {
	cfg = applyDefaults(cfg)

	if cfg.Edge == nil {
		return nil, fmt.Errorf("edge is required")
	}
	if cfg.LocalMailboxID == "" {
		return nil, fmt.Errorf("local mailbox id is required")
	}
	if cfg.RemoteMailboxID == "" {
		return nil, fmt.Errorf("remote mailbox id is required")
	}
	if cfg.PullMaxEnvelopes == 0 {
		return nil, fmt.Errorf("pull max envelopes must be > 0")
	}
	if cfg.PullWaitTimeout <= 0 {
		return nil, fmt.Errorf("pull wait timeout must be > 0")
	}

	ctx, cancel := context.WithCancel(context.Background())

	c := &Client{
		cfg: cfg,

		cancel: cancel,

		pending: make(map[string][]byte),
		waiters: make(map[string][]chan struct{}),
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.run(ctx)
	}()

	log.InfoS(ctx, "Mailbox client started",
		slog.String("local_mailbox", cfg.LocalMailboxID),
		slog.String("remote_mailbox", cfg.RemoteMailboxID))

	return c, nil
}

// Stop shuts down background polling and unblocks any waiters.
func (c *Client) Stop() {
	log.InfoS(context.TODO(), "Stopping mailbox client")

	c.cancel()
	c.wg.Wait()

	c.mu.Lock()
	defer c.mu.Unlock()

	for correlationID, waiters := range c.waiters {
		for _, ch := range waiters {
			close(ch)
		}

		delete(c.waiters, correlationID)
	}

	log.InfoS(context.TODO(), "Mailbox client stopped")
}

// SendRPC sends a request payload and returns a SendResult containing the
// correlation id and idempotency key used for the send.
func (c *Client) SendRPC(ctx context.Context, method mailboxrpc.ServiceMethod,
	req proto.Message,
	opts mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	msgID, err := randomID(16)
	if err != nil {
		return mailboxrpc.SendResult{}, err
	}

	idempotencyKey := opts.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey, err = randomID(16)
		if err != nil {
			return mailboxrpc.SendResult{}, err
		}
	}

	correlationID := opts.CorrelationID
	if correlationID == "" {
		correlationID = idempotencyKey
	}

	body, err := anypb.New(req)
	if err != nil {
		return mailboxrpc.SendResult{}, err
	}

	envelope := &mailboxpb.Envelope{
		ProtocolVersion: c.cfg.ProtocolVersion,
		MsgId:           msgID,
		IdempotencyKey:  idempotencyKey,
		Sender:          c.cfg.LocalMailboxID,
		Recipient:       c.cfg.RemoteMailboxID,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Headers:         opts.Headers,
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
			Service:       method.Service,
			Method:        method.Method,
			CorrelationId: correlationID,
			ReplyTo:       c.cfg.LocalMailboxID,
		},
	}

	resp, err := c.cfg.Edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: envelope,
	})
	if err != nil {
		log.WarnS(ctx, "Send failed", err,
			slog.String("service", method.Service),
			slog.String("method", method.Method))

		return mailboxrpc.SendResult{}, err
	}

	if !statusOK(resp.Status) {
		sendErr := statusError("Send", resp.Status)
		log.WarnS(ctx, "Send returned non-OK status", sendErr,
			slog.String("service", method.Service),
			slog.String("method", method.Method))

		return mailboxrpc.SendResult{}, sendErr
	}

	log.DebugS(ctx, "Sent RPC request",
		slog.String("service", method.Service),
		slog.String("method", method.Method),
		slog.String("correlation_id", correlationID))

	return mailboxrpc.SendResult{
		CorrelationID:  correlationID,
		IdempotencyKey: idempotencyKey,
	}, nil
}

// AwaitRPC blocks until a response for correlationID is received.
func (c *Client) AwaitRPC(ctx context.Context, correlationID string,
	resp proto.Message) error {

	for {
		data, ok := c.popPending(correlationID)
		if ok {
			return (proto.UnmarshalOptions{
				DiscardUnknown: true,
			}).Unmarshal(data, resp)
		}

		ch := c.addWaiter(correlationID)
		select {
		case <-ch:
		case <-ctx.Done():
			c.removeWaiter(correlationID, ch)
			return ctx.Err()
		}
	}
}

// applyDefaults fills unset fields with defaults.
func applyDefaults(cfg Config) Config {
	def := DefaultConfig()

	if cfg.PullMaxEnvelopes == 0 {
		cfg.PullMaxEnvelopes = def.PullMaxEnvelopes
	}
	if cfg.PullWaitTimeout == 0 {
		cfg.PullWaitTimeout = def.PullWaitTimeout
	}

	return cfg
}

// run polls Pull and acks envelopes after caching correlated responses.
func (c *Client) run(ctx context.Context) {
	log.DebugS(ctx, "Poll loop starting",
		slog.String("mailbox_id", c.cfg.LocalMailboxID))

	for {
		select {
		case <-ctx.Done():
			log.DebugS(ctx, "Poll loop exiting")
			return

		default:
		}

		cursor := c.loadCursor()
		waitMs := uint32(c.cfg.PullWaitTimeout.Milliseconds())

		resp, err := c.cfg.Edge.Pull(ctx, &mailboxpb.PullRequest{
			MailboxId:     c.cfg.LocalMailboxID,
			MaxEnvelopes:  c.cfg.PullMaxEnvelopes,
			WaitTimeoutMs: waitMs,
			Cursor:        cursor,
		})
		if err != nil {
			log.WarnS(ctx, "Pull failed, retrying", err)
			c.sleepRetry(ctx)
			continue
		}

		if !statusOK(resp.Status) {
			log.DebugS(ctx, "Pull returned non-OK status")
			c.sleepRetry(ctx)
			continue
		}

		if len(resp.Envelopes) > 0 {
			log.DebugS(ctx, "Pulled envelopes",
				slog.Int("count", len(resp.Envelopes)),
				slog.Uint64("cursor", cursor),
				slog.Uint64("next_cursor", resp.NextCursor))
		}

		for _, env := range resp.Envelopes {
			c.handleEnvelope(env)
		}

		if resp.NextCursor > cursor {
			ackOK := c.ackUpTo(ctx, resp.NextCursor)
			if ackOK {
				c.storeCursor(resp.NextCursor)
				continue
			}

			log.DebugS(ctx, "AckUpTo failed, retrying",
				slog.Uint64("cursor", resp.NextCursor))

			c.sleepRetry(ctx)
		}
	}
}

// sleepRetry backs off briefly after a transient pull/ack failure.
func (c *Client) sleepRetry(ctx context.Context) {
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// ackUpTo calls AckUpTo and returns true on success.
func (c *Client) ackUpTo(ctx context.Context, cursor uint64) bool {
	resp, err := c.cfg.Edge.AckUpTo(ctx, &mailboxpb.AckUpToRequest{
		MailboxId: c.cfg.LocalMailboxID,
		Cursor:    cursor,
	})
	if err != nil {
		return false
	}

	return statusOK(resp.Status)
}

// handleEnvelope caches correlated responses and wakes waiters.
func (c *Client) handleEnvelope(env *mailboxpb.Envelope) {
	if env == nil || env.Rpc == nil {
		return
	}

	if env.Rpc.Kind != mailboxpb.RpcMeta_KIND_RESPONSE {
		return
	}

	correlationID := env.Rpc.CorrelationId
	if correlationID == "" {
		return
	}

	if env.Body == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// If we already have a response for this correlation id, keep the
	// first response and ignore duplicates.
	if _, exists := c.pending[correlationID]; !exists {
		payload := env.Body.Value
		payloadCopy := make([]byte, len(payload))
		copy(payloadCopy, payload)

		c.pending[correlationID] = payloadCopy

		log.DebugS(context.TODO(), "Cached response",
			slog.String("correlation_id", correlationID),
			slog.Int("payload_bytes", len(payloadCopy)))
	} else {
		log.DebugS(context.TODO(), "Ignored duplicate response",
			slog.String("correlation_id", correlationID))
	}

	waiters := c.waiters[correlationID]
	for _, ch := range waiters {
		close(ch)
	}
	delete(c.waiters, correlationID)
}

// popPending returns and removes a cached response for correlationID.
func (c *Client) popPending(correlationID string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, ok := c.pending[correlationID]
	if !ok {
		return nil, false
	}

	delete(c.pending, correlationID)

	return data, true
}

// addWaiter registers a waiter for correlationID and returns its channel.
func (c *Client) addWaiter(correlationID string) chan struct{} {
	ch := make(chan struct{})

	c.mu.Lock()
	defer c.mu.Unlock()

	c.waiters[correlationID] = append(c.waiters[correlationID], ch)

	return ch
}

// removeWaiter removes a previously registered waiter channel.
func (c *Client) removeWaiter(correlationID string, ch chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	waiters := c.waiters[correlationID]
	for i := range waiters {
		if waiters[i] == ch {
			waiters[i] = waiters[len(waiters)-1]
			waiters = waiters[:len(waiters)-1]
			break
		}
	}

	if len(waiters) == 0 {
		delete(c.waiters, correlationID)
		return
	}

	c.waiters[correlationID] = waiters
}

// loadCursor returns the current pull cursor.
func (c *Client) loadCursor() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.cursor
}

// storeCursor sets the pull cursor.
func (c *Client) storeCursor(cursor uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cursor = cursor
}

// randomID generates an opaque id backed by crypto/rand.
func randomID(nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf), nil
}

var _ mailboxrpc.RPCClient = (*Client)(nil)
