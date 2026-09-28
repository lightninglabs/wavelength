package vtxo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// bestHeightRef returns a fixed chain tip without publishing a subscription
// epoch. It models backends where startup must explicitly query the already
// synchronized height instead of waiting for the next block.
type bestHeightRef struct {
	height int32
}

// failingVTXOActorRef preserves its state and rejects every Ask.
type failingVTXOActorRef struct {
	id    string
	state VTXOState
}

// inlineVTXOActorRef runs a real VTXO actor synchronously. Manager tests use
// it to observe the actor's manager-bound outbox before replaying that message
// as the manager's next serialized turn.
type inlineVTXOActorRef struct {
	actor *VTXOActor
}

// ID returns the wrapped actor identifier.
func (i *inlineVTXOActorRef) ID() string {
	return i.actor.cfg.VTXO.Outpoint.String()
}

// Tell runs the wrapped actor without waiting for a typed response.
func (i *inlineVTXOActorRef) Tell(ctx context.Context,
	msg actormsg.VTXOActorMsg) error {

	_, err := i.actor.Receive(ctx, msg).Unpack()

	return err
}

// TryTell delegates to Tell because this test double has no mailbox.
func (i *inlineVTXOActorRef) TryTell(ctx context.Context,
	msg actormsg.VTXOActorMsg) error {

	return i.Tell(ctx, msg)
}

// Ask completes a Future with the wrapped actor's real FSM result.
func (i *inlineVTXOActorRef) Ask(ctx context.Context,
	msg actormsg.VTXOActorMsg) actor.Future[actormsg.VTXOActorResp] {

	promise := actor.NewPromise[actormsg.VTXOActorResp]()
	promise.Complete(i.actor.Receive(ctx, msg))

	return promise.Future()
}

var _ VTXOActorRef = (*inlineVTXOActorRef)(nil)

// ID returns the test actor identifier.
func (f *failingVTXOActorRef) ID() string {
	return f.id
}

// Tell accepts fire-and-forget messages without changing test state.
func (f *failingVTXOActorRef) Tell(_ context.Context,
	_ actormsg.VTXOActorMsg) error {

	return nil
}

// TryTell delegates to Tell because this test double does not model mailbox
// saturation.
func (f *failingVTXOActorRef) TryTell(ctx context.Context,
	msg actormsg.VTXOActorMsg) error {

	return f.Tell(ctx, msg)
}

// Ask returns a deterministic actor failure.
func (f *failingVTXOActorRef) Ask(_ context.Context,
	_ actormsg.VTXOActorMsg) actor.Future[actormsg.VTXOActorResp] {

	promise := actor.NewPromise[actormsg.VTXOActorResp]()
	promise.Complete(
		fn.Err[actormsg.VTXOActorResp](
			errors.New("actor unavailable"),
		),
	)

	return promise.Future()
}

var _ VTXOActorRef = (*failingVTXOActorRef)(nil)

// ID returns the test actor identifier.
func (b *bestHeightRef) ID() string {
	return "best-height"
}

// Tell implements the chain source actor reference.
func (b *bestHeightRef) Tell(_ context.Context,
	_ chainsource.ChainSourceMsg) error {

	return nil
}

// TryTell delegates to Tell, which is all this double needs: no test
// drives the non-blocking path through it.
func (b *bestHeightRef) TryTell(ctx context.Context,
	msg chainsource.ChainSourceMsg) error {

	return b.Tell(ctx, msg)
}

// Ask returns the configured best height.
func (b *bestHeightRef) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	if _, ok := msg.(*chainsource.BestHeightRequest); !ok {
		promise.Complete(
			fn.Err[chainsource.ChainSourceResp](
				errors.New("unexpected chain source message"),
			),
		)

		return promise.Future()
	}

	promise.Complete(
		fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{
				Height: b.height,
			},
		),
	)

	return promise.Future()
}

