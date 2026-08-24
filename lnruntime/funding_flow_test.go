package lnruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	clientdb "github.com/lightninglabs/wavelength/db"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/clock"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const testFundingCapacity = btcutil.Amount(200_000)

type fundingFinalization struct {
	channelPoint wire.OutPoint
}

// fundingFlowNode contains one composed runtime and the callbacks used to
// observe native lnd funding and channel-link activation.
type fundingFlowNode struct {
	key         *btcec.PrivateKey
	db          *channeldb.DB
	runtime     *Runtime
	notifier    *VirtualFundingNotifier
	peer        *Peer
	fundingWire *FundingWire
	finalized   chan fundingFinalization
	links       chan *chanstate.OpenChannel
	failures    chan error
	intents     *staticIntentSource

	restoreAddsDisabled atomic.Bool
}

// fundingNegotiationSink models the already-tested durable channel FSM while
// this component test exercises the production native funding coordinator.
type fundingNegotiationSink struct {
	mu     sync.Mutex
	node   *fundingFlowNode
	party  arkchannel.Party
	record arkchannel.Record
}

// fundingWireTestSink keeps the service's backing fact current for wire-side
// validation while the existing funding sink models the remaining barriers.
type fundingWireTestSink struct {
	service *arkchannel.Service
	mirror  *fundingNegotiationSink
}

// Apply records immutable backing in the service queried by FundingWire and
// delegates the lnd finalization and activation barriers to the test sink.
func (s *fundingWireTestSink) Apply(ctx context.Context, id arkchannel.ID,
	event arkchannel.Event) (arkchannel.Record, error) {

	if _, ok := event.(*arkchannel.BackingSigned); ok {
		if _, err := s.service.Apply(ctx, id, event); err != nil {
			return arkchannel.Record{}, err
		}
	}

	return s.mirror.Apply(ctx, id, event)
}

// noOpChannelRecoveryManager satisfies composition in lnd-focused tests. The
// durable recovery archive and activation barrier are exercised separately.
type noOpChannelRecoveryManager struct{}

// ExportRecoveryPackage returns an unused package in these funding tests.
func (*noOpChannelRecoveryManager) ExportRecoveryPackage(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding) (
	arkchannel.RecoveryPackage, error) {

	return arkchannel.RecoveryPackage{}, nil
}

// InstallRecoveryPackage accepts the unused package in these funding tests.
func (*noOpChannelRecoveryManager) InstallRecoveryPackage(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding,
	arkchannel.RecoveryPackage) error {

	return nil
}

// activeFundingFlow is one fully activated unpublished channel and the two
// endpoint snapshots that authorized it.
type activeFundingFlow struct {
	hubChannel    *chanstate.OpenChannel
	clientChannel *chanstate.OpenChannel
	hubSink       *fundingNegotiationSink
	clientSink    *fundingNegotiationSink
}

// cooperativeCloseActionExecutor keeps this composed test focused on the
// direct-close actions while using the production process and services.
type cooperativeCloseActionExecutor struct {
	closer arkchannel.ChannelCooperativeCloser
}

// BindChannelEventSink connects the local endpoint to its durable service.
func (e *cooperativeCloseActionExecutor) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	binder, ok := e.closer.(arkchannel.ChannelEventSinkBinder)
	if !ok {
		return fmt.Errorf("cooperative close process cannot bind " +
			"event sink")
	}

	return binder.BindChannelEventSink(sink)
}

// Execute dispatches the three durable cooperative-close actions.
func (e *cooperativeCloseActionExecutor) Execute(ctx context.Context,
	id arkchannel.ID, action arkchannel.Action) error {

	switch action := action.(type) {
	case *arkchannel.NegotiateCooperativeClose:
		return e.closer.NegotiateCooperativeClose(
			ctx, id, action.Terms, action.Source, action.Backing,
			action.Request,
		)

	case *arkchannel.PublishCooperativeClose:
		return e.closer.PublishCooperativeClose(
			ctx, id, action.Terms, action.Source, action.Close,
		)

	case *arkchannel.FinalizeCooperativeClose:
		return e.closer.FinalizeCooperativeClose(
			ctx, id, action.Terms, action.Backing, action.Source,
			action.Request, action.Close,
		)

	default:
		return fmt.Errorf("unexpected cooperative close action %T",
			action)
	}
}

// blockingCooperativeClosePublisher models the ordinary OOR completion barrier
// so the test can prove lnd remains open until replacement VTXOs finalize.
type blockingCooperativeClosePublisher struct {
	published chan arkchannel.CooperativeClose
	confirm   chan struct{}
}

// cooperativeCloseSigningOrder records which policy role signs the exact
// settlement transaction first.
type cooperativeCloseSigningOrder struct {
	mu    sync.Mutex
	calls []string
}

// recordingCooperativeCloseSigner wraps an ordinary lnd signer without
// changing any signing behavior.
type recordingCooperativeCloseSigner struct {
	input.Signer

	label string
	order *cooperativeCloseSigningOrder
}

// SignOutputRaw records the role and delegates the signature operation.
func (s *recordingCooperativeCloseSigner) SignOutputRaw(tx *wire.MsgTx,
	desc *input.SignDescriptor) (input.Signature, error) {

	s.order.mu.Lock()
	s.order.calls = append(s.order.calls, s.label)
	s.order.mu.Unlock()

	return s.Signer.SignOutputRaw(tx, desc)
}

// snapshot returns an isolated signing order.
func (o *cooperativeCloseSigningOrder) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]string(nil), o.calls...)
}

// exactCooperativeCloseDeliveryValidator models a wallet ownership lookup for
// one expected payout script.
func exactCooperativeCloseDeliveryValidator(
	expected []byte) CooperativeCloseDeliveryValidator {

	return CooperativeCloseDeliveryValidatorFunc(func(_ context.Context,
		_ arkchannel.ID, script []byte) error {

		if !bytes.Equal(script, expected) {
			return fmt.Errorf("payout script is not wallet owned")
		}

		return nil
	})
}

