//go:build systest

package systest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/subscribe"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// serverMailboxPrefix is the mailbox ID prefix for server-side
// per-client mailboxes. Envelopes sent to these mailboxes are
// client-to-server messages.
const serverMailboxPrefix = "server-for-"

// InstrumentedMailbox wraps a production MailboxServiceClient with
// transcript recording, per-direction per-client buffering, and event
// subscription. It implements mailboxpb.MailboxServiceClient so both
// server-side (PerClientConfig.Edge) and client-side
// (ConnectorConfig.Edge) connectors can use it as their transport.
//
// Buffering intercepts at Send() — when a direction is buffered for a
// client, the envelope is held in a pending queue and not forwarded to
// the underlying mailbox. Flush methods release pending envelopes.
//
// Only Send() records to the transcript (not Pull) to avoid the
// double-publishing bug identified in PR #132 review.
type InstrumentedMailbox struct {
	// inner is the underlying production mailbox client.
	inner mailboxpb.MailboxServiceClient

	// transcript records all envelope traffic for test assertions.
	transcript *MessageTranscript

	mu sync.Mutex

	// bufferedC2S tracks whether C2S messages are buffered per
	// client.
	bufferedC2S map[clientconn.ClientID]bool

	// bufferedS2C tracks whether S2C messages are buffered per
	// client.
	bufferedS2C map[clientconn.ClientID]bool

	// pendingC2S holds buffered client-to-server envelopes per
	// client.
	pendingC2S map[clientconn.ClientID][]*mailboxpb.Envelope

	// pendingS2C holds buffered server-to-client envelopes per
	// client.
	pendingS2C map[clientconn.ClientID][]*mailboxpb.Envelope

	// serverMailboxes maps server mailbox IDs ("server-for-X") to
	// the corresponding client ID for direction detection.
	serverMailboxes map[string]clientconn.ClientID

	// clientMailboxes maps client mailbox IDs to their client ID.
	clientMailboxes map[string]clientconn.ClientID

	// eventServers maps client IDs to subscribe.Server instances
	// that broadcast S2C events. Used by WaitForEvent and
	// Subscribe.
	eventServers map[clientconn.ClientID]*subscribe.Server
}

// NewInstrumentedMailbox creates a new instrumented mailbox wrapper
// around the given production mailbox client.
func NewInstrumentedMailbox(inner mailboxpb.MailboxServiceClient,
	transcript *MessageTranscript) *InstrumentedMailbox {

	return &InstrumentedMailbox{
		inner:           inner,
		transcript:      transcript,
		bufferedC2S:     make(map[clientconn.ClientID]bool),
		bufferedS2C:     make(map[clientconn.ClientID]bool),
		pendingC2S:      make(map[clientconn.ClientID][]*mailboxpb.Envelope),
		pendingS2C:      make(map[clientconn.ClientID][]*mailboxpb.Envelope),
		serverMailboxes: make(map[string]clientconn.ClientID),
		clientMailboxes: make(map[string]clientconn.ClientID),
		eventServers:    make(map[clientconn.ClientID]*subscribe.Server),
	}
}

// RegisterMailboxPair registers a client's mailbox pair for direction
// detection and event subscription. The server mailbox ID is the
// server's per-client mailbox (e.g., "server-for-alice"), and the
// client mailbox ID is the client's mailbox (e.g., "alice").
func (m *InstrumentedMailbox) RegisterMailboxPair(
	clientID clientconn.ClientID,
	serverMBID, clientMBID string) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.serverMailboxes[serverMBID] = clientID
	m.clientMailboxes[clientMBID] = clientID

	// Create a subscribe.Server for this client if one doesn't
	// exist already.
	if _, ok := m.eventServers[clientID]; !ok {
		m.eventServers[clientID] = subscribe.NewServer()
	}
}