// TestReconcileExpiryUsesCurrentTip verifies startup does not depend on a new
// block notification. A locally Live but already-expired VTXO is classified
// and then driven into the ordinary refresh reservation, while a safe VTXO is
// left untouched.
func TestReconcileExpiryUsesCurrentTip(t *testing.T) {
	t.Parallel()

	expired := makeDescriptor(t, 50_000, 0)
	safe := makeDescriptor(t, 40_000, 1)
	tip := expired.BatchExpiry
	safe.BatchExpiry = tip + 1_000

	store := &MockVTXOStore{}
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit,
	).Return([]*Descriptor{}, nil)
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusForfeiting,
	).Return([]*Descriptor{}, nil)

	mgr := NewManager(&ManagerConfig{
		Store: store,
		ChainSource: &bestHeightRef{
			height: tip,
		},
		DeferAutomaticRefreshUntilRoundReady: true,
	})
	require.True(t, mgr.roundStartupPending)
	mgr.actors = map[wire.OutPoint]VTXOActorRef{
		expired.Outpoint: newMockVTXOActorRef(
			expired.Outpoint.String(), &LiveState{
				VTXO: expired,
			},
		),
		safe.Outpoint: newMockVTXOActorRef(
			safe.Outpoint.String(), &LiveState{
				VTXO: safe,
			},
		),
	}

	response, err := runTestExpiryReconcile(t, mgr).Unpack()
	require.NoError(t, err)
	require.False(t, mgr.roundStartupPending)
	reconcileResponse, ok := response.(*ReconcileExpiryResponse)
	require.True(t, ok)
	require.Equal(t, 2, reconcileResponse.Checked)
	require.IsType(
		t, &PendingForfeitState{}, actorState(t, mgr, expired.Outpoint),
	)
	require.IsType(t, &LiveState{}, actorState(t, mgr, safe.Outpoint))
	store.AssertExpectations(t)
}

// TestReconcileExpiryRetriesDeferredExpiredRefresh verifies the complete
// startup handshake at one chain tip: the manager rolls an automatic refresh
// back while the round actor is unavailable, the reconcile opens the gate and
// re-drives the expired actor, and the actor's queued relay reaches round on
// the manager's next serialized turn.
func TestReconcileExpiryRetriesDeferredExpiredRefresh(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	tip := desc.BatchExpiry
	managerCapture := newMockManagerRef(t)
	vtxoActor := newRefreshTestActor(h, desc, managerCapture, nil)
	vtxoActor.state = &PendingForfeitState{
		VTXO:              desc,
		RequestedAtHeight: tip,
	}
	ref := &inlineVTXOActorRef{actor: vtxoActor}
	roundActor := newMockRoundActorRef(t)
	roundActor.tellErr = actor.ErrNoActorsAvailable

	h.store.On(
		"UpdateVTXOStatus", mock.Anything, desc.Outpoint,
		VTXOStatusExpired,
	).Return(nil).Once()
	h.store.On(
		"ListVTXOsByStatus", mock.Anything,
		VTXOStatusPendingForfeit,
	).Return([]*Descriptor{}, nil).Once()
	h.store.On(
		"ListVTXOsByStatus", mock.Anything, VTXOStatusForfeiting,
	).Return([]*Descriptor{}, nil).Once()
	h.store.On(
		"UpdateVTXOStatus", mock.Anything, desc.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil).Once()

	mgr := NewManager(
		&ManagerConfig{
			Store: h.store,
			ChainSource: &bestHeightRef{
				height: tip,
			},
			RoundActor:                           roundActor,
			DeferAutomaticRefreshUntilRoundReady: true,
		},
	)
	mgr.actors[desc.Outpoint] = ref

	request := autoRefreshLeaderRequest(desc, tip)
	request.ExpandCohort = false
	_, err := mgr.Receive(t.Context(), &RelayToRoundMsg{
		Payload: request,
	}).Unpack()
	require.NoError(t, err)
	require.Empty(t, roundActor.getMessages())
	require.IsType(t, &ExpiredState{}, vtxoActor.state)

	roundActor.tellErr = nil
	_, err = runTestExpiryReconcile(t, mgr).Unpack()
	require.NoError(t, err)
	require.False(t, mgr.roundStartupPending)

	messages := managerCapture.getMessages()
	require.Len(t, messages, 1)
	relay, ok := messages[0].(*RelayToRoundMsg)
	require.True(t, ok)
	_, err = mgr.Receive(t.Context(), relay).Unpack()
	require.NoError(t, err)
	require.Len(t, roundActor.getMessages(), 1)
	h.store.AssertExpectations(t)
}

