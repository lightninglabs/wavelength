package wallet

import (
	"context"
	"fmt"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockBoardingBackend implements BoardingBackend for testing.
type MockBoardingBackend struct {
	mock.Mock
}

func (m *MockBoardingBackend) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	args := m.Called(ctx, family)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).(*keychain.KeyDescriptor), args.Error(1)
}

func (m *MockBoardingBackend) ImportTaprootScript(ctx context.Context,
	script *waddrmgr.Tapscript) (btcaddr.Address, error) {

	args := m.Called(ctx, script)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).(btcaddr.Address), args.Error(1)
}

func (m *MockBoardingBackend) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*Utxo, error) {

	args := m.Called(ctx, minConfs, maxConfs)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).([]*Utxo), args.Error(1)
}

func (m *MockBoardingBackend) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (*TxInfo, error) {

	args := m.Called(ctx, txid)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).(*TxInfo), args.Error(1)
}

func (m *MockBoardingBackend) GetBlock(ctx context.Context,
	blockHash chainhash.Hash) (*wire.MsgBlock, error) {

	args := m.Called(ctx, blockHash)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).(*wire.MsgBlock), args.Error(1)
}

// MockBoardingStore implements BoardingStore for testing.
type MockBoardingStore struct {
	mock.Mock
}

func (m *MockBoardingStore) InsertBoardingAddress(ctx context.Context,
	addr *BoardingAddress) error {

	args := m.Called(ctx, addr)

	return args.Error(0)
}

func (m *MockBoardingStore) LookupBoardingAddress(ctx context.Context,
	pkScript []byte) (*BoardingAddress, error) {

	args := m.Called(ctx, pkScript)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).(*BoardingAddress), args.Error(1)
}

func (m *MockBoardingStore) ListAllBoardingAddresses(ctx context.Context) (
	[]*BoardingAddress, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).([]*BoardingAddress), args.Error(1)
}

func (m *MockBoardingStore) InsertBoardingIntents(ctx context.Context,
	intents ...BoardingIntent) error {

	args := m.Called(ctx, intents)

	return args.Error(0)
}

func (m *MockBoardingStore) FetchBoardingIntents(ctx context.Context) (
	[]BoardingIntent, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).([]BoardingIntent), args.Error(1)
}

func (m *MockBoardingStore) FetchBoardingIntentsByStatus(ctx context.Context,
	status BoardingStatus) ([]BoardingIntent, error) {

	args := m.Called(ctx, status)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).([]BoardingIntent), args.Error(1)
}

func (m *MockBoardingStore) FetchBoardingIntentOutpoints(ctx context.Context) (
	[]wire.OutPoint, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).([]wire.OutPoint), args.Error(1)
}

func (m *MockBoardingStore) FetchBoardingIntentsByStatusAndMinHeight(
	ctx context.Context, status BoardingStatus, minHeight int32) (
	[]BoardingIntent, error) {

	args := m.Called(ctx, status, minHeight)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).([]BoardingIntent), args.Error(1)
}

func (m *MockBoardingStore) GetIntent(ctx context.Context,
	outpoint wire.OutPoint) (*BoardingIntent, error) {

	args := m.Called(ctx, outpoint)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).(*BoardingIntent), args.Error(1)
}

func (m *MockBoardingStore) LookupIntentByScript(ctx context.Context,
	pkScript []byte) (*BoardingIntent, error) {

	args := m.Called(ctx, pkScript)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).(*BoardingIntent), args.Error(1)
}

func (m *MockBoardingStore) UpsertPendingIntent(ctx context.Context,
	intent PendingIntent) error {

	args := m.Called(ctx, intent)

	return args.Error(0)
}

func (m *MockBoardingStore) ListPendingIntents(ctx context.Context,
	kind PendingIntentKind) ([]PendingIntent, error) {

	args := m.Called(ctx, kind)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).([]PendingIntent), args.Error(1)
}

func (m *MockBoardingStore) DeletePendingIntent(ctx context.Context,
	id PendingIntentID) error {

	args := m.Called(ctx, id)

	return args.Error(0)
}

func (m *MockBoardingStore) ClearPendingIntentsByKind(ctx context.Context,
	kind PendingIntentKind) error {

	args := m.Called(ctx, kind)

	return args.Error(0)
}

// mockChainSourceBehavior implements actor.ActorBehavior for testing.
type mockChainSourceBehavior struct {
	epochChan chan chainsource.BlockEpoch
}

func (m *mockChainSourceBehavior) Receive(ctx context.Context,
	msg chainsource.ChainSourceMsg) fn.Result[chainsource.ChainSourceResp] {

	switch msg.(type) {
	case *chainsource.SubscribeBlocksRequest:
		// Return an iterator that yields from our test channel.
		resp := &chainsource.SubscribeBlocksResponse{
			//nolint:ll
			Iterator: func(yield func(chainsource.BlockEpoch) bool) {
				for epoch := range m.epochChan {
					if !yield(epoch) {
						return
					}
				}
			},
		}

		return fn.Ok[chainsource.ChainSourceResp](resp)

	default:
		return fn.Err[chainsource.ChainSourceResp](
			fmt.Errorf("unknown message type"),
		)
	}
}

// newMockChainSourceActor creates a mock chainsource actor for testing.
func newMockChainSourceActor(
	epochChan chan chainsource.BlockEpoch) actor.ActorRef[
	chainsource.ChainSourceMsg,
	chainsource.ChainSourceResp,
] {

	behavior := &mockChainSourceBehavior{epochChan: epochChan}
	a := actor.NewActor(actor.ActorConfig[chainsource.ChainSourceMsg,
		chainsource.ChainSourceResp]{
		ID:          "mock-chainsource",
		Behavior:    behavior,
		MailboxSize: 10,
	})
	a.Start()

	return a.Ref()
}

// TestCreateBoardingAddress tests the address creation flow.
func TestCreateBoardingAddress(t *testing.T) {
	t.Parallel()

	// Set up mocks.
	testKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	keyDesc := &keychain.KeyDescriptor{
		PubKey: testKey.PubKey(),
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(BoardingKeyFamily),
			Index:  5,
		},
	}

	rootHash := []byte{0x01, 0x02, 0x03}
	taprootKey := txscript.ComputeTaprootOutputKey(
		testKey.PubKey(), rootHash,
	)
	testAddr, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	backend := &MockBoardingBackend{}
	backend.On(
		"DeriveNextKey", mock.Anything,
		keychain.KeyFamily(BoardingKeyFamily),
	).Return(
		keyDesc, nil,
	)
	backend.On(
		"ImportTaprootScript", mock.Anything, mock.Anything,
	).Return(
		testAddr, nil,
	)

	store := &MockBoardingStore{}
	store.On(
		"InsertBoardingAddress", mock.Anything, mock.Anything,
	).Return(nil)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	// Create a boarding address.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	req := &CreateBoardingAddressRequest{
		OperatorKey: operatorKey.PubKey(),
		ExitDelay:   144,
	}

	result := walletActor.Receive(t.Context(), req)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*CreateBoardingAddressResponse)

	// Verify response.
	require.NotNil(t, resp.Address)
	require.NotNil(t, resp.ClientKey)

	backend.AssertExpectations(t)
	store.AssertExpectations(t)
}

