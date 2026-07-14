package serverconn

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// fakeEgressExec is a synchronous Exec[egressTx] for connector unit tests. It
// runs Read/Commit closures immediately with no real transaction or lease
// fence, so a handler's build-then-send-then-Commit flow can be exercised
// without standing up a durable mailbox. When commitErr is set, Commit returns
// it without running the closure, used to simulate a lost lease.
type fakeEgressExec struct {
	commitErr error
	commits   int
}

// Read runs fn against the empty egress store.
func (e *fakeEgressExec) Read(ctx context.Context,
	fn func(context.Context, egressTx) error) error {

	return fn(ctx, egressTx{})
}

// Stage runs fn against the empty egress store. The egress behavior never
// stages -- it has no domain state to persist before its side effect -- but the
// method is required to satisfy Exec[egressTx].
func (e *fakeEgressExec) Stage(ctx context.Context,
	fn func(context.Context, egressTx) error) error {

	return fn(ctx, egressTx{})
}

// Commit counts the call and runs fn against the empty egress store unless a
// commitErr was injected.
func (e *fakeEgressExec) Commit(ctx context.Context,
	fn func(context.Context, egressTx) error) error {

	e.commits++
	if e.commitErr != nil {
		return e.commitErr
	}

	return fn(ctx, egressTx{})
}

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

// memCheckpointStore is a minimal in-memory implementation of the checkpoint
// and delivery portions of actor.DeliveryStore used by serverconn tests.
type memCheckpointStore struct {
	mu          sync.Mutex
	checkpoints map[string]*actor.Checkpoint

	messages    map[string]*storeMessage
	askResults  map[string]*actor.AskResult
	processed   map[string]bool
	deadLetters map[string]*actor.DeadLetter
	outbox      map[string]*actor.OutboxMessage
}

// storeMessage tracks mailbox delivery state in memory.
type storeMessage struct {
	leased      actor.LeasedMessage
	availableAt time.Time
}