// UnregisterClient removes all state for a client. Called when a
// client is stopped for restart testing.
func (m *InstrumentedMailbox) UnregisterClient(
	clientID clientconn.ClientID) {

	m.mu.Lock()
	defer m.mu.Unlock()

	// Remove mailbox ID mappings.
	for mbID, cID := range m.serverMailboxes {
		if cID == clientID {
			delete(m.serverMailboxes, mbID)
		}
	}
	for mbID, cID := range m.clientMailboxes {
		if cID == clientID {
			delete(m.clientMailboxes, mbID)
		}
	}

	// Stop the event server.
	if server, ok := m.eventServers[clientID]; ok {
		server.Stop()
		delete(m.eventServers, clientID)
	}

	// Clear pending buffers.
	delete(m.bufferedC2S, clientID)
	delete(m.bufferedS2C, clientID)
	delete(m.pendingC2S, clientID)
	delete(m.pendingS2C, clientID)
}

// SetBufferedC2S enables or disables buffering for client-to-server
// messages for a specific client. When enabled, Send() holds C2S
// envelopes in a pending queue.
func (m *InstrumentedMailbox) SetBufferedC2S(
	clientID clientconn.ClientID, buffered bool) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.bufferedC2S[clientID] = buffered
}

// SetBufferedS2C enables or disables buffering for server-to-client
// messages for a specific client. When enabled, Send() holds S2C
// envelopes in a pending queue.
func (m *InstrumentedMailbox) SetBufferedS2C(
	clientID clientconn.ClientID, buffered bool) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.bufferedS2C[clientID] = buffered
}

// FlushNextC2S delivers the next buffered C2S envelope for the given
// client to the underlying mailbox.
func (m *InstrumentedMailbox) FlushNextC2S(
	clientID clientconn.ClientID) error {

	m.mu.Lock()
	pending := m.pendingC2S[clientID]
	if len(pending) == 0 {
		m.mu.Unlock()

		return fmt.Errorf(
			"no buffered C2S messages for client %s",
			clientID,
		)
	}

	env := pending[0]
	m.pendingC2S[clientID] = pending[1:]
	m.mu.Unlock()

	_, err := m.inner.Send(context.Background(),
		&mailboxpb.SendRequest{Envelope: env},
	)

	return err
}

// FlushAllC2S delivers all buffered C2S envelopes for the given
// client to the underlying mailbox.
func (m *InstrumentedMailbox) FlushAllC2S(
	clientID clientconn.ClientID) error {

	m.mu.Lock()
	pending := m.pendingC2S[clientID]
	m.pendingC2S[clientID] = nil
	m.mu.Unlock()

	for _, env := range pending {
		_, err := m.inner.Send(context.Background(),
			&mailboxpb.SendRequest{Envelope: env},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// FlushNextS2C delivers the next buffered S2C envelope for the given
// client to the underlying mailbox and notifies event subscribers.
func (m *InstrumentedMailbox) FlushNextS2C(
	clientID clientconn.ClientID) error {

	m.mu.Lock()
	pending := m.pendingS2C[clientID]
	if len(pending) == 0 {
		m.mu.Unlock()

		return fmt.Errorf(
			"no buffered S2C messages for client %s",
			clientID,
		)
	}

	env := pending[0]
	m.pendingS2C[clientID] = pending[1:]
	m.mu.Unlock()

	_, err := m.inner.Send(context.Background(),
		&mailboxpb.SendRequest{Envelope: env},
	)
	if err != nil {
		return err
	}

	m.notifySubscribers(clientID, env)

	return nil
}

// FlushAllS2C delivers all buffered S2C envelopes for the given
// client and notifies event subscribers for each flushed envelope.
func (m *InstrumentedMailbox) FlushAllS2C(
	clientID clientconn.ClientID) error {

	m.mu.Lock()
	pending := m.pendingS2C[clientID]
	m.pendingS2C[clientID] = nil
	m.mu.Unlock()

	for _, env := range pending {
		_, err := m.inner.Send(context.Background(),
			&mailboxpb.SendRequest{Envelope: env},
		)
		if err != nil {
			return err
		}

		m.notifySubscribers(clientID, env)
	}

	return nil
}

// PendingC2SCount returns the number of buffered C2S messages for a
// client.
func (m *InstrumentedMailbox) PendingC2SCount(
	clientID clientconn.ClientID) int {

	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.pendingC2S[clientID])
}

// PendingS2CCount returns the number of buffered S2C messages for a
// client.
func (m *InstrumentedMailbox) PendingS2CCount(
	clientID clientconn.ClientID) int {

	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.pendingS2C[clientID])
}

// Subscribe returns a subscribe.Client that receives all S2C events
// delivered to the given client. The subscription must be canceled
// when no longer needed.
func (m *InstrumentedMailbox) Subscribe(
	clientID clientconn.ClientID) (*subscribe.Client, error) {

	m.mu.Lock()
	server, ok := m.eventServers[clientID]
	m.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf(
			"no event server for client %s "+
				"(call RegisterMailboxPair first)",
			clientID,
		)
	}

	return server.Subscribe()
}