// TestRegisterNotifier tests registering for confirmation notifications.
func TestRegisterNotifier(t *testing.T) {
	t.Parallel()

	backend := &MockBoardingBackend{}
	store := &MockBoardingStore{}
	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	// Create a test notifier using the actor package helper.
	notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
		"test-notifier", 10,
	)

	// Register the notifier.
	req := &RegisterConfirmationNotifierRequest{
		NotifierID:    "test-notifier",
		NotifyActor:   notifyRef,
		BacklogHeight: fn.None[int32](),
	}

	result := walletActor.Receive(t.Context(), req)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*RegisterConfirmationNotifierResponse)
	require.True(t, resp.Success)

	// Verify notifier was added with default minConf.
	notifier, exists := walletActor.notifiers["test-notifier"]
	require.True(t, exists)
	require.Equal(t, uint32(MinBoardingConfs), notifier.minConf)

	// Unregister the notifier.
	unreg := &UnregisterConfirmationNotifierRequest{
		NotifierID: "test-notifier",
	}

	result = walletActor.Receive(t.Context(), unreg)
	require.True(t, result.IsOk())

	respVal, _ = result.Unpack()

	//nolint:forcetypeassert
	unregResp := respVal.(*UnregisterConfirmationNotifierResponse)
	require.True(t, unregResp.Success)

	// Verify notifier was removed.
	_, exists = walletActor.notifiers["test-notifier"]
	require.False(t, exists)

	// Test registering with custom MinConf.
	customMinConf := uint32(6)
	//nolint:ll
	customNotifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
		"custom-notifier", 10,
	)
	customReq := &RegisterConfirmationNotifierRequest{
		NotifierID:    "custom-notifier",
		NotifyActor:   customNotifyRef,
		BacklogHeight: fn.None[int32](),
		MinConf:       fn.Some(customMinConf),
	}

	result = walletActor.Receive(t.Context(), customReq)
	require.True(t, result.IsOk())

	// Verify custom minConf was stored.
	customNotifier, exists := walletActor.notifiers["custom-notifier"]
	require.True(t, exists)
	require.Equal(t, customMinConf, customNotifier.minConf)
}

// TestProcessNewUtxo tests UTXO detection and notification.
func TestProcessNewUtxo(t *testing.T) {
	t.Parallel()

	// Create test keys.
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create test address.
	rootHash := []byte{0xaa, 0xbb, 0xcc}
	taprootKey := txscript.ComputeTaprootOutputKey(
		clientKey.PubKey(), rootHash,
	)
	address, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)

	boardingAddr := &BoardingAddress{
		Address: address,
		Tapscript: &waddrmgr.Tapscript{
			Type:     waddrmgr.TapscriptTypeFullTree,
			RootHash: rootHash,
		},
		KeyDesc: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: 42,
				Index:  0,
			},
		},
		OperatorKey: operatorKey.PubKey(),
		ExitDelay:   144,
	}

	// Create a test UTXO outpoint.
	testOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
			0x22,
		},
		Index: 0,
	}

	// Create a mock transaction with the expected output.
	mockTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				Value:    100000,
				PkScript: pkScript,
			},
		},
	}

	// Use the epoch block hash as the confirmation block hash for
	// the mock. This simulates the happy path where the UTXO was
	// confirmed in the current epoch block.
	epochHash := chainhash.Hash{0xaa, 0xbb}

	backend := &MockBoardingBackend{}
	backend.On(
		"GetTransaction", mock.Anything, testOutpoint.Hash,
	).Return(&TxInfo{
		Tx:          mockTx,
		BlockHash:   &epochHash,
		BlockHeight: 100,
	}, nil)

	// Return a block containing the mock tx so TxProof can be built.
	mockBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{
			mockTx,
		},
	}
	backend.On(
		"GetBlock", mock.Anything, epochHash,
	).Return(mockBlock, nil)

	store := &MockBoardingStore{}
	store.On(
		"LookupBoardingAddress", mock.Anything, pkScript,
	).Return(
		boardingAddr, nil,
	)
	store.On(
		"InsertBoardingIntents", mock.Anything, mock.Anything,
	).Return(
		nil,
	)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	// Initialize the actor's state.
	walletActor.seenUtxos = fn.NewSet[UtxoKey]()
	walletActor.notifiers = make(map[string]notifierInfo)

	// Register a notifier using the actor package helper.
	notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
		"test", 10,
	)

	req := &RegisterConfirmationNotifierRequest{
		NotifierID:  "test",
		NotifyActor: notifyRef,
	}

	result := walletActor.Receive(t.Context(), req)
	require.True(t, result.IsOk())

	testUtxo := &Utxo{
		Outpoint:      testOutpoint,
		PkScript:      pkScript,
		Amount:        100000,
		Confirmations: 6,
	}

	// Process the UTXO.
	epoch := chainsource.BlockEpoch{
		Height: 100,
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
		},
	}

	walletActor.processUtxo(t.Context(), epoch, testUtxo)

	// Verify notification was sent.
	select {
	case event := <-notifyRef.Messages():
		require.Equal(t, testOutpoint, event.Outpoint)
		require.Equal(t, btcutil.Amount(100000), event.ChainInfo.Amount)
		require.Equal(t, int32(100), event.ChainInfo.ConfHeight)
		require.Equal(t, epoch.Hash, event.ChainInfo.ConfHash)
		require.Equal(
			t, address.String(), event.Address.Address.String(),
		)

	default:
		t.Fatal("notification not received")
	}

	// Verify UTXO was added to seenUtxos.
	key := NewUtxoKey(testOutpoint)
	require.True(t, walletActor.seenUtxos.Contains(key))

	// Process same UTXO again - should not notify.
	walletActor.processUtxo(t.Context(), epoch, testUtxo)

	select {
	case <-notifyRef.Messages():
		t.Fatal("received duplicate notification")

	default:
		// Expected - no duplicate notification.
	}

	backend.AssertExpectations(t)
	store.AssertExpectations(t)
}

// TestProcessUtxoMinConfFiltering tests that notifiers only receive events
// when the UTXO's confirmation count meets their minConf threshold.
func TestProcessUtxoMinConfFiltering(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rootHash := []byte{0xaa, 0xbb, 0xcc}
	taprootKey := txscript.ComputeTaprootOutputKey(
		clientKey.PubKey(), rootHash,
	)
	address, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)

	boardingAddr := &BoardingAddress{
		Address: address,
		Tapscript: &waddrmgr.Tapscript{
			Type:     waddrmgr.TapscriptTypeFullTree,
			RootHash: rootHash,
		},
		KeyDesc: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: 42,
				Index:  0,
			},
		},
		OperatorKey: operatorKey.PubKey(),
		ExitDelay:   144,
	}

	// Define test outpoints upfront so we can mock GetTransaction.
	testOutpoint1 := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
			0x22,
		},
		Index: 0,
	}
	testOutpoint2 := wire.OutPoint{
		Hash: chainhash.Hash{
			0x33,
			0x44,
		},
		Index: 0,
	}

	// Create mock transactions with the expected outputs.
	mockTx1 := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				Value:    100000,
				PkScript: pkScript,
			},
		},
	}
	mockTx2 := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				Value:    200000,
				PkScript: pkScript,
			},
		},
	}

	epochHash := chainhash.Hash{0xaa, 0xbb}

	backend := &MockBoardingBackend{}
	backend.On(
		"GetTransaction", mock.Anything, testOutpoint1.Hash,
	).Return(&TxInfo{
		Tx:          mockTx1,
		BlockHash:   &epochHash,
		BlockHeight: 100,
	}, nil)
	backend.On(
		"GetTransaction", mock.Anything, testOutpoint2.Hash,
	).Return(&TxInfo{
		Tx:          mockTx2,
		BlockHash:   &epochHash,
		BlockHeight: 100,
	}, nil)

	// Return blocks containing the respective mock txs.
	backend.On(
		"GetBlock", mock.Anything, epochHash,
	).Return(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{mockTx1},
	}, nil).Once()
	backend.On(
		"GetBlock", mock.Anything, epochHash,
	).Return(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{mockTx2},
	}, nil).Once()

	store := &MockBoardingStore{}
	store.On(
		"LookupBoardingAddress", mock.Anything, pkScript,
	).Return(
		boardingAddr, nil,
	)
	store.On(
		"InsertBoardingIntents", mock.Anything, mock.Anything,
	).Return(
		nil,
	)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	walletActor.seenUtxos = fn.NewSet[UtxoKey]()
	walletActor.notifiers = make(map[string]notifierInfo)

	// Register a notifier with default minConf (1).
	lowConfRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
		"low-conf", 10,
	)
	lowConfReq := &RegisterConfirmationNotifierRequest{
		NotifierID:  "low-conf",
		NotifyActor: lowConfRef,
		MinConf:     fn.None[uint32](),
	}
	result := walletActor.Receive(t.Context(), lowConfReq)
	require.True(t, result.IsOk())

	// Register a notifier requiring 6 confirmations.
	highConfRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
		"high-conf", 10,
	)
	highConfReq := &RegisterConfirmationNotifierRequest{
		NotifierID:  "high-conf",
		NotifyActor: highConfRef,
		MinConf:     fn.Some(uint32(6)),
	}
	result = walletActor.Receive(t.Context(), highConfReq)
	require.True(t, result.IsOk())

	// Create a test UTXO with only 3 confirmations.
	testUtxo := &Utxo{
		Outpoint:      testOutpoint1,
		PkScript:      pkScript,
		Amount:        100000,
		Confirmations: 3,
	}
	epoch := chainsource.BlockEpoch{
		Height: 100,
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
		},
	}
	walletActor.processUtxo(t.Context(), epoch, testUtxo)

	// Low-conf notifier should receive notification (3 >= 1).
	select {
	case event := <-lowConfRef.Messages():
		require.Equal(t, testOutpoint1, event.Outpoint)

	default:
		t.Fatal("low-conf notifier should have received notification")
	}

	// High-conf notifier should NOT receive notification (3 < 6).
	select {
	case <-highConfRef.Messages():
		t.Fatal(
			"high-conf notifier should not have received " +
				"notification",
		)

	// Expected - no notification for high-conf notifier.
	default:
	}

	// Process another UTXO with 6 confirmations.
	walletActor.seenUtxos = fn.NewSet[UtxoKey]()

	testUtxo2 := &Utxo{
		Outpoint:      testOutpoint2,
		PkScript:      pkScript,
		Amount:        200000,
		Confirmations: 6,
	}

	walletActor.processUtxo(t.Context(), epoch, testUtxo2)

	// Both notifiers should receive notification this time.
	select {
	case event := <-lowConfRef.Messages():
		require.Equal(t, testUtxo2.Outpoint, event.Outpoint)

	default:
		t.Fatal("low-conf notifier should have received notification")
	}

	select {
	case event := <-highConfRef.Messages():
		require.Equal(t, testUtxo2.Outpoint, event.Outpoint)

	default:
		t.Fatal("high-conf notifier should have received notification")
	}

	backend.AssertExpectations(t)
	store.AssertExpectations(t)
}