// SettleCooperativeClose validates the exact OOR authorization and waits for
// the simulated durable OOR completion.
func (p *blockingCooperativeClosePublisher) SettleCooperativeClose(
	ctx context.Context, _ arkchannel.ID, _ arkchannel.Terms,
	_ arkchannel.VTXOBinding, _ arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	if settlement.TxID == (chainhash.Hash{}) {
		return fmt.Errorf("cooperative close OOR session ID is empty")
	}

	select {
	case p.published <- settlement:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-p.confirm:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// Apply records funding facts and releases virtual confirmation only after
// both native lnd endpoints have finalized their initial commitments.
func (s *fundingNegotiationSink) Apply(_ context.Context, id arkchannel.ID,
	event arkchannel.Event) (arkchannel.Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.record.Snapshot.Terms.ID {
		return arkchannel.Record{}, fmt.Errorf("unexpected channel ID")
	}

	snapshot := &s.record.Snapshot
	switch event := event.(type) {
	case *arkchannel.BackingSigned:
		backing := event.Backing.Clone()
		snapshot.Backing = &backing

	case *arkchannel.FundingFinalized:
		if event.Party == arkchannel.PartyClient {
			snapshot.ClientFinalized = true
		} else {
			snapshot.HubFinalized = true
		}
		if snapshot.ClientFinalized && snapshot.HubFinalized &&
			snapshot.Terms.Funder == s.party {

			snapshot.OORFinalized = true
			snapshot.RecoveryReady = true
			if err := s.node.runtime.Funding().ConfirmBacking(
				snapshot.Backing.ChannelPoint.Hash,
			); err != nil {
				return arkchannel.Record{}, err
			}
		}

	case *arkchannel.OORFinalized:
		if event.SessionID != snapshot.Source.OORSessionID {
			return arkchannel.Record{}, fmt.Errorf("unexpected " +
				"OOR session")
		}
		snapshot.OORFinalized = true
		snapshot.RecoveryReady = true
		if err := s.node.runtime.Funding().ConfirmBacking(
			snapshot.Backing.ChannelPoint.Hash,
		); err != nil {
			return arkchannel.Record{}, err
		}

	case *arkchannel.ChannelActive:
		snapshot.Phase = arkchannel.PhaseActive

	default:
		return arkchannel.Record{}, fmt.Errorf("unexpected channel "+
			"event %T", event)
	}
	s.record.Revision++

	return s.record, nil
}

// fundingFlowTransport preserves lnd wire ordering over an in-memory version
// of the application transport used between Wavelength and swapdk-server.
type fundingFlowTransport struct {
	remote *fundingFlowNode
	queue  chan []lnwire.Message
	quit   chan struct{}
	wg     sync.WaitGroup
	stop   sync.Once
}

// newFundingFlowTransport starts a FIFO delivery loop that models the
// production mailbox boundary: durable admission completes before the remote
// lnd subsystem handles the message.
func newFundingFlowTransport(remote *fundingFlowNode) *fundingFlowTransport {
	transport := &fundingFlowTransport{
		remote: remote,
		queue:  make(chan []lnwire.Message, 100),
		quit:   make(chan struct{}),
	}
	transport.wg.Add(1)
	go transport.deliver()

	return transport
}

// SendMessages admits one ordered message batch without synchronously calling
// into the remote lnd runtime.
func (t *fundingFlowTransport) SendMessages(_ bool,
	messages ...lnwire.Message) error {

	batch := append([]lnwire.Message(nil), messages...)
	select {
	case t.queue <- batch:
		return nil

	case <-t.quit:
		return fmt.Errorf("funding flow transport stopped")
	}
}

// Stop terminates the delivery loop.
func (t *fundingFlowTransport) Stop() {
	t.stop.Do(func() {
		close(t.quit)
		t.wg.Wait()
	})
}

// deliver handles admitted batches in FIFO order on the remote side.
func (t *fundingFlowTransport) deliver() {
	defer t.wg.Done()

	for {
		select {
		case messages := <-t.queue:
			if err := t.dispatch(messages...); err != nil {
				select {
				case t.remote.failures <- err:
				case <-t.quit:
				}
			}

		case <-t.quit:
			return
		}
	}
}

// dispatch routes funding messages to funding.Manager and commitment updates
// to the native channel link.
func (t *fundingFlowTransport) dispatch(messages ...lnwire.Message) error {
	for _, message := range messages {
		switch message := message.(type) {
		case *lnwire.OpenChannel, *lnwire.AcceptChannel,
			*lnwire.FundingCreated, *lnwire.FundingSigned,
			*lnwire.ChannelReady, *lnwire.Warning, *lnwire.Error:

			if err := t.remote.runtime.Funding().ProcessMessage(
				message, t.remote.peer,
			); err != nil {
				return err
			}

		case lnwire.LinkUpdater:
			if err := t.remote.runtime.HandleChannelMessage(
				message,
			); err != nil {
				return err
			}

		case *lnwire.ChannelReestablish:
			go t.deliverReestablish(message)

		case *lnwire.Custom:
			if t.remote.fundingWire == nil ||
				!t.remote.fundingWire.Handles(message) {
				return fmt.Errorf("unknown custom funding " +
					"message")
			}

			if err := t.remote.fundingWire.Handle(
				context.Background(), message,
			); err != nil {
				return err
			}

		case *lnwire.NodeAnnouncement1, *lnwire.ChannelAnnouncement1,
			*lnwire.ChannelUpdate1:

			// Private runtimes have no graph or gossiper.

		default:
			return fmt.Errorf("unexpected lnd message %T", message)
		}
	}

	return nil
}

// noOpFundingActionExecutor keeps this wire test focused on recording the
// peer-readiness barrier without dispatching channel negotiation.
type noOpFundingActionExecutor struct{}

// ValidatePreparedOOR accepts the fixture after its normal state validation.
func (*noOpFundingActionExecutor) ValidatePreparedOOR(context.Context,
	arkchannel.Terms, arkchannel.VTXOBinding) error {

	return nil
}

// Execute accepts an unused action.
func (*noOpFundingActionExecutor) Execute(context.Context, arkchannel.ID,
	arkchannel.Action) error {

	return nil
}

// TestFundingWireRecordsPeerReadiness proves the hub-to-client reverse
// transport durably records readiness without synchronously dispatching the
// client's channel action.
func TestFundingWireRecordsPeerReadiness(t *testing.T) {
	t.Parallel()

	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)

	rawStore := clientdb.NewTestDB(t)
	store := clientdb.NewStore(
		rawStore.DB, rawStore.Queries, rawStore.Backend(),
		btclog.Disabled,
	).NewArkChannelStore(clock.NewDefaultClock())
	coordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	service, err := arkchannel.NewService(
		coordinator, &noOpFundingActionExecutor{},
	)
	require.NoError(t, err)

	record := fundingIntentRecord(
		t, hub, client, lndfunding.PendingChanID{2, 4, 6, 8},
	)
	_, err = service.RegisterReceiveIntent(
		t.Context(), record.Snapshot.Terms,
	)
	require.NoError(t, err)
	_, err = service.BindPreparedOOR(
		t.Context(), record.Snapshot.Terms.ID, *record.Snapshot.Source,
	)
	require.NoError(t, err)

	clientEndpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyClient, client.runtime.Funding(),
		input.NewMockSigner(
			[]*btcec.PrivateKey{client.key}, nil,
		),
		keychain.KeyDescriptor{
			PubKey: client.key.PubKey(),
		},
	)
	require.NoError(t, err)
	clientWire, err := NewFundingWire(client.peer)
	require.NoError(t, err)
	client.fundingWire = clientWire
	t.Cleanup(clientWire.Close)
	require.NoError(
		t,
		clientWire.BindServer(
			FundingWireServerConfig{
				Service: service,
				Funding: clientEndpoint,
			},
		),
	)

	hubWire, err := NewFundingWire(hub.peer)
	require.NoError(t, err)
	hub.fundingWire = hubWire
	t.Cleanup(hubWire.Close)
	_, err = hubWire.Counterparty().ApplyChannelEvent(
		t.Context(), record.Snapshot.Terms.ID,
		&arkchannel.FundingPeerReady{},
	)
	require.NoError(t, err)

	stored, err := service.GetChannel(
		t.Context(), record.Snapshot.Terms.ID,
	)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseNegotiating, stored.Snapshot.Phase)
}