// TestReconcileExpiryContinuesRecoveredExpired verifies a VTXO already
// persisted as Expired needs only one application of the startup tip to enter
// the ordinary refresh flow.
func TestReconcileExpiryContinuesRecoveredExpired(t *testing.T) {
	t.Parallel()

	expired := makeDescriptor(t, 50_000, 0)
	tip := expired.BatchExpiry

	store := &MockVTXOStore{}
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit,
	).Return([]*Descriptor{}, nil)
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusForfeiting,
	).Return([]*Descriptor{}, nil)

	mgr := NewManager(&ManagerConfig{
		Store:       store,
		ChainSource: &bestHeightRef{height: tip},
	})
	mgr.actors = map[wire.OutPoint]VTXOActorRef{
		expired.Outpoint: newMockVTXOActorRef(
			expired.Outpoint.String(), &ExpiredState{
				VTXO: expired,
			},
		),
	}

	_, err := runTestExpiryReconcile(t, mgr).Unpack()
	require.NoError(t, err)
	require.IsType(
		t, &PendingForfeitState{}, actorState(t, mgr, expired.Outpoint),
	)
	store.AssertExpectations(t)
}

// TestReconcileExpiryFinalizesConfirmedForfeitCrashGap verifies startup
// repairs a crash after round finalization but before the best-effort VTXO
// confirmation Tell. A positive settlement height terminalizes the old VTXO;
// an active checkpoint (height zero) and actor errors remain Forfeiting.
func TestReconcileExpiryFinalizesConfirmedForfeitCrashGap(t *testing.T) {
	t.Parallel()

	confirmed := makeDescriptor(t, 50_000, 0)
	confirmed.Status = VTXOStatusForfeiting
	confirmed.ForfeitRoundID = "confirmed-round"
	confirmedTxID := chainhash.Hash{0xaa}
	confirmed.Settlement = fn.Some(Settlement{
		TxID:   confirmedTxID,
		Height: 101,
		FeeSat: 500,
	})

	active := makeDescriptor(t, 40_000, 1)
	active.Status = VTXOStatusForfeiting
	active.ForfeitRoundID = "active-round"
	active.Settlement = fn.Some(Settlement{
		TxID: chainhash.Hash{0xbb},
	})

	actorFailure := makeDescriptor(t, 30_000, 2)
	actorFailure.Status = VTXOStatusForfeiting
	actorFailure.ForfeitRoundID = "confirmed-but-unavailable"
	actorFailure.Settlement = fn.Some(Settlement{
		TxID:   chainhash.Hash{0xcc},
		Height: 102,
	})

	store := &MockVTXOStore{}
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit,
	).Return([]*Descriptor{}, nil)
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusForfeiting,
	).Return([]*Descriptor{confirmed, active, actorFailure}, nil)

	confirmedRef := newMockVTXOActorRef(
		confirmed.Outpoint.String(), &ForfeitingState{
			VTXO:       confirmed,
			NewRoundID: confirmed.ForfeitRoundID,
		},
	)
	activeRef := newMockVTXOActorRef(
		active.Outpoint.String(), &ForfeitingState{
			VTXO:       active,
			NewRoundID: active.ForfeitRoundID,
		},
	)
	failedRef := &failingVTXOActorRef{
		id: actorFailure.Outpoint.String(),
		state: &ForfeitingState{
			VTXO:       actorFailure,
			NewRoundID: actorFailure.ForfeitRoundID,
		},
	}

	mgr := NewManager(&ManagerConfig{
		Store:       store,
		ChainSource: &bestHeightRef{height: 100},
	})
	mgr.actors = map[wire.OutPoint]VTXOActorRef{
		confirmed.Outpoint:    confirmedRef,
		active.Outpoint:       activeRef,
		actorFailure.Outpoint: failedRef,
	}

	response, err := runTestExpiryReconcile(t, mgr).Unpack()
	require.NoError(t, err)
	reconcileResponse, ok := response.(*ReconcileExpiryResponse)
	require.True(t, ok)
	require.Equal(t, 1, reconcileResponse.Checked)

	confirmedState, ok := confirmedRef.state.(*ForfeitedState)
	require.True(t, ok, "confirmed VTXO must become Forfeited")
	require.Equal(t, confirmedTxID, confirmedState.CommitmentTxID)
	require.IsType(t, &ForfeitingState{}, activeRef.state)
	require.IsType(t, &ForfeitingState{}, failedRef.state)
	store.AssertExpectations(t)
}

