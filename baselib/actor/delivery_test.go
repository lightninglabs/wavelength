package actor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// mockDeliveryStore is a test implementation of DeliveryStore.
type mockDeliveryStore struct {
	mu sync.Mutex

	// Messages maps message ID to message data.
	messages map[string]*LeasedMessage

	// AskResults maps promise ID to result.
	askResults map[string]*AskResult

	// Processed tracks processed message IDs.
	processed map[string]bool

	// Checkpoints maps actor ID to checkpoint.
	checkpoints map[string]*Checkpoint

	// DeadLetters stores dead-lettered messages.
	deadLetters map[string]*DeadLetter

	// Outbox stores outbox messages.
	outbox map[string]*OutboxMessage

	// Error injection for testing.
	injectError error

	// injectOutboxError causes only EnqueueOutbox to fail.
	injectOutboxError error

	// injectEnqueueError causes only EnqueueMessage to fail.
	injectEnqueueError error
}

func newMockDeliveryStore() *mockDeliveryStore {
	return &mockDeliveryStore{
		messages:    make(map[string]*LeasedMessage),
		askResults:  make(map[string]*AskResult),
		processed:   make(map[string]bool),
		checkpoints: make(map[string]*Checkpoint),
		deadLetters: make(map[string]*DeadLetter),
		outbox:      make(map[string]*OutboxMessage),
	}
}

func (m *mockDeliveryStore) EnqueueMessage(ctx context.Context, params EnqueueParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectEnqueueError != nil {
		return m.injectEnqueueError
	}

	if m.injectError != nil {
		return m.injectError
	}

	// Match the ON CONFLICT (id) DO NOTHING semantics of the real SQL
	// query: if a message with this ID already exists, silently succeed
	// without overwriting. This enables receiver-side deduplication for
	// outbox delivery retries.
	if _, exists := m.messages[params.ID]; exists {
		return nil
	}

	m.messages[params.ID] = &LeasedMessage{
		ID:              params.ID,
		MailboxID:       params.MailboxID,
		MessageType:     params.MessageType,
		Payload:         params.Payload,
		PromiseID:       params.PromiseID,
		CallbackActorID: params.CallbackActorID,
		CorrelationID:   params.CorrelationID,
		Priority:        params.Priority,
		MaxAttempts:     params.MaxAttempts,
		CreatedAt:       time.Now(),
	}

	return nil
}

func (m *mockDeliveryStore) LeaseNextMessage(
	ctx context.Context,
	mailboxID string,
	leaseToken string,
	leaseDuration time.Duration,
) (*LeasedMessage, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return nil, m.injectError
	}

	now := time.Now()

	for _, msg := range m.messages {
		if msg.MailboxID != mailboxID {
			continue
		}

		// Skip if already leased and not expired.
		if msg.LeaseToken != "" && msg.LeaseUntil.After(now) {
			continue
		}

		// Lease this message.
		msg.LeaseToken = leaseToken
		msg.LeaseUntil = now.Add(leaseDuration)
		msg.Attempts++

		return msg, nil
	}

	return nil, nil
}

func (m *mockDeliveryStore) AckMessage(ctx context.Context, id, leaseToken string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return 0, m.injectError
	}

	msg, ok := m.messages[id]
	if !ok {
		return 0, nil
	}

	if msg.LeaseToken != leaseToken {
		return 0, nil
	}

	delete(m.messages, id)

	return 1, nil
}

func (m *mockDeliveryStore) NackMessage(
	ctx context.Context,
	id, leaseToken string,
	retryAfter time.Duration,
) (int64, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return 0, m.injectError
	}

	msg, ok := m.messages[id]
	if !ok {
		return 0, nil
	}

	if msg.LeaseToken != leaseToken {
		return 0, nil
	}

	// Release the lease.
	msg.LeaseToken = ""
	msg.LeaseUntil = time.Time{}

	return 1, nil
}

func (m *mockDeliveryStore) ExtendLease(
	ctx context.Context,
	id, leaseToken string,
	extension time.Duration,
) (int64, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return 0, m.injectError
	}

	msg, ok := m.messages[id]
	if !ok {
		return 0, nil
	}

	if msg.LeaseToken != leaseToken {
		return 0, nil
	}

	msg.LeaseUntil = time.Now().Add(extension)

	return 1, nil
}