// TestProcessUtxoProofOmittedWhenTxNotInBlock verifies that when GetBlock
// succeeds but the boarding transaction is not found in the returned block
// (e.g., during catch-up after downtime where the block hash is wrong), the
// TxProof is omitted (None) but the boarding intent is still persisted and
// the notifier still receives the event.
func TestProcessUtxoProofOmittedWhenTxNotInBlock(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rootHash := []byte{0xaa, 0xbb, 0xcc}
	taprootKey := txscript.ComputeTaprootOutputKey(
		clientKey.PubKey(), rootHash,
	)
	address, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)

	boardingAddr := &BoardingAddress{
		Address: address,
		Tapscript: &waddrmgr.Tapscript{
			Type:     waddrmgr.TapscriptTypeFullTree,
			RootHash: rootHash,
		},
		KeyDesc: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: 42,
				Index:  0,
			},
		},
		OperatorKey: operatorKey.PubKey(),
		ExitDelay:   144,
	}

	testOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
			0x22,
		},
		Index: 0,
	}

	// The actual boarding transaction.
	boardingTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				Value:    100000,
				PkScript: pkScript,
			},
		},
	}

	// A different transaction that will be in the block instead.
	// This simulates the catch-up case where the block returned
	// doesn't contain the boarding tx.
	otherTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				Value: 50000,
				PkScript: []byte{
					0xde,
					0xad,
				},
			},
		},
	}

	confBlockHash := chainhash.Hash{0xcc, 0xdd}

	backend := &MockBoardingBackend{}
	backend.On(
		"GetTransaction", mock.Anything, testOutpoint.Hash,
	).Return(&TxInfo{
		Tx:          boardingTx,
		BlockHash:   &confBlockHash,
		BlockHeight: 100,
	}, nil)

	// Return a block that does NOT contain the boarding tx — only
	// otherTx. This simulates a block hash mismatch.
	backend.On(
		"GetBlock", mock.Anything, confBlockHash,
	).Return(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{otherTx},
	}, nil)

	store := &MockBoardingStore{}
	store.On(
		"LookupBoardingAddress", mock.Anything, pkScript,
	).Return(boardingAddr, nil)
	store.On(
		"InsertBoardingIntents", mock.Anything, mock.Anything,
	).Return(nil)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	walletActor.seenUtxos = fn.NewSet[UtxoKey]()
	walletActor.notifiers = make(map[string]notifierInfo)

	notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
		"test", 10,
	)
	req := &RegisterConfirmationNotifierRequest{
		NotifierID:  "test",
		NotifyActor: notifyRef,
	}
	result := walletActor.Receive(t.Context(), req)
	require.True(t, result.IsOk())

	testUtxo := &Utxo{
		Outpoint:      testOutpoint,
		PkScript:      pkScript,
		Amount:        100000,
		Confirmations: 6,
	}

	epoch := chainsource.BlockEpoch{
		Height: 100,
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
		},
	}

	processed := walletActor.processUtxo(t.Context(), epoch, testUtxo)
	require.True(t, processed)

	// Notification should still be sent even without a proof.
	select {
	case event := <-notifyRef.Messages():
		require.Equal(t, testOutpoint, event.Outpoint)
		require.Equal(
			t, btcutil.Amount(100000), event.ChainInfo.Amount,
		)

		// The TxProof should be None since the tx was not
		// found in the block.
		require.True(t, event.ChainInfo.TxProof.IsNone())

	default:
		t.Fatal("notification not received")
	}

	// UTXO should still be marked as seen.
	key := NewUtxoKey(testOutpoint)
	require.True(t, walletActor.seenUtxos.Contains(key))

	backend.AssertExpectations(t)
	store.AssertExpectations(t)
}

// TestProcessUtxoUsesActualConfirmationBlock verifies that catch-up flows use
// the transaction's actual confirmation block metadata for persisted intents
// and TxProofs instead of the current polling epoch.
func TestProcessUtxoUsesActualConfirmationBlock(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rootHash := []byte{0xaa, 0xbb, 0xcc}
	taprootKey := txscript.ComputeTaprootOutputKey(
		clientKey.PubKey(), rootHash,
	)
	address, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)

	boardingAddr := &BoardingAddress{
		Address: address,
		Tapscript: &waddrmgr.Tapscript{
			Type: waddrmgr.TaprootKeySpendRootHash,
			ControlBlock: &txscript.ControlBlock{
				InternalKey: clientKey.PubKey(),
			},
			RootHash: rootHash,
		},
		KeyDesc: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: 42,
				Index:  0,
			},
		},
		OperatorKey: operatorKey.PubKey(),
		ExitDelay:   144,
	}

	testOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
			0x22,
		},
		Index: 0,
	}

	boardingTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				Value:    100000,
				PkScript: pkScript,
			},
		},
	}

	confBlockHash := chainhash.Hash{0xcc, 0xdd}
	confHeight := int32(95)
	epoch := chainsource.BlockEpoch{
		Height: 100,
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
		},
	}

	backend := &MockBoardingBackend{}
	backend.On(
		"GetTransaction", mock.Anything, testOutpoint.Hash,
	).Return(&TxInfo{
		Tx:          boardingTx,
		BlockHash:   &confBlockHash,
		BlockHeight: confHeight,
	}, nil)
	backend.On(
		"GetBlock", mock.Anything, confBlockHash,
	).Return(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{boardingTx},
	}, nil)

	var inserted BoardingIntent
	store := &MockBoardingStore{}
	store.On(
		"LookupBoardingAddress", mock.Anything, pkScript,
	).Return(boardingAddr, nil)
	store.On(
		"InsertBoardingIntents", mock.Anything, mock.Anything,
	).Run(func(args mock.Arguments) {
		intents, ok := args.Get(1).([]BoardingIntent)
		require.True(t, ok)
		require.NotEmpty(t, intents)

		inserted = intents[0]
	}).Return(nil)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)
	walletActor.seenUtxos = fn.NewSet[UtxoKey]()
	walletActor.notifiers = make(map[string]notifierInfo)

	processed := walletActor.processUtxo(t.Context(), epoch, &Utxo{
		Outpoint:      testOutpoint,
		PkScript:      pkScript,
		Amount:        100000,
		Confirmations: 6,
	})
	require.True(t, processed)

	require.Equal(t, confHeight, inserted.ChainInfo.ConfHeight)
	require.Equal(t, confBlockHash, inserted.ChainInfo.ConfHash)
	require.True(t, inserted.ChainInfo.TxProof.IsSome())

	txProof := inserted.ChainInfo.TxProof.UnsafeFromSome()
	require.Equal(t, uint32(confHeight), txProof.BlockHeight)
	require.Equal(t, testOutpoint, txProof.ClaimedOutPoint)

	backend.AssertExpectations(t)
	store.AssertExpectations(t)
}

