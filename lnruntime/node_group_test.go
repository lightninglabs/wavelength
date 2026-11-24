package lnruntime

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/stretchr/testify/require"
)

// TestNativeNodeGroupWaitsForReplacement preserves funding recovery requests
// across member disconnection and releases waiting callbacks on group stop.
func TestNativeNodeGroupWaitsForReplacement(t *testing.T) {
	t.Parallel()
	db, err := clientdb.OpenChannelDB(t.TempDir())
	require.NoError(t, err)
	group, err := NewNativeNodeGroup(
		db, (&staticIntentSource{}).FindByPendingChannelID,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, group.Stop()) })
	cfg := groupTestNodeConfig(t, testIntentKey(t))
	first, err := group.NewNode(cfg)
	require.NoError(t, err)
	first.peer.Disconnect(context.Canceled)
	online := make(chan lnpeer.Peer, 1)
	done := make(chan struct{})
	go func() {
		group.notifyWhenOnline(first.peer.PubKey(), online)
		close(done)
	}()
	select {
	case <-online:
		t.Fatal("disconnected session supplied to funding recovery")

	case <-time.After(20 * time.Millisecond):
	}
	replacement, err := group.NewNode(cfg)
	require.NoError(t, err)
	select {
	case peer := <-online:
		require.Same(t, replacement.peer, peer)

	case <-time.After(time.Second):
		t.Fatal("funding recovery did not observe replacement session")
	}
	<-done
	require.NoError(t, first.Retire())
	current, err := group.node(replacement.peer.PubKey())
	require.NoError(t, err)
	require.Same(t, replacement, current)
	waiting := make(chan struct{})
	go func() {
		group.notifyWhenOnline([33]byte{}, make(chan lnpeer.Peer))
		close(waiting)
	}()
	require.NoError(t, group.Stop())
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("group shutdown stranded an online notification")
	}
}

// groupTestLifecycle admits native ownership without any persisted channels.
type groupTestLifecycle struct {
	contractcourt.AuxChannelLifecycle
}

// ChainWatchOwner leaves ordinary lnd restoration enabled in the empty DB.
func (*groupTestLifecycle) ChainWatchOwner(context.Context,
	*chanstate.OpenChannel) (contractcourt.ChainWatchOwner, error) {

	return contractcourt.ChainWatchOwnerLnd, nil
}

// TestNativeNodeGroupOwnsSharedEngine proves one peer cannot stop another's
// invoice, funding, or channel database lifetime.
func TestNativeNodeGroupOwnsSharedEngine(t *testing.T) {
	t.Parallel()

	db, err := clientdb.OpenChannelDB(t.TempDir())
	require.NoError(t, err)
	group, err := NewNativeNodeGroup(
		db, (&staticIntentSource{}).FindByPendingChannelID,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, group.Stop()) })
	key := testIntentKey(t)
	first, err := group.NewNode(groupTestNodeConfig(t, key))
	require.NoError(t, err)
	second, err := group.NewNode(groupTestNodeConfig(t, key))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Stop()) })
	require.Same(t, first.Runtime(), second.Runtime())
	require.Same(t, first.db, second.db)
	require.Same(t, first.notifier, second.notifier)
	require.NotSame(t, first.Peer(), second.Peer())
	require.NotSame(t, first.FundingEndpoint(), second.FundingEndpoint())
	require.NoError(t, first.Start())
	require.NoError(t, second.Start())
	require.NoError(t, first.Stop())
	require.False(t, group.stopped)
	_, hash, err := second.AddInvoice(t.Context(), 1_000)
	require.NoError(t, err)
	_, err = second.Runtime().Invoices().LookupInvoice(t.Context(), hash)
	require.NoError(t, err)
	_, err = db.ChannelStateDB().FetchAllOpenChannels()
	require.NoError(t, err)
	require.NoError(t, group.Stop())
	require.NoError(t, group.Stop())
	_, err = group.NewNode(groupTestNodeConfig(t, key))
	require.ErrorContains(t, err, "stopped")
}