func (m *mockDeliveryStore) MoveToDeadLetter(ctx context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return m.injectError
	}

	msg, ok := m.messages[id]
	if !ok {
		return nil
	}

	m.deadLetters[id] = &DeadLetter{
		ID:            id,
		Source:        "mailbox",
		ActorID:       msg.MailboxID,
		MessageType:   msg.MessageType,
		Payload:       msg.Payload,
		FailureReason: reason,
		Attempts:      msg.Attempts,
		CreatedAt:     time.Now(),
	}

	return nil
}

func (m *mockDeliveryStore) DeleteMessage(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return m.injectError
	}

	delete(m.messages, id)

	return nil
}

func (m *mockDeliveryStore) SaveAskResult(ctx context.Context, params AskResultParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return m.injectError
	}

	m.askResults[params.PromiseID] = &AskResult{
		PromiseID:  params.PromiseID,
		ResultBlob: params.ResultBlob,
		ErrorText:  params.ErrorText,
		CreatedAt:  time.Now(),
		ExpiresAt:  params.ExpiresAt,
	}

	return nil
}

func (m *mockDeliveryStore) GetAskResult(ctx context.Context, promiseID string) (*AskResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return nil, m.injectError
	}

	return m.askResults[promiseID], nil
}

func (m *mockDeliveryStore) DeleteAskResult(ctx context.Context, promiseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.askResults, promiseID)

	return nil
}

func (m *mockDeliveryStore) EnqueueOutbox(ctx context.Context, params OutboxParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectOutboxError != nil {
		return m.injectOutboxError
	}

	if m.injectError != nil {
		return m.injectError
	}

	m.outbox[params.ID] = &OutboxMessage{
		ID:            params.ID,
		SourceActorID: params.SourceActorID,
		TargetActorID: params.TargetActorID,
		MessageType:   params.MessageType,
		Payload:       params.Payload,
		DomainKey:     params.DomainKey,
		Version:       params.Version,
		Status:        "pending",
		CreatedAt:     time.Now(),
	}

	return nil
}

func (m *mockDeliveryStore) ClaimOutboxBatch(
	ctx context.Context, params OutboxClaimParams,
) ([]OutboxMessage, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return nil, m.injectError
	}

	var result []OutboxMessage
	for _, msg := range m.outbox {
		if msg.Status == "pending" {
			msg.DeliveryAttempts++
			msg.ClaimToken = params.ClaimToken
			result = append(result, *msg)

			if len(result) >= params.Limit {
				break
			}
		}
	}

	return result, nil
}

func (m *mockDeliveryStore) CompleteOutbox(
	ctx context.Context, id, claimToken string,
) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	if msg, ok := m.outbox[id]; ok {
		msg.Status = "completed"
	}

	return nil
}

func (m *mockDeliveryStore) FailOutbox(
	ctx context.Context, id, claimToken string,
) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	if msg, ok := m.outbox[id]; ok {
		msg.Status = "dead_letter"
	}

	return nil
}

func (m *mockDeliveryStore) IsProcessed(ctx context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.processed[id], nil
}

func (m *mockDeliveryStore) MarkProcessed(
	ctx context.Context,
	id, actorID string,
	ttl time.Duration,
) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.processed[id] = true

	return nil
}

func (m *mockDeliveryStore) SaveCheckpoint(ctx context.Context, params CheckpointParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectError != nil {
		return m.injectError
	}

	m.checkpoints[params.ActorID] = &Checkpoint{
		ActorID:   params.ActorID,
		StateType: params.StateType,
		StateData: params.StateData,
		Version:   params.Version,
		UpdatedAt: time.Now(),
	}

	return nil
}

func (m *mockDeliveryStore) LoadCheckpoint(ctx context.Context, actorID string) (*Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.checkpoints[actorID], nil
}

func (m *mockDeliveryStore) DeleteCheckpoint(ctx context.Context, actorID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.checkpoints, actorID)

	return nil
}

func (m *mockDeliveryStore) GetDeadLetter(ctx context.Context, id string) (*DeadLetter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.deadLetters[id], nil
}

func (m *mockDeliveryStore) ListDeadLetters(ctx context.Context, actorID string, limit int) ([]DeadLetter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []DeadLetter
	for _, dl := range m.deadLetters {
		if dl.ActorID == actorID {
			result = append(result, *dl)

			if len(result) >= limit {
				break
			}
		}
	}

	return result, nil
}