// newMemCheckpointStore creates a new empty checkpoint store.
func newMemCheckpointStore() *memCheckpointStore {
	return &memCheckpointStore{
		checkpoints: make(map[string]*actor.Checkpoint),
		messages:    make(map[string]*storeMessage),
		askResults:  make(map[string]*actor.AskResult),
		processed:   make(map[string]bool),
		deadLetters: make(map[string]*actor.DeadLetter),
		outbox:      make(map[string]*actor.OutboxMessage),
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
func (s *memCheckpointStore) LoadCheckpoint(ctx context.Context,
	actorID string) (*actor.Checkpoint, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	cp, ok := s.checkpoints[actorID]
	if !ok {
		return nil, nil
	}

	return cp, nil
}

// EnqueueMessage persists a mailbox message in memory.
func (s *memCheckpointStore) EnqueueMessage(
	ctx context.Context, params actor.EnqueueParams,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	// Mirror production ON CONFLICT DO NOTHING semantics for receiver-side
	// deduplication on retry deliveries.
	if _, exists := s.messages[params.ID]; exists {
		return nil
	}

	payloadCopy := append([]byte(nil), params.Payload...)

	s.messages[params.ID] = &storeMessage{
		leased: actor.LeasedMessage{
			ID:              params.ID,
			MailboxID:       params.MailboxID,
			MessageType:     params.MessageType,
			Payload:         payloadCopy,
			PromiseID:       params.PromiseID,
			CallbackActorID: params.CallbackActorID,
			CorrelationID:   params.CorrelationID,
			Priority:        params.Priority,
			Attempts:        0,
			MaxAttempts:     params.MaxAttempts,
			CreatedAt:       time.Now(),
		},
		availableAt: params.AvailableAt,
	}

	return nil
}

// LeaseNextMessage leases the next available mailbox message.
func (s *memCheckpointStore) LeaseNextMessage(ctx context.Context,
	mailboxID string, leaseToken string, leaseDuration time.Duration) (
	*actor.LeasedMessage, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	var selected *storeMessage
	for _, msg := range s.messages {
		if msg.leased.MailboxID != mailboxID {
			continue
		}

		if msg.availableAt.After(now) {
			continue
		}

		if msg.leased.LeaseToken != "" &&
			msg.leased.LeaseUntil.After(now) {

			continue
		}

		if selected == nil ||
			msg.availableAt.Before(selected.availableAt) {

			selected = msg
		}
	}

	if selected == nil {
		return nil, nil
	}

	selected.leased.LeaseToken = leaseToken
	selected.leased.LeaseUntil = now.Add(leaseDuration)
	selected.leased.Attempts++

	leasedCopy := selected.leased
	leasedCopy.Payload = append(
		[]byte(nil), selected.leased.Payload...,
	)

	return &leasedCopy, nil
}

// PeekNextMessage claims the next available message without taking a lease,
// mirroring LeaseNextMessage's eligibility/ordering but without mutating the
// lease token, lease expiry, or attempts. The returned message carries an
// empty lease token.
func (s *memCheckpointStore) PeekNextMessage(ctx context.Context,
	mailboxID string) (*actor.LeasedMessage, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	var selected *storeMessage
	for _, msg := range s.messages {
		if msg.leased.MailboxID != mailboxID {
			continue
		}

		if msg.availableAt.After(now) {
			continue
		}

		if msg.leased.LeaseToken != "" &&
			msg.leased.LeaseUntil.After(now) {

			continue
		}

		if selected == nil ||
			msg.availableAt.Before(selected.availableAt) {

			selected = msg
		}
	}

	if selected == nil {
		return nil, nil
	}

	leasedCopy := selected.leased
	leasedCopy.LeaseToken = ""
	leasedCopy.LeaseUntil = time.Time{}
	leasedCopy.Payload = append(
		[]byte(nil), selected.leased.Payload...,
	)

	return &leasedCopy, nil
}

// AckMessage acknowledges a leased message by ID and lease token.
func (s *memCheckpointStore) AckMessage(ctx context.Context, id,
	leaseToken string) (int64, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	msg, ok := s.messages[id]
	if !ok {
		return 0, nil
	}

	if msg.leased.LeaseToken != leaseToken {
		return 0, nil
	}

	delete(s.messages, id)

	return 1, nil
}

// AckMessageByID acknowledges a message by ID without lease-token validation,
// mirroring the unfenced leaseless ack.
func (s *memCheckpointStore) AckMessageByID(ctx context.Context, id string) (
	int64, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.messages[id]; !ok {
		return 0, nil
	}

	delete(s.messages, id)

	return 1, nil
}

// NackMessage releases a leased message for later redelivery.
func (s *memCheckpointStore) NackMessage(ctx context.Context, id,
	leaseToken string, retryAfter time.Duration) (int64, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	msg, ok := s.messages[id]
	if !ok {
		return 0, nil
	}

	if msg.leased.LeaseToken != leaseToken {
		return 0, nil
	}

	msg.leased.LeaseToken = ""
	msg.leased.LeaseUntil = time.Time{}
	msg.availableAt = time.Now().Add(retryAfter)

	return 1, nil
}

// NackMessageByID releases a message by ID without lease-token validation and
// increments attempts, mirroring the unfenced leaseless nack.
func (s *memCheckpointStore) NackMessageByID(ctx context.Context, id string,
	retryAfter time.Duration) (int64, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	msg, ok := s.messages[id]
	if !ok {
		return 0, nil
	}

	msg.leased.LeaseToken = ""
	msg.leased.LeaseUntil = time.Time{}
	msg.leased.Attempts++
	msg.availableAt = time.Now().Add(retryAfter)

	return 1, nil
}

// ExtendLease extends a message lease when lease token matches.
func (s *memCheckpointStore) ExtendLease(ctx context.Context, id,
	leaseToken string, extension time.Duration) (int64, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	msg, ok := s.messages[id]
	if !ok {
		return 0, nil
	}

	if msg.leased.LeaseToken != leaseToken {
		return 0, nil
	}

	msg.leased.LeaseUntil = time.Now().Add(extension)

	return 1, nil
}

// MoveToDeadLetter moves a mailbox message to the dead letter map.
func (s *memCheckpointStore) MoveToDeadLetter(
	ctx context.Context, id, reason string,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	msg, ok := s.messages[id]
	if !ok {
		return nil
	}

	payloadCopy := append([]byte(nil), msg.leased.Payload...)

	s.deadLetters[id] = &actor.DeadLetter{
		ID:            id,
		Source:        "mailbox",
		ActorID:       msg.leased.MailboxID,
		MessageType:   msg.leased.MessageType,
		Payload:       payloadCopy,
		FailureReason: reason,
		Attempts:      msg.leased.Attempts,
		CreatedAt:     time.Now(),
	}

	return nil
}

// DeleteMessage removes a mailbox message by ID.
func (s *memCheckpointStore) DeleteMessage(
	ctx context.Context, id string,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.messages, id)

	return nil
}

// SaveAskResult stores an ask result in memory.
func (s *memCheckpointStore) SaveAskResult(
	ctx context.Context, params actor.AskResultParams,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	resultBlob := append([]byte(nil), params.ResultBlob...)

	s.askResults[params.PromiseID] = &actor.AskResult{
		PromiseID:  params.PromiseID,
		ResultBlob: resultBlob,
		ErrorText:  params.ErrorText,
		CreatedAt:  time.Now(),
		ExpiresAt:  params.ExpiresAt,
	}

	return nil
}

// GetAskResult retrieves an ask result from memory.
func (s *memCheckpointStore) GetAskResult(ctx context.Context,
	promiseID string) (*actor.AskResult, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	result, ok := s.askResults[promiseID]
	if !ok {
		return nil, nil
	}

	clone := *result
	clone.ResultBlob = append([]byte(nil), result.ResultBlob...)

	return &clone, nil
}

// DeleteAskResult removes an ask result by promise ID.
func (s *memCheckpointStore) DeleteAskResult(
	ctx context.Context, promiseID string,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.askResults, promiseID)

	return nil
}

