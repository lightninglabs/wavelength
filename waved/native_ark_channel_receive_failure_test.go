package waved

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// receiveFailurePeer adapts a production service and SQL-backed FSM to the
// process peer surface used by the hub controller.
type receiveFailurePeer struct {
	lnruntime.ProcessFundingPeer

	mu sync.Mutex

	service           *arkchannel.Service
	failureRequests   int
	peerEventAttempts int
	peerEventFailures int
	peerEventFailure  error
}

// failPeerEvents drops the next count authenticated peer notifications.
func (p *receiveFailurePeer) failPeerEvents(count int, err error) {
	p.mu.Lock()
	p.peerEventFailures = count
	p.peerEventFailure = err
	p.mu.Unlock()
}

// failureRequestCount returns the number of delivered abort requests.
func (p *receiveFailurePeer) failureRequestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.failureRequests
}

// peerEventAttemptCount returns all attempted authenticated notifications.
func (p *receiveFailurePeer) peerEventAttemptCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.peerEventAttempts
}

// GetFundingChannel returns the peer's durable SQL-backed channel state.
func (p *receiveFailurePeer) GetFundingChannel(ctx context.Context,
	id arkchannel.ID) (lnruntime.FundingChannelState, error) {

	record, err := p.service.GetChannel(ctx, id)
	if err != nil {
		return lnruntime.FundingChannelState{}, err
	}
	snapshot := record.Snapshot

	return lnruntime.FundingChannelState{
		Terms: snapshot.Terms, Source: snapshot.Source,
		Backing: snapshot.Backing, Phase: snapshot.Phase,
		OORFinalized: snapshot.OORFinalized,
		OORAborted:   snapshot.OORAborted,
		Failure:      snapshot.Failure,
		Revision:     record.Revision,
	}, nil
}

// FailReceiveIntent asks this peer's local-funder service to drive the
// authoritative pre-PONR abort.
func (p *receiveFailurePeer) FailReceiveIntent(ctx context.Context,
	id arkchannel.ID, reason string) error {

	p.mu.Lock()
	p.failureRequests++
	p.mu.Unlock()
	record, err := p.service.ApplyLocalEvent(
		ctx, id, &arkchannel.Fail{
			Reason: reason,
		},
	)
	if err != nil {
		return err
	}
	if record.Snapshot.Phase != arkchannel.PhaseFailed ||
		!record.Snapshot.OORAborted {
		return fmt.Errorf("receive intent did not reach failed state")
	}

	return nil
}

// ApplyChannelEvent admits the hub's authenticated OOR terminal fact through
// the peer side of the production service.
func (p *receiveFailurePeer) ApplyChannelEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	p.mu.Lock()
	p.peerEventAttempts++
	if p.peerEventFailures > 0 {
		p.peerEventFailures--
		err := p.peerEventFailure
		p.mu.Unlock()

		return arkchannel.Record{}, err
	}
	p.mu.Unlock()

	return p.service.ApplyPeerEvent(ctx, id, event)
}

// receiveFailureStore injects a failure after one SQL compare-and-swap has
// committed, modeling a canceled caller that did not observe its durable bind.
type receiveFailureStore struct {
	arkchannel.Store

	mu sync.Mutex

	failNextWrite bool
	cancel        context.CancelFunc
	writeErr      error
}

// failNextCommittedWrite arms one post-commit response failure.
func (s *receiveFailureStore) failNextCommittedWrite(cancel context.CancelFunc,
	err error) {

	s.mu.Lock()
	s.failNextWrite = true
	s.cancel = cancel
	s.writeErr = err
	s.mu.Unlock()
}

// CompareAndSwap commits through the production store before dropping the
// result and canceling the original request.
func (s *receiveFailureStore) CompareAndSwap(ctx context.Context,
	id arkchannel.ID, revision uint64, snapshot arkchannel.Snapshot) (
	arkchannel.Record, error) {

	record, err := s.Store.CompareAndSwap(ctx, id, revision, snapshot)
	if err != nil {
		return arkchannel.Record{}, err
	}

	s.mu.Lock()
	if !s.failNextWrite {
		s.mu.Unlock()

		return record, nil
	}
	s.failNextWrite = false
	cancel := s.cancel
	writeErr := s.writeErr
	s.cancel = nil
	s.writeErr = nil
	s.mu.Unlock()

	cancel()

	return arkchannel.Record{}, writeErr
}