func (m *mockDeliveryStore) DeleteDeadLetter(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.deadLetters, id)

	return nil
}

func (m *mockDeliveryStore) ExpireLeases(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for _, msg := range m.messages {
		if msg.LeaseUntil.Before(now) {
			msg.LeaseToken = ""
			msg.LeaseUntil = time.Time{}
		}
	}

	return nil
}

func (m *mockDeliveryStore) CleanupExpired(ctx context.Context) error {
	return nil
}

// Verify mockDeliveryStore implements DeliveryStore.
var _ DeliveryStore = (*mockDeliveryStore)(nil)

// TestDeliveryAck tests basic Ack functionality.
func TestDeliveryAck(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	// Add a message to the store.
	msgID := "test-msg-1"
	leaseToken := "test-lease-token"
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
	}

	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	// Ack should succeed.
	err := delivery.Ack(ctx, fn.Ok("success"))
	require.NoError(t, err)

	// Message should be deleted from store.
	require.Empty(t, store.messages)

	// Second Ack should fail.
	err = delivery.Ack(ctx, fn.Ok("success"))
	require.Equal(t, ErrAlreadyAcked, err)
}

// TestDeliveryAckWithPromise tests Ack with Ask pattern.
func TestDeliveryAckWithPromise(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	msgID := "test-msg-1"
	leaseToken := "test-lease-token"
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
	}

	promise := NewPromise[string]()
	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		Promise:     promise,
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	// Ack should succeed and complete promise.
	err := delivery.Ack(ctx, fn.Ok("the result"))
	require.NoError(t, err)

	// Promise should be completed.
	result := promise.Future().Await(ctx)
	value, err := result.Unpack()
	require.NoError(t, err)
	require.Equal(t, "the result", value)

	// Ask result should be persisted.
	require.Len(t, store.askResults, 1)
}

// TestDeliveryNack tests basic Nack functionality.
func TestDeliveryNack(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	msgID := "test-msg-1"
	leaseToken := "test-lease-token"
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
	}

	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	// Nack should succeed.
	err := delivery.Nack(ctx, errors.New("transient error"), 5*time.Second)
	require.NoError(t, err)

	// Message should still be in store but lease released.
	require.Len(t, store.messages, 1)
	msg := store.messages[msgID]
	require.Empty(t, msg.LeaseToken)

	// Second Nack should fail.
	err = delivery.Nack(ctx, errors.New("error"), 5*time.Second)
	require.Equal(t, ErrAlreadyAcked, err)
}

// TestDeliveryNackPoisonPill tests that messages exceeding max attempts are
// moved to dead letter queue (poison pill handling).
func TestDeliveryNackPoisonPill(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	msgID := "test-msg-1"
	leaseToken := "test-lease-token"

	// Message at max attempts.
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		MessageType: "poison.message",
		Payload:     []byte("poison data"),
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    10, // At max.
		MaxAttempts: 10,
	}

	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    10, // At max.
		MaxAttempts: 10,
		store:       store,
	}

	require.True(t, delivery.ShouldDeadLetter())

	// Nack should move to dead letter instead of retry.
	err := delivery.Nack(ctx, errors.New("permanent error"), 5*time.Second)
	require.NoError(t, err)

	// Message should be deleted.
	require.Empty(t, store.messages)

	// Message should be in dead letter queue.
	require.Len(t, store.deadLetters, 1)
	dl := store.deadLetters[msgID]
	require.Equal(t, "mailbox", dl.Source)
	require.Contains(t, dl.FailureReason, "max attempts reached")
}

// TestDeliveryExtend tests lease extension.
func TestDeliveryExtend(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	msgID := "test-msg-1"
	leaseToken := "test-lease-token"
	initialLease := time.Now().Add(30 * time.Second)
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		LeaseToken:  leaseToken,
		LeaseUntil:  initialLease,
		Attempts:    1,
		MaxAttempts: 10,
	}

	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		LeaseToken:  leaseToken,
		LeaseUntil:  initialLease,
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	// Extend should succeed.
	err := delivery.Extend(ctx, 60*time.Second)
	require.NoError(t, err)

	// Local state should be updated.
	require.True(t, delivery.LeaseUntil.After(initialLease))
}