// TestGetActiveBoardingAddresses tests querying boarding addresses.
func TestGetActiveBoardingAddresses(t *testing.T) {
	t.Parallel()

	// Create test addresses.
	addresses := make([]*BoardingAddress, 3)
	for i := 0; i < 3; i++ {
		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		rootHash := []byte{byte(i), byte(i + 1)}
		taprootKey := txscript.ComputeTaprootOutputKey(
			privKey.PubKey(), rootHash,
		)
		addr, err := btcaddr.NewAddressTaproot(
			taprootKey.SerializeCompressed()[1:],
			&chaincfg.RegressionNetParams,
		)
		require.NoError(t, err)

		addresses[i] = &BoardingAddress{
			Address: addr,
			KeyDesc: keychain.KeyDescriptor{
				PubKey: privKey.PubKey(),
			},
			OperatorKey: privKey.PubKey(),
			ExitDelay:   144,
		}
	}

	backend := &MockBoardingBackend{}
	store := &MockBoardingStore{}
	store.On("ListAllBoardingAddresses", mock.Anything).Return(
		addresses, nil,
	)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	// Query addresses.
	req := &GetActiveBoardingAddressesRequest{}
	result := walletActor.Receive(t.Context(), req)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*GetActiveBoardingAddressesResponse)

	// Verify all addresses returned.
	require.Len(t, resp.Addresses, 3)
	for i, addr := range resp.Addresses {
		require.Equal(
			t, addresses[i].Address.String(), addr.Address.String(),
		)
	}

	store.AssertExpectations(t)
}

// TestGetBoardingBalance tests balance calculation.
func TestGetBoardingBalance(t *testing.T) {
	t.Parallel()

	backend := &MockBoardingBackend{}
	store := &MockBoardingStore{}
	// handleGetBoardingBalance queries lifecycle statuses (confirmed,
	// adopted, sweep_pending, swept) so monitoring callers can break
	// boarding balance down by lifecycle. Empty results are sufficient for
	// the happy-path zero-balance assertion.
	for _, status := range []BoardingStatus{
		BoardingStatusConfirmed, BoardingStatusAdopted,
		BoardingStatusSweepPending, BoardingStatusSwept,
	} {
		store.On(
			"FetchBoardingIntentsByStatus",
			mock.Anything, status,
		).Return(
			[]BoardingIntent{}, nil,
		)
	}
	backend.On(
		"ListUnspent", mock.Anything, int32(0), int32(
			MaxConfsForListUnspent,
		),
	).Return([]*Utxo{}, nil)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	// Query balance.
	req := &GetBoardingBalanceRequest{}

	result := walletActor.Receive(t.Context(), req)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*GetBoardingBalanceResponse)

	// With no intents, all balance breakdowns should be zero.
	require.Equal(t, btcutil.Amount(0), resp.TotalBalance)
	require.Equal(t, 0, resp.UtxoCount)
	require.Equal(t, btcutil.Amount(0), resp.UnconfirmedBalance)
	require.Equal(t, 0, resp.UnconfirmedUtxoCount)
	require.Equal(t, btcutil.Amount(0), resp.AdoptedBalance)
	require.Equal(t, btcutil.Amount(0), resp.PendingSweepBalance)
	require.Equal(t, btcutil.Amount(0), resp.SweptBalance)

	backend.AssertExpectations(t)
	store.AssertExpectations(t)
}

// TestGetBoardingBalanceIncludesAdoptedIntents verifies that a boarding UTXO
// adopted into a round remains visible while the resulting VTXO is not yet
// live.
func TestGetBoardingBalanceIncludesAdoptedIntents(t *testing.T) {
	t.Parallel()

	backend := &MockBoardingBackend{}
	store := &MockBoardingStore{}
	store.On(
		"FetchBoardingIntentsByStatus",
		mock.Anything, BoardingStatusConfirmed,
	).Return([]BoardingIntent{}, nil)
	store.On(
		"FetchBoardingIntentsByStatus",
		mock.Anything, BoardingStatusAdopted,
	).Return([]BoardingIntent{{
		Status: BoardingStatusAdopted,
		ChainInfo: BoardingChainInfo{
			Amount: 100_000,
		},
	}}, nil)
	store.On(
		"FetchBoardingIntentsByStatus",
		mock.Anything, BoardingStatusSweepPending,
	).Return([]BoardingIntent{}, nil)
	store.On(
		"FetchBoardingIntentsByStatus",
		mock.Anything, BoardingStatusSwept,
	).Return([]BoardingIntent{}, nil)
	backend.On(
		"ListUnspent", mock.Anything, int32(0), int32(
			MaxConfsForListUnspent,
		),
	).Return([]*Utxo{}, nil)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	result := walletActor.Receive(
		t.Context(), &GetBoardingBalanceRequest{},
	)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()
	resp := respVal.(*GetBoardingBalanceResponse) //nolint:forcetypeassert
	require.Equal(t, btcutil.Amount(100_000), resp.AdoptedBalance)
	require.Equal(t, btcutil.Amount(0), resp.TotalBalance)
	require.Equal(t, btcutil.Amount(0), resp.UnconfirmedBalance)

	backend.AssertExpectations(t)
	store.AssertExpectations(t)
}

// TestGetBoardingBalanceFiltersUnconfirmedUTXOs confirms pending boarding
// balance only includes zero-conf outputs that pay to known boarding scripts.
func TestGetBoardingBalanceFiltersUnconfirmedUTXOs(t *testing.T) {
	t.Parallel()

	backend := &MockBoardingBackend{}
	store := &MockBoardingStore{}
	for _, status := range []BoardingStatus{
		BoardingStatusConfirmed, BoardingStatusAdopted,
		BoardingStatusSweepPending, BoardingStatusSwept,
	} {
		store.On(
			"FetchBoardingIntentsByStatus",
			mock.Anything, status,
		).Return(
			[]BoardingIntent{}, nil,
		)
	}

	boardingScript := []byte{1, 2, 3}
	changeScript := []byte{4, 5, 6}
	confirmedBoardingScript := []byte{7, 8, 9}
	backend.On(
		"ListUnspent", mock.Anything, int32(0), int32(
			MaxConfsForListUnspent,
		),
	).Return([]*Utxo{
		{
			PkScript:      boardingScript,
			Amount:        1_000_000,
			Confirmations: 0,
		},
		{
			PkScript:      changeScript,
			Amount:        149_000_000,
			Confirmations: 0,
		},
		{
			PkScript:      confirmedBoardingScript,
			Amount:        2_000_000,
			Confirmations: 1,
		},
	}, nil)

	store.On(
		"LookupBoardingAddress", mock.Anything, boardingScript,
	).Return(&BoardingAddress{}, nil)
	store.On(
		"LookupBoardingAddress", mock.Anything, changeScript,
	).Return(nil, fmt.Errorf("not a boarding address"))

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	result := walletActor.Receive(
		t.Context(), &GetBoardingBalanceRequest{},
	)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()
	resp := respVal.(*GetBoardingBalanceResponse) //nolint:forcetypeassert
	require.Equal(t, btcutil.Amount(1_000_000), resp.UnconfirmedBalance)
	require.Equal(t, 1, resp.UnconfirmedUtxoCount)

	backend.AssertExpectations(t)
	store.AssertExpectations(t)
}

