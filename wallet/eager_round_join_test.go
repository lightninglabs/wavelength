package wallet

import (
	"context"
	"testing"
	"time"

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
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// eagerBoardFixture wires the minimum mocks the eager-board path exercises:
// one boarding address, one fresh UTXO paying to it, one assembling round
// actor that captures the resulting TriggerBoardMsg.
type eagerBoardFixture struct {
	backend    *MockBoardingBackend
	store      *MockBoardingStore
	roundActor *mockRoundActorBehavior
	wallet     *Ark
	epoch      chainsource.BlockEpoch
}

// newEagerBoardFixture sets up a wallet actor wired against a mock backend /
// store / round actor. The eager flag selects whether the wallet is built
// with WithEagerRoundJoin. The store is configured for the full
// processUtxo → handleBoard chain when eager is true; when eager is false,
// only the processUtxo expectations are set so an unwanted Fetch /
// UpsertPendingIntent call would fail the test.
func newEagerBoardFixture(t *testing.T, eager bool) *eagerBoardFixture {
	t.Helper()

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
				Family: keychain.KeyFamily(BoardingKeyFamily),
				Index:  0,
			},
		},
		OperatorKey: operatorKey.PubKey(),
		ExitDelay:   144,
	}

	outpoint := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 0}
	mockTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{{
			Value:    100_000,
			PkScript: pkScript,
		}},
	}
	epochHash := chainhash.Hash{0xbb}
	utxo := &Utxo{
		Outpoint:      outpoint,
		PkScript:      pkScript,
		Amount:        100_000,
		Confirmations: 6,
	}

	backend := &MockBoardingBackend{}
	backend.On(
		"ListUnspent", mock.Anything, int32(MinBoardingConfs),
		int32(MaxConfsForListUnspent),
	).Return([]*Utxo{utxo}, nil)
	backend.On(
		"GetTransaction", mock.Anything, outpoint.Hash,
	).Return(&TxInfo{
		Tx:          mockTx,
		BlockHash:   &epochHash,
		BlockHeight: 100,
	}, nil)
	backend.On(
		"GetBlock", mock.Anything, epochHash,
	).Return(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{mockTx},
	}, nil)

	store := &MockBoardingStore{}
	store.On(
		"LookupBoardingAddress", mock.Anything, pkScript,
	).Return(boardingAddr, nil)
	store.On(
		"InsertBoardingIntents", mock.Anything, mock.Anything,
	).Return(nil)

	if eager {
		// The eager path runs handleBoard inline, which reads the
		// confirmed boarding set and persists one pending row.
		// Return the freshly inserted intent so the round Tell is
		// not short-circuited by an empty boarding balance.
		confirmedIntent := BoardingIntent{
			Address:  *boardingAddr,
			Outpoint: outpoint,
			ChainInfo: BoardingChainInfo{
				ConfHeight: 100,
				ConfHash:   epochHash,
				ConfTx:     mockTx,
				OutPoint:   outpoint,
				Amount:     btcutil.Amount(100_000),
			},
			Status: BoardingStatusConfirmed,
		}
		store.On(
			"FetchBoardingIntentsByStatus", mock.Anything,
			BoardingStatusConfirmed,
		).Return([]BoardingIntent{confirmedIntent}, nil)
		store.On(
			"UpsertPendingIntent", mock.Anything,
			mock.Anything,
		).Return(nil)
	}

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		// Use Background ctx because t.Context is cancelled before
		// Cleanup runs.
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	})

	roundActor := &mockRoundActorBehavior{}
	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName, roundKey, roundActor,
	)

	opts := []ArkOption{WithClock(clock.NewDefaultClock())}
	if eager {
		opts = append(opts, WithEagerRoundJoin(true))
	}

	w := NewArk(
		backend, store, nil, nil, system, fn.None[ledger.Sink](),
		btclog.Disabled, opts...,
	)
	w.seenUtxos = fn.NewSet[UtxoKey]()
	w.notifiers = make(map[string]notifierInfo)

	return &eagerBoardFixture{
		backend:    backend,
		store:      store,
		roundActor: roundActor,
		wallet:     w,
		epoch: chainsource.BlockEpoch{
			Height: 100,
			Hash:   epochHash,
		},
	}
}