// receiveFailureExecutor drives the real FSM's cleanup actions while
// injecting only the native lnd negotiation result under test.
type receiveFailureExecutor struct {
	party                   arkchannel.Party
	service                 *arkchannel.Service
	remote                  *receiveFailurePeer
	negotiationErr          error
	receiveAbortDeliveryErr error
	cancelRequest           context.CancelFunc
	aborts                  int
	abortRequests           int
	cancellations           int
	cleanupCtxErr           error
}

// Execute injects negotiation failure and reflects terminal OOR/lnd facts
// through the same production service transitions used by the daemon.
func (e *receiveFailureExecutor) Execute(ctx context.Context, id arkchannel.ID,
	action arkchannel.Action) error {

	switch action := action.(type) {
	case *arkchannel.NegotiateFunding:
		if e.cancelRequest != nil {
			e.cancelRequest()
		}

		return e.negotiationErr

	case *arkchannel.AbortOOR:
		e.cleanupCtxErr = ctx.Err()
		if action.Terms.Funder != e.party {
			e.abortRequests++
			if e.remote == nil {
				return fmt.Errorf("receive abort peer is " +
					"unavailable")
			}

			return e.remote.FailReceiveIntent(
				ctx, id, action.Reason,
			)
		}
		e.aborts++
		_, err := e.service.ApplyLocalEvent(
			ctx, id, &arkchannel.OORAborted{
				SessionID: action.Source.OORSessionID,
				Reason:    action.Reason,
			},
		)

		return err

	case *arkchannel.RequestReceiveIntentAbort:
		e.abortRequests++
		e.cleanupCtxErr = ctx.Err()
		if e.receiveAbortDeliveryErr != nil {
			return e.receiveAbortDeliveryErr
		}
		if e.remote == nil {
			return fmt.Errorf("receive abort peer is unavailable")
		}
		if err := e.remote.FailReceiveIntent(
			ctx, id, action.Reason,
		); err != nil {
			return err
		}

		return nil

	case *arkchannel.CancelFunding:
		e.cancellations++
		if e.party == arkchannel.PartyHub {
			_, err := e.remote.ApplyChannelEvent(
				ctx, id, &arkchannel.OORAborted{
					SessionID: action.Source.OORSessionID,
					Reason:    action.Reason,
				},
			)
			if err != nil {
				return err
			}
		}
		_, err := e.service.ApplyLocalEvent(
			ctx, id, &arkchannel.FundingCanceled{},
		)

		return err

	default:
		return fmt.Errorf("unexpected receive failure action %T",
			action)
	}
}

// receiveFailureFixture contains paired SQL-backed channel services.
type receiveFailureFixture struct {
	controller        *NativeArkChannelController
	hubService        *arkchannel.Service
	clientService     *arkchannel.Service
	hubCoordinator    *arkchannel.Coordinator
	clientCoordinator *arkchannel.Coordinator
	hubStore          *receiveFailureStore
	clientStore       *receiveFailureStore
	hubExecutor       *receiveFailureExecutor
	clientExecutor    *receiveFailureExecutor
	hubPeer           *receiveFailurePeer
	clientPeer        *receiveFailurePeer
	terms             arkchannel.Terms
	binding           arkchannel.VTXOBinding
	backing           arkchannel.Backing
	hash              lntypes.Hash
}

// receiveResumeExecutor completes the two replayable actions exercised by the
// receive synchronizer without replacing its durable SQL-backed FSM.
type receiveResumeExecutor struct {
	service     *arkchannel.Service
	recoveries  atomic.Int32
	activations atomic.Int32
}