// TestNativeNodeGroupRetiredPeerFunding verifies that completed peers do not
// participate in funding lookup and disconnect alone retains the lifecycle
// owner.
func TestNativeNodeGroupRetiredPeerFunding(t *testing.T) {
	t.Parallel()
	db, err := clientdb.OpenChannelDB(t.TempDir())
	require.NoError(t, err)
	directory := &staticIntentSource{}
	group, err := NewNativeNodeGroup(db, directory.FindByPendingChannelID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, group.Stop()) })
	key := testIntentKey(t)
	alice, err := group.NewNode(groupTestNodeConfig(t, key))
	require.NoError(t, err)
	bob, err := group.NewNode(groupTestNodeConfig(t, key))
	require.NoError(t, err)
	require.NoError(t, alice.Stop())
	_, err = group.node(alice.peer.PubKey())
	require.NoError(t, err)
	require.NoError(t, group.acquireOwner(alice))
	retired := make(chan error, 1)
	go func() {
		retired <- alice.Retire()
	}()
	require.Eventually(t, func() bool {
		_, err := group.node(alice.peer.PubKey())

		return err != nil
	}, time.Second, time.Millisecond)
	select {
	case err := <-retired:
		t.Fatalf("retirement returned before owner callback: %v", err)

	default:
	}
	alice.ownerCalls.Done()
	require.NoError(t, <-retired)
	_, err = group.node(alice.peer.PubKey())
	require.Error(t, err)
	directory.record.Snapshot.Terms.ClientNodeKey = bob.peer.PubKey()
	id := [32]byte{1}
	directory.record.Snapshot.Terms.PendingChannelID = id
	record, err := group.FindByPendingChannelID(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, bob.peer.PubKey(), record.Snapshot.Terms.ClientNodeKey)
}

// TestNativePaymentWaitsForReestablishment proves an Ark-active snapshot alone
// cannot create a terminal native attempt while its lnd link is unavailable.
func TestNativePaymentWaitsForReestablishment(t *testing.T) {
	t.Parallel()

	terms, source, clientKey, hubKey := statusPollChannel(t)
	backing := statusPollBacking(t, terms, source, clientKey, hubKey)
	db, err := clientdb.OpenChannelDB(t.TempDir())
	require.NoError(t, err)
	group, err := NewNativeNodeGroup(
		db, (&staticIntentSource{}).FindByPendingChannelID,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, group.Stop()) })
	cfg := groupTestNodeConfig(t, hubKey)
	cfg.RemoteNodeKey = clientKey.PubKey()
	node, err := group.NewNode(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	hash := (&lntypes.Preimage{43}).Hash()
	_, err = node.PayInvoiceResult(ctx, arkchannel.Record{
		Snapshot: arkchannel.Snapshot{
			Terms: terms, Source: &source, Backing: &backing,
			Phase: arkchannel.PhaseActive,
		},
	}, hash, 1_000)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = node.Runtime().Payments().ControlTower().FetchPayment(
		t.Context(), hash,
	)
	require.ErrorIs(t, err, paymentsdb.ErrPaymentNotInitiated)
}

// groupTestNodeConfig supplies native lnd dependencies without a server daemon.
func groupTestNodeConfig(t *testing.T, key *btcec.PrivateKey) NativeNodeConfig {
	t.Helper()
	remote := testIntentKey(t)

	return NativeNodeConfig{
		Party: arkchannel.PartyHub,
		Chain: fixedHeightChain{
			height: 800_000,
		},
		Notifier: newRuntimeNotifier(800_000),
		WalletController: &mock.WalletController{
			RootKey: key,
		},
		KeyRing: &mock.SecretKeyRing{
			RootKey: key,
		},
		Signer:       mock.NewSingleSigner(key),
		FeeEstimator: chainfee.NewStaticEstimator(1_250, 253),
		NetParams:    &chaincfg.RegressionNetParams,
		IdentityKey: keychain.KeyDescriptor{
			PubKey: key.PubKey(),
		},
		BackingKey: keychain.KeyDescriptor{
			PubKey: key.PubKey(),
		},
		RemoteNodeKey:    remote.PubKey(),
		Transport:        &recordingTransport{},
		Intents:          &staticIntentSource{},
		ChannelLifecycle: &groupTestLifecycle{},
	}
}