// TestGetConfirmedBoardingIntents verifies the wallet actor returns the
// currently confirmed boarding intents for restart-safe round retries.
func TestGetConfirmedBoardingIntents(t *testing.T) {
	t.Parallel()

	backend := &MockBoardingBackend{}
	store := &MockBoardingStore{}
	confirmed := []BoardingIntent{{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("confirmed")),
			Index: 1,
		},
		Status: BoardingStatusConfirmed,
		ChainInfo: BoardingChainInfo{
			Amount: 42_000,
		},
	}}
	store.On(
		"FetchBoardingIntentsByStatus",
		mock.Anything, BoardingStatusConfirmed,
	).Return(
		confirmed, nil,
	)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	result := walletActor.Receive(
		t.Context(), &GetConfirmedBoardingIntentsRequest{},
	)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()
	resp, ok := respVal.(*GetConfirmedBoardingIntentsResponse)
	require.True(t, ok)
	require.Len(t, resp.Intents, 1)
	require.Equal(t, confirmed[0].Outpoint, resp.Intents[0].Outpoint)
	require.Equal(
		t, confirmed[0].ChainInfo.Amount,
		resp.Intents[0].ChainInfo.Amount,
	)

	store.AssertExpectations(t)
}

// TestSplitBoardingAmount verifies board fanout preserves total balance while
// producing stable per-output target amounts.
func TestSplitBoardingAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		total     btcutil.Amount
		count     uint32
		want      []btcutil.Amount
		wantError string
	}{
		{
			name:  "zero count defaults to one output",
			total: 10_000,
			count: 0,
			want: []btcutil.Amount{
				10_000,
			},
		},
		{
			name:  "even split",
			total: 12_000,
			count: 3,
			want: []btcutil.Amount{
				4_000,
				4_000,
				4_000,
			},
		},
		{
			name:  "remainder is spread across leading outputs",
			total: 10_001,
			count: 3,
			want: []btcutil.Amount{
				3_334,
				3_334,
				3_333,
			},
		},
		{
			name:      "count cannot exceed sats",
			total:     2,
			count:     3,
			wantError: "too small",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := splitBoardingAmount(
				test.total, test.count,
			)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)

				return
			}

			require.NoError(t, err)
			require.Equal(t, test.want, got)
			require.Equal(t, test.total, sumBoardingAmounts(got))
		})
	}
}

