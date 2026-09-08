package waved

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// receiveFailurePeer adapts a production service and SQL-backed FSM to the
// process peer surface used by the hub controller.
type receiveFailurePeer struct {
	lnruntime.ProcessFundingPeer

	service         *arkchannel.Service
	failureRequests int
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

	p.failureRequests++
	_, err := p.service.ApplyLocalEvent(
		ctx, id, &arkchannel.Fail{
			Reason: reason,
		},
	)

	return err
}

// ApplyChannelEvent admits the hub's authenticated OOR terminal fact through
// the peer side of the production service.
func (p *receiveFailurePeer) ApplyChannelEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	return p.service.ApplyPeerEvent(ctx, id, event)
}

// receiveFailureExecutor drives the real FSM's cleanup actions while
// injecting only the native lnd negotiation result under test.
type receiveFailureExecutor struct {
	party          arkchannel.Party
	service        *arkchannel.Service
	remote         *receiveFailurePeer
	negotiationErr error
	cancelRequest  context.CancelFunc
	aborts         int
	cancellations  int
	cleanupCtxErr  error
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
		e.aborts++
		e.cleanupCtxErr = ctx.Err()
		_, err := e.service.ApplyLocalEvent(
			ctx, id, &arkchannel.OORAborted{
				SessionID: action.Source.OORSessionID,
				Reason:    action.Reason,
			},
		)

		return err

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
	controller     *NativeArkChannelController
	hubService     *arkchannel.Service
	clientService  *arkchannel.Service
	hubExecutor    *receiveFailureExecutor
	clientExecutor *receiveFailureExecutor
	hubPeer        *receiveFailurePeer
	clientPeer     *receiveFailurePeer
	terms          arkchannel.Terms
	hash           lntypes.Hash
}

// newReceiveFailureFixture creates paired production stores, coordinators,
// and FSMs at the exact pre-PONR negotiation phase.
func newReceiveFailureFixture(t *testing.T, negotiationErr error,
	bindClient bool) *receiveFailureFixture {

	t.Helper()
	terms := testPrePONRTerms(t)
	hash := lntypes.Hash(terms.PaymentHash)
	terms.ID = arkchannel.ReceiveIntentID(hash)
	binding := testPrePONRBinding(t, terms)

	newEndpoint := func(party arkchannel.Party) (*arkchannel.Service,
		*arkchannel.Coordinator, *receiveFailureExecutor) {

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
		store := raw.NewArkChannelStore(
			clock.NewTestClock(
				time.Unix(40_000, 0),
			),
		)
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

		return service, coordinator, executor
	}

	hubService, hubCoordinator, hubExecutor := newEndpoint(
		arkchannel.PartyHub,
	)
	clientService, clientCoordinator, clientExecutor := newEndpoint(
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

	return &receiveFailureFixture{
		controller: &NativeArkChannelController{
			party:   arkchannel.PartyHub,
			service: hubService,
			remote:  clientPeer,
		},
		hubService: hubService, clientService: clientService,
		hubExecutor: hubExecutor, clientExecutor: clientExecutor,
		hubPeer: hubPeer, clientPeer: clientPeer,
		terms: terms, hash: hash,
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
// failure terminates its source-less record before asking the hub to abort the
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
	require.Equal(t, 1, fixture.hubPeer.failureRequests)
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
	require.False(t, client.Snapshot.OORAborted)
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