// EnqueueOutbox stores an outbox message in memory.
func (s *memCheckpointStore) EnqueueOutbox(
	ctx context.Context, params actor.OutboxParams,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	payloadCopy := append([]byte(nil), params.Payload...)

	s.outbox[params.ID] = &actor.OutboxMessage{
		ID:            params.ID,
		SourceActorID: params.SourceActorID,
		TargetActorID: params.TargetActorID,
		MessageType:   params.MessageType,
		Payload:       payloadCopy,
		DomainKey:     params.DomainKey,
		Version:       params.Version,
		Status:        "pending",
		CreatedAt:     time.Now(),
	}

	return nil
}

// ClaimOutboxBatch claims pending outbox messages up to the given limit.
func (s *memCheckpointStore) ClaimOutboxBatch(ctx context.Context,
	params actor.OutboxClaimParams) ([]actor.OutboxMessage, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	var result []actor.OutboxMessage

	for _, msg := range s.outbox {
		if msg.Status != "pending" {
			continue
		}

		msg.ClaimToken = params.ClaimToken
		msg.DeliveryAttempts++

		copyMsg := *msg
		copyMsg.Payload = append([]byte(nil), msg.Payload...)
		result = append(result, copyMsg)

		if len(result) >= params.Limit {
			break
		}
	}

	return result, nil
}

// CompleteOutbox marks a claimed outbox message as completed.
func (s *memCheckpointStore) CompleteOutbox(
	ctx context.Context, id, claimToken string,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	msg, ok := s.outbox[id]
	if !ok {
		return nil
	}

	if msg.ClaimToken != claimToken {
		return nil
	}

	msg.Status = "completed"

	return nil
}