// TestFundingWireNegotiatesHubFundedChannel proves the production reverse
// transport supports the complete hub-initiated lnd funding exchange.
func TestFundingWireNegotiatesHubFundedChannel(t *testing.T) {
	t.Parallel()

	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)
	require.NoError(t, hub.runtime.Start())
	require.NoError(t, client.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, hub.runtime.Stop())
		require.NoError(t, client.runtime.Stop())
	})

	record := fundingIntentRecord(
		t, hub, client, lndfunding.PendingChanID{2, 4, 6, 9},
	)
	hub.intents.record = record
	client.intents.record = record

	rawStore := clientdb.NewTestDB(t)
	store := clientdb.NewStore(
		rawStore.DB, rawStore.Queries, rawStore.Backend(),
		btclog.Disabled,
	).NewArkChannelStore(clock.NewDefaultClock())
	coordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	service, err := arkchannel.NewService(
		coordinator, &noOpFundingActionExecutor{},
	)
	require.NoError(t, err)
	_, err = service.RegisterReceiveIntent(
		t.Context(), record.Snapshot.Terms,
	)
	require.NoError(t, err)
	_, err = service.BindPreparedOOR(
		t.Context(), record.Snapshot.Terms.ID, *record.Snapshot.Source,
	)
	require.NoError(t, err)
	_, err = service.RecordChannelEvent(
		t.Context(), record.Snapshot.Terms.ID,
		&arkchannel.FundingPeerReady{},
	)
	require.NoError(t, err)

	hubEndpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyHub, hub.runtime.Funding(),
		input.NewMockSigner(
			[]*btcec.PrivateKey{hub.key}, nil,
		),
		keychain.KeyDescriptor{
			PubKey: hub.key.PubKey(),
		},
	)
	require.NoError(t, err)
	clientEndpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyClient, client.runtime.Funding(),
		input.NewMockSigner(
			[]*btcec.PrivateKey{client.key}, nil,
		),
		keychain.KeyDescriptor{
			PubKey: client.key.PubKey(),
		},
	)
	require.NoError(t, err)
	hubSink := &fundingNegotiationSink{
		node: hub, party: arkchannel.PartyHub, record: record,
	}
	clientSink := &fundingNegotiationSink{
		node: client, party: arkchannel.PartyClient, record: record,
	}
	require.NoError(t, hubEndpoint.BindChannelEventSink(hubSink))
	require.NoError(
		t,
		clientEndpoint.BindChannelEventSink(
			&fundingWireTestSink{
				service: service,
				mirror:  clientSink,
			},
		),
	)

	clientWire, err := NewFundingWire(client.peer)
	require.NoError(t, err)
	client.fundingWire = clientWire
	t.Cleanup(clientWire.Close)
	require.NoError(
		t,
		clientWire.BindServer(
			FundingWireServerConfig{
				Service: service,
				Funding: clientEndpoint,
			},
		),
	)
	hubWire, err := NewFundingWire(hub.peer)
	require.NoError(t, err)
	hub.fundingWire = hubWire
	t.Cleanup(hubWire.Close)

	negotiator, err := NewChannelNegotiator(
		hubEndpoint, hubWire.Counterparty(), hub.peer,
		&noOpChannelRecoveryManager{},
	)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(
		t, negotiator.NegotiateChannel(
			ctx, record.Snapshot.Terms.ID, record.Snapshot.Terms,
			*record.Snapshot.Source,
		),
	)

	require.Equal(t, arkchannel.PhaseActive, hubSink.record.Snapshot.Phase)
	require.Equal(
		t, arkchannel.PhaseActive, clientSink.record.Snapshot.Phase,
	)
	hubChannel := awaitLink(t, hub)
	clientChannel := awaitLink(t, client)
	require.Equal(
		t, hubChannel.FundingOutpoint, clientChannel.FundingOutpoint,
	)
	require.Positive(t, hubChannel.LocalCommitment.LocalBalance)
	require.Zero(t, clientChannel.LocalCommitment.LocalBalance)
}

// deliverReestablish models durable mailbox retry while the remote runtime is
// still rebuilding its link during a simultaneous restart.
func (t *fundingFlowTransport) deliverReestablish(
	message *lnwire.ChannelReestablish) {

	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		err := t.remote.runtime.HandleChannelReestablish(
			message, t.remote.peer,
			func(*chanstate.OpenChannel) (LinkConfig, error) {
				cfg := testLinkConfig(
					t.remote.peer, t.remote.failures,
				)
				cfg.AddsDisabled = t.remote.
					restoreAddsDisabled.Load()

				return cfg, nil
			},
		)
		if err == nil {
			return
		}
		if !errors.Is(err, htlcswitch.ErrChannelLinkNotFound) {
			t.remote.failures <- err

			return
		}

		select {
		case <-ticker.C:
		case <-deadline:
			t.remote.failures <- err

			return
		}
	}
}

// TestNativeFundingFlowPaysBothDirections proves the composed funding manager
// negotiates an unpublished channel and hands it to the same native links used
// by lnd's invoice and payment lifecycles.
func TestNativeFundingFlowPaysBothDirections(t *testing.T) {
	t.Parallel()

	alice := newFundingFlowNode(t, arkchannel.PartyHub)
	bob := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, alice, bob)
	require.NoError(t, alice.runtime.Start())
	require.NoError(t, bob.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, alice.runtime.Stop())
		require.NoError(t, bob.runtime.Stop())
	})

	pendingID := lndfunding.PendingChanID{1, 3, 3, 7}
	record := fundingIntentRecord(t, alice, bob, pendingID)
	flow := activateFundingFlowChannel(t, alice, bob, record)
	aliceSink := flow.hubSink
	bobSink := flow.clientSink
	require.NotNil(t, aliceSink.record.Snapshot.Backing)
	require.Equal(
		t, arkchannel.PhaseActive, aliceSink.record.Snapshot.Phase,
	)
	require.Equal(t, arkchannel.PhaseActive,
		bobSink.record.Snapshot.Phase)
	aliceChannel := flow.hubChannel
	bobChannel := flow.clientChannel
	require.Equal(
		t, aliceChannel.FundingOutpoint, bobChannel.FundingOutpoint,
	)
	require.False(t, aliceChannel.IsPending)
	require.False(t, bobChannel.IsPending)
	require.Positive(t, aliceChannel.LocalCommitment.LocalBalance)
	require.Zero(t, bobChannel.LocalCommitment.LocalBalance)
	bobLink, err := bob.runtime.GetLink(bobChannel.ShortChanID())
	require.NoError(t, err)
	require.Zero(t, bobLink.Bandwidth())
	aliceLink, err := alice.runtime.GetLink(aliceChannel.ShortChanID())
	require.NoError(t, err)
	require.GreaterOrEqual(
		t, aliceLink.Bandwidth(), lnwire.MilliSatoshi(30_000_000),
	)

	const firstAmount = lnwire.MilliSatoshi(30_000_000)
	payRuntimeInvoice(t, alice, bob, firstAmount, lntypes.Preimage{1, 2, 3})
	for _, node := range []*fundingFlowNode{alice, bob} {
		_, err := node.runtime.QuiesceChannel(
			t.Context(), aliceChannel.FundingOutpoint,
		)
		require.NoError(t, err)
	}
	alice.runtime.ResumeChannel(aliceChannel.FundingOutpoint)
	bob.runtime.ResumeChannel(bobChannel.FundingOutpoint)

	// Restart only Bob's link. Alice recycles its still-live link when the
	// mailbox delivers Bob's new channel_reestablish handshake.
	bob.runtime.RemoveLink(bobChannel.FundingOutpoint)
	restored, err := bob.runtime.RestorePeerLinks(
		bob.peer, func(*chanstate.OpenChannel) (LinkConfig, error) {
			return testLinkConfig(bob.peer, bob.failures), nil
		},
	)
	require.NoError(t, err)
	require.Len(t, restored, 1)
	recycleDeadline := time.After(5 * time.Second)
	recycleTicker := time.NewTicker(10 * time.Millisecond)
	defer recycleTicker.Stop()
	for {
		newAliceLink, linkErr := alice.runtime.GetLink(
			aliceChannel.ShortChanID(),
		)
		if linkErr == nil && newAliceLink != aliceLink {
			break
		}
		select {
		case failure := <-alice.failures:
			require.NoError(t, failure)

		case failure := <-bob.failures:
			require.NoError(t, failure)

		case <-recycleTicker.C:
		case <-recycleDeadline:
			t.Fatal("live peer did not recycle its channel link")
		}
	}

	const returnAmount = lnwire.MilliSatoshi(10_000_000)
	payRuntimeInvoice(
		t, bob, alice, returnAmount, lntypes.Preimage{7, 8, 9},
	)

	closeTx := cooperativelyCloseFundingFlowChannel(
		t, alice, bob, aliceChannel.FundingOutpoint,
	)
	require.Equal(
		t, aliceChannel.FundingOutpoint,
		closeTx.TxIn[0].PreviousOutPoint,
	)
}