// TestReconcileConfirmedForfeitsListErrorFailsClosed verifies a store lookup
// failure cannot terminalize a recovered Forfeiting VTXO.
func TestReconcileConfirmedForfeitsListErrorFailsClosed(t *testing.T) {
	t.Parallel()

	desc := makeDescriptor(t, 50_000, 0)
	desc.Status = VTXOStatusForfeiting
	ref := newMockVTXOActorRef(
		desc.Outpoint.String(), &ForfeitingState{
			VTXO: desc,
		},
	)
	store := &MockVTXOStore{}
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusForfeiting,
	).Return([]*Descriptor(nil), errors.New("store unavailable"))

	mgr := NewManager(&ManagerConfig{Store: store})
	mgr.actors[desc.Outpoint] = ref

	plan := &expiryReconcilePlan{
		actors:  mgr.actors,
		store:   store,
		log:     mgr.logger(t.Context()),
		timeout: defaultForfeitVTXOActorAskTimeout,
	}
	reconciled := plan.reconcileConfirmedForfeits(t.Context())
	require.Empty(t, reconciled)
	require.IsType(t, &ForfeitingState{}, ref.state)
	store.AssertExpectations(t)
}

// runTestExpiryReconcile drives the prepared pass outside the manager turn
// for the synchronous state-machine fixtures above.
func runTestExpiryReconcile(t *testing.T, mgr *Manager) fn.Result[ManagerResp] {
	t.Helper()
	response, err := mgr.handleReconcileExpiry(t.Context()).Unpack()
	if err != nil {
		return fn.Err[ManagerResp](err)
	}
	plan, ok := response.(*expiryReconcilePlan)
	require.True(t, ok)

	result, err := plan.run(t.Context()).Unpack()
	if err != nil {
		return fn.Err[ManagerResp](err)
	}

	return fn.Ok[ManagerResp](result)
}

// busyManagerRef inserts ordinary manager traffic ahead of a child's reply.
// With a one-slot mailbox the child can finish only if the manager keeps
// draining while the startup caller waits for that child.
type busyManagerRef struct {
	actor.ActorRef[ManagerMsg, ManagerResp]
}

// Tell queues competing traffic before delivering the real child outbox.
func (b busyManagerRef) Tell(ctx context.Context, msg ManagerMsg) error {
	if err := b.ActorRef.Tell(
		ctx, &GetActiveVTXOCountRequest{},
	); err != nil {
		return err
	}

	return b.ActorRef.Tell(ctx, msg)
}

