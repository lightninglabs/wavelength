//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// NotificationReceiver is a simple actor that forwards
// BoardingUtxoConfirmedEvent to a channel for test verification.
type NotificationReceiver struct {
	ch chan<- wallet.BoardingUtxoConfirmedEvent
}

// NewNotificationReceiver creates a new notification receiver actor.
func NewNotificationReceiver(
	ch chan<- wallet.BoardingUtxoConfirmedEvent) *NotificationReceiver {

	return &NotificationReceiver{ch: ch}
}

// Receive implements the actor.ActorBehavior interface.
func (n *NotificationReceiver) Receive(ctx context.Context,
	msg wallet.BoardingUtxoConfirmedEvent) fn.Result[fn.Unit] {

	select {
	case n.ch <- msg:
	case <-ctx.Done():
	}

	return fn.Ok(fn.Unit{})
}

// GenerateOperatorKey creates a test operator public key.
func GenerateOperatorKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return privKey.PubKey()
}

// SpawnNotificationReceiver creates and spawns a notification receiver actor
// that forwards events to the provided channel.
func SpawnNotificationReceiver(t *testing.T, h *SysTestHarness,
	ch chan wallet.BoardingUtxoConfirmedEvent,
) actor.TellOnlyRef[wallet.BoardingUtxoConfirmedEvent] {

	t.Helper()

	receiver := NewNotificationReceiver(ch)

	// Create a service key for the notification receiver.
	key := actor.NewServiceKey[wallet.BoardingUtxoConfirmedEvent, fn.Unit](
		"notification-receiver",
	)
	ref := actor.RegisterWithSystem(
		h.ActorSystem(),
		"notification-receiver", key, receiver,
	)

	// Return as TellOnlyRef since we only need Tell capability.
	// ActorRef implements TellOnlyRef, so the interface conversion is
	// automatic.
	return ref
}

// BoardingWalletFixture holds the common components for boarding wallet tests.
// It encapsulates the harness, chain source, wallet actor, and wallet
// reference so tests can focus on test logic rather than setup boilerplate.
type BoardingWalletFixture struct {
	// T is the test instance.
	T *testing.T

	// Harness is the systest harness with Docker infrastructure.
	Harness *SysTestHarness

	// ChainSource is the chain source actor reference.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// WalletActor is the underlying wallet actor instance.
	WalletActor *wallet.Ark

	// Wallet is the wallet actor reference for sending messages.
	Wallet actor.ActorRef[wallet.WalletMsg, wallet.WalletResp]
}

// NewBoardingWalletFixture creates a complete boarding wallet test fixture. It
// sets up the harness, chain source actor, boarding backend, wallet actor, and
// starts everything. The caller only needs to write test logic.
func NewBoardingWalletFixture(t *testing.T) *BoardingWalletFixture {
	t.Helper()

	h := NewSysTestHarness(t)
	ctx := h.Context()

	// Create and start the chain source actor, along with the wallet and
	// all its deps.
	chainSourceRef := h.NewChainSourceActor()
	backend := h.NewBoardingBackend()
	walletActor := wallet.NewArk(
		backend, h.BoardingStore(), nil, chainSourceRef,
		h.ActorSystem(),
		fn.Some(
			ledger.NewSink(
				h.ActorSystem(),
			),
		),
		h.SubLogger(wallet.Subsystem),
	)

	// Next, we'll make a service key for tha wallet, then register it with
	// the system to obtain its ref.
	walletKey := actor.NewServiceKey[wallet.WalletMsg, wallet.WalletResp](
		"boarding-wallet",
	)
	walletRef := actor.RegisterWithSystem(
		h.ActorSystem(),
		"boarding-wallet", walletKey, walletActor,
	)

	// Now that the wallet has been registered with the system, we'll start
	// it so it can perform its start up duties.
	err := walletActor.Start(ctx, walletRef)
	require.NoError(t, err)

	return &BoardingWalletFixture{
		T:           t,
		Harness:     h,
		ChainSource: chainSourceRef,
		WalletActor: walletActor,
		Wallet:      walletRef,
	}
}

// Context returns the test context from the harness.
func (f *BoardingWalletFixture) Context() context.Context {
	return f.Harness.Context()
}

// CreateBoardingAddress creates a new boarding address with the given exit
// delay. Returns the address string.
func (f *BoardingWalletFixture) CreateBoardingAddress(
	exitDelay uint32) *wallet.CreateBoardingAddressResponse {

	f.T.Helper()

	operatorKey := GenerateOperatorKey(f.T)
	req := &wallet.CreateBoardingAddressRequest{
		OperatorKey: operatorKey,
		ExitDelay:   exitDelay,
	}

	future := f.Wallet.Ask(f.Context(), req)
	result := future.Await(f.Context())
	require.True(
		f.T, result.IsOk(),
		"create address failed: %v", result.Err(),
	)

	respVal, err := result.Unpack()
	require.NoError(f.T, err)

	resp, ok := respVal.(*wallet.CreateBoardingAddressResponse)
	require.True(f.T, ok, "unexpected response type: %T", respVal)

	f.T.Logf("Created boarding address: %s", resp.Address.String())

	return resp
}