// newMultiUTXOEagerBoardFixture wires three confirmed boarding
// UTXOs paying to the same boarding address, all surfacing in one
// block-epoch. Used by the coalescing test to pin down the "one
// TriggerBoardMsg per block, not per UTXO" invariant.
func newMultiUTXOEagerBoardFixture(t *testing.T) *eagerBoardFixture {
	t.Helper()

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
				Family: keychain.KeyFamily(BoardingKeyFamily),
				Index:  0,
			},
		},
		OperatorKey: operatorKey.PubKey(),
		ExitDelay:   144,
	}

	epochHash := chainhash.Hash{0xbb}

	const numUTXOs = 3
	utxos := make([]*Utxo, 0, numUTXOs)
	confirmedIntents := make([]BoardingIntent, 0, numUTXOs)

	backend := &MockBoardingBackend{}
	for i := byte(0); i < numUTXOs; i++ {
		outpoint := wire.OutPoint{
			Hash: chainhash.Hash{
				0xaa,
				i,
			},
			Index: 0,
		}
		amount := btcutil.Amount(50_000 + int64(i)*10_000)
		mockTx := &wire.MsgTx{
			TxOut: []*wire.TxOut{{
				Value:    int64(amount),
				PkScript: pkScript,
			}},
		}

		utxos = append(utxos, &Utxo{
			Outpoint:      outpoint,
			PkScript:      pkScript,
			Amount:        amount,
			Confirmations: 6,
		})
		confirmedIntents = append(confirmedIntents, BoardingIntent{
			Address:  *boardingAddr,
			Outpoint: outpoint,
			ChainInfo: BoardingChainInfo{
				ConfHeight: 100,
				ConfHash:   epochHash,
				ConfTx:     mockTx,
				OutPoint:   outpoint,
				Amount:     amount,
			},
			Status: BoardingStatusConfirmed,
		})

		backend.On(
			"GetTransaction", mock.Anything, outpoint.Hash,
		).Return(&TxInfo{
			Tx:          mockTx,
			BlockHash:   &epochHash,
			BlockHeight: 100,
		}, nil)
		backend.On(
			"GetBlock", mock.Anything, epochHash,
		).Return(&wire.MsgBlock{
			Transactions: []*wire.MsgTx{mockTx},
		}, nil).Once()
	}
	backend.On(
		"ListUnspent", mock.Anything, int32(MinBoardingConfs),
		int32(MaxConfsForListUnspent),
	).Return(utxos, nil)

	store := &MockBoardingStore{}
	store.On(
		"LookupBoardingAddress", mock.Anything, pkScript,
	).Return(boardingAddr, nil)
	store.On(
		"InsertBoardingIntents", mock.Anything, mock.Anything,
	).Return(nil)
	store.On(
		"FetchBoardingIntentsByStatus", mock.Anything,
		BoardingStatusConfirmed,
	).Return(confirmedIntents, nil)
	store.On(
		"UpsertPendingIntent", mock.Anything, mock.Anything,
	).Return(nil)

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	})

	roundActor := &mockRoundActorBehavior{}
	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName, roundKey, roundActor,
	)

	w := NewArk(
		backend, store, nil, nil, system, fn.None[ledger.Sink](),
		btclog.Disabled,
		WithClock(
			clock.NewDefaultClock(),
		),
		WithEagerRoundJoin(true),
	)
	w.seenUtxos = fn.NewSet[UtxoKey]()
	w.notifiers = make(map[string]notifierInfo)

	return &eagerBoardFixture{
		backend:    backend,
		store:      store,
		roundActor: roundActor,
		wallet:     w,
		epoch: chainsource.BlockEpoch{
			Height: 100,
			Hash:   epochHash,
		},
	}
}

// TestEagerRoundJoinAutoBoardsOnBlockEpoch pins down the eager-board path:
// when WithEagerRoundJoin is on, a block-epoch that surfaces a fresh
// boarding UTXO must drive handleBoard inline and produce one
// TriggerBoardMsg to the round actor without the caller having to issue
// a follow-up Board RPC.
func TestEagerRoundJoinAutoBoardsOnBlockEpoch(t *testing.T) {
	t.Parallel()

	fix := newEagerBoardFixture(t, true)

	// handleBlockEpoch records the tip atomically (no heavy work);
	// handleProcessTipTick is what runs ListUnspent + the eager-board
	// hook on the next tick. Drive both directly so the test does
	// not depend on the runTipTickLoop goroutine being started.
	res := fix.wallet.handleBlockEpoch(t.Context(), fix.epoch)
	require.True(
		t, res.IsOk(),
		"block-epoch handler must succeed; got %v", res.Err(),
	)
	res = fix.wallet.handleProcessTipTick(t.Context())
	require.True(
		t, res.IsOk(),
		"tip-tick handler must succeed; got %v", res.Err(),
	)

	require.Eventually(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"eager round-join must self-issue one TriggerBoardMsg per "+
			"block-epoch that produced a new boarding UTXO")
}