// Execute records the same durable events produced by native recovery and lnd
// activation after their side effects complete.
func (e *receiveResumeExecutor) Execute(ctx context.Context, id arkchannel.ID,
	action arkchannel.Action) error {

	switch action := action.(type) {
	case *arkchannel.PrepareRecovery:
		e.recoveries.Add(1)
		record, err := e.service.RecordLocalEvent(
			ctx, id, &arkchannel.RecoveryPackageInstalled{
				Party: arkchannel.PartyClient,
			},
		)
		if err != nil {
			return err
		}
		if record.Snapshot.Phase != arkchannel.PhaseActivating {
			return nil
		}
		_, err = e.service.ResumeChannelAction(ctx, id)

		return err

	case *arkchannel.ActivateChannel:
		e.activations.Add(1)
		_, err := e.service.RecordLocalEvent(
			ctx, id, &arkchannel.ChannelActive{
				ChannelPointHash: action.
					Backing.
					ChannelPoint.
					Hash,
				ChannelPointIndex: action.
					Backing.
					ChannelPoint.
					Index,
			},
		)

		return err

	default:
		return fmt.Errorf("unexpected receive resume action %T", action)
	}
}

// unavailableReceivePeer detects any unnecessary remote poll after local
// durable state already contains everything needed to finish activation.
type unavailableReceivePeer struct {
	lnruntime.ProcessFundingPeer

	gets atomic.Int32
}

// GetFundingChannel rejects remote state reads in the local-first replay test.
func (p *unavailableReceivePeer) GetFundingChannel(context.Context,
	arkchannel.ID) (lnruntime.FundingChannelState, error) {

	p.gets.Add(1)

	return lnruntime.FundingChannelState{}, fmt.Errorf("remote funding " +
		"state is unavailable")
}

// testReceiveFailureBacking creates a real signed VTXO-to-channel transaction
// for replay tests that must reach the backing-ready phase.
func testReceiveFailureBacking(t *testing.T, terms arkchannel.Terms,
	binding arkchannel.VTXOBinding,
	clientKey, hubKey *btcec.PrivateKey) arkchannel.Backing {

	t.Helper()
	packet, err := psbt.New(nil, []*wire.TxOut{{
		Value: int64(terms.Capacity), PkScript: []byte{0x51, 0x20, 1},
	}}, 2, 0, nil)
	require.NoError(t, err)
	template, err := arkchannel.NewBackingTemplate(packet, terms, binding)
	require.NoError(t, err)
	sign := func(party arkchannel.Party,
		key *btcec.PrivateKey) input.Signature {

		desc, err := template.SignDescriptor(
			terms, party, keychain.KeyDescriptor{
				PubKey: key.PubKey(),
			},
		)
		require.NoError(t, err)
		sig, err := input.NewMockSigner(
			[]*btcec.PrivateKey{key}, nil,
		).SignOutputRaw(template.Packet().UnsignedTx, desc)
		require.NoError(t, err)

		return sig
	}
	backing, err := template.Complete(
		terms, binding, sign(arkchannel.PartyClient, clientKey),
		sign(arkchannel.PartyHub, hubKey),
	)
	require.NoError(t, err)

	return backing
}