// TestSendBacklog tests the sendBacklog method which delivers historical
// confirmation events to newly registered notifiers.
func TestSendBacklog(t *testing.T) {
	t.Parallel()

	// Helper function to create a test boarding address.
	makeTestAddress := func(t *testing.T) (*BoardingAddress, []byte) {
		t.Helper()

		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		rootHash := []byte{0xaa, 0xbb, 0xcc}
		taprootKey := txscript.ComputeTaprootOutputKey(
			privKey.PubKey(), rootHash,
		)
		address, err := btcaddr.NewAddressTaproot(
			taprootKey.SerializeCompressed()[1:],
			&chaincfg.RegressionNetParams,
		)
		require.NoError(t, err)

		pkScript, err := txscript.PayToAddrScript(address)
		require.NoError(t, err)

		boardingAddr := &BoardingAddress{
			Address: address,
			Tapscript: &waddrmgr.Tapscript{
				Type:     waddrmgr.TapscriptTypeFullTree,
				RootHash: rootHash,
			},
			KeyDesc: keychain.KeyDescriptor{
				PubKey: privKey.PubKey(),
				KeyLocator: keychain.KeyLocator{
					Family: 42,
					Index:  0,
				},
			},
			OperatorKey: privKey.PubKey(),
			ExitDelay:   144,
		}

		return boardingAddr, pkScript
	}

	// Helper function to create a test intent with chain info.
	makeTestIntent := func(
		t *testing.T, addr *BoardingAddress, confHeight int32,
	) BoardingIntent {

		t.Helper()

		outpoint := wire.OutPoint{
			Hash: chainhash.Hash{
				byte(confHeight),
				0x22,
			},
			Index: 0,
		}

		return BoardingIntent{
			Address:  *addr,
			Outpoint: outpoint,
			ChainInfo: BoardingChainInfo{
				ConfHeight: confHeight,
				ConfHash: chainhash.Hash{
					0xaa,
					byte(confHeight),
				},
				OutPoint: outpoint,
				Amount:   btcutil.Amount(100000 * confHeight),
			},
			Status: BoardingStatusConfirmed,
		}
	}

	t.Run("empty backlog", func(t *testing.T) {
		t.Parallel()

		backend := &MockBoardingBackend{}
		store := &MockBoardingStore{}
		store.On(
			"FetchBoardingIntentsByStatusAndMinHeight",
			mock.Anything, BoardingStatusConfirmed, int32(100),
		).Return(
			[]BoardingIntent{}, nil,
		)

		epochChan := make(chan chainsource.BlockEpoch, 1)
		chainSource := newMockChainSourceActor(epochChan)

		walletActor := NewArk(
			backend, store, nil, chainSource, nil,
			fn.None[ledger.Sink](), btclog.Disabled,
		)

		// Create a notifier using the actor package helper.
		//nolint:ll
		notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
			"test", 10,
		)

		// Call sendBacklog with no intents.
		walletActor.sendBacklog(t.Context(), notifyRef, 100)

		// Verify no events were sent.
		select {
		case <-notifyRef.Messages():
			t.Fatal("unexpected event received for empty backlog")

		default:
			// Expected - no events.
		}

		store.AssertExpectations(t)
	})

	t.Run("single intent in backlog", func(t *testing.T) {
		t.Parallel()

		addr, _ := makeTestAddress(t)
		intent := makeTestIntent(t, addr, 150)

		backend := &MockBoardingBackend{}
		store := &MockBoardingStore{}
		store.On(
			"FetchBoardingIntentsByStatusAndMinHeight",
			mock.Anything, BoardingStatusConfirmed, int32(100),
		).Return(
			[]BoardingIntent{intent}, nil,
		)

		epochChan := make(chan chainsource.BlockEpoch, 1)
		chainSource := newMockChainSourceActor(epochChan)

		walletActor := NewArk(
			backend, store, nil, chainSource, nil,
			fn.None[ledger.Sink](), btclog.Disabled,
		)

		// Create a notifier using the actor package helper.
		//nolint:ll
		notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
			"test", 10,
		)

		// Call sendBacklog with fromHeight below the intent.
		walletActor.sendBacklog(t.Context(), notifyRef, 100)

		// Verify event was sent.
		select {
		case event := <-notifyRef.Messages():
			confHeight := event.ChainInfo.ConfHeight
			require.Equal(t, int32(150), confHeight)
			require.Equal(
				t, btcutil.Amount(15000000),
				event.ChainInfo.Amount,
			)

		default:
			t.Fatal("expected event not received")
		}

		store.AssertExpectations(t)
	})

	t.Run("multiple intents in backlog", func(t *testing.T) {
		t.Parallel()

		addr, _ := makeTestAddress(t)
		intents := []BoardingIntent{
			makeTestIntent(t, addr, 100),
			makeTestIntent(t, addr, 150),
			makeTestIntent(t, addr, 200),
		}

		backend := &MockBoardingBackend{}
		store := &MockBoardingStore{}
		store.On(
			"FetchBoardingIntentsByStatusAndMinHeight",
			mock.Anything, BoardingStatusConfirmed, int32(0),
		).Return(
			intents, nil,
		)

		epochChan := make(chan chainsource.BlockEpoch, 1)
		chainSource := newMockChainSourceActor(epochChan)

		walletActor := NewArk(
			backend, store, nil, chainSource, nil,
			fn.None[ledger.Sink](), btclog.Disabled,
		)

		// Create a notifier using the actor package helper.
		//nolint:ll
		notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
			"test", 10,
		)

		// Call sendBacklog with fromHeight 0 to get all.
		walletActor.sendBacklog(t.Context(), notifyRef, 0)

		// Verify all 3 events were sent.
		receivedHeights := make(map[int32]bool)
		for i := 0; i < 3; i++ {
			select {
			case event := <-notifyRef.Messages():
				//nolint:ll
				receivedHeights[event.ChainInfo.ConfHeight] = true

			default:
				t.Fatalf("expected event %d not received", i)
			}
		}

		require.True(t, receivedHeights[100])
		require.True(t, receivedHeights[150])
		require.True(t, receivedHeights[200])

		// Verify no extra events.
		select {
		case <-notifyRef.Messages():
			t.Fatal("unexpected extra event received")

		default:
			// Expected.
		}

		store.AssertExpectations(t)
	})

	t.Run("height filtering", func(t *testing.T) {
		t.Parallel()

		addr, _ := makeTestAddress(t)
		intents := []BoardingIntent{
			// At the confirmation height threshold.
			makeTestIntent(t, addr, 100),

			// Above the confirmation height threshold.
			makeTestIntent(t, addr, 150),
			makeTestIntent(t, addr, 200),
		}

		backend := &MockBoardingBackend{}
		store := &MockBoardingStore{}

		// The database filters by height, so only intents >= 100 are
		// returned.
		store.On(
			"FetchBoardingIntentsByStatusAndMinHeight",
			mock.Anything, BoardingStatusConfirmed, int32(100),
		).Return(
			intents, nil,
		)

		epochChan := make(chan chainsource.BlockEpoch, 1)
		chainSource := newMockChainSourceActor(epochChan)

		walletActor := NewArk(
			backend, store, nil, chainSource, nil,
			fn.None[ledger.Sink](), btclog.Disabled,
		)

		// Create a notifier using the actor package helper.
		//nolint:ll
		notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
			"test", 10,
		)

		// Call sendBacklog with fromHeight 100.
		walletActor.sendBacklog(t.Context(), notifyRef, 100)

		// Verify only heights >= 100 were sent (3 events).
		receivedHeights := make(map[int32]bool)
		for i := 0; i < 3; i++ {
			select {
			case event := <-notifyRef.Messages():
				//nolint:ll
				receivedHeights[event.ChainInfo.ConfHeight] = true

			default:
				t.Fatalf("expected event %d not received", i)
			}
		}

		// Height 50 should not be included.
		require.False(t, receivedHeights[50])
		// Heights 100, 150, 200 should be included.
		require.True(t, receivedHeights[100])
		require.True(t, receivedHeights[150])
		require.True(t, receivedHeights[200])

		// Verify no extra events.
		select {
		case <-notifyRef.Messages():
			t.Fatal("unexpected extra event received")

		default:
			// Expected.
		}

		store.AssertExpectations(t)
	})

	// Backlog rebuild path: a persisted row with TxProof=None (legacy,
	// pre-migration-000010, or transient persistence loss) must have its
	// proof rebuilt from the chain backend before delivery so the round
	// actor receives a usable BoardingRequest.TxProof on standalone
	// daemon restart. This is the regression test for the boarding
	// failure observed in lwwallet mode where the server rejects the
	// join request: "TxProof is required when server has no chain
	// source".
	t.Run("rebuilds proof from backend on backlog", func(t *testing.T) {
		t.Parallel()

		clientKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		operatorKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		// Build a boarding address with a fully-formed taproot
		// tapscript so buildBoardingTxProof's
		// ControlBlock.InternalKey check passes.
		tapscript, err := arkscript.VTXOTapScript(
			clientKey.PubKey(), operatorKey.PubKey(), 144,
		)
		require.NoError(t, err)

		taprootKey := txscript.ComputeTaprootOutputKey(
			&arkscript.ARKNUMSKey, tapscript.RootHash,
		)
		address, err := btcaddr.NewAddressTaproot(
			taprootKey.SerializeCompressed()[1:],
			&chaincfg.RegressionNetParams,
		)
		require.NoError(t, err)

		pkScript, err := txscript.PayToAddrScript(address)
		require.NoError(t, err)

		boardingAddr := BoardingAddress{
			Address:   address,
			Tapscript: tapscript,
			KeyDesc: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
				KeyLocator: keychain.KeyLocator{
					Family: 42,
					Index:  0,
				},
			},
			OperatorKey: operatorKey.PubKey(),
			ExitDelay:   144,
		}

		// Construct the confirmation tx and block that the rebuild
		// path will pull through the chain backend.
		confTx := wire.NewMsgTx(2)
		confTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0xde, 0xad},
				Index: 0,
			},
		})
		confTx.AddTxOut(&wire.TxOut{
			Value:    100000,
			PkScript: pkScript,
		})

		confHash := chainhash.Hash{0xbe, 0xef}
		mockBlock := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{
				confTx,
			},
		}

		intent := BoardingIntent{
			Address: boardingAddr,
			Outpoint: wire.OutPoint{
				Hash:  confTx.TxHash(),
				Index: 0,
			},
			ChainInfo: BoardingChainInfo{
				ConfHeight: 200,
				ConfHash:   confHash,
				ConfTx:     confTx,
				OutPoint: wire.OutPoint{
					Hash:  confTx.TxHash(),
					Index: 0,
				},
				Amount: 100000,
				// Persisted row carries no proof — this is the
				// pre-migration / corruption-tolerated state.
			},
			Status: BoardingStatusConfirmed,
		}

		backend := &MockBoardingBackend{}
		backend.On(
			"GetBlock", mock.Anything, confHash,
		).Return(mockBlock, nil)

		store := &MockBoardingStore{}
		store.On(
			"FetchBoardingIntentsByStatusAndMinHeight",
			mock.Anything, BoardingStatusConfirmed, int32(0),
		).Return(
			[]BoardingIntent{intent}, nil,
		)

		// A successful rebuild must re-persist so subsequent reads
		// hydrate the rebuilt proof directly. Match any intent —
		// the in-memory mutation happens before the call lands.
		store.On(
			"InsertBoardingIntents",
			mock.Anything, mock.Anything,
		).Return(nil)

		epochChan := make(chan chainsource.BlockEpoch, 1)
		chainSource := newMockChainSourceActor(epochChan)

		walletActor := NewArk(
			backend, store, nil, chainSource, nil,
			fn.None[ledger.Sink](), btclog.Disabled,
		)

		//nolint:ll
		notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
			"test", 10,
		)

		walletActor.sendBacklog(t.Context(), notifyRef, 0)

		select {
		case event := <-notifyRef.Messages():
			require.True(
				t, event.ChainInfo.TxProof.IsSome(),
				"backlog event must carry rebuilt TxProof",
			)
			rebuilt := event.ChainInfo.TxProof.UnsafeFromSome()
			require.Equal(
				t, uint32(200), rebuilt.BlockHeight,
			)
			require.Equal(
				t, intent.Outpoint, rebuilt.ClaimedOutPoint,
			)

		default:
			t.Fatal("expected backlog event with rebuilt proof")
		}

		backend.AssertExpectations(t)
		store.AssertExpectations(t)
		store.AssertCalled(
			t, "InsertBoardingIntents", mock.Anything,
			mock.Anything,
		)
	})

	// Negative case: when the persisted intent has no ConfTx (legacy
	// data that was never fully populated), the rebuild path is
	// skipped and the event is delivered with TxProof=None rather than
	// blocking the backlog.
	t.Run("rebuild skipped without conf tx", func(t *testing.T) {
		t.Parallel()

		addr, _ := makeTestAddress(t)
		intent := makeTestIntent(t, addr, 100)
		// makeTestIntent leaves ConfTx nil — exercise that branch.
		require.Nil(t, intent.ChainInfo.ConfTx)

		backend := &MockBoardingBackend{}
		store := &MockBoardingStore{}
		store.On(
			"FetchBoardingIntentsByStatusAndMinHeight",
			mock.Anything, BoardingStatusConfirmed, int32(0),
		).Return(
			[]BoardingIntent{intent}, nil,
		)

		epochChan := make(chan chainsource.BlockEpoch, 1)
		chainSource := newMockChainSourceActor(epochChan)

		walletActor := NewArk(
			backend, store, nil, chainSource, nil,
			fn.None[ledger.Sink](), btclog.Disabled,
		)

		//nolint:ll
		notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
			"test", 10,
		)

		walletActor.sendBacklog(t.Context(), notifyRef, 0)

		select {
		case event := <-notifyRef.Messages():
			require.True(
				t, event.ChainInfo.TxProof.IsNone(),
				"event without ConfTx must keep proof None",
			)

		default:
			t.Fatal("expected backlog event")
		}

		// GetBlock must not have been called: the rebuild path is
		// skipped before reaching the backend when ConfTx is nil.
		backend.AssertNotCalled(
			t, "GetBlock", mock.Anything, mock.Anything,
		)
		// InsertBoardingIntents must not have been called either:
		// no rebuild → no re-persist.
		store.AssertNotCalled(
			t, "InsertBoardingIntents", mock.Anything,
			mock.Anything,
		)
		store.AssertExpectations(t)
	})

	// L-1: An intent loaded with a non-nil ConfTx but a zero ConfHash is
	// the third early-return branch of maybeRebuildBoardingProof. It
	// represents a partially-populated legacy row where the chain hash
	// was never persisted (e.g. a backend that produced a TxInfo with
	// BlockHash == nil). We must not call GetBlock with a zero hash —
	// the chain backend would either error or, worse, return a block
	// for an unrelated chainhash. The event ships with TxProof=None.
	t.Run("rebuild skipped with zero conf hash", func(t *testing.T) {
		t.Parallel()

		addr, _ := makeTestAddress(t)
		intent := makeTestIntent(t, addr, 100)

		// Populate ConfTx so the second branch passes, but leave
		// ConfHash at its zero value — the third early-return
		// guard should fire.
		confTx := wire.NewMsgTx(2)
		confTx.AddTxOut(&wire.TxOut{Value: 50000})
		intent.ChainInfo.ConfTx = confTx
		intent.ChainInfo.ConfHash = chainhash.Hash{}

		backend := &MockBoardingBackend{}
		store := &MockBoardingStore{}
		store.On(
			"FetchBoardingIntentsByStatusAndMinHeight",
			mock.Anything, BoardingStatusConfirmed, int32(0),
		).Return(
			[]BoardingIntent{intent}, nil,
		)

		epochChan := make(chan chainsource.BlockEpoch, 1)
		chainSource := newMockChainSourceActor(epochChan)

		walletActor := NewArk(
			backend, store, nil, chainSource, nil,
			fn.None[ledger.Sink](), btclog.Disabled,
		)

		//nolint:ll
		notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
			"test", 10,
		)

		walletActor.sendBacklog(t.Context(), notifyRef, 0)

		select {
		case event := <-notifyRef.Messages():
			require.True(
				t, event.ChainInfo.TxProof.IsNone(),
				"event with zero ConfHash must keep proof None",
			)

		default:
			t.Fatal("expected backlog event")
		}

		// Neither the chain backend nor the persistence layer
		// should be touched: zero ConfHash short-circuits before
		// either call.
		backend.AssertNotCalled(
			t, "GetBlock", mock.Anything, mock.Anything,
		)
		store.AssertNotCalled(
			t, "InsertBoardingIntents", mock.Anything,
			mock.Anything,
		)
		store.AssertExpectations(t)
	})

	// L-3: Rebuild reaches the chain backend but the returned block
	// does not contain the conf tx (txIdx < 0 in buildBoardingTxProof,
	// realistic during a reorg between confirmation and rebuild). The
	// helper must leave TxProof=None, ship the event anyway, and never
	// call InsertBoardingIntents — re-persisting an empty proof would
	// be a no-op under the COALESCE upsert but would still cost a
	// write.
	t.Run("rebuild failure when block missing conf tx", func(t *testing.T) {
		t.Parallel()

		clientKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		operatorKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		tapscript, err := arkscript.VTXOTapScript(
			clientKey.PubKey(), operatorKey.PubKey(), 144,
		)
		require.NoError(t, err)
		taprootKey := txscript.ComputeTaprootOutputKey(
			&arkscript.ARKNUMSKey, tapscript.RootHash,
		)
		address, err := btcaddr.NewAddressTaproot(
			taprootKey.SerializeCompressed()[1:],
			&chaincfg.RegressionNetParams,
		)
		require.NoError(t, err)
		pkScript, err := txscript.PayToAddrScript(address)
		require.NoError(t, err)

		boardingAddr := BoardingAddress{
			Address:   address,
			Tapscript: tapscript,
			KeyDesc: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
				KeyLocator: keychain.KeyLocator{
					Family: 42,
					Index:  0,
				},
			},
			OperatorKey: operatorKey.PubKey(),
			ExitDelay:   144,
		}

		// confTx is what the persisted intent claims confirmed —
		// but the block returned by GetBlock contains a different
		// transaction, simulating a reorg where the original conf
		// block was replaced.
		confTx := wire.NewMsgTx(2)
		confTx.AddTxOut(&wire.TxOut{
			Value:    100000,
			PkScript: pkScript,
		})

		// otherTx is the only transaction in the "rebuilt" block —
		// confTx is absent, so buildBoardingTxProof's
		// `txIdx < 0` guard fires.
		otherTx := wire.NewMsgTx(2)
		otherTx.AddTxOut(&wire.TxOut{
			Value:    50000,
			PkScript: []byte{0x51, 0x20, 0xaa, 0xbb},
		})

		confHash := chainhash.Hash{0xfe, 0xed}
		mockBlock := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{
				otherTx,
			},
		}

		intent := BoardingIntent{
			Address: boardingAddr,
			Outpoint: wire.OutPoint{
				Hash:  confTx.TxHash(),
				Index: 0,
			},
			ChainInfo: BoardingChainInfo{
				ConfHeight: 200,
				ConfHash:   confHash,
				ConfTx:     confTx,
				OutPoint: wire.OutPoint{
					Hash:  confTx.TxHash(),
					Index: 0,
				},
				Amount: 100000,
			},
			Status: BoardingStatusConfirmed,
		}

		backend := &MockBoardingBackend{}
		backend.On(
			"GetBlock", mock.Anything, confHash,
		).Return(mockBlock, nil)

		store := &MockBoardingStore{}
		store.On(
			"FetchBoardingIntentsByStatusAndMinHeight",
			mock.Anything, BoardingStatusConfirmed, int32(0),
		).Return(
			[]BoardingIntent{intent}, nil,
		)

		epochChan := make(chan chainsource.BlockEpoch, 1)
		chainSource := newMockChainSourceActor(epochChan)

		walletActor := NewArk(
			backend, store, nil, chainSource, nil,
			fn.None[ledger.Sink](), btclog.Disabled,
		)

		//nolint:ll
		notifyRef := actor.NewChannelTellOnlyRef[BoardingUtxoConfirmedEvent](
			"test", 10,
		)

		walletActor.sendBacklog(t.Context(), notifyRef, 0)

		select {
		case event := <-notifyRef.Messages():
			require.True(
				t, event.ChainInfo.TxProof.IsNone(),
				"event must ship with TxProof=None when "+
					"the rebuilt block lacks the conf tx",
			)

		default:
			t.Fatal("expected backlog event")
		}

		// The chain backend was consulted but produced no usable
		// proof; re-persistence must NOT have fired.
		backend.AssertExpectations(t)
		store.AssertNotCalled(
			t, "InsertBoardingIntents", mock.Anything,
			mock.Anything,
		)
		store.AssertExpectations(t)
	})
}