// TestEagerRoundJoinCoalescesMultiUTXOBlockEpoch pins down the
// per-block (not per-UTXO) dispatch invariant: even when several
// boarding UTXOs confirm in the same block-epoch, the eager-board
// path produces exactly one TriggerBoardMsg. This is the test that
// would catch a regression where the inline handleBoard call leaks
// into the per-UTXO loop of processUtxo.
func TestEagerRoundJoinCoalescesMultiUTXOBlockEpoch(t *testing.T) {
	t.Parallel()

	fix := newMultiUTXOEagerBoardFixture(t)

	// Drive both handlers directly: the lightweight block-epoch
	// handler stores the tip, the tip-tick handler runs ListUnspent
	// + the eager-board hook.
	res := fix.wallet.handleBlockEpoch(t.Context(), fix.epoch)
	require.True(
		t, res.IsOk(),
		"block-epoch handler must succeed; got %v", res.Err(),
	)
	res = fix.wallet.handleProcessTipTick(t.Context())
	require.True(
		t, res.IsOk(),
		"tip-tick handler must succeed; got %v", res.Err(),
	)

	// Allow the actor system to drain the Tell.
	require.Eventually(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"three UTXOs confirming in one block must produce exactly "+
			"one TriggerBoardMsg, not three")

	// Beyond the eventual count, also confirm there is no late
	// over-trigger: poll for a brief window and assert the counter
	// stays pinned at 1.
	require.Never(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() > 1
	}, 250*time.Millisecond, 25*time.Millisecond,
		"eager-board must not retrigger after the initial dispatch")
}

// TestEagerRoundJoinDisabledSkipsAutoBoard verifies the off-default: the
// same confirmation that auto-boards under eager mode must NOT touch the
// pending-board store or the round actor when the flag is off. This is
// load-bearing for non-SDK hosts (wavecli, server deployments) whose
// callers expect explicit Board RPCs to be the only path that triggers a
// TriggerBoardMsg.
func TestEagerRoundJoinDisabledSkipsAutoBoard(t *testing.T) {
	t.Parallel()

	fix := newEagerBoardFixture(t, false)

	// Drive both handlers: when eager mode is off, neither the
	// block-epoch handler nor the tip-tick handler may dispatch
	// a TriggerBoardMsg.
	res := fix.wallet.handleBlockEpoch(t.Context(), fix.epoch)
	require.True(
		t, res.IsOk(),
		"block-epoch handler must succeed; got %v", res.Err(),
	)
	res = fix.wallet.handleProcessTipTick(t.Context())
	require.True(
		t, res.IsOk(),
		"tip-tick handler must succeed; got %v", res.Err(),
	)

	// processUtxo persists the intent, but no Fetch / Upsert / Tell
	// should follow. Give the actor system enough time to misbehave.
	require.Never(t, func() bool {
		return fix.roundActor.TriggerBoardCalls() > 0
	}, 250*time.Millisecond, 25*time.Millisecond,
		"non-eager wallet must not Tell the round actor on a "+
			"boarding confirmation")
}

// TestEagerRoundJoinSetsTriggerRegistrationOnLeave verifies the second
// half of the eager round-join contract: cooperative-leave intents are
// shipped with TriggerRegistration=true so the round FSM advances out
// of PendingRoundAssembly immediately. The off-default keeps the
// batched leave semantics that operator-driven hosts rely on.
//
// The two cases run as named subtests so a failure on the first leg
// does not short-circuit the second; this is the standard pattern for
// parametric Go tests and keeps the failure surface narrow.
func TestEagerRoundJoinSetsTriggerRegistrationOnLeave(t *testing.T) {
	t.Parallel()

	t.Run("eager=true", func(t *testing.T) {
		t.Parallel()
		runLeaveTriggerRegistrationCase(t, true)
	})
	t.Run("eager=false", func(t *testing.T) {
		t.Parallel()
		runLeaveTriggerRegistrationCase(t, false)
	})
}

// runLeaveTriggerRegistrationCase exercises handleLeaveVTXOs with the
// eager flag set to the supplied value and asserts that the captured
// RegisterIntentMsg's TriggerRegistration field matches.
func runLeaveTriggerRegistrationCase(t *testing.T, eager bool) {
	t.Helper()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50_000,
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			PolicyTemplate: []byte{
				0xde, 0xad, 0xbe, 0xef,
			},
			Expiry: 100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	})

	mgrKey := actormsg.VTXOManagerServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.VTXOManagerServiceKeyName, mgrKey, mgr,
	)

	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName, roundKey, roundActor,
	)

	opts := []ArkOption{}
	if eager {
		opts = append(opts, WithEagerRoundJoin(true))
	}

	w := NewArk(
		nil, nil, testVTXOReader(vtxoDescs), nil, system,
		fn.None[ledger.Sink](), btclog.Disabled, opts...,
	)

	req := &LeaveVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			op,
		},
		DestOutput: &wire.TxOut{
			PkScript: []byte{
				0x51,
				0x20,
				0xaa,
			},
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(
		t, result.IsOk(),
		"leave handler must succeed; got %v", result.Err(),
	)

	require.Equal(
		t, 1, roundActor.registerCalls,
		"round actor must observe exactly one RegisterIntentMsg",
	)
	require.NotNil(t, roundActor.capturedIntent)
	require.Equal(
		t, eager, roundActor.capturedIntent.TriggerRegistration, "Tr"+
			"iggerRegistration must mirror the eagerRoundJoin "+
			"flag (eager=%v)", eager,
	)
}
