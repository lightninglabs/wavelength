package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// capturingTellRef is a TellOnlyRef that stores every message it
// receives. Tests use it to capture the NotifyActor the ledger
// installs on the chain source, then drive it with a synthetic
// BlockEpoch to assert the mapper shape.
type capturingTellRef struct {
	mu       sync.Mutex
	id       string
	received []LedgerMsg
}

// newCapturingTellRef constructs a capturingTellRef with the
// given actor identifier.
func newCapturingTellRef(id string) *capturingTellRef {
	return &capturingTellRef{id: id}
}

// ID returns the mock actor identifier.
func (c *capturingTellRef) ID() string { return c.id }

// Tell records the message in the internal buffer.
func (c *capturingTellRef) Tell(_ context.Context, msg LedgerMsg) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.received = append(c.received, msg)

	return nil
}

// messages returns a copy of every captured message.
func (c *capturingTellRef) messages() []LedgerMsg {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]LedgerMsg{}, c.received...)
}

// chainSourceRequest is one entry in mockChainSource's captured
// request log. Ask and Tell requests are appended in call order
// so tests can assert both the operation and the payload.
type chainSourceRequest struct {
	method string
	msg    chainsource.ChainSourceMsg
}

// mockChainSource is a minimal ActorRef stand-in. It records every
// Ask/Tell and completes Asks with a pre-seeded response (or
// error). Keeping the mock hand-rolled avoids the testify/mock
// ceremony for what is a two-method surface and lets assertions
// reach into the exact request instance the ledger sent.
type mockChainSource struct {
	mu sync.Mutex

	id        string
	askResult fn.Result[chainsource.ChainSourceResp]

	// tellErr is returned from Tell when non-nil. Lets tests
	// drive the unsubscribe warn-and-continue path without
	// wiring a second mock type.
	tellErr error

	requests []chainSourceRequest
}

// newMockChainSource constructs a mockChainSource that completes
// Ask calls with the given result. Tests that only care about the
// Ask payload can pass fn.Ok(&chainsource.SubscribeBlocksResponse{}).
func newMockChainSource(
	askResult fn.Result[chainsource.ChainSourceResp]) *mockChainSource {

	return &mockChainSource{
		id:        "mock-chain-source",
		askResult: askResult,
	}
}

// ID returns the mock actor identifier.
func (m *mockChainSource) ID() string { return m.id }

// Tell records the message and returns tellErr (nil by default).
func (m *mockChainSource) Tell(_ context.Context,
	msg chainsource.ChainSourceMsg) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests = append(m.requests, chainSourceRequest{
		method: "Tell",
		msg:    msg,
	})

	return m.tellErr
}

// Ask records the message and returns a pre-completed future.
func (m *mockChainSource) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	m.mu.Lock()
	m.requests = append(m.requests, chainSourceRequest{
		method: "Ask",
		msg:    msg,
	})
	result := m.askResult
	m.mu.Unlock()

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	promise.Complete(result)

	return promise.Future()
}

// capturedRequests returns a copy of the request log in call
// order.
func (m *mockChainSource) capturedRequests() []chainSourceRequest {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]chainSourceRequest{}, m.requests...)
}

// Compile-time check that mockChainSource satisfies the
// ActorRef[M,R] shape the ledger expects.
var _ actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
] = (*mockChainSource)(nil)

// newSubscribeTestActor builds a LedgerActor wired with an
// optional ChainSource and a deterministic actor ID. The utxo
// tracker and clock match the other handler tests so assertions
// stay consistent across the package.
func newSubscribeTestActor(t *testing.T, actorID string,
	cs fn.Option[actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]]) *LedgerActor {

	t.Helper()

	return &LedgerActor{
		cfg: ActorConfig{
			LedgerStore:     &mockLedgerStore{},
			TreasuryTracker: fees.NewTreasuryTracker(),
			ChainSource:     cs,
		},
		actorID: actorID,
		log:     disabledLogger(),
		clk:     clock.NewTestClock(fixedTestTime()),
		utxo:    newUTXOTracker(),
	}
}