// TestIntentCompositionRequiresVTXOReader verifies that refresh and leave
// return per-outpoint errors instead of panicking when the wallet is created
// without a VTXOReader.
func TestIntentCompositionRequiresVTXOReader(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	defer func() {
		err := system.Shutdown(t.Context())
		require.NoError(t, err)
	}()

	backend := &MockBoardingBackend{}
	store := &MockBoardingStore{}

	var chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]
	walletActor := NewArk(
		backend, store, nil, chainSource, system,
		fn.None[ledger.Sink](), btclog.Disabled,
	)

	refreshOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("refresh-without-reader")),
		Index: 0,
	}
	refreshResult := walletActor.handleRefreshVTXOs(t.Context(),
		&RefreshVTXOsRequest{
			TargetOutpoints: []wire.OutPoint{refreshOutpoint},
		},
	)
	refreshRespVal, err := refreshResult.Unpack()
	require.NoError(t, err)

	refreshResp, ok := refreshRespVal.(*RefreshVTXOsResponse)
	require.True(t, ok, "unexpected response type: %T", refreshRespVal)
	require.Zero(t, refreshResp.RefreshingCount)
	require.ErrorContains(
		t, refreshResp.Errors[refreshOutpoint],
		"VTXO reader not configured",
	)

	leaveOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("leave-without-reader")),
		Index: 1,
	}
	leaveResult := walletActor.handleLeaveVTXOs(t.Context(),
		&LeaveVTXOsRequest{
			TargetOutpoints: []wire.OutPoint{leaveOutpoint},
			DestOutput: &wire.TxOut{
				Value:    1234,
				PkScript: []byte{0x00, 0x14, 0x01, 0x02},
			},
		},
	)
	leaveRespVal, err := leaveResult.Unpack()
	require.NoError(t, err)

	leaveResp, ok := leaveRespVal.(*LeaveVTXOsResponse)
	require.True(t, ok, "unexpected response type: %T", leaveRespVal)
	require.Zero(t, leaveResp.LeavingCount)
	require.ErrorContains(
		t, leaveResp.Errors[leaveOutpoint],
		"VTXO reader not configured",
	)
}