// FundAddress sends the specified amount to the given address and mines a
// block to confirm it.
func (f *BoardingWalletFixture) FundAddress(addr string,
	amount btcutil.Amount) {

	f.T.Helper()

	f.Harness.Harness.Faucet(addr, amount)

	f.T.Logf("Sent %d satoshis to %s", amount, addr)

	f.Harness.Harness.Generate(1)
	f.Harness.WaitForLNDSync()

	f.T.Log("Mined 1 block")
}

// GetBalance queries and returns the current boarding balance.
//
//nolint:ll
func (f *BoardingWalletFixture) GetBalance() *wallet.GetBoardingBalanceResponse {
	f.T.Helper()

	req := &wallet.GetBoardingBalanceRequest{}
	future := f.Wallet.Ask(f.Context(), req)
	result := future.Await(f.Context())
	require.True(f.T, result.IsOk(), "get balance failed: %v", result.Err())

	respVal, err := result.Unpack()
	require.NoError(f.T, err)

	resp, ok := respVal.(*wallet.GetBoardingBalanceResponse)
	require.True(f.T, ok, "unexpected response type: %T", respVal)

	return resp
}

// WaitForBalance waits until the wallet has a positive balance.
func (f *BoardingWalletFixture) WaitForBalance(timeout time.Duration) {
	f.T.Helper()

	require.Eventually(f.T, func() bool {
		bal := f.GetBalance()
		f.T.Logf(
			"Current balance: %d satoshis, UTXO count: %d",
			bal.TotalBalance, bal.UtxoCount,
		)

		return bal.TotalBalance > 0
	}, timeout, 500*time.Millisecond, "wallet did not recognize balance")
}

// GetActiveAddresses returns all active boarding addresses as a map keyed by
// address string for efficient lookups.
//
//nolint:ll
func (f *BoardingWalletFixture) GetActiveAddresses() map[string]*wallet.BoardingAddress {
	f.T.Helper()

	req := &wallet.GetActiveBoardingAddressesRequest{}
	future := f.Wallet.Ask(f.Context(), req)
	result := future.Await(f.Context())
	require.True(f.T, result.IsOk())

	respVal, err := result.Unpack()
	require.NoError(f.T, err)

	resp, ok := respVal.(*wallet.GetActiveBoardingAddressesResponse)
	require.True(f.T, ok, "unexpected response type: %T", respVal)

	// Convert to map for efficient lookups.
	addrMap := make(map[string]*wallet.BoardingAddress, len(resp.Addresses))
	for _, addr := range resp.Addresses {
		addrMap[addr.Address.String()] = addr
	}

	return addrMap
}

// RegisterNotifier registers a notification receiver and returns the channel
// to receive events on.
func (f *BoardingWalletFixture) RegisterNotifier(
	notifierID string) <-chan wallet.BoardingUtxoConfirmedEvent {

	f.T.Helper()

	notifyCh := make(chan wallet.BoardingUtxoConfirmedEvent, 1)
	notifyRef := SpawnNotificationReceiver(f.T, f.Harness, notifyCh)

	req := &wallet.RegisterConfirmationNotifierRequest{
		NotifierID:    notifierID,
		NotifyActor:   notifyRef,
		BacklogHeight: fn.None[int32](),
	}

	future := f.Wallet.Ask(f.Context(), req)
	result := future.Await(f.Context())
	require.True(
		f.T, result.IsOk(),
		"register notifier failed: %v", result.Err(),
	)

	f.T.Logf("Registered confirmation notifier: %s", notifierID)

	return notifyCh
}

// UnregisterNotifier unregisters a notification receiver.
func (f *BoardingWalletFixture) UnregisterNotifier(notifierID string) {
	f.T.Helper()

	req := &wallet.UnregisterConfirmationNotifierRequest{
		NotifierID: notifierID,
	}

	future := f.Wallet.Ask(f.Context(), req)
	result := future.Await(f.Context())
	require.True(
		f.T, result.IsOk(),
		"unregister notifier failed: %v", result.Err(),
	)

	f.T.Logf("Unregistered confirmation notifier: %s", notifierID)
}

// Store returns the underlying BoardingStore for direct queries in tests.
func (f *BoardingWalletFixture) Store() wallet.BoardingStore {
	return f.Harness.BoardingStore()
}

