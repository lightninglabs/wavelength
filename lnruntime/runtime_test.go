package lnruntime

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestRuntimeStartsNativeSubsystems verifies the composed lifecycle starts and
// stops without constructing an lnd daemon or graph.
func TestRuntimeStartsNativeSubsystems(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	notifier := newRuntimeNotifier(800_000)
	runtime, err := NewRuntime(RuntimeConfig{
		DB:       db,
		Chain:    fixedHeightChain{height: 800_000},
		Notifier: notifier,
		OnionKey: &keychain.PrivKeyECDH{PrivKey: nodeKey},
		Signer: input.NewMockSigner(
			[]*btcec.PrivateKey{nodeKey},
			&chaincfg.RegressionNetParams,
		),
		FeeEstimator: chainfee.NewStaticEstimator(1_250, 253),
		WitnessBeacon: &runtimeWitnessBeacon{
			cache: db.NewWitnessCache(),
		},
		SelfNode: route.NewVertex(nodeKey.PubKey()),
	})
	require.NoError(t, err)
	require.NoError(t, runtime.Start())
	require.NoError(t, runtime.Start())
	require.NoError(t, runtime.Stop())
	require.NoError(t, runtime.Stop())
	require.GreaterOrEqual(t, notifier.epochRegistrations.Load(), int32(2))
}

// TestForceCloseSingleFlight verifies duplicate RPC delivery joins the request
// already blocked in Ark materialization instead of entering lnd twice.
func TestForceCloseSingleFlight(t *testing.T) {
	t.Parallel()

	channelPoint := wire.OutPoint{Index: 7}
	runtime := &Runtime{}
	started := make(chan struct{})
	release := make(chan struct{})
	closeTx := wire.NewMsgTx(2)
	var calls atomic.Int32
	forceClose := func() (*wire.MsgTx, error) {
		calls.Add(1)
		close(started)
		<-release

		return closeTx, nil
	}
	type result struct {
		tx  *wire.MsgTx
		err error
	}
	results := make(chan result, 2)
	request := func() {
		tx, err := runtime.runForceClose(channelPoint, forceClose)
		results <- result{tx: tx, err: err}
	}

	go request()
	<-started
	go request()

	require.NoError(t, runtime.ResumeForceCloseChannel(channelPoint))
	require.True(t, runtime.forceCloseIsActive(channelPoint))
	close(release)
	for range 2 {
		outcome := <-results
		require.NoError(t, outcome.err)
		require.Same(t, closeTx, outcome.tx)
	}
	require.Equal(t, int32(1), calls.Load())
	require.False(t, runtime.forceCloseIsActive(channelPoint))
}

// TestForceCloseSummaryTxID accepts only an exact local or remote force-close
// record as proof that a competing endpoint completed the close.
func TestForceCloseSummaryTxID(t *testing.T) {
	t.Parallel()

	channelPoint := wire.OutPoint{Index: 7}
	closingTxID := chainhash.Hash{1}
	for _, closeType := range []channeldb.ClosureType{
		channeldb.LocalForceClose, channeldb.RemoteForceClose,
	} {
		summary := &channeldb.ChannelCloseSummary{
			ChanPoint:   channelPoint,
			ClosingTXID: closingTxID,
			CloseType:   closeType,
		}
		result, err := forceCloseSummaryTxID(summary, channelPoint)
		require.NoError(t, err)
		require.Equal(t, closingTxID, result)
	}

	_, err := forceCloseSummaryTxID(&channeldb.ChannelCloseSummary{
		ChanPoint:   channelPoint,
		ClosingTXID: closingTxID,
		CloseType:   channeldb.CooperativeClose,
	}, channelPoint)
	require.ErrorContains(t, err, "not a force close")

	_, err = forceCloseSummaryTxID(&channeldb.ChannelCloseSummary{
		ChanPoint:   wire.OutPoint{Index: 8},
		ClosingTXID: closingTxID,
		CloseType:   channeldb.RemoteForceClose,
	}, channelPoint)
	require.ErrorContains(t, err, "summary is for")

	_, err = forceCloseSummaryTxID(&channeldb.ChannelCloseSummary{
		ChanPoint: channelPoint,
		CloseType: channeldb.LocalForceClose,
	}, channelPoint)
	require.ErrorContains(t, err, "transaction ID is missing")
}