// WaitForEvent waits for an event matching the predicate on the given
// client's subscription. Returns the matching envelope or an error on
// timeout.
func (m *InstrumentedMailbox) WaitForEvent(
	clientID clientconn.ClientID,
	predicate func(*mailboxpb.Envelope) bool,
	timeout time.Duration) (*mailboxpb.Envelope, error) {

	sub, err := m.Subscribe(clientID)
	if err != nil {
		return nil, err
	}
	defer sub.Cancel()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case update := <-sub.Updates():
			env, ok := update.(*mailboxpb.Envelope)
			if !ok {
				continue
			}

			if predicate(env) {
				return env, nil
			}

		case <-sub.Quit():
			return nil, fmt.Errorf("subscription closed")

		case <-timer.C:
			return nil, fmt.Errorf(
				"timeout waiting for event for "+
					"client %s", clientID,
			)
		}
	}
}

// Send implements mailboxpb.MailboxServiceClient. It intercepts
// outbound envelopes to record transcript entries, detect direction,
// and optionally buffer messages.
func (m *InstrumentedMailbox) Send(ctx context.Context,
	in *mailboxpb.SendRequest,
	_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	if in == nil || in.Envelope == nil {
		return nil, fmt.Errorf("nil request or envelope")
	}

	env := in.Envelope

	// Detect direction and client ID.
	dir, clientID := m.detectDirection(env)
	typeName := extractFriendlyTypeName(env)

	// Always record to transcript, even when buffering.
	m.transcript.RecordTypeName(dir, clientID, typeName)

	// Check buffering and hold envelope if needed.
	m.mu.Lock()
	switch dir {
	case ClientToServer:
		if m.bufferedC2S[clientID] {
			clone := cloneEnvelope(env)
			m.pendingC2S[clientID] = append(
				m.pendingC2S[clientID], clone,
			)
			m.mu.Unlock()

			return &mailboxpb.SendResponse{}, nil
		}

	case ServerToClient:
		if m.bufferedS2C[clientID] {
			clone := cloneEnvelope(env)
			m.pendingS2C[clientID] = append(
				m.pendingS2C[clientID], clone,
			)
			m.mu.Unlock()

			return &mailboxpb.SendResponse{}, nil
		}
	}
	m.mu.Unlock()

	// Delegate to underlying mailbox.
	resp, err := m.inner.Send(ctx, in)
	if err != nil {
		return nil, err
	}

	// Notify event subscribers for S2C messages (unbuffered
	// path).
	if dir == ServerToClient {
		m.notifySubscribers(clientID, env)
	}

	return resp, nil
}

// Pull implements mailboxpb.MailboxServiceClient. Delegates to the
// underlying mailbox.
func (m *InstrumentedMailbox) Pull(ctx context.Context,
	in *mailboxpb.PullRequest,
	_ ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	if in == nil {
		return nil, fmt.Errorf("nil request")
	}

	return m.inner.Pull(ctx, in)
}

