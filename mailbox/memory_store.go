package mailbox

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/btcsuite/btclog/v2"
	"google.golang.org/protobuf/proto"
)

// MemoryStore is an in-memory mailbox Store implementation.
//
// MemoryStore is intended for tests and for validating semantics. It is not a
// durable implementation.
type MemoryStore struct {
	mu        sync.Mutex
	mailboxes map[string]*mailboxState

	cfg StoreConfig
	log btclog.Logger
}

// mailboxState holds the per-recipient mailbox state.
type mailboxState struct {
	mu sync.Mutex

	nextSeq   uint64
	ackCursor uint64
	envelopes []storedEnvelope

	notifyCh chan struct{}
}

// storedEnvelope is a single envelope stored alongside its sequence number.
type storedEnvelope struct {
	seq uint64
	env *Envelope
}

// NewMemoryStore creates an empty in-memory mailbox store.
func NewMemoryStore(opts ...StoreOption) *MemoryStore {
	cfg := DefaultStoreConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	return &MemoryStore{
		mailboxes: make(map[string]*mailboxState),
		cfg:       cfg,
		log:       cfg.Log,
	}
}

// Append stores env in memory and returns the assigned sequence number.
func (s *MemoryStore) Append(
	ctx context.Context,
	env *Envelope,
) (uint64, error) {

	_ = ctx

	if env == nil {
		return 0, fmt.Errorf("missing envelope")
	}
	if env.Recipient == "" {
		return 0, fmt.Errorf("missing recipient")
	}
	if env.MsgId == "" {
		return 0, fmt.Errorf("missing msg_id")
	}

	state := s.getOrCreate(env.Recipient)

	state.mu.Lock()
	defer state.mu.Unlock()

	if s.cfg.MaxEnvelopesPerMailbox > 0 {
		if len(state.envelopes) >= s.cfg.MaxEnvelopesPerMailbox {
			return 0, &ErrMailboxFull{
				Recipient: env.Recipient,
				Max:       s.cfg.MaxEnvelopesPerMailbox,
			}
		}
	}

	storedAny := proto.Clone(env)
	stored, ok := storedAny.(*Envelope)
	if !ok {
		return 0, fmt.Errorf("unexpected clone type %T", storedAny)
	}

	seq := state.nextSeq + 1
	stored.EventSeq = seq

	if s.cfg.MaxEnvelopeBytes > 0 {
		size := proto.Size(stored)
		if size > s.cfg.MaxEnvelopeBytes {
			return 0, &ErrEnvelopeTooLarge{
				Size: size,
				Max:  s.cfg.MaxEnvelopeBytes,
			}
		}
	}

	state.nextSeq = seq

	state.envelopes = append(state.envelopes, storedEnvelope{
		seq: seq,
		env: stored,
	})

	state.notify()

	s.log.DebugS(ctx, "Appended envelope",
		slog.String("recipient", env.Recipient),
		slog.Uint64("seq", seq))

	return seq, nil
}

// Pull returns up to limit envelopes starting at cursor.
func (s *MemoryStore) Pull(ctx context.Context, recipient string,
	cursor uint64, limit int) ([]*Envelope, uint64, error) {

	if recipient == "" {
		return nil, 0, fmt.Errorf("missing recipient")
	}
	if limit <= 0 {
		return nil, 0, fmt.Errorf("limit must be positive")
	}

	state := s.getOrCreate(recipient)

	for {
		state.mu.Lock()
		envelopes := state.pullLocked(cursor, limit)
		nextCursor := cursor
		if len(envelopes) > 0 {
			nextCursor = envelopes[len(envelopes)-1].EventSeq + 1
		}
		notifyCh := state.notifyCh
		state.mu.Unlock()

		if len(envelopes) > 0 {
			return envelopes, nextCursor, nil
		}

		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()

		case <-notifyCh:
		}
	}
}

// AckUpTo advances the ack cursor and discards older envelopes.
func (s *MemoryStore) AckUpTo(ctx context.Context, recipient string,
	cursor uint64) error {

	_ = ctx

	if recipient == "" {
		return fmt.Errorf("missing recipient")
	}

	state := s.getOrCreate(recipient)

	state.mu.Lock()
	defer state.mu.Unlock()

	if cursor <= state.ackCursor {
		return nil
	}

	state.ackCursor = cursor
	state.gcLocked()

	s.log.DebugS(ctx, "Acked envelopes",
		slog.String("recipient", recipient),
		slog.Uint64("cursor", cursor))

	return nil
}

// getOrCreate returns the mailboxState for recipient, creating it if needed.
func (s *MemoryStore) getOrCreate(recipient string) *mailboxState {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.mailboxes[recipient]
	if ok {
		return state
	}

	state = newMailboxState()
	s.mailboxes[recipient] = state

	return state
}

// newMailboxState creates an initialized mailboxState.
func newMailboxState() *mailboxState {
	return &mailboxState{
		notifyCh: make(chan struct{}),
	}
}

// notify wakes any Pull callers blocked on this mailbox state.
func (s *mailboxState) notify() {
	close(s.notifyCh)
	s.notifyCh = make(chan struct{})
}

// pullLocked returns envelopes starting at cursor, respecting the ack cursor.
//
// pullLocked requires s.mu to be held.
func (s *mailboxState) pullLocked(cursor uint64,
	limit int) []*Envelope {

	startSeq := cursor
	if s.ackCursor > startSeq {
		startSeq = s.ackCursor
	}

	startIdx := sort.Search(len(s.envelopes), func(i int) bool {
		return s.envelopes[i].seq >= startSeq
	})
	if startIdx >= len(s.envelopes) {
		return nil
	}

	n := len(s.envelopes) - startIdx
	if n > limit {
		n = limit
	}

	out := make([]*Envelope, 0, n)
	for i := startIdx; i < len(s.envelopes) && len(out) < limit; i++ {
		se := s.envelopes[i]
		clonedAny := proto.Clone(se.env)
		cloned, ok := clonedAny.(*Envelope)
		if !ok {
			// This should never happen. If it does, it indicates
			// a programming error.
			//
			// Returning the original pointer would break Pull's
			// isolation guarantee by allowing callers to mutate
			// stored envelopes.
			panic(fmt.Sprintf(
				"unexpected clone type %T", clonedAny,
			))
		}

		out = append(out, cloned)
	}

	return out
}

// gcLocked discards any envelopes older than the ack cursor.
//
// gcLocked requires s.mu to be held.
func (s *mailboxState) gcLocked() {
	if s.ackCursor == 0 {
		return
	}

	keepAt := sort.Search(len(s.envelopes), func(i int) bool {
		return s.envelopes[i].seq >= s.ackCursor
	})

	if keepAt == 0 {
		return
	}

	s.envelopes = append(
		[]storedEnvelope(nil), s.envelopes[keepAt:]...,
	)
}