// newReceiveFailureFixture creates paired production stores, coordinators,
// and FSMs at the exact pre-PONR negotiation phase.
func newReceiveFailureFixture(t *testing.T, negotiationErr error,
	bindClient bool) *receiveFailureFixture {

	t.Helper()
	terms := testPrePONRTerms(t)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hubKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	copy(
		terms.VTXO.ClientChannelKey[:],
		clientKey.PubKey().SerializeCompressed(),
	)
	copy(
		terms.VTXO.HubChannelKey[:],
		hubKey.PubKey().SerializeCompressed(),
	)
	hash := lntypes.Hash(terms.PaymentHash)
	terms.ID = arkchannel.ReceiveIntentID(hash)
	binding := testPrePONRBinding(t, terms)
	backing := testReceiveFailureBacking(
		t, terms, binding, clientKey, hubKey,
	)

	newEndpoint := func(party arkchannel.Party) (*arkchannel.Service,
		*arkchannel.Coordinator, *receiveFailureExecutor,
		*receiveFailureStore) {

		raw, err := db.NewStoreFromConfig(
			db.DefaultConfig(
				t.TempDir(),
			),
			btclog.Disabled,
		)
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, raw.Close())
		})
		store := &receiveFailureStore{Store: raw.NewArkChannelStore(
			clock.NewTestClock(
				time.Unix(40_000, 0),
			),
		)}
		coordinator, err := arkchannel.NewCoordinator(store)
		require.NoError(t, err)
		executor := &receiveFailureExecutor{
			party: party, negotiationErr: negotiationErr,
		}
		service, err := arkchannel.NewService(
			party, coordinator, executor,
		)
		require.NoError(t, err)
		executor.service = service

		return service, coordinator, executor, store
	}

	hubService, hubCoordinator, hubExecutor, hubStore := newEndpoint(
		arkchannel.PartyHub,
	)
	clientService, clientCoordinator, clientExecutor,
		clientStore := newEndpoint(
		arkchannel.PartyClient,
	)
	clientPeer := &receiveFailurePeer{service: clientService}
	hubExecutor.remote = clientPeer

	for _, endpoint := range []struct {
		coordinator *arkchannel.Coordinator
		bindSource  bool
	}{
		{
			coordinator: hubCoordinator,
			bindSource:  true,
		},
		{
			coordinator: clientCoordinator,
			bindSource:  bindClient,
		},
	} {
		_, err := endpoint.coordinator.Request(t.Context(), terms)
		require.NoError(t, err)
		if !endpoint.bindSource {
			continue
		}
		_, _, err = endpoint.coordinator.Apply(
			t.Context(), terms.ID, &arkchannel.BindVTXO{
				Binding: binding,
			},
		)
		require.NoError(t, err)
		_, _, err = endpoint.coordinator.Apply(
			t.Context(), terms.ID, &arkchannel.FundingPeerReady{},
		)
		require.NoError(t, err)
	}
	hubPeer := &receiveFailurePeer{service: hubService}
	clientExecutor.remote = hubPeer

	return &receiveFailureFixture{
		controller: &NativeArkChannelController{
			party:   arkchannel.PartyHub,
			service: hubService,
			remote:  clientPeer,
		},
		hubService: hubService, clientService: clientService,
		hubCoordinator:    hubCoordinator,
		clientCoordinator: clientCoordinator,
		hubStore:          hubStore, clientStore: clientStore,
		hubExecutor: hubExecutor, clientExecutor: clientExecutor,
		hubPeer: hubPeer, clientPeer: clientPeer,
		terms: terms, binding: binding, backing: backing, hash: hash,
	}
}

// advanceBackingReady records a fully signed native channel at both endpoints
// without executing the funder's still-pre-PONR CommitOOR action.
func (f *receiveFailureFixture) advanceBackingReady(t *testing.T) {
	t.Helper()

	for _, coordinator := range []*arkchannel.Coordinator{
		f.hubCoordinator, f.clientCoordinator,
	} {
		for _, event := range []arkchannel.Event{
			&arkchannel.BackingSigned{
				Backing: f.backing,
			},
			&arkchannel.FundingFinalized{
				Party: arkchannel.PartyClient,
			},
			&arkchannel.FundingFinalized{
				Party: arkchannel.PartyHub,
			},
		} {
			_, _, err := coordinator.Apply(
				t.Context(), f.terms.ID, event,
			)
			require.NoError(t, err)
		}

		record, err := coordinator.Get(t.Context(), f.terms.ID)
		require.NoError(t, err)
		require.Equal(
			t, arkchannel.PhaseBackingReady, record.Snapshot.Phase,
		)
	}
}

// TestManifestIncomingChannelCleansDefinitiveFailure proves a local lnd
// rejection releases the prepared OOR and both durable endpoint records even
// when the originating request is canceled as the rejection returns.
func TestManifestIncomingChannelCleansDefinitiveFailure(t *testing.T) {
	t.Parallel()

	negotiationErr := errors.New("lnd rejected channel negotiation")
	fixture := newReceiveFailureFixture(t, negotiationErr, true)
	ctx, cancel := context.WithCancel(t.Context())
	fixture.hubExecutor.cancelRequest = cancel

	_, err := fixture.controller.ManifestIncomingChannel(
		ctx, fixture.hash, fixture.terms.Capacity,
		fixture.terms.Capacity, fixture.terms.ReservedSCID,
	)
	require.ErrorIs(t, err, ErrReceiveChannelFallback)
	require.ErrorIs(t, err, negotiationErr)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.NoError(t, fixture.hubExecutor.cleanupCtxErr)
	require.Equal(t, 1, fixture.hubExecutor.aborts)
	require.Equal(t, 1, fixture.hubExecutor.cancellations)
	require.Equal(t, 1, fixture.clientExecutor.cancellations)

	for _, service := range []*arkchannel.Service{
		fixture.hubService, fixture.clientService,
	} {
		record, loadErr := service.GetChannel(
			t.Context(), fixture.terms.ID,
		)
		require.NoError(t, loadErr)
		require.Equal(t, arkchannel.PhaseFailed, record.Snapshot.Phase)
		require.True(t, record.Snapshot.OORAborted)
	}
}