// TestNativeFundingFlowRestoresQuiescedLinks proves a restart in any durable
// cooperative-close phase installs both links with new HTLC adds disabled.
func TestNativeFundingFlowRestoresQuiescedLinks(t *testing.T) {
	t.Parallel()

	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)
	require.NoError(t, hub.runtime.Start())
	require.NoError(t, client.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, hub.runtime.Stop())
		require.NoError(t, client.runtime.Stop())
	})

	record := fundingIntentRecord(
		t, hub, client, lndfunding.PendingChanID{8, 1, 7, 2},
	)
	flow := activateFundingFlowChannel(t, hub, client, record)
	hub.restoreAddsDisabled.Store(true)
	client.restoreAddsDisabled.Store(true)
	client.runtime.RemoveLink(flow.clientChannel.FundingOutpoint)

	restored, err := client.runtime.RestorePeerLinks(
		client.peer,
		func(*chanstate.OpenChannel) (LinkConfig, error) {
			cfg := testLinkConfig(client.peer, client.failures)
			cfg.AddsDisabled = true

			return cfg, nil
		},
	)
	require.NoError(t, err)
	require.Len(t, restored, 1)

	require.Eventually(t, func() bool {
		for _, node := range []*fundingFlowNode{hub, client} {
			channels, err := node.db.ChannelStateDB().
				FetchAllOpenChannels()
			if err != nil || len(channels) != 1 {
				return false
			}
			link, err := node.runtime.GetLink(
				channels[0].ShortChanID(),
			)
			if err != nil ||
				!link.IsFlushing(htlcswitch.Incoming) ||
				!link.IsFlushing(htlcswitch.Outgoing) {
				return false
			}
		}

		return true
	}, 5*time.Second, 10*time.Millisecond)
}

