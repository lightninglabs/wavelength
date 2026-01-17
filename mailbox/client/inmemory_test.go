package mailboxclient_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// inMemoryMailbox is a minimal in-memory implementation of the MailboxService
// semantics needed for unit tests.
type inMemoryMailbox struct {
	mu sync.Mutex

	nextSeq uint64

	// mailboxes stores all envelopes by mailbox id.
	mailboxes map[string][]*mailboxpb.Envelope

	// ackedUpTo stores the ack watermark (event_seq < ackedUpTo are acked).
	ackedUpTo map[string]uint64

	notify chan struct{}
}

// newInMemoryMailbox constructs an empty mailbox edge.
func newInMemoryMailbox() *inMemoryMailbox {
	mb := &inMemoryMailbox{
		nextSeq:   1,
		mailboxes: make(map[string][]*mailboxpb.Envelope),
		ackedUpTo: make(map[string]uint64),
		notify:    make(chan struct{}),
	}

	return mb
}

// send enqueues envelope into recipient mailbox and assigns an event_seq.
func (m *inMemoryMailbox) send(envelope *mailboxpb.Envelope) *mailboxpb.Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	if envelope == nil {
		return &mailboxpb.Status{
			Ok:      false,
			Code:    "INVALID_ARGUMENT",
			Message: "missing envelope",
		}
	}

	envCopy, ok := cloneEnvelope(envelope)
	if !ok {
		return &mailboxpb.Status{
			Ok:      false,
			Code:    "INVALID_ARGUMENT",
			Message: "unexpected envelope type",
		}
	}
	envCopy.EventSeq = m.nextSeq
	m.nextSeq++

	recipient := envCopy.Recipient
	m.mailboxes[recipient] = append(m.mailboxes[recipient], envCopy)

	close(m.notify)
	m.notify = make(chan struct{})

	return okStatus()
}

// pull returns envelopes with event_seq >= cursor and not acked.
func (m *inMemoryMailbox) pull(ctx context.Context, mailboxID string,
	cursor uint64, maxEnvelopes uint32,
	wait time.Duration) ([]*mailboxpb.Envelope, uint64, *mailboxpb.Status) {

	deadline := time.Now().Add(wait)

	for {
		m.mu.Lock()
		envs, next := m.pullLocked(mailboxID, cursor, maxEnvelopes)
		if len(envs) > 0 || wait == 0 {
			m.mu.Unlock()
			return envs, next, okStatus()
		}

		notify := m.notify
		m.mu.Unlock()

		now := time.Now()
		if !now.Before(deadline) {
			return nil, cursor, okStatus()
		}

		remaining := deadline.Sub(now)
		timer := time.NewTimer(remaining)
		select {
		case <-notify:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, cursor, okStatus()
		}
		timer.Stop()
	}
}

// pullLocked assumes m.mu is held.
func (m *inMemoryMailbox) pullLocked(mailboxID string, cursor uint64,
	maxEnvelopes uint32) ([]*mailboxpb.Envelope, uint64) {

	acked := m.ackedUpTo[mailboxID]

	var result []*mailboxpb.Envelope
	var maxSeq uint64

	for _, env := range m.mailboxes[mailboxID] {
		if env.EventSeq < acked {
			continue
		}
		if env.EventSeq < cursor {
			continue
		}

		clone, ok := cloneEnvelope(env)
		if !ok {
			continue
		}

		result = append(result, clone)
		if env.EventSeq > maxSeq {
			maxSeq = env.EventSeq
		}

		if uint32(len(result)) >= maxEnvelopes {
			break
		}
	}

	if len(result) == 0 {
		return nil, cursor
	}

	return result, maxSeq + 1
}

// ackUpTo advances the ack cursor.
func (m *inMemoryMailbox) ackUpTo(mailboxID string,
	cursor uint64) *mailboxpb.Status {

	m.mu.Lock()
	defer m.mu.Unlock()

	if cursor > m.ackedUpTo[mailboxID] {
		m.ackedUpTo[mailboxID] = cursor
	}

	return okStatus()
}

// cloneEnvelope makes a deep copy of env and returns false if proto.Clone does
// not return the expected type.
func cloneEnvelope(env *mailboxpb.Envelope) (*mailboxpb.Envelope, bool) {
	clone := proto.Clone(env)
	typed, ok := clone.(*mailboxpb.Envelope)
	if !ok {
		return nil, false
	}

	return typed, true
}

// okStatus returns a successful mailbox status.
func okStatus() *mailboxpb.Status {
	return &mailboxpb.Status{Ok: true}
}

// fakeMailboxServiceClient adapts the in-memory edge to MailboxServiceClient.
type fakeMailboxServiceClient struct {
	mb *inMemoryMailbox
}

// Send implements mailboxpb.MailboxServiceClient.
func (c *fakeMailboxServiceClient) Send(
	ctx context.Context,
	in *mailboxpb.SendRequest,
	_ ...grpc.CallOption,
) (*mailboxpb.SendResponse, error) {

	_ = ctx

	if in == nil {
		return nil, fmt.Errorf("nil request")
	}

	status := c.mb.send(in.Envelope)

	return &mailboxpb.SendResponse{Status: status}, nil
}

// Pull implements mailboxpb.MailboxServiceClient.
func (c *fakeMailboxServiceClient) Pull(
	ctx context.Context,
	in *mailboxpb.PullRequest,
	_ ...grpc.CallOption,
) (*mailboxpb.PullResponse, error) {

	if in == nil {
		return nil, fmt.Errorf("nil request")
	}

	wait := time.Duration(in.WaitTimeoutMs) * time.Millisecond

	envs, next, status := c.mb.pull(
		ctx, in.MailboxId, in.Cursor, in.MaxEnvelopes, wait,
	)

	return &mailboxpb.PullResponse{
		Status:     status,
		Envelopes:  envs,
		NextCursor: next,
	}, nil
}

// AckUpTo implements mailboxpb.MailboxServiceClient.
func (c *fakeMailboxServiceClient) AckUpTo(ctx context.Context,
	in *mailboxpb.AckUpToRequest, _ ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	_ = ctx

	if in == nil {
		return nil, fmt.Errorf("nil request")
	}

	status := c.mb.ackUpTo(in.MailboxId, in.Cursor)

	return &mailboxpb.AckUpToResponse{Status: status}, nil
}
