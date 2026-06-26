package credit

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newTestDelivery builds a real sqlite-backed durable mailbox delivery store,
// the same transport the daemon wires in production. The credit control-plane
// store is faked in-memory (it is decoupled via the credit.Store interface,
// mirroring how the oor registry tests fake their control-plane store).
func newTestDelivery(t *testing.T) actor.DeliveryStore {
	t.Helper()

	sqlDB := db.NewTestDB(t)
	delivery, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB, sqlDB.Backend(), clock.NewDefaultClock(),
		btclog.Disabled,
	)
	require.NoError(t, err)

	return delivery
}

// waitForState polls the store until the operation reaches the wanted state or
// the deadline elapses.
func waitForState(t *testing.T, store Store, opID string, want State) {
	t.Helper()

	require.Eventually(t, func() bool {
		rec, err := store.GetOperation(context.Background(), opID)
		if err != nil {
			return false
		}

		return State(rec.State) == want
	}, 5*time.Second, 20*time.Millisecond)
}

// TestRegistryPayNoTopupEndToEnd drives the full stack: a registry admission
// durably pre-writes the row, spawns the per-operation child, and the child
// drives the FSM to completion through the real durable mailbox.
func TestRegistryPayNoTopupEndToEnd(t *testing.T) {
	t.Parallel()

	store, delivery := newFakeStore(), newTestDelivery(t)
	server, daemon := newFakeServer(), newFakeDaemon()

	registry, err := NewRegistry(RegistryConfig{
		Store:         store,
		DeliveryStore: delivery,
		Server:        server,
		Daemon:        daemon,
		PollInterval:  50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)

	ctx := context.Background()
	resp, err := registry.Ref().Ask(ctx, &StartCreditPayRequest{
		OpKey:        "pay:nofee",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		TopupSat:     0,
		MaxCreditSat: 500,
	}).Await(ctx).Unpack()
	require.NoError(t, err)

	start, ok := resp.(*StartCreditResponse)
	require.True(t, ok)
	require.NotEmpty(t, start.OpID)
	require.False(t, start.Existing)

	waitForState(t, store, start.OpID, StateCompleted)
	require.Equal(t, 1, server.startPayCnt)
}

// TestRegistryDedupReturnsExisting asserts a second admission under the same op
// key returns the already-admitted operation instead of creating a new one.
func TestRegistryDedupReturnsExisting(t *testing.T) {
	t.Parallel()

	store, delivery := newFakeStore(), newTestDelivery(t)
	server, daemon := newFakeServer(), newFakeDaemon()
	server.startPayErr = errContextStub{} // park the op before completion

	registry, err := NewRegistry(RegistryConfig{
		Store:         store,
		DeliveryStore: delivery,
		Server:        server,
		Daemon:        daemon,
		PollInterval:  50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)

	ctx := context.Background()
	req := func() *StartCreditPayRequest {
		return &StartCreditPayRequest{
			OpKey:        "pay:dup",
			Invoice:      "lnbc1",
			PaymentHash:  payHash(),
			AmountSat:    500,
			MaxCreditSat: 500,
		}
	}

	first, err := registry.Ref().Ask(ctx, req()).Await(ctx).Unpack()
	require.NoError(t, err)
	firstResp, ok := first.(*StartCreditResponse)
	require.True(t, ok)
	require.False(t, firstResp.Existing)

	// Let the row commit before the second admission dedups against it.
	require.Eventually(t, func() bool {
		_, err := store.GetOperation(ctx, firstResp.OpID)

		return err == nil
	}, 5*time.Second, 20*time.Millisecond)

	second, err := registry.Ref().Ask(ctx, req()).Await(ctx).Unpack()
	require.NoError(t, err)
	secondResp, ok := second.(*StartCreditResponse)
	require.True(t, ok)
	require.True(t, secondResp.Existing)
	require.Equal(t, firstResp.OpID, secondResp.OpID)
}

// TestRegistryReceiveReturnsInvoice asserts a receive admission creates the
// server-owned invoice synchronously and returns it in the response, while the
// durable row advances to awaiting settlement.
func TestRegistryReceiveReturnsInvoice(t *testing.T) {
	t.Parallel()

	store, delivery := newFakeStore(), newTestDelivery(t)
	server, daemon := newFakeServer(), newFakeDaemon()

	registry, err := NewRegistry(RegistryConfig{
		Store:         store,
		DeliveryStore: delivery,
		Server:        server,
		Daemon:        daemon,
		PollInterval:  50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)

	ctx := context.Background()
	resp, err := registry.Ref().Ask(ctx, &StartCreditReceiveRequest{
		OpKey:     "recv:abc",
		AmountSat: 42,
		Memo:      "coffee",
	}).Await(ctx).Unpack()
	require.NoError(t, err)

	start, ok := resp.(*StartCreditResponse)
	require.True(t, ok)
	require.NotEmpty(t, start.OpID)
	require.Equal(t, "lnbc-recv:abc", start.Invoice)
	require.NotEmpty(t, start.PaymentHash)

	// The durable row was created synchronously, already past invoice
	// creation, awaiting settlement.
	rec, err := store.GetOperation(ctx, start.OpID)
	require.NoError(t, err)
	require.Equal(t, string(StateAwaitingSettlement), rec.State)
	require.Equal(t, "lnbc-recv:abc", rec.Invoice)
	require.Equal(t, 1, server.createCalls["recv:abc"])
}

// TestRegistryListSurfacesTerminalCreditOnly drives a credit-only pay to
// completion, then asserts the full list surfaces the terminal row (carrying
// the credit-only marker so the wallet projector can own its transition) while
// the pending-only list excludes it.
func TestRegistryListSurfacesTerminalCreditOnly(t *testing.T) {
	t.Parallel()

	store, delivery := newFakeStore(), newTestDelivery(t)
	server, daemon := newFakeServer(), newFakeDaemon()

	registry, err := NewRegistry(RegistryConfig{
		Store:         store,
		DeliveryStore: delivery,
		Server:        server,
		Daemon:        daemon,
		PollInterval:  50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)

	ctx := context.Background()
	resp, err := registry.Ref().Ask(ctx, &StartCreditPayRequest{
		OpKey:        "pay:creditonly",
		Invoice:      "lnbc1",
		PaymentHash:  payHash(),
		AmountSat:    500,
		MaxCreditSat: 500,
		CreditOnly:   true,
	}).Await(ctx).Unpack()
	require.NoError(t, err)
	start, ok := resp.(*StartCreditResponse)
	require.True(t, ok)
	require.NotEmpty(t, start.OpID)

	// A credit-only pay reconciles to settlement against the server ledger,
	// so the server must mark the pay operation debited before the FSM
	// completes.
	ph := payHash()
	server.setPayOp(ph[:], ServerStateDebited)

	waitForState(t, store, start.OpID, StateCompleted)

	// The full list surfaces the terminal op with the credit-only marker.
	full, err := registry.Ref().Ask(
		ctx, &ListCreditOpsRequest{PendingOnly: false},
	).Await(ctx).Unpack()
	require.NoError(t, err)
	fullList, ok := full.(*ListCreditOpsResponse)
	require.True(t, ok)

	op, found := findOp(fullList.Ops, start.OpID)
	require.True(t, found, "terminal op missing from full list")
	require.Equal(t, StateCompleted, op.State)
	require.False(t, op.Pending)
	require.True(t, op.CreditOnly)

	// The pending-only list excludes the terminal op.
	pendingResp, err := registry.Ref().Ask(
		ctx, &ListCreditOpsRequest{PendingOnly: true},
	).Await(ctx).Unpack()
	require.NoError(t, err)
	pendingList, ok := pendingResp.(*ListCreditOpsResponse)
	require.True(t, ok)
	_, found = findOp(pendingList.Ops, start.OpID)
	require.False(t, found, "terminal op leaked into pending-only list")
}

// findOp returns the summary with the given op id, if present.
func findOp(ops []CreditOpSummary, opID string) (CreditOpSummary, bool) {
	for _, op := range ops {
		if op.OpID == opID {
			return op, true
		}
	}

	return CreditOpSummary{}, false
}

// errContextStub is a stand-in transient error that keeps a pay parked in the
// paying state without completing.
type errContextStub struct{}

func (errContextStub) Error() string { return "stubbed transient pay error" }