// TestNativeFundingFlowInArkCooperativeClose proves an active unpublished
// channel can carry payments in both directions and then settle its clean lnd
// balances with an ordinary OOR package over the channel VTXO's 3-of-3 path.
// The backing channel point never becomes an input to the close package.
func TestNativeFundingFlowInArkCooperativeClose(t *testing.T) {
	t.Parallel()

	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)
	require.NoError(t, hub.runtime.Start())
	require.NoError(t, client.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, hub.runtime.Stop())
		require.NoError(t, client.runtime.Stop())
	})

	clientArkKey := testIntentKey(t)
	hubArkKey := testIntentKey(t)
	operatorArkKey := testIntentKey(t)
	pendingID := lndfunding.PendingChanID{4, 8, 1, 5}
	record := fundingIntentRecord(t, hub, client, pendingID)
	record.Snapshot.Terms.Kind = arkchannel.KindPromotion
	record.Snapshot.Terms.Funder = arkchannel.PartyClient
	record.Snapshot.Terms.PaymentHash = [32]byte{}
	record.Snapshot.Terms.VTXO.ClientArkKey = compressedIntentKey(
		clientArkKey,
	)
	record.Snapshot.Terms.VTXO.HubArkKey = compressedIntentKey(hubArkKey)
	record.Snapshot.Terms.VTXO.ArkOperatorKey = compressedIntentKey(
		operatorArkKey,
	)
	record.Snapshot.Source = testIntentBinding(
		t, record.Snapshot.Terms, testFundingCapacity+1_000, 1,
	)
	request := arkchannel.CooperativeCloseRequest{
		Initiator:            arkchannel.PartyClient,
		ClientDeliveryScript: client.key.PubKey().SerializeCompressed(),
		HubDeliveryScript:    hub.key.PubKey().SerializeCompressed(),
	}
	flow := activateFundingFlowChannel(t, hub, client, record)
	channelPoint := flow.hubChannel.FundingOutpoint
	require.Equal(t, channelPoint, flow.clientChannel.FundingOutpoint)
	require.Zero(t, flow.hubChannel.LocalCommitment.LocalBalance)
	require.Positive(t, flow.clientChannel.LocalCommitment.LocalBalance)

	payRuntimeInvoice(
		t, client, hub, lnwire.MilliSatoshi(30_000_000),
		lntypes.Preimage{1, 4, 1, 5},
	)
	payRuntimeInvoice(
		t, hub, client, lnwire.MilliSatoshi(10_000_000),
		lntypes.Preimage{9, 2, 6, 5},
	)

	hubRawStore := clientdb.NewTestDB(t)
	clientRawStore := clientdb.NewTestDB(t)
	hubStore := clientdb.NewStore(
		hubRawStore.DB, hubRawStore.Queries, hubRawStore.Backend(),
		btclog.Disabled,
	).NewArkChannelStore(clock.NewDefaultClock())
	clientStore := clientdb.NewStore(
		clientRawStore.DB, clientRawStore.Queries,
		clientRawStore.Backend(), btclog.Disabled,
	).NewArkChannelStore(clock.NewDefaultClock())
	_, err := hubStore.Create(t.Context(), flow.hubSink.record.Snapshot)
	require.NoError(t, err)
	_, err = clientStore.Create(
		t.Context(), flow.clientSink.record.Snapshot,
	)
	require.NoError(t, err)
	hubFSM, err := arkchannel.NewCoordinator(hubStore)
	require.NoError(t, err)
	clientFSM, err := arkchannel.NewCoordinator(clientStore)
	require.NoError(t, err)

	signingOrder := &cooperativeCloseSigningOrder{}
	hubEndpoint, err := NewNativeCooperativeCloseEndpoint(
		arkchannel.PartyHub, hub.runtime,
		&recordingCooperativeCloseSigner{
			Signer: input.NewMockSigner(
				[]*btcec.PrivateKey{hubArkKey}, nil,
			),
			label: "hub",
			order: signingOrder,
		},
		keychain.KeyDescriptor{PubKey: hubArkKey.PubKey()},
		exactCooperativeCloseDeliveryValidator(
			request.HubDeliveryScript,
		),
	)
	require.NoError(t, err)
	clientEndpoint, err := NewNativeCooperativeCloseEndpoint(
		arkchannel.PartyClient, client.runtime, nil,
		keychain.KeyDescriptor{},
		exactCooperativeCloseDeliveryValidator(
			request.ClientDeliveryScript,
		),
	)
	require.NoError(t, err)
	publisher := &blockingCooperativeClosePublisher{
		published: make(chan arkchannel.CooperativeClose, 1),
		confirm:   make(chan struct{}),
	}
	defended := make(chan wire.OutPoint, 1)
	hubClose, err := NewHubCooperativeCloseProcess(
		hubEndpoint,
		&stableCooperativeCloseDelivery{
			script: request.HubDeliveryScript,
		},
		CooperativeCloseObserverFunc(func(context.Context,
			chainhash.Hash, btcutil.Amount) error {

			return nil
		}),
		CooperativeCloseDefenderFunc(func(_ context.Context,
			outpoint wire.OutPoint) error {

			defended <- outpoint

			return nil
		}),
	)
	require.NoError(t, err)
	hubExecutor := &HubCooperativeCloseExecutor{
		HubCooperativeCloseProcess: hubClose,
	}
	hubService, err := arkchannel.NewService(
		hubFSM, &cooperativeCloseActionExecutor{
			closer: hubExecutor,
		},
	)
	require.NoError(t, err)
	hubRPC, err := NewCooperativeClosePeerRPCServer(
		record.Snapshot.Terms.ClientNodeKey, hubClose,
	)
	require.NoError(t, err)
	mux := mailboxrpc.NewServeMux()
	arkchannelrpc.RegisterArkChannelPeerServiceMailboxServer(mux, hubRPC)
	mailboxPeer, err := NewMailboxCooperativeClosePeer(
		newLoopbackCooperativeCloseRPC(mux),
	)
	require.NoError(t, err)
	lossyPeer := &lossyCooperativeClosePeer{
		ProcessCooperativeClosePeer: mailboxPeer,
		loseBegin:                   true,
		loseComplete:                true,
	}
	clientDelivery := &stableCooperativeCloseDelivery{
		script: request.ClientDeliveryScript,
	}
	clientClose, err := NewClientCooperativeCloseProcess(
		clientEndpoint, lossyPeer, publisher, clientDelivery,
	)
	require.NoError(t, err)
	clientService, err := arkchannel.NewService(
		clientFSM, &cooperativeCloseActionExecutor{
			closer: clientClose,
		},
	)
	require.NoError(t, err)

	_, err = clientClose.RequestCooperativeClose(
		t.Context(), record.Snapshot.Terms.ID,
	)
	require.ErrorContains(t, err, "lost begin response")
	require.Equal(t, 1, clientDelivery.callCount())

	_, err = clientClose.RequestCooperativeClose(
		t.Context(), record.Snapshot.Terms.ID,
	)
	require.ErrorContains(t, err, "lost complete response")
	require.Equal(t, 2, clientDelivery.callCount())

	// The client received nothing, but the hub must already hold the
	// exact hub authorization. OOR signing cannot start until the client
	// recovers it and completes the durable acknowledgement barrier.
	hubCompleted, err := hubStore.Get(
		t.Context(), record.Snapshot.Terms.ID,
	)
	require.NoError(t, err)
	clientWaiting, err := clientStore.Get(
		t.Context(), record.Snapshot.Terms.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, hubCompleted.Snapshot.CooperativeClose)
	require.True(t, hubCompleted.Snapshot.HubCloseSigned)
	require.False(t, hubCompleted.Snapshot.ClientCloseSigned)
	require.Nil(t, clientWaiting.Snapshot.CooperativeClose)
	require.Equal(
		t, []string{"hub"},
		signingOrder.snapshot(),
	)
	select {
	case <-publisher.published:
		t.Fatal("cooperative close OOR started before client recovery")

	default:
	}

	closeResult := make(chan error, 1)
	go func() {
		_, closeErr := clientClose.RequestCooperativeClose(
			t.Context(), record.Snapshot.Terms.ID,
		)
		closeResult <- closeErr
	}()

	var settlement arkchannel.CooperativeClose
	select {
	case settlement = <-publisher.published:
	case err := <-closeResult:
		require.NoError(t, err)
		t.Fatal("cooperative close completed without OOR settlement")

	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for cooperative OOR settlement")
	}
	require.Equal(
		t, []string{"hub"},
		signingOrder.snapshot(),
	)
	require.NoError(
		t, settlement.Validate(
			record.Snapshot.Terms, *record.Snapshot.Source, request,
		),
	)
	require.Len(t, settlement.Transaction, schnorr.SignatureSize)
	require.NotEqual(t, channelPoint.Hash, settlement.TxID)

	// OOR submission has started, but lnd must retain both open-channel
	// records until the OOR actor reports durable completion.
	for _, node := range []*fundingFlowNode{hub, client} {
		_, err := node.db.ChannelStateDB().FetchChannel(channelPoint)
		require.NoError(t, err)
	}
	hubSigned, err := hubStore.Get(t.Context(), record.Snapshot.Terms.ID)
	require.NoError(t, err)
	clientSigned, err := clientStore.Get(
		t.Context(), record.Snapshot.Terms.ID,
	)
	require.NoError(t, err)
	require.Equal(
		t, arkchannel.PhaseCoopCloseSigned, hubSigned.Snapshot.Phase,
	)
	require.Equal(
		t, arkchannel.PhaseCoopCloseSigned, clientSigned.Snapshot.Phase,
	)

	close(publisher.confirm)
	select {
	case err := <-closeResult:
		require.NoError(t, err)

	case <-time.After(10 * time.Second):
		t.Fatal("timeout finalizing cooperative OOR close")
	}
	require.Equal(t, 2, clientDelivery.callCount())
	closed, err := clientClose.RequestCooperativeClose(
		t.Context(), record.Snapshot.Terms.ID,
	)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseClosed, closed.Snapshot.Phase)

	for party, state := range map[arkchannel.Party]struct {
		node  *fundingFlowNode
		store *clientdb.ArkChannelStoreDB
	}{
		arkchannel.PartyHub: {
			node: hub, store: hubStore,
		},
		arkchannel.PartyClient: {
			node: client, store: clientStore,
		},
	} {
		closedRecord, err := state.store.Get(
			t.Context(), record.Snapshot.Terms.ID,
		)
		require.NoError(t, err)
		require.Equal(
			t, arkchannel.PhaseClosed, closedRecord.Snapshot.Phase,
		)
		_, err = state.node.db.ChannelStateDB().FetchChannel(
			channelPoint,
		)
		require.ErrorIs(t, err, channeldb.ErrChannelNotFound)
		closedChannel, err := state.node.db.
			ChannelStateDB().
			FetchClosedChannel(&channelPoint)
		require.NoError(t, err)
		require.Equal(
			t, channeldb.CooperativeClose, closedChannel.CloseType,
		)
		require.Equal(t, settlement.TxID, closedChannel.ClosingTXID)
		expectedBalance := settlement.Proposal.ClientBalance
		if party == arkchannel.PartyHub {
			expectedBalance = settlement.Proposal.HubBalance
		}
		require.Equal(t, expectedBalance, closedChannel.SettledBalance)
	}
	expectedHubReplacement, err := settlement.ReplacementOutPoint(
		record.Snapshot.Terms, *record.Snapshot.Source, request,
		arkchannel.PartyHub,
	)
	require.NoError(t, err)
	_, err = hubService.Apply(
		t.Context(), record.Snapshot.Terms.ID,
		&arkchannel.SourceSpent{
			OutPoint:     record.Snapshot.Source.OutPoint,
			SpendingTxID: chainhash.Hash{9, 9, 9},
		},
	)
	require.NoError(t, err)
	select {
	case outpoint := <-defended:
		require.Equal(t, expectedHubReplacement, outpoint)

	case <-time.After(time.Second):
		t.Fatal("hub replacement was not defended")
	}
	_ = clientService
}

