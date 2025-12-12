package rounds

import (
	"context"
	"sync"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
)

// mockClientConnRef implements actor.TellOnlyRef[clientconn.ClientConnMsg]
// and captures all messages sent to clients.
type mockClientConnRef struct {
	t        *testing.T
	id       string
	messages []clientconn.ClientConnMsg
	mu       sync.Mutex
}

// newMockClientConnRef creates a new mock client connection reference.
func newMockClientConnRef(t *testing.T) *mockClientConnRef {
	return &mockClientConnRef{
		t:        t,
		id:       "mock-clients-conn",
		messages: make([]clientconn.ClientConnMsg, 0),
	}
}

// ID returns the ID of this mock actor reference.
func (m *mockClientConnRef) ID() string {
	return m.id
}

// Tell captures a message sent to clients.
func (m *mockClientConnRef) Tell(_ context.Context,
	msg clientconn.ClientConnMsg) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, msg)
}

// getMessages returns a copy of all captured messages.
//
//nolint:unused
func (m *mockClientConnRef) getMessages() []clientconn.ClientConnMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]clientconn.ClientConnMsg{}, m.messages...)
}

// clearMessages clears all captured messages.
//
//nolint:unused
func (m *mockClientConnRef) clearMessages() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = nil
}

// actorTestHarness provides test infrastructure for the rounds Actor.
type actorTestHarness struct {
	t *testing.T

	actor   *Actor
	cfg     *ActorConfig
	clients *mockClientConnRef
}

// newActorTestHarness creates a new actor test harness with default
// configuration.
func newActorTestHarness(t *testing.T) *actorTestHarness {
	t.Helper()

	clients := newMockClientConnRef(t)

	cfg := &ActorConfig{
		Logger:      btclog.Disabled,
		ClientsConn: clients,
	}

	actorResult := NewActor(cfg)
	actor, err := actorResult.Unpack()
	require.NoError(t, err)

	return &actorTestHarness{
		t:       t,
		actor:   actor,
		cfg:     cfg,
		clients: clients,
	}
}

// start initializes the actor by calling Start.
func (h *actorTestHarness) start(ctx context.Context) {
	h.t.Helper()

	err := h.actor.Start(ctx)
	require.NoError(h.t, err)
}

// assertRoundCount verifies the actor is tracking the expected number of
// rounds.
func (h *actorTestHarness) assertRoundCount(expected int) {
	h.t.Helper()

	require.Len(h.t, h.actor.rounds, expected)
}

// assertCurrentRoundExists verifies the actor has a current round set.
func (h *actorTestHarness) assertCurrentRoundExists() {
	h.t.Helper()

	require.NotNil(h.t, h.actor.currentRound)
}

// TestActorStart verifies that the actor correctly initializes on Start,
// creating a current round FSM.
func TestActorStart(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.start(t.Context())

	// Verify actor created a current round.
	h.assertCurrentRoundExists()
	h.assertRoundCount(1)

	// Verify round ID is set.
	require.NotEmpty(t, h.actor.currentRound.RoundID)
}
