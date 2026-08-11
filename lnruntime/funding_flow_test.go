package lnruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
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
	key       *btcec.PrivateKey
	db        *channeldb.DB
	runtime   *Runtime
	notifier  *VirtualFundingNotifier
	peer      *Peer
	finalized chan fundingFinalization
	links     chan *chanstate.OpenChannel
	failures  chan error
	intents   *staticIntentSource
}

// fundingNegotiationSink models the already-tested durable channel FSM while
// this component test exercises the production native funding coordinator.
type fundingNegotiationSink struct {
	mu     sync.Mutex
	node   *fundingFlowNode
	party  arkchannel.Party
	record arkchannel.Record
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

// blockingCooperativeClosePublisher models the unroller's confirmation
// barrier so the test can prove lnd remains open until settlement confirms.
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
		script []byte) error {

		if !bytes.Equal(script, expected) {
			return fmt.Errorf("payout script is not wallet owned")
		}

		return nil
	})
}

// SettleCooperativeClose records the direct spend and waits for confirmation.
func (p *blockingCooperativeClosePublisher) SettleCooperativeClose(
	ctx context.Context, _ arkchannel.ID, source arkchannel.VTXOBinding,
	settlement arkchannel.CooperativeClose) error {

	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(
		bytes.NewReader(settlement.Transaction),
	); err != nil {
		return err
	}
	if len(tx.TxIn) != 1 || tx.TxIn[0].PreviousOutPoint != source.OutPoint {
		return fmt.Errorf("cooperative close does not spend source " +
			"VTXO")
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
}

// SendMessages dispatches funding messages to funding.Manager and commitment
// updates to the native channel link.
func (t *fundingFlowTransport) SendMessages(_ bool,
	messages ...lnwire.Message) error {

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

		case *lnwire.NodeAnnouncement1, *lnwire.ChannelAnnouncement1,
			*lnwire.ChannelUpdate1:

			// Private runtimes have no graph or gossiper.

		default:
			return fmt.Errorf("unexpected lnd message %T", message)
		}
	}

	return nil
}