// TestBlockEpochBurstCoalescesToOneTipPass verifies that a burst of N
// BlockEpochNotification Tells does not pile up N ListUnspent calls
// behind every other Ask on the wallet actor. The block-epoch handler
// must complete in constant time (atomic store of the latest tip),
// the tip-tick handler must collapse the burst into one ListUnspent
// against the latest height, and subsequent ticks must short-circuit
// when the tip has not advanced. This is the load-bearing fix for
// bug-2 in BUGS_FOUND.md: a 200-block catch-up burst previously took
// ~7 minutes to drain while every wallet RPC hung behind it.
func TestBlockEpochBurstCoalescesToOneTipPass(t *testing.T) {
	t.Parallel()

	backend := &MockBoardingBackend{}
	store := &MockBoardingStore{}

	// The per-tick handler issues exactly one ListUnspent per tip
	// advance. What this test pins is the COALESCING property: one
	// tick — not one per block — services the entire burst. With
	// burstSize=200, the pre-fix path would have issued 200
	// ListUnspent calls.
	backend.On(
		"ListUnspent", mock.Anything, mock.Anything, mock.Anything,
	).Return([]*Utxo{}, nil)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	// Inject a 200-block burst directly via Receive: each call only
	// records the latest tip into the atomic pointer and returns,
	// so this loop should complete near-instantly without touching
	// ListUnspent.
	const burstSize = 200
	for height := int32(1); height <= burstSize; height++ {
		result := walletActor.Receive(
			t.Context(), BlockEpochNotification{
				BlockEpoch: chainsource.BlockEpoch{
					Height: height,
					Hash: chainhash.Hash{
						byte(height),
					},
				},
			},
		)
		require.True(
			t, result.IsOk(),
			"BlockEpochNotification handler must not error",
		)
	}

	// The atomic tip must reflect the latest height in the burst.
	tip := walletActor.latestKnownTip.Load()
	require.NotNil(t, tip)
	require.Equal(t, int32(burstSize), tip.Height)

	// processedTipHeight must still be zero — no tick has run yet,
	// so no ListUnspent should have been called.
	require.Zero(t, walletActor.processedTipHeight.Load())

	// One tick services the entire burst: a single ListUnspent runs
	// against the latest tip and processedTipHeight advances to it.
	// The pre-fix per-block path would have fired ListUnspent up to
	// burstSize times; coalescing collapses that to one. A backend
	// whose UTXO reporting lags the block epoch surfaces the missing
	// UTXO on the next chain advance (whichever tick processes the
	// new tip will re-run the scan), so we do not need an inline
	// multi-tick retry budget here.
	result := walletActor.Receive(
		t.Context(), ProcessTipTickNotification{},
	)
	require.True(t, result.IsOk())
	require.Equal(
		t, int32(burstSize), walletActor.processedTipHeight.Load(),
		"a single tick must process the latest tip in the burst",
	)
	require.Equal(
		t, 1, countMockListUnspentCalls(backend),
		"the burst tick must issue exactly one ListUnspent",
	)

	// A follow-up tick with no new blocks must short-circuit:
	// processedTipHeight matches the latest tip and no further
	// ListUnspent should fire.
	result = walletActor.Receive(
		t.Context(), ProcessTipTickNotification{},
	)
	require.True(t, result.IsOk())
	require.Equal(
		t, 1, countMockListUnspentCalls(backend),
		"an idle tick at an already-processed tip must not issue "+
			"any ListUnspent calls",
	)
}

// countMockListUnspentCalls returns how many times the mock backend
// has observed a ListUnspent invocation across the test so far.
// Counting from the mock.Mock.Calls slice keeps the assertion robust
// against retry-budget changes: the test cares about how many calls
// fired, not which specific arguments hit which expectation.
func countMockListUnspentCalls(b *MockBoardingBackend) int {
	count := 0
	for _, call := range b.Calls {
		if call.Method == "ListUnspent" {
			count++
		}
	}

	return count
}

// TestBlockEpochHandlerNeverCallsListUnspent locks down the contract
// that handleBlockEpoch is a pure atomic-store. If a future change
// re-adds work to the block-epoch hot path, bug-2 reopens for any
// burst — this test catches that regression. The companion
// TestBlockEpochBurstCoalescesToOneTipPass covers the deferred work;
// here we only check what the per-block handler does NOT do.
func TestBlockEpochHandlerNeverCallsListUnspent(t *testing.T) {
	t.Parallel()

	backend := &MockBoardingBackend{}
	store := &MockBoardingStore{}

	// No backend method is allowed to fire from a BlockEpoch handler.
	// We do not call backend.On(...) at all — any call would panic
	// the mock.

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, nil, chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
	)

	result := walletActor.Receive(t.Context(), BlockEpochNotification{
		BlockEpoch: chainsource.BlockEpoch{
			Height: 42,
			Hash:   chainhash.Hash{0x42},
		},
	})
	require.True(t, result.IsOk())

	// Mock has no expectations — if any backend method fired, the
	// mock would have panicked above.
	backend.AssertExpectations(t)
	store.AssertExpectations(t)
}
