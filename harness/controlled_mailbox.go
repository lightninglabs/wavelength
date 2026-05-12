package harness

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// ControlledMailboxClient wraps a mailbox edge client and can pause selected
// outbound message types before they reach the operator.
type ControlledMailboxClient struct {
	mu sync.Mutex

	inner mailboxpb.MailboxServiceClient

	pausedTypes map[string]struct{}
	pending     []*mailboxpb.Envelope
}

// NewControlledMailboxClient creates an empty controlled mailbox wrapper.
func NewControlledMailboxClient() *ControlledMailboxClient {
	return &ControlledMailboxClient{
		pausedTypes: make(map[string]struct{}),
	}
}

// SetInner updates the currently active mailbox edge client.
func (m *ControlledMailboxClient) SetInner(
	inner mailboxpb.MailboxServiceClient) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.inner = inner
}

// PauseType starts buffering outbound sends whose body type matches typeName.
func (m *ControlledMailboxClient) PauseType(typeName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pausedTypes[typeName] = struct{}{}
}

// ResumeType stops buffering outbound sends for the given message type.
func (m *ControlledMailboxClient) ResumeType(typeName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.pausedTypes, typeName)
}

// ClearPausedTypes removes all message-type pauses.
func (m *ControlledMailboxClient) ClearPausedTypes() {
	m.mu.Lock()
	defer m.mu.Unlock()

	clear(m.pausedTypes)
}

// PendingCount returns the total number of buffered envelopes.
func (m *ControlledMailboxClient) PendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.pending)
}

// PendingTypeCount returns the number of buffered envelopes with the requested
// friendly body type name.
func (m *ControlledMailboxClient) PendingTypeCount(typeName string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	var count int
	for _, env := range m.pending {
		if extractFriendlyTypeName(env) == typeName {
			count++
		}
	}

	return count
}

// DropAllPending discards all buffered envelopes without sending them.
func (m *ControlledMailboxClient) DropAllPending() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pending = nil
}

// WaitForPendingType blocks until a buffered envelope of the requested type is
// present or the context expires.
func (m *ControlledMailboxClient) WaitForPendingType(ctx context.Context,
	typeName string) error {

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		if m.PendingTypeCount(typeName) > 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
		}
	}
}

// FlushAll delivers all buffered envelopes through the current inner mailbox
// client in FIFO order.
func (m *ControlledMailboxClient) FlushAll() error {
	for {
		m.mu.Lock()
		if len(m.pending) == 0 {
			m.mu.Unlock()

			return nil
		}

		env := m.pending[0]
		m.pending = m.pending[1:]
		inner := m.inner
		m.mu.Unlock()

		if inner == nil {
			m.mu.Lock()
			m.pending = append(
				[]*mailboxpb.Envelope{env}, m.pending...,
			)
			m.mu.Unlock()

			return fmt.Errorf("inner mailbox client not configured")
		}

		_, err := inner.Send(
			context.Background(), &mailboxpb.SendRequest{
				Envelope: env,
			},
		)
		if err != nil {
			m.mu.Lock()
			m.pending = append(
				[]*mailboxpb.Envelope{env}, m.pending...,
			)
			m.mu.Unlock()

			return err
		}
	}
}

// Send buffers paused message types and otherwise forwards the request to the
// wrapped mailbox edge client.
func (m *ControlledMailboxClient) Send(ctx context.Context,
	in *mailboxpb.SendRequest, opts ...grpc.CallOption) (
	*mailboxpb.SendResponse, error) {

	if in == nil || in.Envelope == nil {
		return nil, fmt.Errorf("send request must include an envelope")
	}

	typeName := extractFriendlyTypeName(in.Envelope)

	m.mu.Lock()
	_, paused := m.pausedTypes[typeName]
	if paused {
		m.pending = append(m.pending, cloneEnvelope(in.Envelope))
		m.mu.Unlock()

		return &mailboxpb.SendResponse{}, nil
	}

	inner := m.inner
	m.mu.Unlock()

	if inner == nil {
		return nil, fmt.Errorf("inner mailbox client not configured")
	}

	return inner.Send(ctx, in, opts...)
}

// Pull delegates to the wrapped mailbox edge client.
func (m *ControlledMailboxClient) Pull(ctx context.Context,
	in *mailboxpb.PullRequest, opts ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	inner, err := m.innerClient()
	if err != nil {
		return nil, err
	}

	return inner.Pull(ctx, in, opts...)
}

// AckUpTo delegates to the wrapped mailbox edge client.
func (m *ControlledMailboxClient) AckUpTo(ctx context.Context,
	in *mailboxpb.AckUpToRequest, opts ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	inner, err := m.innerClient()
	if err != nil {
		return nil, err
	}

	return inner.AckUpTo(ctx, in, opts...)
}

func (m *ControlledMailboxClient) innerClient() (mailboxpb.MailboxServiceClient,
	error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.inner == nil {
		return nil, fmt.Errorf("inner mailbox client not configured")
	}

	return m.inner, nil
}

func extractFriendlyTypeName(env *mailboxpb.Envelope) string {
	if env == nil || env.Body == nil {
		return ""
	}

	typeURL := env.Body.GetTypeUrl()
	if typeURL == "" {
		return ""
	}

	if idx := strings.LastIndexByte(typeURL, '.'); idx >= 0 {
		return typeURL[idx+1:]
	}

	if idx := strings.LastIndexByte(typeURL, '/'); idx >= 0 {
		return typeURL[idx+1:]
	}

	return typeURL
}

func cloneEnvelope(env *mailboxpb.Envelope) *mailboxpb.Envelope {
	cloned, ok := proto.Clone(env).(*mailboxpb.Envelope)
	if !ok {
		panic("cloned envelope has unexpected type")
	}

	return cloned
}

var _ mailboxpb.MailboxServiceClient = (*ControlledMailboxClient)(nil)