// TestNativeFundingFlowPromotesClientVTXO proves the same prepared-OOR flow
// opens a client-funded channel with all initial liquidity on the client side.
func TestNativeFundingFlowPromotesClientVTXO(t *testing.T) {
	t.Parallel()
	testNativeFundingFlowPromotesClientVTXO(t)
}

// testNativeFundingFlowPromotesClientVTXO exercises ordinary wallet-VTXO
// promotion independently from the hub-funded receive-intent flow.
func testNativeFundingFlowPromotesClientVTXO(t *testing.T) {
	t.Helper()

	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)
	require.NoError(t, hub.runtime.Start())
	require.NoError(t, client.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, hub.runtime.Stop())
		require.NoError(t, client.runtime.Stop())
	})

	pendingID := lndfunding.PendingChanID{7, 2, 0, 4}
	record := fundingIntentRecord(t, hub, client, pendingID)
	record.Snapshot.Terms.Kind = arkchannel.KindPromotion
	record.Snapshot.Terms.Funder = arkchannel.PartyClient
	record.Snapshot.Terms.Capacity = testFundingCapacity
	record.Snapshot.Terms.PaymentHash = [32]byte{}
	record.Snapshot.Source = testIntentBinding(
		t, record.Snapshot.Terms, testFundingCapacity+1_000, 1,
	)
	hub.intents.record = record
	client.intents.record = record

	hubEndpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyHub, hub.runtime.Funding(),
		input.NewMockSigner(
			[]*btcec.PrivateKey{hub.key}, nil,
		),
		keychain.KeyDescriptor{
			PubKey: hub.key.PubKey(),
		},
	)
	require.NoError(t, err)
	clientEndpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyClient, client.runtime.Funding(),
		input.NewMockSigner(
			[]*btcec.PrivateKey{client.key}, nil,
		),
		keychain.KeyDescriptor{
			PubKey: client.key.PubKey(),
		},
	)
	require.NoError(t, err)
	hubSink := &fundingNegotiationSink{
		node: hub, party: arkchannel.PartyHub, record: record,
	}
	clientSink := &fundingNegotiationSink{
		node: client, party: arkchannel.PartyClient, record: record,
	}
	require.NoError(t, hubEndpoint.BindChannelEventSink(hubSink))
	require.NoError(t, clientEndpoint.BindChannelEventSink(clientSink))
	negotiator, err := NewChannelNegotiator(
		clientEndpoint, hubEndpoint, client.peer,
		&noOpChannelRecoveryManager{},
	)
	require.NoError(t, err)
	require.NoError(
		t,
		negotiator.NegotiateChannel(
			t.Context(), record.Snapshot.Terms.ID,
			record.Snapshot.Terms, *record.Snapshot.Source,
		),
	)

	hubChannel := awaitLink(t, hub)
	clientChannel := awaitLink(t, client)
	require.Equal(
		t, clientChannel.FundingOutpoint, hubChannel.FundingOutpoint,
	)
	require.Positive(t, clientChannel.LocalCommitment.LocalBalance)
	require.Zero(t, hubChannel.LocalCommitment.LocalBalance)
}

// activateFundingFlowChannel negotiates and activates one native lnd channel
// against the exact prepared OOR channel-policy output in record.
func activateFundingFlowChannel(t *testing.T, hub, client *fundingFlowNode,
	record arkchannel.Record) activeFundingFlow {

	t.Helper()

	hub.intents.record = record
	client.intents.record = record
	hubEndpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyHub, hub.runtime.Funding(),
		input.NewMockSigner(
			[]*btcec.PrivateKey{hub.key}, nil,
		),
		keychain.KeyDescriptor{
			PubKey: hub.key.PubKey(),
		},
	)
	require.NoError(t, err)
	clientEndpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyClient, client.runtime.Funding(),
		input.NewMockSigner(
			[]*btcec.PrivateKey{client.key}, nil,
		),
		keychain.KeyDescriptor{
			PubKey: client.key.PubKey(),
		},
	)
	require.NoError(t, err)
	hubSink := &fundingNegotiationSink{
		node: hub, party: arkchannel.PartyHub, record: record,
	}
	clientSink := &fundingNegotiationSink{
		node: client, party: arkchannel.PartyClient, record: record,
	}
	require.NoError(t, hubEndpoint.BindChannelEventSink(hubSink))
	require.NoError(t, clientEndpoint.BindChannelEventSink(clientSink))
	initiator := hubEndpoint
	responder := clientEndpoint
	initiatorPeer := hub.peer
	if record.Snapshot.Terms.FundingInitiator() ==
		arkchannel.PartyClient {

		initiator = clientEndpoint
		responder = hubEndpoint
		initiatorPeer = client.peer
	}
	negotiator, err := NewChannelNegotiator(
		initiator, responder, initiatorPeer,
		&noOpChannelRecoveryManager{},
	)
	require.NoError(t, err)
	require.NoError(
		t,
		negotiator.NegotiateChannel(
			t.Context(), record.Snapshot.Terms.ID,
			record.Snapshot.Terms, *record.Snapshot.Source,
		),
	)
	require.Equal(t, arkchannel.PhaseActive, hubSink.record.Snapshot.Phase)
	require.Equal(
		t, arkchannel.PhaseActive, clientSink.record.Snapshot.Phase,
	)
	hubChannel := awaitLink(t, hub)
	clientChannel := awaitLink(t, client)
	require.Equal(
		t, hubChannel.FundingOutpoint, clientChannel.FundingOutpoint,
	)
	require.False(t, hubChannel.IsPending)
	require.False(t, clientChannel.IsPending)

	return activeFundingFlow{
		hubChannel:    hubChannel,
		clientChannel: clientChannel,
		hubSink:       hubSink,
		clientSink:    clientSink,
	}
}