// TestFailReceiveIntentUsesRemoteFunder proves a client-side source binding
// failure persists its abort request before asking the hub to release the
// prepared OOR and cancel its lnd reservation.
func TestFailReceiveIntentUsesRemoteFunder(t *testing.T) {
	t.Parallel()

	cause := errors.New("client rejected prepared channel source")
	fixture := newReceiveFailureFixture(t, nil, false)
	controller := &NativeArkChannelController{
		party: arkchannel.PartyClient, service: fixture.clientService,
		remote: fixture.hubPeer,
	}

	err := controller.failReceiveIntentDetached(
		t.Context(), fixture.terms.ID, cause,
	)
	require.ErrorIs(t, err, ErrReceiveChannelFallback)
	require.ErrorIs(t, err, cause)
	require.Equal(t, 1, fixture.hubPeer.failureRequestCount())
	require.Equal(t, 1, fixture.hubExecutor.aborts)
	require.Equal(t, 1, fixture.hubExecutor.cancellations)
	require.Zero(t, fixture.clientExecutor.cancellations)

	hub, err := fixture.hubService.GetChannel(
		t.Context(), fixture.terms.ID,
	)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseFailed, hub.Snapshot.Phase)
	require.True(t, hub.Snapshot.OORAborted)
	client, err := fixture.clientService.GetChannel(
		t.Context(), fixture.terms.ID,
	)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseFailed, client.Snapshot.Phase)
	require.Nil(t, client.Snapshot.Source)
	require.True(t, client.Snapshot.OORAborted)
}

// TestSourceLessReceiveAbortReplaysAfterRestart proves a crash after the
// client commits its abort request cannot lose delivery to the funding hub.
func TestSourceLessReceiveAbortReplaysAfterRestart(t *testing.T) {
	t.Parallel()

	fixture := newReceiveFailureFixture(t, nil, false)
	cause := errors.New("client rejected prepared channel source")
	crashErr := errors.New("injected crash before hub delivery")
	fixture.clientExecutor.receiveAbortDeliveryErr = crashErr

	_, err := fixture.clientService.ApplyLocalEvent(
		t.Context(), fixture.terms.ID,
		&arkchannel.ReceiveIntentAbortRequested{
			Reason: cause.Error(),
		},
	)
	require.ErrorIs(t, err, crashErr)
	require.Zero(t, fixture.hubPeer.failureRequestCount())
	client, err := fixture.clientService.GetChannel(
		t.Context(), fixture.terms.ID,
	)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseCancelling, client.Snapshot.Phase)
	require.Nil(t, client.Snapshot.Source)

	clientCoordinator, err := arkchannel.NewCoordinator(
		fixture.clientStore,
	)
	require.NoError(t, err)
	clientExecutor := &receiveFailureExecutor{
		party: arkchannel.PartyClient, remote: fixture.hubPeer,
	}
	clientService, err := arkchannel.NewService(
		arkchannel.PartyClient, clientCoordinator, clientExecutor,
	)
	require.NoError(t, err)
	clientExecutor.service = clientService
	fixture.clientPeer.service = clientService

	require.NoError(t, clientService.Resume(t.Context()))
	require.Equal(t, 1, fixture.hubPeer.failureRequestCount())
	for _, service := range []*arkchannel.Service{
		fixture.hubService, clientService,
	} {
		record, loadErr := service.GetChannel(
			t.Context(), fixture.terms.ID,
		)
		require.NoError(t, loadErr)
		require.Equal(t, arkchannel.PhaseFailed, record.Snapshot.Phase)
		require.True(t, record.Snapshot.OORAborted)
	}
}