// TestDeliveryStaleLeaseToken tests that operations fail with wrong token.
func TestDeliveryStaleLeaseToken(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	msgID := "test-msg-1"
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		LeaseToken:  "new-token", // Different token.
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
	}

	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		LeaseToken:  "old-token", // Stale token.
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	// Ack should fail.
	err := delivery.Ack(ctx, fn.Ok("success"))
	require.Equal(t, ErrLeaseExpired, err)

	// Reset delivery state.
	delivery.acked = false

	// Nack should fail.
	err = delivery.Nack(ctx, errors.New("error"), 5*time.Second)
	require.Equal(t, ErrLeaseExpired, err)

	// Reset delivery state.
	delivery.acked = false

	// Extend should fail.
	err = delivery.Extend(ctx, 60*time.Second)
	require.Equal(t, ErrLeaseExpired, err)
}

// TestDeliveryHelperMethods tests IsAsk, IsTell, LeaseRemaining, etc.
func TestDeliveryHelperMethods(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()

	// Test Tell delivery.
	tellDelivery := &Delivery[*testTLVMsg, string]{
		ID:          "tell-msg",
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1))},
		Promise:     nil, // Tell has no promise.
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	require.True(t, tellDelivery.IsTell())
	require.False(t, tellDelivery.IsAsk())
	require.False(t, tellDelivery.IsLeaseExpired())
	require.False(t, tellDelivery.ShouldDeadLetter())
	require.True(t, tellDelivery.LeaseRemaining() > 0)

	// Test Ask delivery.
	askDelivery := &Delivery[*testTLVMsg, string]{
		ID:          "ask-msg",
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(2))},
		Promise:     NewPromise[string](),             // Ask has promise.
		LeaseUntil:  time.Now().Add(-1 * time.Second), // Expired.
		Attempts:    10,
		MaxAttempts: 10,
		store:       store,
	}

	require.False(t, askDelivery.IsTell())
	require.True(t, askDelivery.IsAsk())
	require.True(t, askDelivery.IsLeaseExpired())
	require.True(t, askDelivery.ShouldDeadLetter())
	require.True(t, askDelivery.LeaseRemaining() < 0)
}

// TestDeliveryRapidAckNack is a property-based test for Ack/Nack behavior.
func TestDeliveryRapidAckNack(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		store := newMockDeliveryStore()
		ctx := context.Background()

		msgID := rapid.String().Draw(t, "msgID")
		leaseToken := rapid.String().Draw(t, "leaseToken")
		attempts := rapid.IntRange(1, 20).Draw(t, "attempts")
		maxAttempts := rapid.IntRange(1, 20).Draw(t, "maxAttempts")
		doAck := rapid.Bool().Draw(t, "doAck")

		store.messages[msgID] = &LeasedMessage{
			ID:          msgID,
			MailboxID:   "test-actor",
			LeaseToken:  leaseToken,
			LeaseUntil:  time.Now().Add(30 * time.Second),
			Attempts:    attempts,
			MaxAttempts: maxAttempts,
		}

		delivery := &Delivery[*testTLVMsg, string]{
			ID:          msgID,
			Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
			LeaseToken:  leaseToken,
			LeaseUntil:  time.Now().Add(30 * time.Second),
			Attempts:    attempts,
			MaxAttempts: maxAttempts,
			store:       store,
		}

		if doAck {
			err := delivery.Ack(ctx, fn.Ok("result"))
			require.NoError(t, err)
			require.Empty(t, store.messages)
		} else {
			err := delivery.Nack(ctx, errors.New("error"), time.Second)
			require.NoError(t, err)

			if attempts >= maxAttempts {
				// Should be dead-lettered.
				require.Empty(t, store.messages)
				require.Len(t, store.deadLetters, 1)
			} else {
				// Should be released for retry.
				require.Len(t, store.messages, 1)
				require.Empty(t, store.deadLetters)
			}
		}

		// Second operation should fail.
		if doAck {
			err := delivery.Ack(ctx, fn.Ok("result"))
			require.Equal(t, ErrAlreadyAcked, err)
		} else {
			err := delivery.Nack(ctx, errors.New("error"), time.Second)
			require.Equal(t, ErrAlreadyAcked, err)
		}
	})
}