// cooperativelyCloseFundingFlowChannel drives lnd's native cooperative-close
// FSM at both endpoints over the same logical peer boundary used for funding.
func cooperativelyCloseFundingFlowChannel(t *testing.T, alice,
	bob *fundingFlowNode, channelPoint wire.OutPoint) *wire.MsgTx {

	t.Helper()

	aliceBroadcast := make(chan *wire.MsgTx, 1)
	bobBroadcast := make(chan *wire.MsgTx, 1)
	feeRate := chainfee.SatPerKWeight(1_000)
	aliceCloser, err := alice.runtime.NewCooperativeClose(
		CooperativeCloseRequest{
			ChannelPoint:    channelPoint,
			DeliveryAddress: testCloseDeliveryAddress(alice.key),
			IdealFeeRate:    feeRate,
			Closer:          lntypes.Local,
			BroadcastTx: func(tx *wire.MsgTx, _ string) error {
				aliceBroadcast <- tx.Copy()

				return nil
			},
		},
	)
	require.NoError(t, err)
	bobCloser, err := bob.runtime.NewCooperativeClose(
		CooperativeCloseRequest{
			ChannelPoint:    channelPoint,
			DeliveryAddress: testCloseDeliveryAddress(bob.key),
			IdealFeeRate:    feeRate,
			Closer:          lntypes.Remote,
			BroadcastTx: func(tx *wire.MsgTx, _ string) error {
				bobBroadcast <- tx.Copy()

				return nil
			},
		},
	)
	require.NoError(t, err)

	shutdown, err := aliceCloser.ShutdownChan()
	require.NoError(t, err)
	bobShutdown, err := bobCloser.ReceiveShutdown(*shutdown)
	require.NoError(t, err)
	bobOffer, err := bobCloser.BeginNegotiation()
	require.NoError(t, err)
	require.True(t, bobOffer.IsNone())

	_, err = aliceCloser.ReceiveShutdown(bobShutdown.UnwrapOrFail(t))
	require.NoError(t, err)
	aliceOffer, err := aliceCloser.BeginNegotiation()
	require.NoError(t, err)
	require.True(t, aliceOffer.IsSome())

	message := aliceOffer.UnwrapOrFail(t)
	fromAlice := true
	for i := 0; i < 10; i++ {
		if fromAlice {
			next, err := bobCloser.ReceiveClosingSigned(message)
			require.NoError(t, err)
			if next.IsNone() {
				break
			}
			message = next.UnwrapOrFail(t)
		} else {
			next, err := aliceCloser.ReceiveClosingSigned(message)
			require.NoError(t, err)
			if next.IsNone() {
				break
			}
			message = next.UnwrapOrFail(t)
		}

		fromAlice = !fromAlice
	}

	aliceTx, err := aliceCloser.ClosingTx()
	require.NoError(t, err)
	bobTx, err := bobCloser.ClosingTx()
	require.NoError(t, err)
	require.Equal(t, aliceTx.TxHash(), bobTx.TxHash())
	require.Equal(t, aliceTx.TxHash(), (<-aliceBroadcast).TxHash())
	require.Equal(t, bobTx.TxHash(), (<-bobBroadcast).TxHash())

	return aliceTx
}

// testCloseDeliveryAddress returns a valid P2WPKH script for co-op close.
func testCloseDeliveryAddress(
	key *btcec.PrivateKey) chancloser.DeliveryAddrWithKey {

	keyHash := address.Hash160(key.PubKey().SerializeCompressed())
	pkScript := append([]byte{0x00, 0x14}, keyHash...)

	return chancloser.DeliveryAddrWithKey{
		DeliveryAddress: lnwire.DeliveryAddress(pkScript),
	}
}

// TestNativeFundingCancellationAfterFinalization proves a prepared OOR
// transfer can abort after both lnd databases persist the channel but before
// Ark commits it.
func TestNativeFundingCancellationAfterFinalization(t *testing.T) {
	t.Parallel()

	alice := newFundingFlowNode(t, arkchannel.PartyHub)
	bob := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, alice, bob)
	require.NoError(t, alice.runtime.Start())
	require.NoError(t, bob.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, alice.runtime.Stop())
		require.NoError(t, bob.runtime.Stop())
	})

	pendingID := lndfunding.PendingChanID{8, 6, 7, 5}
	record := fundingIntentRecord(t, alice, bob, pendingID)
	alice.intents.record = record
	bob.intents.record = record

	flow, err := alice.runtime.Funding().OpenChannel(FundingOpenRequest{
		Peer:             alice.peer,
		PendingChannelID: pendingID,
		Capacity:         testFundingCapacity,
		PushAmount:       record.Snapshot.Terms.InitialPushAmount(),
	})
	require.NoError(t, err)
	packet := awaitFundingPSBT(t, flow)
	funding, _ := completeChannelBacking(
		t, packet, record, alice, bob,
	)
	require.NoError(t, bob.runtime.Funding().RegisterBacking(funding))
	require.NoError(
		t, alice.runtime.Funding().FinalizeBacking(
			pendingID, packet, funding,
		),
	)
	_ = awaitFinalized(t, alice, flow.Errors)
	_ = awaitFinalized(t, bob, nil)

	channelPoint := wire.OutPoint{
		Hash:  funding.Transaction.TxHash(),
		Index: funding.OutputIndex,
	}
	for _, node := range []*fundingFlowNode{alice, bob} {
		require.NoError(
			t, node.runtime.Funding().CancelBacking(
				pendingID, &channelPoint,
			),
		)
		_, err := node.db.ChannelStateDB().FetchChannel(channelPoint)
		require.ErrorIs(t, err, channeldb.ErrChannelNotFound)
		_, err = node.db.ChannelStateDB().FetchClosedChannel(
			&channelPoint,
		)
		require.NoError(t, err)
		require.NoError(
			t, node.runtime.Funding().CancelBacking(
				pendingID, &channelPoint,
			),
		)
		require.Error(
			t, node.runtime.Funding().ConfirmBacking(
				channelPoint.Hash,
			),
		)
	}
}

// newFundingFlowNode constructs one runtime before its transport-backed peer
// is connected.
func newFundingFlowNode(t *testing.T,
	localParty arkchannel.Party) *fundingFlowNode {

	t.Helper()

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	db := channeldb.OpenForTesting(t, t.TempDir())
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	node := &fundingFlowNode{
		key:       nodeKey,
		db:        db,
		finalized: make(chan fundingFinalization, 1),
		links:     make(chan *chanstate.OpenChannel, 1),
		failures:  make(chan error, 2),
		intents:   &staticIntentSource{},
	}
	baseNotifier := newRuntimeNotifier(800_000)
	node.notifier, err = NewVirtualFundingNotifier(baseNotifier)
	require.NoError(t, err)
	keyRing := &mock.SecretKeyRing{RootKey: nodeKey}
	walletController := &mock.WalletController{RootKey: nodeKey}
	signer := mock.NewSingleSigner(nodeKey)
	intentAcceptor, err := NewIntentAcceptor(localParty, node.intents)
	require.NoError(t, err)
	node.runtime, err = NewRuntime(RuntimeConfig{
		DB:           db,
		Chain:        fixedHeightChain{height: 800_000},
		Notifier:     node.notifier,
		OnionKey:     &keychain.PrivKeyECDH{PrivKey: nodeKey},
		Signer:       signer,
		FeeEstimator: chainfee.NewStaticEstimator(1_250, 253),
		WitnessBeacon: &runtimeWitnessBeacon{
			cache: db.NewWitnessCache(),
		},
		SelfNode: route.NewVertex(nodeKey.PubKey()),
		Funding: &FundingConfig{
			WalletController: walletController,
			KeyRing:          keyRing,
			NetParams:        &chaincfg.RegressionNetParams,
			IdentityKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamilyNodeKey,
				},
				PubKey: nodeKey.PubKey(),
			},
			ChannelAcceptor: intentAcceptor,
			RoutingPolicy: models.ForwardingPolicy{
				MinHTLCOut:    1,
				TimeLockDelta: 18,
			},
			NotifyWhenOnline: func(_ [33]byte,
				peerChan chan<- lnpeer.Peer) {

				peerChan <- node.peer
			},
			WatchNewChannel: func(*chanstate.OpenChannel,
				*btcec.PublicKey) error {

				return nil
			},
			NotifyPendingOpen: func(channelPoint wire.OutPoint,
				_ *chanstate.OpenChannel, _ *btcec.PublicKey) {

				node.finalized <- fundingFinalization{
					channelPoint: channelPoint,
				}
			},
		},
	})
	require.NoError(t, err)

	return node
}