// TestRuntimePaysOverNativeChannelLinks proves Wavelength can run lnd's
// channel and payment state machines without the lnd daemon or peer manager.
func TestRuntimePaysOverNativeChannelLinks(t *testing.T) {
	t.Parallel()

	aliceState, bobState, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)

	aliceNode, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bobNode, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	alice := newTestRuntime(
		t, aliceNode, aliceState.Signer,
	)
	bob := newTestRuntime(t, bobNode, bobState.Signer)
	require.NoError(t, alice.runtime.Start())
	require.NoError(t, bob.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, alice.runtime.Stop())
		require.NoError(t, bob.runtime.Stop())
	})

	aliceTransport := &runtimeMessageTransport{remote: bob.runtime}
	bobTransport := &runtimeMessageTransport{remote: alice.runtime}
	alicePeer := newRuntimePeer(t, bobNode.PubKey(), aliceTransport)
	bobPeer := newRuntimePeer(t, aliceNode.PubKey(), bobTransport)

	failures := make(chan error, 2)
	aliceLinkCfg := testLinkConfig(alicePeer, failures)
	bobLinkCfg := testLinkConfig(bobPeer, failures)
	_, err = alice.runtime.AddLink(aliceState.State(), aliceLinkCfg)
	require.NoError(t, err)
	_, err = bob.runtime.AddLink(bobState.State(), bobLinkCfg)
	require.NoError(t, err)

	preimage := lntypes.Preimage{9, 8, 7, 6}
	const amount = lnwire.MilliSatoshi(25_000)
	_, err = bob.runtime.Invoices().AddInvoice(
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

	scid := aliceState.ShortChanID().ToUint64()
	paymentRoute := &route.Route{
		TotalTimeLock: 800_040,
		TotalAmount:   amount,
		SourcePubKey:  route.NewVertex(aliceNode.PubKey()),
		Hops: []*route.Hop{
			{
				PubKeyBytes: route.NewVertex(
					bobNode.PubKey(),
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
		attempt, sendErr := alice.runtime.Payments().SendToOperator(
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

	case err := <-failures:
		require.NoError(t, err)

	case <-time.After(10 * time.Second):
		t.Fatal("native lnd channel payment did not complete")
	}

	invoice, err := bob.runtime.Invoices().LookupInvoice(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, invoices.ContractSettled, invoice.State)
}

// testRuntime owns one runtime and its independent lnd database.
type testRuntime struct {
	runtime *Runtime
}

// newTestRuntime creates a native component runtime around one channel signer.
func newTestRuntime(t *testing.T, nodeKey *btcec.PrivateKey,
	signer input.Signer) *testRuntime {

	t.Helper()

	db := channeldb.OpenForTesting(t, t.TempDir())
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	notifier := newRuntimeNotifier(800_000)
	runtime, err := NewRuntime(RuntimeConfig{
		DB:           db,
		Chain:        fixedHeightChain{height: 800_000},
		Notifier:     notifier,
		OnionKey:     &keychain.PrivKeyECDH{PrivKey: nodeKey},
		Signer:       signer,
		FeeEstimator: chainfee.NewStaticEstimator(1_250, 253),
		WitnessBeacon: &runtimeWitnessBeacon{
			cache: db.NewWitnessCache(),
		},
		SelfNode: route.NewVertex(nodeKey.PubKey()),
	})
	require.NoError(t, err)

	return &testRuntime{runtime: runtime}
}

// newRuntimePeer creates the peer adapter used by one test link.
func newRuntimePeer(t *testing.T, remoteKey *btcec.PublicKey,
	transport MessageTransport) *Peer {

	t.Helper()

	peer, err := NewPeer(PeerConfig{
		RemoteKey: remoteKey,
		Transport: transport,
		AddChannel: func(*lnpeer.NewChannel, <-chan struct{}) error {
			return nil
		},
	})
	require.NoError(t, err)

	return peer
}

// testLinkConfig returns callbacks suitable for an unpublished test channel.
func testLinkConfig(peer lnpeer.Peer, failures chan<- error) LinkConfig {
	chainEvents := &contractcourt.ChainEventSubscription{
		RemoteUnilateralClosure: make(
			chan *contractcourt.RemoteUnilateralCloseInfo,
		),
		LocalUnilateralClosure: make(
			chan *contractcourt.LocalUnilateralCloseInfo,
		),
		CooperativeClosure: make(
			chan *contractcourt.CooperativeCloseInfo,
		),
		ContractBreach: make(chan *contractcourt.BreachCloseInfo),
		Cancel:         func() {},
	}
	notifyContractUpdate := func(*contractcourt.ContractUpdate) error {
		return nil
	}

	return LinkConfig{
		Peer: peer,
		Policy: models.ForwardingPolicy{
			MinHTLCOut:    1,
			TimeLockDelta: 18,
		},
		ChainEvents:      chainEvents,
		MaxAnchorFeeRate: 2_500,
		OnChannelFailure: func(_ lnwire.ChannelID,
			_ lnwire.ShortChannelID,
			failure htlcswitch.LinkFailureError) {

			failures <- failure
		},
		UpdateContractSignals: func(
			*contractcourt.ContractSignals) error {

			return nil
		},
		NotifyContractUpdate: notifyContractUpdate,
	}
}

// runtimeMessageTransport directly hands each lnd link update to the remote
// runtime. Production uses the same interface over swapdk transport.
type runtimeMessageTransport struct {
	remote *Runtime
}

// SendMessages preserves message order while dispatching to remote links.
func (t *runtimeMessageTransport) SendMessages(_ bool,
	messages ...lnwire.Message) error {

	for _, message := range messages {
		update, ok := message.(lnwire.LinkUpdater)
		if !ok {
			return fmt.Errorf("unexpected link message %T", message)
		}
		if err := t.remote.HandleChannelMessage(update); err != nil {
			return err
		}
	}

	return nil
}

// runtimeNotifier supplies independent block streams to native lnd
// components.
type runtimeNotifier struct {
	height             int32
	started            atomic.Bool
	epochRegistrations atomic.Int32
}

// newRuntimeNotifier creates a started notifier at a fixed height.
func newRuntimeNotifier(height int32) *runtimeNotifier {
	notifier := &runtimeNotifier{height: height}
	notifier.started.Store(true)

	return notifier
}

// RegisterConfirmationsNtfn returns an idle confirmation subscription.
func (*runtimeNotifier) RegisterConfirmationsNtfn(*chainhash.Hash, []byte,
	uint32, uint32, ...chainntnfs.NotifierOption) (
	*chainntnfs.ConfirmationEvent, error) {

	return chainntnfs.NewConfirmationEvent(1, func() {}), nil
}

// RegisterSpendNtfn returns an idle spend subscription.
func (*runtimeNotifier) RegisterSpendNtfn(*wire.OutPoint, []byte, uint32) (
	*chainntnfs.SpendEvent, error) {

	return chainntnfs.NewSpendEvent(func() {}), nil
}

// RegisterBlockEpochNtfn returns a private stream seeded with the current tip.
func (n *runtimeNotifier) RegisterBlockEpochNtfn(*chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	n.epochRegistrations.Add(1)
	epochs := make(chan *chainntnfs.BlockEpoch, 1)
	epochs <- &chainntnfs.BlockEpoch{
		Height: n.height,
		Hash:   &chainhash.Hash{},
	}

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochs,
		Cancel: func() {},
	}, nil
}

// Start marks the notifier active.
func (n *runtimeNotifier) Start() error {
	n.started.Store(true)

	return nil
}

// Started reports notifier state.
func (n *runtimeNotifier) Started() bool {
	return n.started.Load()
}

// Stop marks the notifier inactive.
func (n *runtimeNotifier) Stop() error {
	n.started.Store(false)

	return nil
}

// runtimeWitnessBeacon persists preimages for native channel links. Tests do
// not exercise on-chain subscriptions.
type runtimeWitnessBeacon struct {
	cache *channeldb.WitnessCache
}

// SubscribeUpdates returns an idle subscription for an on-chain resolver.
func (*runtimeWitnessBeacon) SubscribeUpdates(lnwire.ShortChannelID,
	*chanstate.HTLC, *hop.Payload, []byte) (
	*contractcourt.WitnessSubscription, error) {

	updates := make(chan lntypes.Preimage)

	return &contractcourt.WitnessSubscription{
		WitnessUpdates:     updates,
		CancelSubscription: func() {},
	}, nil
}

// LookupPreimage reads lnd's persistent witness cache.
func (b *runtimeWitnessBeacon) LookupPreimage(hash lntypes.Hash) (
	lntypes.Preimage, bool) {

	preimage, err := b.cache.LookupSha256Witness(hash)

	return preimage, err == nil
}

// AddPreimages writes lnd's persistent witness cache.
func (b *runtimeWitnessBeacon) AddPreimages(
	preimages ...lntypes.Preimage) error {

	return b.cache.AddSha256Witnesses(preimages...)
}

var _ chainntnfs.ChainNotifier = (*runtimeNotifier)(nil)
var _ contractcourt.WitnessBeacon = (*runtimeWitnessBeacon)(nil)