// TestBlockEpochToLedgerMsg verifies the chainsource → ledger
// adapter copies the hash byte-for-byte and narrows Height to
// uint32 without truncation in the positive range. The mapper
// is the hot path inside MapBlockEpoch so asserting it
// standalone catches drift that a full-actor test would only
// surface indirectly.
func TestBlockEpochToLedgerMsg(t *testing.T) {
	t.Parallel()

	var hash chainhash.Hash
	for i := range hash {
		hash[i] = byte(i * 3)
	}

	out := blockEpochToLedgerMsg(chainsource.BlockEpoch{
		Height:    12345,
		Hash:      hash,
		Timestamp: 42,
	})

	msg, ok := out.(*BlockEpochMsg)
	require.True(t, ok, "expected *BlockEpochMsg, got %T", out)
	require.Equal(t, uint32(12345), msg.BlockHeight)
	require.Equal(t, [32]byte(hash), msg.BlockHash)
}

// TestSubscribeBlockEpochsNoChainSource verifies the subscribe
// path degrades cleanly when no chain source is configured.
// Production deployments must wire one, but unit-test harnesses
// that drive handleBlockEpoch directly need to Start without
// panicking on the None branch.
func TestSubscribeBlockEpochsNoChainSource(t *testing.T) {
	t.Parallel()

	a := newSubscribeTestActor(
		t, "test-no-cs", fn.None[actor.ActorRef[
			chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
		]](),
	)

	target := newCapturingTellRef("target")
	require.NoError(
		t,
		a.subscribeBlockEpochs(
			context.Background(), target,
		),
	)
	require.Empty(
		t, target.messages(),
		"no target messages expected when chain source is None",
	)
}

// TestSubscribeBlockEpochsRegistersCallback verifies the
// subscribe path asks the chain source with the derived CallerID
// and installs a NotifyActor that routes BlockEpoch notifications
// through blockEpochToLedgerMsg onto the target ref. This is the
// end-to-end contract for the UTXO diff subsystem: a block
// arriving at chainsource must become a BlockEpochMsg in the
// ledger mailbox.
func TestSubscribeBlockEpochsRegistersCallback(t *testing.T) {
	t.Parallel()

	cs := newMockChainSource(
		fn.Ok[chainsource.ChainSourceResp](
			&chainsource.SubscribeBlocksResponse{},
		),
	)
	a := newSubscribeTestActor(
		t, "test-sub",
		fn.Some(
			actor.ActorRef[
				chainsource.ChainSourceMsg,
				chainsource.ChainSourceResp,
			](
				cs,
			),
		),
	)

	target := newCapturingTellRef("target")
	require.NoError(
		t,
		a.subscribeBlockEpochs(
			context.Background(), target,
		),
	)

	reqs := cs.capturedRequests()
	require.Len(t, reqs, 1)
	require.Equal(t, "Ask", reqs[0].method)

	subReq, ok := reqs[0].msg.(*chainsource.SubscribeBlocksRequest)
	require.True(
		t, ok, "expected SubscribeBlocksRequest, got %T", reqs[0].msg,
	)
	require.Equal(t, a.blockEpochCallerID(), subReq.CallerID)
	require.Equal(t, "ledger.test-sub", subReq.CallerID)
	require.True(
		t, subReq.NotifyActor.IsSome(),
		"subscribe must install a NotifyActor",
	)

	// The NotifyActor installed with chainsource is a mapping
	// wrapper: Tell'ing it a chainsource.BlockEpoch must deliver
	// a BlockEpochMsg to the target.
	notifyRef := subReq.NotifyActor.UnsafeFromSome()

	var hash chainhash.Hash
	for i := range hash {
		hash[i] = byte(0xa0 + i)
	}
	err := notifyRef.Tell(
		context.Background(), chainsource.BlockEpoch{
			Height: 900_001,
			Hash:   hash,
		},
	)
	require.NoError(t, err)

	delivered := target.messages()
	require.Len(t, delivered, 1)
	msg, ok := delivered[0].(*BlockEpochMsg)
	require.True(t, ok, "expected *BlockEpochMsg, got %T",
		delivered[0])
	require.Equal(t, uint32(900_001), msg.BlockHeight)
	require.Equal(t, [32]byte(hash), msg.BlockHash)
}