// AckUpTo implements mailboxpb.MailboxServiceClient. Passes through
// to the underlying mailbox.
func (m *InstrumentedMailbox) AckUpTo(ctx context.Context,
	in *mailboxpb.AckUpToRequest,
	_ ...grpc.CallOption) (*mailboxpb.AckUpToResponse, error) {

	if in == nil {
		return nil, fmt.Errorf("nil request")
	}

	return m.inner.AckUpTo(ctx, in)
}

// detectDirection determines the message direction and associated
// client ID from the envelope's sender and recipient fields.
func (m *InstrumentedMailbox) detectDirection(
	env *mailboxpb.Envelope) (
	MessageDirection, clientconn.ClientID) {

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if the recipient is a server mailbox (C2S).
	if clientID, ok := m.serverMailboxes[env.Recipient]; ok {
		return ClientToServer, clientID
	}

	// Check if the recipient is a client mailbox (S2C).
	if clientID, ok := m.clientMailboxes[env.Recipient]; ok {
		return ServerToClient, clientID
	}

	// Fallback: use the mailbox prefix convention.
	if strings.HasPrefix(env.Recipient, serverMailboxPrefix) {
		clientID := clientconn.ClientID(
			strings.TrimPrefix(
				env.Recipient, serverMailboxPrefix,
			),
		)

		return ClientToServer, clientID
	}

	// Default to S2C with recipient as client ID.
	return ServerToClient, clientconn.ClientID(env.Recipient)
}

// notifySubscribers sends the envelope to the event server for the
// given client.
func (m *InstrumentedMailbox) notifySubscribers(
	clientID clientconn.ClientID,
	env *mailboxpb.Envelope) {

	m.mu.Lock()
	server, ok := m.eventServers[clientID]
	m.mu.Unlock()

	if ok {
		_ = server.SendUpdate(env)
	}
}

// Transcript returns the underlying message transcript.
func (m *InstrumentedMailbox) Transcript() *MessageTranscript {
	return m.transcript
}

// extractFriendlyTypeName extracts a short type name from the
// envelope's body TypeUrl for backward-compatible transcript
// assertions.
//
// Examples:
//
//	"type.googleapis.com/roundpb.ClientSuccessResp" → "ClientSuccessResp"
//	"type.googleapis.com/oorpb.SubmitPackageRequest" → "SubmitPackageRequest"
func extractFriendlyTypeName(env *mailboxpb.Envelope) string {
	if env == nil || env.Body == nil {
		return "Unknown"
	}

	typeURL := env.Body.TypeUrl

	// Extract the segment after the last '.'.
	if idx := strings.LastIndex(typeURL, "."); idx >= 0 {
		return typeURL[idx+1:]
	}

	// Fallback: extract after last '/'.
	if idx := strings.LastIndex(typeURL, "/"); idx >= 0 {
		return typeURL[idx+1:]
	}

	return typeURL
}

// cloneEnvelope creates a deep copy of an envelope for buffering.
func cloneEnvelope(env *mailboxpb.Envelope) *mailboxpb.Envelope {
	return proto.Clone(env).(*mailboxpb.Envelope)
}

// EnvelopeBodyIs returns a predicate that matches envelopes whose
// body TypeUrl contains the given type name suffix.
func EnvelopeBodyIs(typeName string) func(*mailboxpb.Envelope) bool {
	return func(env *mailboxpb.Envelope) bool {
		if env.Body == nil {
			return false
		}

		return strings.HasSuffix(
			env.Body.TypeUrl, "."+typeName,
		)
	}
}

// Compile-time check that InstrumentedMailbox implements the
// MailboxServiceClient interface.
var _ mailboxpb.MailboxServiceClient = (*InstrumentedMailbox)(nil)
