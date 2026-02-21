package clientconn

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	clientsqlc "github.com/lightninglabs/darepo-client/db/sqlc"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

// inMemoryMailbox is a minimal in-memory implementation of the MailboxService
// semantics needed for clientconn unit tests.
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

// cloneEnvelope makes a deep copy of env and returns false if proto.Clone
// does not return the expected type.
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
// and delivery portions of actor.DeliveryStore used by clientconn tests.
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

// EnqueueMessage persists a mailbox message in memory.
func (s *memCheckpointStore) EnqueueMessage(
	ctx context.Context, params actor.EnqueueParams,
) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	// Mirror production ON CONFLICT DO NOTHING semantics for
	// receiver-side deduplication on retry deliveries.
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
func (s *memCheckpointStore) LeaseNextMessage(
	ctx context.Context, mailboxID string, leaseToken string,
	leaseDuration time.Duration,
) (*actor.LeasedMessage, error) {

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

// AckMessage acknowledges a leased message by ID and lease token.
func (s *memCheckpointStore) AckMessage(
	ctx context.Context, id, leaseToken string,
) (int64, error) {

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

// NackMessage releases a leased message for later redelivery.
func (s *memCheckpointStore) NackMessage(
	ctx context.Context, id, leaseToken string,
	retryAfter time.Duration,
) (int64, error) {

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

// ExtendLease extends a message lease when lease token matches.
func (s *memCheckpointStore) ExtendLease(
	ctx context.Context, id, leaseToken string,
	extension time.Duration,
) (int64, error) {

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
func (s *memCheckpointStore) GetAskResult(
	ctx context.Context, promiseID string,
) (*actor.AskResult, error) {

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
func (s *memCheckpointStore) ClaimOutboxBatch(
	ctx context.Context, params actor.OutboxClaimParams,
) ([]actor.OutboxMessage, error) {

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
func (s *memCheckpointStore) IsProcessed(
	ctx context.Context, id string,
) (bool, error) {

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
func (s *memCheckpointStore) GetDeadLetter(
	ctx context.Context, id string,
) (*actor.DeadLetter, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	deadLetter, ok := s.deadLetters[id]
	if !ok {
		return nil, nil
	}

	copyDeadLetter := *deadLetter
	copyDeadLetter.Payload = append(
		[]byte(nil), deadLetter.Payload...,
	)

	return &copyDeadLetter, nil
}

// ListDeadLetters lists dead letters for the given actor ID.
func (s *memCheckpointStore) ListDeadLetters(
	ctx context.Context, actorID string, limit int,
) ([]actor.DeadLetter, error) {

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

// Compile-time check.
var _ actor.DeliveryStore = (*memCheckpointStore)(nil)

// transcript is a thread-safe, append-only log of values used by test
// behaviors as an observation point. Tests poll the transcript with
// require.Eventually rather than blocking on channels, making assertion
// order less fragile and deadlock-free.
type transcript[T any] struct {
	mu      sync.Mutex
	entries []T
}

// append records a new entry in the transcript.
func (t *transcript[T]) append(entry T) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.entries = append(t.entries, entry)
}

// all returns a snapshot copy of all recorded entries.
func (t *transcript[T]) all() []T {
	t.mu.Lock()
	defer t.mu.Unlock()

	cp := make([]T, len(t.entries))
	copy(cp, t.entries)

	return cp
}

// entryCount returns the number of recorded entries.
func (t *transcript[T]) entryCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.entries)
}

// newTestPerClientConfig returns a PerClientConfig pre-populated with test
// defaults: an in-memory mailbox edge, the given store, fixed mailbox IDs,
// protocol version 1, and a short pull wait timeout suitable for test speed.
// Callers can override individual fields on the returned config before
// passing it to NewClientRuntime or NewClientConnectionActor.
//
// The mailbox topology mirrors production:
//   - localMailboxID ("server-for-client-1") is the server's per-client
//     ingress mailbox that clients send to
//   - remoteMailboxID ("client-1") is the client's mailbox that the server
//     sends events and RPCs to
func newTestPerClientConfig(
	mb *inMemoryMailbox, store actor.DeliveryStore,
) PerClientConfig {

	cfg := DefaultPerClientConfig()
	cfg.Edge = &fakeMailboxServiceClient{mb: mb}
	cfg.Store = store
	cfg.LocalMailboxID = "server-for-client-1"
	cfg.RemoteMailboxID = "client-1"
	cfg.ProtocolVersion = 1
	cfg.PullWaitTimeout = 50 * time.Millisecond

	// Provide a default no-op dispatcher so configs passed through
	// NewClientRuntime satisfy the non-empty Dispatchers validation.
	// Tests that need specific routing override this field.
	cfg.Dispatchers = DispatcherMap{
		{Service: "test.v1.Noop", Method: "Noop"}: func(
			_ context.Context,
			_ *mailboxpb.Envelope,
		) error {

			return nil
		},
	}

	return cfg
}

// deliveryStoreShim wraps a TxAwareDeliveryStore and exposes only the
// non-transactional actor.DeliveryStore interface. This mirrors
// darepo/db.deliveryStoreShim and prevents the DurableActor from using
// transactional processing, which avoids SQLite deadlocks when the actor
// behavior starts its own DB operations.
type deliveryStoreShim struct {
	actor.DeliveryStore
}

// newRealDeliveryStore creates an actor.DeliveryStore backed by a fresh
// per-test SQLite database. This exercises the full persistence path
// through the shared actordelivery schema, bypassing darepo/db to avoid
// an import cycle (darepo/db imports darepo/clientconn).
//
// The SQLite connection is configured with the same PRAGMA options and
// connection pool settings used in production (mirroring
// darepo-client/db/sqlite.go:NewSqliteStore): WAL journal mode,
// immediate transaction locks, busy timeout, full synchronous mode, and
// fullfsync on macOS. This prevents SQLITE_BUSY errors under parallel
// test load.
func newRealDeliveryStore(t *testing.T) actor.DeliveryStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "clientconn_test.db")

	// Build the DSN using the same _pragma format as
	// darepo-client/db/sqlite.go. The modernc.org/sqlite driver
	// expects PRAGMA options as _pragma=key=value query parameters.
	pragmaOptions := []struct {
		name  string
		value string
	}{
		{name: "foreign_keys", value: "on"},
		{name: "journal_mode", value: "WAL"},
		{name: "busy_timeout", value: "5000"},
		{name: "synchronous", value: "full"},
		{name: "fullfsync", value: "true"},
	}

	sqliteOptions := make(url.Values)
	for _, opt := range pragmaOptions {
		sqliteOptions.Add(
			"_pragma",
			fmt.Sprintf("%v=%v", opt.name, opt.value),
		)
	}

	dsn := fmt.Sprintf(
		"%v?%v&%v",
		dbPath, sqliteOptions.Encode(),
		"_txlock=immediate",
	)

	sqlDB, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)

	// Match the production connection pool settings to avoid
	// connection churn under parallel test load.
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	sqlDB.SetConnMaxLifetime(10 * time.Minute)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
	})

	// Run the actor-delivery schema migrations on the fresh DB.
	err = actordelivery.RunMigrations(
		sqlDB, clientsqlc.BackendTypeSqlite,
	)
	require.NoError(t, err)

	store, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB, clientsqlc.BackendTypeSqlite,
		clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	// Wrap in a shim to expose only DeliveryStore (not
	// TxAwareDeliveryStore) so the DurableActor uses
	// non-transactional processing.
	return &deliveryStoreShim{DeliveryStore: store}
}