// FailOutbox marks a claimed outbox message as dead-lettered.
func (s *memCheckpointStore) FailOutbox(
	ctx context.Context, id, claimToken string,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	msg, ok := s.outbox[id]
	if !ok {
		return nil
	}

	if msg.ClaimToken != claimToken {
		return nil
	}

	msg.Status = "dead_letter"

	return nil
}

// IsProcessed returns true when the message ID is marked processed.
func (s *memCheckpointStore) IsProcessed(ctx context.Context, id string) (bool,
	error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.processed[id], nil
}

// MarkProcessed records a processed message ID.
func (s *memCheckpointStore) MarkProcessed(
	ctx context.Context, id, actorID string,
	ttl time.Duration,
) error {

	_ = ctx
	_ = actorID
	_ = ttl

	s.mu.Lock()
	defer s.mu.Unlock()

	s.processed[id] = true

	return nil
}

// DeleteCheckpoint deletes the checkpoint for actorID.
func (s *memCheckpointStore) DeleteCheckpoint(
	ctx context.Context, actorID string,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.checkpoints, actorID)

	return nil
}

// GetDeadLetter returns a dead letter entry by ID.
func (s *memCheckpointStore) GetDeadLetter(ctx context.Context, id string) (
	*actor.DeadLetter, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	deadLetter, ok := s.deadLetters[id]
	if !ok {
		return nil, nil
	}

	copyDeadLetter := *deadLetter
	copyDeadLetter.Payload = append([]byte(nil), deadLetter.Payload...)

	return &copyDeadLetter, nil
}

// ListDeadLetters lists dead letters for the given actor ID.
func (s *memCheckpointStore) ListDeadLetters(ctx context.Context,
	actorID string, limit int) ([]actor.DeadLetter, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	var result []actor.DeadLetter

	for _, deadLetter := range s.deadLetters {
		if deadLetter.ActorID != actorID {
			continue
		}

		copyDeadLetter := *deadLetter
		copyDeadLetter.Payload = append(
			[]byte(nil), deadLetter.Payload...,
		)
		result = append(result, copyDeadLetter)

		if len(result) >= limit {
			break
		}
	}

	return result, nil
}

// DeleteDeadLetter deletes a dead-letter entry by ID.
func (s *memCheckpointStore) DeleteDeadLetter(
	ctx context.Context, id string,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.deadLetters, id)

	return nil
}

// ExpireLeases clears lease tokens for expired messages.
func (s *memCheckpointStore) ExpireLeases(
	ctx context.Context,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for _, msg := range s.messages {
		if msg.leased.LeaseUntil.Before(now) {
			msg.leased.LeaseToken = ""
			msg.leased.LeaseUntil = time.Time{}
		}
	}

	return nil
}

// CleanupExpired removes expired ask results.
func (s *memCheckpointStore) CleanupExpired(
	ctx context.Context,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for id, askResult := range s.askResults {
		if askResult.ExpiresAt.Before(now) {
			delete(s.askResults, id)
		}
	}

	return nil
}

// ExecTx satisfies actor.TxAwareDeliveryStore so the in-memory store can back
// the Read/Commit execution path. The store has no real database transaction,
// so it runs fn against itself directly; each store method takes the store's
// own mutex, which is sufficient for the single-threaded handler unit tests and
// the competing-consumer tests that exercise lease fencing through the real
// method bodies.
func (s *memCheckpointStore) ExecTx(ctx context.Context, _ bool,
	fn actor.TxFunc) error {

	return fn(ctx, s)
}

// Compile-time checks.
var (
	_ actor.DeliveryStore        = (*memCheckpointStore)(nil)
	_ actor.TxAwareDeliveryStore = (*memCheckpointStore)(nil)
)

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
	cfg.Store = store
	cfg.LocalMailboxID = "client-1"
	cfg.RemoteMailboxID = "server-1"
	cfg.ArkProtocolVersion = 1
	cfg.PullWaitTimeout = 50 * time.Millisecond

	return cfg
}
