package wallet

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
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
	script *waddrmgr.Tapscript) (btcutil.Address, error) {

	args := m.Called(ctx, script)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).(btcutil.Address), args.Error(1)
}

func (m *MockBoardingBackend) ListUnspent(ctx context.Context,
	minConfs, maxConfs int32) ([]*Utxo, error) {

	args := m.Called(ctx, minConfs, maxConfs)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).([]*Utxo), args.Error(1)
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

func (m *MockBoardingStore) FetchBoardingIntentOutpoints(
	ctx context.Context,
) ([]wire.OutPoint, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	//nolint:forcetypeassert
	return args.Get(0).([]wire.OutPoint), args.Error(1)
}

func (m *MockBoardingStore) FetchBoardingIntentsByStatusAndMinHeight(
	ctx context.Context, status BoardingStatus, minHeight int32,
) ([]BoardingIntent, error) {

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
//
//nolint:ll
func newMockChainSourceActor(epochChan chan chainsource.BlockEpoch) actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp] {

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
	testAddr, err := btcutil.NewAddressTaproot(
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
		backend, store, chainSource, btclog.Disabled,
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
		backend, store, chainSource, btclog.Disabled,
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
	address, err := btcutil.NewAddressTaproot(
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

	backend := &MockBoardingBackend{}
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
		backend, store, chainSource, btclog.Disabled,
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

	// Create a test UTXO.
	testOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x11, 0x22},
		Index: 0,
	}

	testUtxo := &Utxo{
		Outpoint:      testOutpoint,
		PkScript:      pkScript,
		Amount:        100000,
		Confirmations: 6,
	}

	// Process the UTXO.
	epoch := chainsource.BlockEpoch{
		Height: 100,
		Hash:   chainhash.Hash{0xaa, 0xbb},
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
	address, err := btcutil.NewAddressTaproot(
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

	backend := &MockBoardingBackend{}
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
		backend, store, chainSource, btclog.Disabled,
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
	testOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x11, 0x22},
		Index: 0,
	}
	testUtxo := &Utxo{
		Outpoint:      testOutpoint,
		PkScript:      pkScript,
		Amount:        100000,
		Confirmations: 3,
	}
	epoch := chainsource.BlockEpoch{
		Height: 100,
		Hash:   chainhash.Hash{0xaa, 0xbb},
	}
	walletActor.processUtxo(t.Context(), epoch, testUtxo)

	// Low-conf notifier should receive notification (3 >= 1).
	select {
	case event := <-lowConfRef.Messages():
		require.Equal(t, testOutpoint, event.Outpoint)

	default:
		t.Fatal("low-conf notifier should have received notification")
	}

	// High-conf notifier should NOT receive notification (3 < 6).
	select {
	case <-highConfRef.Messages():
		t.Fatal("high-conf notifier should not have received " +
			"notification")

	// Expected - no notification for high-conf notifier.
	default:
	}

	// Process another UTXO with 6 confirmations.
	walletActor.seenUtxos = fn.NewSet[UtxoKey]()

	testUtxo2 := &Utxo{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x33, 0x44},
			Index: 0,
		},
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
		addr, err := btcutil.NewAddressTaproot(
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
		backend, store, chainSource, btclog.Disabled,
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
	store.On(
		"FetchBoardingIntentsByStatus",
		mock.Anything, BoardingStatusConfirmed,
	).Return(
		[]BoardingIntent{}, nil,
	)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	chainSource := newMockChainSourceActor(epochChan)

	walletActor := NewArk(
		backend, store, chainSource, btclog.Disabled,
	)

	// Query balance.
	req := &GetBoardingBalanceRequest{}

	result := walletActor.Receive(t.Context(), req)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*GetBoardingBalanceResponse)

	// With no intents, balance should be zero.
	require.Equal(t, btcutil.Amount(0), resp.TotalBalance)
	require.Equal(t, 0, resp.UtxoCount)

	store.AssertExpectations(t)
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
		address, err := btcutil.NewAddressTaproot(
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
			Hash:  chainhash.Hash{byte(confHeight), 0x22},
			Index: 0,
		}

		return BoardingIntent{
			Address:  *addr,
			Outpoint: outpoint,
			ChainInfo: BoardingChainInfo{
				ConfHeight: confHeight,
				//nolint:ll
				ConfHash: chainhash.Hash{0xaa, byte(confHeight)},
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
			backend, store, chainSource, btclog.Disabled,
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
			backend, store, chainSource, btclog.Disabled,
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
			require.Equal(t, int32(150), event.ChainInfo.ConfHeight)
			//nolint:ll
			require.Equal(t, btcutil.Amount(15000000), event.ChainInfo.Amount)
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
			backend, store, chainSource, btclog.Disabled,
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
			backend, store, chainSource, btclog.Disabled,
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
}