// connectFundingFlowNodes creates each endpoint's view of the single logical
// peer and installs newly opened channels into its native switch.
func connectFundingFlowNodes(t *testing.T, alice, bob *fundingFlowNode) {
	t.Helper()

	alice.peer = newFundingFlowPeer(t, alice, bob)
	bob.peer = newFundingFlowPeer(t, bob, alice)
}

// newFundingFlowPeer adapts one side of the direct application transport.
func newFundingFlowPeer(t *testing.T, local, remote *fundingFlowNode) *Peer {
	t.Helper()

	transport := newFundingFlowTransport(remote)
	t.Cleanup(transport.Stop)

	var peer *Peer
	peer, err := NewPeer(PeerConfig{
		RemoteKey: remote.key.PubKey(),
		Transport: transport,
		AddChannel: func(channel *lnpeer.NewChannel,
			_ <-chan struct{}) error {

			_, err := local.runtime.AddLink(
				channel.OpenChannel,
				testLinkConfig(peer, local.failures),
			)
			if err != nil {
				return err
			}
			local.links <- channel.OpenChannel

			return nil
		},
	})
	require.NoError(t, err)

	return peer
}

// awaitFundingPSBT waits until lnd has negotiated both multisig keys and asks
// Ark to construct the external backing transaction.
func awaitFundingPSBT(t *testing.T, flow *FundingFlow) *psbt.Packet {
	t.Helper()

	for {
		select {
		case update := <-flow.Updates:
			psbtUpdate := update.GetPsbtFund()
			if psbtUpdate == nil {
				continue
			}
			packet, err := psbt.NewFromRawBytes(
				bytes.NewReader(psbtUpdate.Psbt), false,
			)
			require.NoError(t, err)

			return packet

		case err := <-flow.Errors:
			require.NoError(t, err)

		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for lnd funding PSBT")
		}
	}
}

// fundingIntentRecord binds the lnd node and materialization keys to one exact
// prepared OOR output.
func fundingIntentRecord(t *testing.T, hub, client *fundingFlowNode,
	pendingID lndfunding.PendingChanID) arkchannel.Record {

	t.Helper()

	record, _ := testReceiveIntentRecord(t)
	record.Snapshot.Terms.PendingChannelID = pendingID
	record.Snapshot.Terms.Capacity = testFundingCapacity
	record.Snapshot.Terms.HubNodeKey = compressedIntentKey(hub.key)
	record.Snapshot.Terms.ClientNodeKey = compressedIntentKey(client.key)
	record.Snapshot.Terms.VTXO.HubChannelKey = compressedIntentKey(hub.key)
	record.Snapshot.Terms.VTXO.ClientChannelKey = compressedIntentKey(
		client.key,
	)
	record.Snapshot.Source = testIntentBinding(
		t, record.Snapshot.Terms, testFundingCapacity+1_000, 1,
	)

	return record
}

// completeChannelBacking has both parties validate their local lnd funding
// reservation, then signs the real channel-policy VTXO spend.
func completeChannelBacking(t *testing.T, packet *psbt.Packet,
	record arkchannel.Record, hub,
	client *fundingFlowNode) (VirtualFunding, arkchannel.Backing) {

	t.Helper()
	terms := record.Snapshot.Terms
	template, err := arkchannel.NewBackingTemplate(
		packet, terms, *record.Snapshot.Source,
	)
	require.NoError(t, err)

	for _, node := range []*fundingFlowNode{hub, client} {
		expected, err := node.runtime.Funding().ExpectedFundingOutput(
			terms.PendingChannelID,
		)
		require.NoError(t, err)
		require.NoError(t, template.ValidateFundingOutput(expected))
	}

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
		terms, *record.Snapshot.Source,
		sign(arkchannel.PartyClient, client.key),
		sign(arkchannel.PartyHub, hub.key),
	)
	require.NoError(t, err)
	funding, err := virtualFundingFromBacking(terms, backing)
	require.NoError(t, err)

	return funding, backing
}

// awaitFinalized waits for lnd's commitment-signature safety barrier.
func awaitFinalized(t *testing.T, node *fundingFlowNode,
	fundingErrors <-chan error) fundingFinalization {

	t.Helper()

	select {
	case pendingID := <-node.finalized:
		return pendingID

	case err := <-node.failures:
		require.NoError(t, err)

	case err := <-fundingErrors:
		require.NoError(t, err)

	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for lnd funding finalization")
	}

	return fundingFinalization{}
}

// awaitLink waits until funding.Manager hands the open channel to the peer.
func awaitLink(t *testing.T, node *fundingFlowNode) *chanstate.OpenChannel {
	t.Helper()

	select {
	case channel := <-node.links:
		return channel

	case err := <-node.failures:
		require.NoError(t, err)

	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for lnd channel link")
	}

	return nil
}

// payRuntimeInvoice sends a fixed one-hop payment through lnd's native
// control tower, switch, channel links, and invoice registry.
func payRuntimeInvoice(t *testing.T, payer, payee *fundingFlowNode,
	amount lnwire.MilliSatoshi, preimage lntypes.Preimage) {

	t.Helper()

	_, err := payee.runtime.Invoices().AddInvoice(
		t.Context(), &invoices.Invoice{
			CreationDate: time.Now(),
			Terms: invoices.ContractTerm{
				FinalCltvDelta:  18,
				Expiry:          time.Hour,
				PaymentPreimage: &preimage,
				Value:           amount,
				Features:        emptyFeatureVector(),
			},
		}, preimage.Hash(),
	)
	require.NoError(t, err)

	channels, err := payer.db.ChannelStateDB().FetchAllOpenChannels()
	require.NoError(t, err)
	require.Len(t, channels, 1)
	scid := channels[0].ShortChanID().ToUint64()
	paymentRoute := &route.Route{
		TotalTimeLock: 800_040,
		TotalAmount:   amount,
		SourcePubKey:  route.NewVertex(payer.key.PubKey()),
		Hops: []*route.Hop{
			{
				PubKeyBytes: route.NewVertex(
					payee.key.PubKey(),
				),
				ChannelID:        scid,
				OutgoingTimeLock: 800_040,
				AmtToForward:     amount,
				LegacyPayload:    true,
			},
		},
	}

	result := make(chan error, 1)
	go func() {
		attempt, sendErr := payer.runtime.Payments().SendToOperator(
			t.Context(), preimage.Hash(), paymentRoute, nil,
		)
		if sendErr == nil && attempt.Settle == nil {
			sendErr = fmt.Errorf("payment attempt did not settle")
		}
		result <- sendErr
	}()

	select {
	case err := <-result:
		require.NoError(t, err)

	case err := <-payer.failures:
		require.NoError(t, err)

	case err := <-payee.failures:
		require.NoError(t, err)

	case <-time.After(10 * time.Second):
		t.Fatal("native lnd channel payment did not complete")
	}

	invoice, err := payee.runtime.Invoices().LookupInvoice(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, invoices.ContractSettled, invoice.State)
}