// deliverReestablish models durable mailbox retry while the remote runtime is
// still rebuilding its link during a simultaneous restart.
func (t *fundingFlowTransport) deliverReestablish(
	message *lnwire.ChannelReestablish) {

	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		err := t.remote.runtime.HandlePeerMessage(
			context.Background(), message, t.remote.peer,
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

	const firstAmount = lnwire.MilliSatoshi(30_000_000)
	payRuntimeInvoice(t, alice, bob, firstAmount, lntypes.Preimage{1, 2, 3})

	// Rebuild both links from lnd's databases and let the normal
	// channel_reestablish exchange recover the commitment stream.
	alice.runtime.RemoveLink(aliceChannel.FundingOutpoint)
	bob.runtime.RemoveLink(bobChannel.FundingOutpoint)
	restored, err := alice.runtime.RestorePeerLinks(
		alice.peer, func(*chanstate.OpenChannel) (LinkConfig, error) {
			return testLinkConfig(alice.peer, alice.failures), nil
		},
	)
	require.NoError(t, err)
	require.Len(t, restored, 1)
	restored, err = bob.runtime.RestorePeerLinks(
		bob.peer, func(*chanstate.OpenChannel) (LinkConfig, error) {
			return testLinkConfig(bob.peer, bob.failures), nil
		},
	)
	require.NoError(t, err)
	require.Len(t, restored, 1)
	restored, err = alice.runtime.RestorePeerLinks(
		alice.peer, func(*chanstate.OpenChannel) (LinkConfig, error) {
			return testLinkConfig(alice.peer, alice.failures), nil
		},
	)
	require.NoError(t, err)
	require.Empty(t, restored)

	const returnAmount = lnwire.MilliSatoshi(10_000_000)
	payRuntimeInvoice(
		t, bob, alice, returnAmount, lntypes.Preimage{7, 8, 9},
	)

	forceClose, err := alice.runtime.PrepareForceClose(
		aliceChannel.FundingOutpoint,
	)
	require.NoError(t, err)
	require.Equal(
		t, aliceChannel.FundingOutpoint,
		forceClose.CloseTx.TxIn[0].PreviousOutPoint,
	)

	closeTx := cooperativelyCloseFundingFlowChannel(
		t, alice, bob, aliceChannel.FundingOutpoint,
	)
	require.Equal(
		t, aliceChannel.FundingOutpoint,
		closeTx.TxIn[0].PreviousOutPoint,
	)
}

// TestNativeFundingFlowDirectCooperativeClose proves an active unpublished
// channel can carry payments in both directions and then settle its clean lnd
// balances by spending the channel-policy VTXO directly. The backing channel
// point never becomes an input to the closing transaction.
func TestNativeFundingFlowDirectCooperativeClose(t *testing.T) {
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
		Initiator: arkchannel.PartyClient,
		ClientDeliveryScript: testCloseDeliveryAddress(
			client.key,
		).DeliveryAddress,
		HubDeliveryScript: testCloseDeliveryAddress(
			hub.key,
		).DeliveryAddress,
		FeeRate: chainfee.SatPerKWeight(1_000),
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
		arkchannel.PartyClient, client.runtime,
		&recordingCooperativeCloseSigner{
			Signer: input.NewMockSigner(
				[]*btcec.PrivateKey{clientArkKey}, nil,
			),
			label: "client",
			order: signingOrder,
		},
		keychain.KeyDescriptor{PubKey: clientArkKey.PubKey()},
		exactCooperativeCloseDeliveryValidator(
			request.ClientDeliveryScript,
		),
	)
	require.NoError(t, err)
	operatorSigner, err := NewNativeCooperativeCloseOperatorSigner(
		&recordingCooperativeCloseSigner{
			Signer: input.NewMockSigner(
				[]*btcec.PrivateKey{operatorArkKey}, nil,
			),
			label: "operator",
			order: signingOrder,
		},
		keychain.KeyDescriptor{PubKey: operatorArkKey.PubKey()},
	)
	require.NoError(t, err)
	publisher := &blockingCooperativeClosePublisher{
		published: make(chan arkchannel.CooperativeClose, 1),
		confirm:   make(chan struct{}),
	}
	hubClose, err := NewHubCooperativeCloseProcess(
		hubEndpoint, operatorSigner, publisher,
		&stableCooperativeCloseDelivery{
			script: request.HubDeliveryScript,
		},
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
		clientEndpoint, lossyPeer, clientDelivery,
	)
	require.NoError(t, err)
	clientService, err := arkchannel.NewService(
		clientFSM, &cooperativeCloseActionExecutor{
			closer: clientClose,
		},
	)
	require.NoError(t, err)

	_, err = clientClose.RequestCooperativeClose(
		t.Context(), record.Snapshot.Terms.ID, request.FeeRate,
	)
	require.ErrorContains(t, err, "lost begin response")
	require.Equal(t, 1, clientDelivery.callCount())

	_, err = clientClose.RequestCooperativeClose(
		t.Context(), record.Snapshot.Terms.ID, request.FeeRate,
	)
	require.ErrorContains(t, err, "lost complete response")
	require.Equal(t, 2, clientDelivery.callCount())

	// The client received nothing, but the hub must already hold the
	// complete transaction. Neither side may publish until the client
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
		t, []string{"client", "hub", "operator"},
		signingOrder.snapshot(),
	)
	select {
	case <-publisher.published:
		t.Fatal("cooperative close published before client recovery")

	default:
	}

	closeResult := make(chan error, 1)
	go func() {
		_, closeErr := clientClose.RequestCooperativeClose(
			t.Context(), record.Snapshot.Terms.ID, request.FeeRate,
		)
		closeResult <- closeErr
	}()

	var settlement arkchannel.CooperativeClose
	select {
	case settlement = <-publisher.published:
	case err := <-closeResult:
		require.NoError(t, err)
		t.Fatal("cooperative close completed without hub publication")

	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for direct cooperative settlement")
	}
	require.Equal(
		t, []string{"client", "hub", "operator"},
		signingOrder.snapshot(),
	)
	require.NoError(
		t, settlement.Validate(
			record.Snapshot.Terms, *record.Snapshot.Source, request,
		),
	)
	settlementTx := wire.NewMsgTx(2)
	require.NoError(
		t,
		settlementTx.Deserialize(
			bytes.NewReader(settlement.Transaction),
		),
	)
	require.Equal(
		t, record.Snapshot.Source.OutPoint,
		settlementTx.TxIn[0].PreviousOutPoint,
	)
	require.NotEqual(
		t, channelPoint, settlementTx.TxIn[0].PreviousOutPoint,
	)

	// Publication has started, but lnd must retain both open-channel
	// records until the unroller reports confirmation.
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
		t.Fatal("timeout finalizing direct cooperative close")
	}
	require.Equal(t, 2, clientDelivery.callCount())
	closed, err := clientClose.RequestCooperativeClose(
		t.Context(), record.Snapshot.Terms.ID, request.FeeRate,
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
	_ = hubService
	_ = clientService
}

// TestNativeFundingFlowPromotesClientVTXO proves the same prepared-OOR flow
// opens a client-funded channel with all initial liquidity on the client side.
func TestNativeFundingFlowPromotesClientVTXO(t *testing.T) {
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

	pendingID := lndfunding.PendingChanID{7, 2, 0, 4}
	record := fundingIntentRecord(t, hub, client, pendingID)
	record.Snapshot.Terms.Kind = arkchannel.KindPromotion
	record.Snapshot.Terms.Funder = arkchannel.PartyClient
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
	if record.Snapshot.Terms.Funder == arkchannel.PartyClient {
		initiator = clientEndpoint
		responder = hubEndpoint
		initiatorPeer = client.peer
	}
	negotiator, err := NewChannelNegotiator(
		initiator, responder, initiatorPeer,
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

	var peer *Peer
	peer, err := NewPeer(PeerConfig{
		RemoteKey: remote.key.PubKey(),
		Transport: &fundingFlowTransport{remote: remote},
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