// AssertAddressStored verifies a boarding address is correctly persisted with
// full fidelity. It compares the stored address against the expected values.
func (f *BoardingWalletFixture) AssertAddressStored(
	expected *wallet.BoardingAddress) {

	f.T.Helper()

	addresses := f.GetActiveAddresses()

	// Look up by address string.
	found, ok := addresses[expected.Address.String()]
	require.True(
		f.T, ok, "address %s not found in store",
		expected.Address.String(),
	)

	// Verify all fields.
	require.Equal(f.T, expected.ExitDelay, found.ExitDelay)
	require.True(
		f.T, expected.OperatorKey.IsEqual(found.OperatorKey),
		"operator key mismatch",
	)
	require.Equal(
		f.T, expected.KeyDesc.KeyLocator.Family,
		found.KeyDesc.KeyLocator.Family,
	)
	require.Equal(
		f.T, expected.KeyDesc.KeyLocator.Index,
		found.KeyDesc.KeyLocator.Index,
	)
	require.True(
		f.T, expected.KeyDesc.PubKey.IsEqual(found.KeyDesc.PubKey),
		"client pubkey mismatch",
	)
	require.NotNil(
		f.T, found.Tapscript, "tapscript should be reconstructed",
	)
}

// AssertIntentStored verifies a boarding intent is correctly persisted with
// full fidelity, including address details, chain info, and status.
func (f *BoardingWalletFixture) AssertIntentStored(
	expectedAddr *wallet.BoardingAddress,
	expectedAmount btcutil.Amount) *wallet.BoardingIntent {

	f.T.Helper()

	ctx := f.Context()
	intents, err := f.Store().FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusConfirmed,
	)
	require.NoError(f.T, err)

	var found *wallet.BoardingIntent
	for i := range intents {
		if intents[i].Address.Address.String() ==
			expectedAddr.Address.String() {

			found = &intents[i]

			break
		}
	}

	require.NotNil(
		f.T, found, "intent for address %s not found",
		expectedAddr.Address.String(),
	)

	// Verify address fidelity within intent.
	require.Equal(f.T, expectedAddr.ExitDelay, found.Address.ExitDelay)
	require.True(
		f.T,
		expectedAddr.OperatorKey.IsEqual(found.Address.OperatorKey),
	)
	require.True(
		f.T, expectedAddr.KeyDesc.PubKey.IsEqual(
			found.Address.KeyDesc.PubKey,
		),
	)

	// Verify all the chain information is correct.
	require.Equal(f.T, wallet.BoardingStatusConfirmed, found.Status)
	require.Greater(f.T, found.ChainInfo.ConfHeight, int32(0))
	require.NotZero(f.T, found.ChainInfo.ConfHash)
	require.NotEqual(f.T, wire.OutPoint{}, found.Outpoint)
	require.Equal(f.T, found.Outpoint, found.ChainInfo.OutPoint)

	// Verify amount (with tolerance for fees).
	require.InDelta(
		f.T, int64(expectedAmount), int64(found.ChainInfo.Amount),
		10000,
	)

	// Verify the confirmation transaction was fetched. This is critical for
	// the round FSM to extract output values and construct TxProofs.
	require.NotNil(
		f.T, found.ChainInfo.ConfTx,
		"confirmation transaction should be populated",
	)

	// Verify the transaction contains the expected output at the index
	// specified by the outpoint.
	outIdx := found.Outpoint.Index
	require.Less(
		f.T, int(outIdx), len(found.ChainInfo.ConfTx.TxOut),
		"outpoint index out of range",
	)
	txOut := found.ChainInfo.ConfTx.TxOut[outIdx]
	require.InDelta(
		f.T, int64(expectedAmount), txOut.Value, 10000,
		"transaction output value should match expected amount",
	)

	return found
}

// RegisterNotifierWithBacklog registers a notification receiver with backlog
// delivery from the specified height.
func (f *BoardingWalletFixture) RegisterNotifierWithBacklog(notifierID string,
	fromHeight int32) <-chan wallet.BoardingUtxoConfirmedEvent {

	f.T.Helper()

	notifyCh := make(chan wallet.BoardingUtxoConfirmedEvent, 10)
	notifyRef := SpawnNotificationReceiver(f.T, f.Harness, notifyCh)

	req := &wallet.RegisterConfirmationNotifierRequest{
		NotifierID:    notifierID,
		NotifyActor:   notifyRef,
		BacklogHeight: fn.Some(fromHeight),
	}

	future := f.Wallet.Ask(f.Context(), req)
	result := future.Await(f.Context())
	require.True(
		f.T, result.IsOk(),
		"register notifier failed: %v", result.Err(),
	)

	f.T.Logf("Registered notifier with backlog from height %d", fromHeight)

	return notifyCh
}