// TestAbortedHubReplaysPastClientBackingReady proves a lost first abort
// notification cannot turn the client's stale pre-PONR phase into PONR proof.
func TestAbortedHubReplaysPastClientBackingReady(t *testing.T) {
	t.Parallel()

	fixture := newReceiveFailureFixture(t, nil, true)
	fixture.advanceBackingReady(t)
	cause := errors.New("hub rejected receive channel funding")
	notificationErr := errors.New("injected lost abort notification")
	fixture.clientPeer.failPeerEvents(1, notificationErr)

	_, err := fixture.hubService.ApplyLocalEvent(
		t.Context(), fixture.terms.ID, &arkchannel.OORAborted{
			SessionID: fixture.binding.OORSessionID,
			Reason:    cause.Error(),
		},
	)
	require.ErrorIs(t, err, notificationErr)
	hub, err := fixture.hubService.GetChannel(
		t.Context(), fixture.terms.ID,
	)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseCancelling, hub.Snapshot.Phase)
	require.True(t, hub.Snapshot.OORAborted)
	client, err := fixture.clientService.GetChannel(
		t.Context(), fixture.terms.ID,
	)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseBackingReady, client.Snapshot.Phase)

	hubCoordinator, err := arkchannel.NewCoordinator(fixture.hubStore)
	require.NoError(t, err)
	hubExecutor := &receiveFailureExecutor{
		party: arkchannel.PartyHub, remote: fixture.clientPeer,
	}
	hubService, err := arkchannel.NewService(
		arkchannel.PartyHub, hubCoordinator, hubExecutor,
	)
	require.NoError(t, err)
	hubExecutor.service = hubService
	controller := &NativeArkChannelController{
		party: arkchannel.PartyHub, service: hubService,
		remote: fixture.clientPeer,
	}

	err = controller.failReceiveIntentDetached(
		t.Context(), fixture.terms.ID, cause,
	)
	require.ErrorIs(t, err, ErrReceiveChannelFallback)
	require.ErrorIs(t, err, cause)
	require.Equal(t, 2, fixture.clientPeer.peerEventAttemptCount())
	for _, service := range []*arkchannel.Service{
		hubService, fixture.clientService,
	} {
		record, loadErr := service.GetChannel(
			t.Context(), fixture.terms.ID,
		)
		require.NoError(t, loadErr)
		require.Equal(t, arkchannel.PhaseFailed, record.Snapshot.Phase)
		require.True(t, record.Snapshot.OORAborted)
	}
}

// TestSyncReceiveIntentReplaysRemoteCancellation proves the client asks a
// cancelling hub to replay its durable action instead of asserting hub-local
// failure evidence through the peer event API.
func TestSyncReceiveIntentReplaysRemoteCancellation(t *testing.T) {
	t.Parallel()

	fixture := newReceiveFailureFixture(t, nil, true)
	fixture.advanceBackingReady(t)
	cause := errors.New("hub rejected receive channel funding")
	fixture.clientPeer.failPeerEvents(
		1, errors.New("injected lost abort notification"),
	)

	_, err := fixture.hubService.ApplyLocalEvent(
		t.Context(), fixture.terms.ID, &arkchannel.OORAborted{
			SessionID: fixture.binding.OORSessionID,
			Reason:    cause.Error(),
		},
	)
	require.Error(t, err)
	controller := &NativeArkChannelController{
		party: arkchannel.PartyClient, service: fixture.clientService,
		remote: fixture.hubPeer,
	}

	err = controller.syncReceiveIntent(t.Context(), fixture.hash)
	require.ErrorIs(t, err, ErrReceiveChannelFallback)
	require.Equal(t, 1, fixture.hubPeer.failureRequestCount())
	for _, service := range []*arkchannel.Service{
		fixture.hubService, fixture.clientService,
	} {
		record, loadErr := service.GetChannel(
			t.Context(), fixture.terms.ID,
		)
		require.NoError(t, loadErr)
		require.Equal(t, arkchannel.PhaseFailed, record.Snapshot.Phase)
	}
}