// TestReconcileExpiryDrainsChildRelays uses real bounded mailboxes and real
// VTXO FSMs. Every recovered expired child must reach round without burning
// its ask deadline or losing the relay behind a busy manager mailbox.
func TestReconcileExpiryDrainsChildRelays(t *testing.T) {
	t.Parallel()
	h := newVTXOTestHarness(t)
	roundRef := newMockRoundActorRef(t)
	const children = 8
	descs := make([]*Descriptor, children)
	for i := range descs {
		descs[i] = h.newTestDescriptor()
		descs[i].Outpoint.Index = uint32(i)
		h.store.On("UpdateVTXOStatus", mock.Anything,
			descs[i].Outpoint, VTXOStatusPendingForfeit,
		).Return(nil).Once()
	}
	h.store.On("ListVTXOsByStatus", mock.Anything,
		VTXOStatusPendingForfeit,
	).Return([]*Descriptor{}, nil)
	h.store.On("ListVTXOsByStatus", mock.Anything,
		VTXOStatusForfeiting,
	).Return([]*Descriptor{}, nil)
	mgr := NewManager(
		&ManagerConfig{
			Store:      h.store,
			RoundActor: roundRef,
			ChainSource: &bestHeightRef{
				height: descs[0].BatchExpiry,
			},
			ForfeitVTXOActorAskTimeout:           time.Second,
			DeferAutomaticRefreshUntilRoundReady: true,
		},
	)
	managerActor := actor.NewActor(actor.ActorConfig[
		ManagerMsg,
		ManagerResp,
	]{
		ID: "startup-manager", Behavior: mgr, MailboxSize: 1,
	})
	t.Cleanup(managerActor.Stop)
	for _, desc := range descs {
		child := newRefreshTestActor(h, desc, nil, nil)
		child.state = &ExpiredState{VTXO: desc}
		child.cfg.Manager = busyManagerRef{managerActor.Ref()}
		childActor := actor.NewActor(actor.ActorConfig[
			actormsg.VTXOActorMsg, actormsg.VTXOActorResp,
		]{ID: desc.Outpoint.String(), Behavior: child, MailboxSize: 1})
		mgr.actors[desc.Outpoint] = childActor.Ref()
		childActor.Start()
		t.Cleanup(childActor.Stop)
	}
	managerActor.Start()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := ReconcileExpiry(ctx, managerActor.Ref())
	require.NoError(t, err)
	require.Equal(t, children, result.Checked)

	// A manager ask is a FIFO barrier after the final child relay.
	_, err = managerActor.Ref().Ask(ctx,
		&GetActiveVTXOCountRequest{},
	).Await(ctx).Unpack()
	require.NoError(t, err)
	messages := roundRef.getMessages()
	require.Len(t, messages, children)
	seen := make(map[wire.OutPoint]bool)
	for _, msg := range messages {
		req, ok := msg.(*round.RefreshVTXORequest)
		require.True(t, ok)
		require.False(t, seen[req.VTXOOutpoint])
		seen[req.VTXOOutpoint] = true
	}
	h.store.AssertExpectations(t)
}

// TestReconcileExpiryCancellationKeepsManagerResponsive proves a child wait
// neither owns the manager registry nor escapes the startup caller's lifetime.
// A termination can remove the snapshotted child while the pass is waiting.
func TestReconcileExpiryCancellationKeepsManagerResponsive(t *testing.T) {
	t.Parallel()
	desc := makeDescriptor(t, 50_000, 0)
	store := &MockVTXOStore{}
	for _, status := range []VTXOStatus{
		VTXOStatusPendingForfeit, VTXOStatusForfeiting,
	} {
		store.On("ListVTXOsByStatus", mock.Anything, status).
			Return([]*Descriptor{}, nil)
	}
	started := make(chan struct{})
	behavior := actor.NewFunctionBehavior(func(ctx context.Context,
		_ actormsg.VTXOActorMsg) fn.Result[actormsg.VTXOActorResp] {

		close(started)
		<-ctx.Done()

		return fn.Err[actormsg.VTXOActorResp](ctx.Err())
	})
	child := actor.NewActor(actor.ActorConfig[
		actormsg.VTXOActorMsg, actormsg.VTXOActorResp,
	]{
		ID:          "blocked-child",
		Behavior:    behavior,
		MailboxSize: 1,
	})
	child.Start()
	t.Cleanup(child.Stop)
	mgr := NewManager(&ManagerConfig{
		Store:       store,
		ChainSource: &bestHeightRef{height: desc.BatchExpiry},
	})
	mgr.actors[desc.Outpoint] = child.Ref()
	managerActor := actor.NewActor(actor.ActorConfig[
		ManagerMsg,
		ManagerResp,
	]{
		ID: "responsive-manager", Behavior: mgr, MailboxSize: 1,
	})
	managerActor.Start()
	t.Cleanup(managerActor.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ReconcileExpiry(ctx, managerActor.Ref())
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("startup did not reach the child")
	}
	select {
	case err := <-done:
		t.Fatalf("startup returned before its child completed: %v", err)

	default:
	}
	_, err := managerActor.Ref().Ask(ctx, &round.VTXOTerminatedMsg{
		Outpoint: desc.Outpoint,
	}).Await(ctx).Unpack()
	require.NoError(t, err)
	response, err := managerActor.Ref().Ask(ctx,
		&GetActiveVTXOCountRequest{},
	).Await(ctx).Unpack()
	require.NoError(t, err)
	count, ok := response.(*GetActiveVTXOCountResponse)
	require.True(t, ok)
	require.Zero(t, count.Count)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)

	case <-time.After(time.Second):
		t.Fatal("startup pass survived caller cancellation")
	}
	store.AssertExpectations(t)
}
