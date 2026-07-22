package credit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
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

// TestRegistryReceiveAdmitTimeoutFallback verifies that the dedicated receive
// timeout preserves the legacy AdmitTimeout override while using its shorter
// default for callers that left both fields unset.
func TestRegistryReceiveAdmitTimeoutFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		admit   time.Duration
		receive time.Duration
		want    time.Duration
	}{
		{
			name: "default",
			want: DefaultReceiveAdmitTimeout,
		},
		{
			name:  "legacy override",
			admit: 7 * time.Second,
			want:  7 * time.Second,
		},
		{
			name:    "dedicated override",
			admit:   7 * time.Second,
			receive: 3 * time.Second,
			want:    3 * time.Second,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			registry, err := NewRegistry(RegistryConfig{
				Store:               newFakeStore(),
				DeliveryStore:       newTestDelivery(t),
				Server:              newFakeServer(),
				Daemon:              newFakeDaemon(),
				AdmitTimeout:        test.admit,
				ReceiveAdmitTimeout: test.receive,
			})
			require.NoError(t, err)
			t.Cleanup(registry.Stop)

			require.Equal(
				t, test.want,
				registry.behavior.cfg.ReceiveAdmitTimeout,
			)
		})
	}
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

// TestRegistryStopWaitsForResidentChildren asserts Registry.Stop does not
// return until the per-operation durable actors it owns have exited. The
// registry tests share a sqlite-backed delivery store with those children, so
// returning early can race t.TempDir cleanup against a child mailbox still
// touching the sqlite files.
func TestRegistryStopWaitsForResidentChildren(t *testing.T) {
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

	ctx := context.Background()
	resp, err := registry.Ref().Ask(ctx, &StartCreditReceiveRequest{
		OpKey:     "recv:stop",
		AmountSat: 42,
		Memo:      "shutdown",
	}).Await(ctx).Unpack()
	require.NoError(t, err)

	start, ok := resp.(*StartCreditResponse)
	require.True(t, ok)

	child := registry.behavior.active[start.OpID]
	require.NotNil(t, child)

	registry.Stop()

	waitCtx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()
	require.NoError(t, child.Wait(waitCtx))
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

// TestRegistryReapStoreErrorKeepsChild asserts that a transient row lookup
// error cannot stop a resident child. Reaping is safe only after a confirmed
// terminal or missing durable row.
func TestRegistryReapStoreErrorKeepsChild(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	opID := "op-reap-error"
	store.ops[opID] = db.CreditOperationRecord{
		OpID:   opID,
		State:  string(StatePaying),
		Status: db.CreditOpStatusPending,
	}
	store.getErr = errors.New("temporary store failure")

	behavior := &registryBehavior{
		cfg: RegistryConfig{
			Store: store,
		},
		log: btclog.Disabled,
		active: map[string]*OpActor{
			opID: {},
		},
	}

	behavior.reap(context.Background(), opID)
	require.Contains(t, behavior.active, opID)
}

// TestConsiderRedeemInterlock asserts the registry's no-pending-pay/redeem
// interlock: an auto-redeem signal admits a redeem only when no pay or redeem
// operation is already in flight that could consume the same fungible credits.
// A pending pay might spend the credits a redeem would materialize, and a
// pending redeem already owns the materialize, so both block; a pending receive
// only adds credits, so it does not. This is the money-safety guard that moved
// out of the deleted redeemDecision into considerRedeem, exercised here
// directly.
func TestConsiderRedeemInterlock(t *testing.T) {
	t.Parallel()

	seedRow := func(opID string, kind OpKind,
		state State) db.CreditOperationRecord {

		return db.CreditOperationRecord{
			OpID:   opID,
			OpKey:  "seed:" + opID,
			Kind:   kind,
			State:  string(state),
			Status: db.CreditOpStatusPending,
		}
	}

	cases := []struct {
		name       string
		seed       []db.CreditOperationRecord
		wantRedeem bool
	}{
		{
			name: "pending pay blocks redeem",
			seed: []db.CreditOperationRecord{
				seedRow("p", KindPay, StatePaying),
			},
			wantRedeem: false,
		},
		{
			name: "pending redeem blocks redeem",
			seed: []db.CreditOperationRecord{
				seedRow("r", KindRedeem, StateAwaitingOOR),
			},
			wantRedeem: false,
		},
		{
			name: "pending receive does not block",
			seed: []db.CreditOperationRecord{
				seedRow(
					"rc", KindReceive,
					StateAwaitingSettlement,
				),
			},
			wantRedeem: true,
		},
		{
			name:       "no in-flight ops admits redeem",
			seed:       nil,
			wantRedeem: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, delivery := newFakeStore(), newTestDelivery(t)
			server, daemon := newFakeServer(), newFakeDaemon()
			for _, rec := range tc.seed {
				store.ops[rec.OpID] = rec
			}

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
			require.NoError(
				t,
				registry.Ref().Tell(
					ctx, &ConsiderRedeemRequest{
						AvailableSat: 1000,
					},
				),
			)

			// A redeem admission is identified by the "redeem:"
			// op-key prefix considerRedeem mints, so a seeded
			// redeem row (keyed "seed:r") never counts as a fresh
			// admission.
			if tc.wantRedeem {
				require.Eventually(t, func() bool {
					return redeemAdmissions(store) >= 1
				}, 5*time.Second, 20*time.Millisecond)

				return
			}

			require.Never(t, func() bool {
				return redeemAdmissions(store) >= 1
			}, 500*time.Millisecond, 50*time.Millisecond)
		})
	}
}

// redeemAdmissions counts the auto-redeem operations the registry has admitted,
// identified by the "redeem:" op-key prefix considerRedeem mints.
func redeemAdmissions(store Store) int {
	ops, _ := store.ListOperations(context.Background())

	var n int
	for _, op := range ops {
		if strings.HasPrefix(op.OpKey, "redeem:") {
			n++
		}
	}

	return n
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
