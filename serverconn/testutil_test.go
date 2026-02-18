package serverconn

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// inMemoryMailbox is a minimal in-memory implementation of the MailboxService
// semantics needed for connector unit tests.
type inMemoryMailbox struct {
	mu sync.Mutex

	nextSeq uint64

	// mailboxes stores all envelopes by mailbox id.
	mailboxes map[string][]*mailboxpb.Envelope

	// ackedUpTo stores the ack watermark (event_seq < ackedUpTo are
	// acked).
	ackedUpTo map[string]uint64

	notify chan struct{}
}

// newInMemoryMailbox constructs an empty mailbox edge.
func newInMemoryMailbox() *inMemoryMailbox {
	return &inMemoryMailbox{
		nextSeq:   1,
		mailboxes: make(map[string][]*mailboxpb.Envelope),
		ackedUpTo: make(map[string]uint64),
		notify:    make(chan struct{}),
	}
}

// send enqueues envelope into recipient mailbox and assigns an event_seq.
func (m *inMemoryMailbox) send(
	envelope *mailboxpb.Envelope,
) *mailboxpb.Status {

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
	wait time.Duration) ([]*mailboxpb.Envelope, uint64,
	*mailboxpb.Status) {

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

// getAckedUpTo returns the current ack watermark for a mailbox.
func (m *inMemoryMailbox) getAckedUpTo(mailboxID string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.ackedUpTo[mailboxID]
}

// cloneEnvelope makes a deep copy of env and returns false if proto.Clone does
// not return the expected type.
func cloneEnvelope(
	env *mailboxpb.Envelope,
) (*mailboxpb.Envelope, bool) {

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

// fakeMailboxServiceClient adapts the in-memory edge to
// MailboxServiceClient.
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
func (c *fakeMailboxServiceClient) AckUpTo(
	ctx context.Context,
	in *mailboxpb.AckUpToRequest,
	_ ...grpc.CallOption,
) (*mailboxpb.AckUpToResponse, error) {

	_ = ctx

	if in == nil {
		return nil, fmt.Errorf("nil request")
	}

	status := c.mb.ackUpTo(in.MailboxId, in.Cursor)

	return &mailboxpb.AckUpToResponse{Status: status}, nil
}

// memCheckpointStore is a minimal in-memory implementation of the checkpoint
// subset of actor.DeliveryStore needed for ingress loop tests. Only
// SaveCheckpoint and LoadCheckpoint are implemented; all other methods panic.
type memCheckpointStore struct {
	mu          sync.Mutex
	checkpoints map[string]*actor.Checkpoint
}

// newMemCheckpointStore creates a new empty checkpoint store.
func newMemCheckpointStore() *memCheckpointStore {
	return &memCheckpointStore{
		checkpoints: make(map[string]*actor.Checkpoint),
	}
}

// SaveCheckpoint persists a checkpoint in memory.
func (s *memCheckpointStore) SaveCheckpoint(
	ctx context.Context, params actor.CheckpointParams,
) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.checkpoints[params.ActorID] = &actor.Checkpoint{
		ActorID:   params.ActorID,
		StateType: params.StateType,
		StateData: params.StateData,
		Version:   params.Version,
		UpdatedAt: time.Now(),
	}

	return nil
}

// LoadCheckpoint retrieves a previously saved checkpoint.
func (s *memCheckpointStore) LoadCheckpoint(
	ctx context.Context, actorID string,
) (*actor.Checkpoint, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	cp, ok := s.checkpoints[actorID]
	if !ok {
		return nil, nil
	}

	return cp, nil
}

// The remaining DeliveryStore methods are unused in connector tests and panic
// if called.

func (s *memCheckpointStore) EnqueueMessage(
	ctx context.Context, params actor.EnqueueParams,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) LeaseNextMessage(
	ctx context.Context, mailboxID string, leaseToken string,
	leaseDuration time.Duration,
) (*actor.LeasedMessage, error) {

	panic("not implemented")
}

func (s *memCheckpointStore) AckMessage(
	ctx context.Context, id, leaseToken string,
) (int64, error) {

	panic("not implemented")
}

func (s *memCheckpointStore) NackMessage(
	ctx context.Context, id, leaseToken string,
	retryAfter time.Duration,
) (int64, error) {

	panic("not implemented")
}

func (s *memCheckpointStore) ExtendLease(
	ctx context.Context, id, leaseToken string,
	extension time.Duration,
) (int64, error) {

	panic("not implemented")
}

func (s *memCheckpointStore) MoveToDeadLetter(
	ctx context.Context, id, reason string,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) DeleteMessage(
	ctx context.Context, id string,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) SaveAskResult(
	ctx context.Context, params actor.AskResultParams,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) GetAskResult(
	ctx context.Context, promiseID string,
) (*actor.AskResult, error) {

	panic("not implemented")
}

func (s *memCheckpointStore) DeleteAskResult(
	ctx context.Context, promiseID string,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) EnqueueOutbox(
	ctx context.Context, params actor.OutboxParams,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) ClaimOutboxBatch(
	ctx context.Context, params actor.OutboxClaimParams,
) ([]actor.OutboxMessage, error) {

	panic("not implemented")
}

func (s *memCheckpointStore) CompleteOutbox(
	ctx context.Context, id, claimToken string,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) FailOutbox(
	ctx context.Context, id, claimToken string,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) IsProcessed(
	ctx context.Context, id string,
) (bool, error) {

	panic("not implemented")
}

func (s *memCheckpointStore) MarkProcessed(
	ctx context.Context, id, actorID string,
	ttl time.Duration,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) DeleteCheckpoint(
	ctx context.Context, actorID string,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) GetDeadLetter(
	ctx context.Context, id string,
) (*actor.DeadLetter, error) {

	panic("not implemented")
}

func (s *memCheckpointStore) ListDeadLetters(
	ctx context.Context, actorID string, limit int,
) ([]actor.DeadLetter, error) {

	panic("not implemented")
}

func (s *memCheckpointStore) DeleteDeadLetter(
	ctx context.Context, id string,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) ExpireLeases(
	ctx context.Context,
) error {

	panic("not implemented")
}

func (s *memCheckpointStore) CleanupExpired(
	ctx context.Context,
) error {

	panic("not implemented")
}

// Compile-time check.
var _ actor.DeliveryStore = (*memCheckpointStore)(nil)