// TestSyncReceiveIntentResumesLocalRecoveryBeforePollingHub proves a busy
// process mailbox cannot strand a hub-funded channel at backing-ready after
// the funding-wire barriers are already durable on the client.
func TestSyncReceiveIntentResumesLocalRecoveryBeforePollingHub(t *testing.T) {
	t.Parallel()

	fixture := newReceiveFailureFixture(t, nil, true)
	fixture.advanceBackingReady(t)
	for _, event := range []arkchannel.Event{
		&arkchannel.OORFinalized{
			SessionID: fixture.binding.OORSessionID,
		},
		&arkchannel.RecoveryPackageInstalled{
			Party: arkchannel.PartyHub,
		},
	} {
		_, _, err := fixture.clientCoordinator.Apply(
			t.Context(), fixture.terms.ID, event,
		)
		require.NoError(t, err)
	}
	executor := &receiveResumeExecutor{}
	service, err := arkchannel.NewService(
		arkchannel.PartyClient, fixture.clientCoordinator, executor,
	)
	require.NoError(t, err)
	executor.service = service
	remote := &unavailableReceivePeer{}
	controller := &NativeArkChannelController{
		party: arkchannel.PartyClient, service: service, remote: remote,
	}

	require.NoError(
		t,
		controller.syncReceiveIntent(
			t.Context(), fixture.hash,
		),
	)
	require.Zero(t, remote.gets.Load())
	require.Equal(t, int32(1), executor.recoveries.Load())
	require.Equal(t, int32(1), executor.activations.Load())
	record, err := service.GetChannel(t.Context(), fixture.terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseActive, record.Snapshot.Phase)
}

// TestSyncReceiveIntentDetachesCleanupFromCancelledRequest proves the real
// synchronization path starts durable cleanup after its bind response is lost.
func TestSyncReceiveIntentDetachesCleanupFromCancelledRequest(t *testing.T) {
	t.Parallel()

	fixture := newReceiveFailureFixture(t, nil, false)
	cause := errors.New("injected lost binding response")
	ctx, cancel := context.WithCancel(t.Context())
	fixture.clientStore.failNextCommittedWrite(cancel, cause)
	controller := &NativeArkChannelController{
		party: arkchannel.PartyClient, service: fixture.clientService,
		remote: fixture.hubPeer,
	}

	err := controller.syncReceiveIntent(ctx, fixture.hash)
	require.ErrorIs(t, err, ErrReceiveChannelFallback)
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.NoError(t, fixture.clientExecutor.cleanupCtxErr)
	require.Equal(t, 1, fixture.clientExecutor.abortRequests)
	for _, service := range []*arkchannel.Service{
		fixture.hubService, fixture.clientService,
	} {
		record, loadErr := service.GetChannel(
			t.Context(), fixture.terms.ID,
		)
		require.NoError(t, loadErr)
		require.Equal(t, arkchannel.PhaseFailed, record.Snapshot.Phase)
		require.True(t, record.Snapshot.OORAborted)
	}
}

// TestManifestIncomingChannelPreservesReplayableFailure proves caller and
// transport ambiguity leave both durable negotiation actions intact.
func TestManifestIncomingChannelPreservesReplayableFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{
			name: "caller canceled",
			err:  context.Canceled,
		},
		{
			name: "caller deadline",
			err:  context.DeadlineExceeded,
		},
		{
			name: "mailbox outcome ambiguous",
			err: fmt.Errorf("%w: response lost",
				lnruntime.ErrFundingNegotiationAmbiguous),
		},
		{
			name: "transport unavailable",
			err: status.Error(
				codes.Unavailable, "transport unavailable",
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newReceiveFailureFixture(t, test.err, true)
			_, err := fixture.controller.ManifestIncomingChannel(
				t.Context(), fixture.hash,
				fixture.terms.Capacity, fixture.terms.Capacity,
				fixture.terms.ReservedSCID,
			)
			require.ErrorIs(t, err, test.err)
			require.NotErrorIs(t, err, ErrReceiveChannelFallback)
			require.Zero(t, fixture.hubExecutor.aborts)

			for _, service := range []*arkchannel.Service{
				fixture.hubService, fixture.clientService,
			} {
				record, loadErr := service.GetChannel(
					t.Context(), fixture.terms.ID,
				)
				require.NoError(t, loadErr)
				require.Equal(
					t, arkchannel.PhaseNegotiating,
					record.Snapshot.Phase,
				)
				require.False(t, record.Snapshot.OORAborted)
			}
		})
	}
}
