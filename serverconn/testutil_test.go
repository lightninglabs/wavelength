package serverconn

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
	cursor uint64, maxEnvelopes uint32, wait time.Duration) (
	[]*mailboxpb.Envelope, uint64, *mailboxpb.Status) {

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

// fakeMailboxServiceClient adapts the in-memory edge to
// MailboxServiceClient.
type fakeMailboxServiceClient struct {
	mb *inMemoryMailbox
}

// Send implements mailboxpb.MailboxServiceClient.
func (c *fakeMailboxServiceClient) Send(ctx context.Context,
	in *mailboxpb.SendRequest, _ ...grpc.CallOption) (
	*mailboxpb.SendResponse, error) {

	_ = ctx

	if in == nil {
		return nil, fmt.Errorf("nil request")
	}

	status := c.mb.send(in.Envelope)

	return &mailboxpb.SendResponse{Status: status}, nil
}

// Pull implements mailboxpb.MailboxServiceClient.
func (c *fakeMailboxServiceClient) Pull(ctx context.Context,
	in *mailboxpb.PullRequest, _ ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

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

type memCheckpointStore struct{}

func newMemCheckpointStore() *memCheckpointStore {
	return &memCheckpointStore{}
}

type memTransportStore struct {
	mu sync.Mutex

	cursors map[string]AckState
	egress  map[string]*memEgressRow
	order   []string
	next    int
}

type memEgressRow struct {
	env         EgressEnvelope
	status      string
	claimToken  string
	claimUntil  time.Time
	nextAttempt time.Time
}

func newMemTransportStore() *memTransportStore {
	return &memTransportStore{
		cursors: make(map[string]AckState),
		egress:  make(map[string]*memEgressRow),
	}
}

func cursorKey(localMailboxID, remoteMailboxID string) string {
	return localMailboxID + "\x00" + remoteMailboxID
}

func (s *memTransportStore) LoadIngressCursor(_ context.Context, localMailboxID,
	remoteMailboxID string) (AckState, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cursors[cursorKey(localMailboxID, remoteMailboxID)], nil
}

func (s *memTransportStore) SaveIngressCursor(_ context.Context, localMailboxID,
	remoteMailboxID string, state AckState) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cursors[cursorKey(localMailboxID, remoteMailboxID)] = state

	return nil
}

func (s *memTransportStore) RunInIngressTx(ctx context.Context,
	fn func(context.Context) error) error {

	return fn(ctx)
}

func (s *memTransportStore) InsertEgress(_ context.Context,
	env EgressEnvelope) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	if env.ID == "" {
		env.ID = env.IdempotencyKey
	}
	if env.ID == "" {
		env.ID = env.MsgID
	}
	if env.ID == "" {
		s.next++
		env.ID = fmt.Sprintf("egress-%d", s.next)
	}

	if _, ok := s.egress[env.ID]; ok {
		return nil
	}

	env.Envelope = append([]byte(nil), env.Envelope...)
	s.egress[env.ID] = &memEgressRow{
		env:         env,
		status:      "pending",
		nextAttempt: time.Now(),
	}
	s.order = append(s.order, env.ID)

	return nil
}

func (s *memTransportStore) ClaimDueEgress(_ context.Context, owner string,
	limit int, lease time.Duration) ([]EgressEnvelope, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	claimed := make([]EgressEnvelope, 0, limit)

	for _, id := range s.order {
		if len(claimed) >= limit {
			break
		}

		row := s.egress[id]
		if row == nil || row.status != "pending" {
			continue
		}
		if row.nextAttempt.After(now) {
			continue
		}
		if row.claimToken != "" && row.claimUntil.After(now) {
			continue
		}

		s.next++
		row.claimToken = fmt.Sprintf("%s-%d", owner, s.next)
		row.claimUntil = now.Add(lease)
		row.env.ClaimToken = row.claimToken
		row.env.Attempts++

		env := row.env
		env.Envelope = append([]byte(nil), row.env.Envelope...)
		claimed = append(claimed, env)
	}

	return claimed, nil
}

func (s *memTransportStore) MarkEgressSent(_ context.Context, id,
	claimToken string) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	row := s.egress[id]
	if row == nil || row.claimToken != claimToken {
		return nil
	}

	row.status = "sent"

	return nil
}

func (s *memTransportStore) ReleaseEgressForRetry(_ context.Context, id,
	claimToken string, retryAfter time.Duration, _ error) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	row := s.egress[id]
	if row == nil || row.claimToken != claimToken {
		return nil
	}

	row.claimToken = ""
	row.claimUntil = time.Time{}
	row.nextAttempt = time.Now().Add(retryAfter)

	return nil
}

func (s *memTransportStore) ReleaseExpiredEgressClaims(
	_ context.Context) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for _, row := range s.egress {
		if row.claimToken == "" || row.claimUntil.After(now) {
			continue
		}

		row.claimToken = ""
		row.claimUntil = time.Time{}
	}

	return nil
}

var _ TransportStore = (*memTransportStore)(nil)

// newTestConnectorConfig returns a ConnectorConfig pre-populated with test
// defaults: an in-memory mailbox edge, the given store, fixed mailbox IDs
// ("client-1" / "server-1"), protocol version 1, and a short pull wait
// timeout suitable for test speed. Callers can override individual fields
// on the returned config before passing it to NewRuntime or
// NewServerConnectionActor.
func newTestConnectorConfig(
	mb *inMemoryMailbox, store *memCheckpointStore,
) ConnectorConfig {

	cfg := DefaultConnectorConfig()
	cfg.Edge = &fakeMailboxServiceClient{mb: mb}
	cfg.Transport = newMemTransportStore()
	cfg.LocalMailboxID = "client-1"
	cfg.RemoteMailboxID = "server-1"
	cfg.ProtocolVersion = 1
	cfg.PullWaitTimeout = 50 * time.Millisecond

	return cfg
}