// TestSubscribeBlockEpochsPropagatesError verifies that a
// chainsource Ask failure bubbles up to the caller instead of
// being swallowed. Start relies on this signal to roll back the
// durable runtime rather than leaving the actor half-wired with
// no block notifications.
func TestSubscribeBlockEpochsPropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("chain source unavailable")
	cs := newMockChainSource(fn.Err[chainsource.ChainSourceResp](wantErr))
	a := newSubscribeTestActor(
		t, "test-err",
		fn.Some(
			actor.ActorRef[
				chainsource.ChainSourceMsg,
				chainsource.ChainSourceResp,
			](
				cs,
			),
		),
	)

	err := a.subscribeBlockEpochs(
		context.Background(), newCapturingTellRef("target"),
	)
	require.ErrorIs(t, err, wantErr)
}

// TestUnsubscribeBlockEpochsNoChainSource verifies the
// unsubscribe path is a no-op when the actor was never wired to
// a chain source. Stop calls this unconditionally; a nil-deref
// here would block shutdown.
func TestUnsubscribeBlockEpochsNoChainSource(t *testing.T) {
	t.Parallel()

	a := newSubscribeTestActor(
		t, "test-unsub-no-cs", fn.None[actor.ActorRef[
			chainsource.ChainSourceMsg,
			chainsource.ChainSourceResp,
		]](),
	)

	// No panic and no side effects.
	a.unsubscribeBlockEpochs(context.Background())
}

// TestUnsubscribeBlockEpochsSendsCancel verifies the unsubscribe
// path tells the chain source with the same CallerID the
// subscribe path used. A mismatch would leak the subscription
// inside chainsource's registration map and let a stopped ledger
// keep receiving block epochs until process exit.
func TestUnsubscribeBlockEpochsSendsCancel(t *testing.T) {
	t.Parallel()

	cs := newMockChainSource(
		fn.Ok[chainsource.ChainSourceResp](
			&chainsource.UnsubscribeBlocksResponse{},
		),
	)
	a := newSubscribeTestActor(
		t, "test-unsub",
		fn.Some(
			actor.ActorRef[
				chainsource.ChainSourceMsg,
				chainsource.ChainSourceResp,
			](
				cs,
			),
		),
	)

	a.unsubscribeBlockEpochs(context.Background())

	reqs := cs.capturedRequests()
	require.Len(t, reqs, 1)
	require.Equal(t, "Tell", reqs[0].method)

	unsubReq, ok := reqs[0].msg.(*chainsource.UnsubscribeBlocksRequest)
	require.True(
		t, ok, "expected UnsubscribeBlocksRequest, got %T", reqs[0].msg,
	)
	require.Equal(t, a.blockEpochCallerID(), unsubReq.CallerID)
}

// TestUnsubscribeBlockEpochsSwallowsTellError verifies the
// unsubscribe path logs-and-continues when the chain source's
// Tell fails. Stop has no return value, so if this path panicked
// or leaked the error it would either crash shutdown or hide a
// genuine bug -- the contract is "warn and move on".
func TestUnsubscribeBlockEpochsSwallowsTellError(t *testing.T) {
	t.Parallel()

	cs := newMockChainSource(
		fn.Ok[chainsource.ChainSourceResp](
			&chainsource.UnsubscribeBlocksResponse{},
		),
	)
	cs.tellErr = errors.New("chain source gone")

	a := newSubscribeTestActor(
		t, "test-unsub-err",
		fn.Some(
			actor.ActorRef[
				chainsource.ChainSourceMsg,
				chainsource.ChainSourceResp,
			](
				cs,
			),
		),
	)

	// Must not panic and must not propagate the error.
	a.unsubscribeBlockEpochs(context.Background())

	reqs := cs.capturedRequests()
	require.Len(t, reqs, 1)
	require.Equal(t, "Tell", reqs[0].method)
}
