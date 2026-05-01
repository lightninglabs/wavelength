package darepod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type sendOORTestWallet struct {
	mu sync.Mutex

	selections [][]wallet.SelectedVTXO
	unlocks    [][]wire.OutPoint
	selects    int
}

func (w *sendOORTestWallet) Receive(_ context.Context,
	msg wallet.WalletMsg) fn.Result[wallet.WalletResp] {

	w.mu.Lock()
	defer w.mu.Unlock()

	switch msg := msg.(type) {
	case *wallet.SelectAndLockVTXOsRequest:
		w.selects++

		if len(w.selections) == 0 {
			return fn.Err[wallet.WalletResp](
				fmt.Errorf("unexpected select for %d sats",
					msg.TargetAmount),
			)
		}

		selected := append([]wallet.SelectedVTXO(nil),
			w.selections[0]...)
		w.selections = w.selections[1:]

		var total btcutil.Amount
		for i := range selected {
			total += selected[i].Amount
		}

		return fn.Ok[wallet.WalletResp](
			&wallet.SelectAndLockVTXOsResponse{
				SelectedVTXOs: selected,
				TotalSelected: total,
			},
		)

	case *wallet.UnlockVTXOsRequest:
		w.unlocks = append(w.unlocks,
			append([]wire.OutPoint(nil), msg.Outpoints...))

		return fn.Ok[wallet.WalletResp](
			&wallet.UnlockVTXOsResponse{
				UnlockedCount: len(msg.Outpoints),
			},
		)

	default:
		return fn.Err[wallet.WalletResp](
			fmt.Errorf("unexpected wallet message %T", msg),
		)
	}
}

func (w *sendOORTestWallet) unlockBatches() [][]wire.OutPoint {
	w.mu.Lock()
	defer w.mu.Unlock()

	batches := make([][]wire.OutPoint, 0, len(w.unlocks))
	for i := range w.unlocks {
		batches = append(batches,
			append([]wire.OutPoint(nil), w.unlocks[i]...))
	}

	return batches
}

func (w *sendOORTestWallet) selectCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.selects
}

type sendOORNoopOutboxHandler struct{}

func (h *sendOORNoopOutboxHandler) Handle(_ context.Context,
	_ oor.SessionID, _ oor.OutboxEvent) ([]oor.Event, error) {

	return nil, nil
}

type blockingSendOORActor struct {
	once      sync.Once
	started   chan struct{}
	release   chan struct{}
	completed chan struct{}
	response  oor.ActorResp
}

func (a *blockingSendOORActor) Receive(ctx context.Context,
	msg oor.OORDurableMsg) fn.Result[oor.ActorResp] {

	if _, ok := msg.(*oor.StartTransferRequest); !ok {
		return fn.Err[oor.ActorResp](
			fmt.Errorf("unexpected OOR message %T", msg),
		)
	}

	a.once.Do(func() {
		close(a.started)
	})
	defer close(a.completed)

	select {
	case <-a.release:
		return fn.Ok(a.response)

	case <-ctx.Done():
		return fn.Err[oor.ActorResp](ctx.Err())
	}
}

// TestSendOORReturnsExistingIdempotencyKeyBeforeWalletSelection verifies a
// keyed retry returns the existing OOR session before acquiring fresh wallet
// inputs.
func TestSendOORReturnsExistingIdempotencyKeyBeforeWalletSelection(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		amountSat      = int64(10000)
		exitDelay      = uint32(10)
		idempotencyKey = "rpc-send-oor-idempotency-key"
	)

	vtxoStore, deliveryStore := newSendOORTestStores(t)

	firstDesc := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x31, btcutil.Amount(amountSat),
	)

	require.NoError(t, vtxoStore.SaveVTXO(ctx, firstDesc))

	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{
			{selectedVTXOFromDescriptor(firstDesc)},
		},
	}

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		require.NoError(t, system.Shutdown(shutdownCtx))
	})

	walletKey := actor.NewServiceKey[
		wallet.WalletMsg, wallet.WalletResp,
	]("send-oor-test-wallet")
	walletRef := walletKey.Spawn(
		system, "send-oor-test-wallet", testWallet,
	)

	oorActor := oor.NewOORClientActor(oor.ClientActorCfg{
		Log:           fn.Some[btclog.Logger](btclog.Disabled),
		OutboxHandler: &sendOORNoopOutboxHandler{},
		DeliveryStore: deliveryStore,
		ActorSystem:   system,
		ActorID:       "send-oor-test-actor",
	})
	defer oorActor.Stop()

	walletReady := make(chan struct{})
	close(walletReady)

	server := &Server{
		cfg:         &Config{},
		log:         btclog.Disabled,
		walletReady: walletReady,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey:        operatorKey.PubKey().SerializeCompressed(),
				VtxoExitDelay: exitDelay,
				DustLimit:     1,
			},
		}),
		actorSystem: system,
		vtxoStore:   vtxoStore,
		walletRef:   fn.Some(walletRef),
	}

	rpcServer := NewRPCServer(server)
	recipient := sendOORPolicyRecipient(
		t, recipientKey.PubKey(), operatorKey.PubKey(),
		exitDelay, amountSat,
	)

	firstResp, err := rpcServer.SendOOR(ctx, &daemonrpc.SendOORRequest{
		Recipient:      recipient,
		IdempotencyKey: idempotencyKey,
	})
	require.NoError(t, err)
	require.Equal(t, "submitted", firstResp.Status)
	require.NotEmpty(t, firstResp.SessionId)
	require.Empty(t, testWallet.unlockBatches())

	secondResp, err := rpcServer.SendOOR(ctx, &daemonrpc.SendOORRequest{
		Recipient:      recipient,
		IdempotencyKey: idempotencyKey,
	})
	require.NoError(t, err)
	require.Equal(t, firstResp.SessionId, secondResp.SessionId)
	require.Equal(t, 1, testWallet.selectCount())
	require.Empty(t, testWallet.unlockBatches())
}

// TestSendOORUnlocksSelectedInputsForExistingSession verifies the daemon
// releases freshly selected wallet inputs when the OOR actor returns an
// existing deterministic session after input selection.
func TestSendOORUnlocksSelectedInputsForExistingSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		amountSat = int64(10000)
		exitDelay = uint32(10)
	)

	vtxoStore, deliveryStore := newSendOORTestStores(t)

	desc := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x31, btcutil.Amount(amountSat),
	)

	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))

	selectedVTXO := selectedVTXOFromDescriptor(desc)
	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{
			{selectedVTXO},
			{selectedVTXO},
		},
	}

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		require.NoError(t, system.Shutdown(shutdownCtx))
	})

	walletKey := actor.NewServiceKey[
		wallet.WalletMsg, wallet.WalletResp,
	]("send-oor-test-wallet")
	walletRef := walletKey.Spawn(
		system, "send-oor-test-wallet", testWallet,
	)

	oorActor := oor.NewOORClientActor(oor.ClientActorCfg{
		Log:           fn.Some[btclog.Logger](btclog.Disabled),
		OutboxHandler: &sendOORNoopOutboxHandler{},
		DeliveryStore: deliveryStore,
		ActorSystem:   system,
		ActorID:       "send-oor-test-actor",
	})
	defer oorActor.Stop()

	walletReady := make(chan struct{})
	close(walletReady)

	server := &Server{
		cfg:         &Config{},
		log:         btclog.Disabled,
		walletReady: walletReady,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey:        operatorKey.PubKey().SerializeCompressed(),
				VtxoExitDelay: exitDelay,
				DustLimit:     1,
			},
		}),
		actorSystem: system,
		vtxoStore:   vtxoStore,
		walletRef:   fn.Some(walletRef),
	}

	rpcServer := NewRPCServer(server)
	recipient := sendOORPolicyRecipient(
		t, recipientKey.PubKey(), operatorKey.PubKey(),
		exitDelay, amountSat,
	)

	firstResp, err := rpcServer.SendOOR(ctx, &daemonrpc.SendOORRequest{
		Recipient: recipient,
	})
	require.NoError(t, err)
	require.Equal(t, "submitted", firstResp.Status)
	require.NotEmpty(t, firstResp.SessionId)
	require.Empty(t, testWallet.unlockBatches())

	secondResp, err := rpcServer.SendOOR(ctx, &daemonrpc.SendOORRequest{
		Recipient: recipient,
	})
	require.NoError(t, err)
	require.Equal(t, firstResp.SessionId, secondResp.SessionId)
	require.Equal(t, 2, testWallet.selectCount())

	require.Eventually(t, func() bool {
		batches := testWallet.unlockBatches()
		if len(batches) != 1 {
			return false
		}

		return len(batches[0]) == 1 &&
			batches[0][0] == desc.Outpoint
	}, 5*time.Second, 50*time.Millisecond)
}

// TestSendOORWaitDeadlineDoesNotUnlockSubmittedInputs verifies that once a
// detached OOR actor Ask has been submitted, a caller wait deadline does not
// release wallet-selected inputs while that actor work is still in flight.
func TestSendOORWaitDeadlineDoesNotUnlockSubmittedInputs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		amountSat = int64(10000)
		exitDelay = uint32(10)
	)

	vtxoStore, _ := newSendOORTestStores(t)

	desc := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x31, btcutil.Amount(amountSat),
	)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))

	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{
			{selectedVTXOFromDescriptor(desc)},
		},
	}

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		require.NoError(t, system.Shutdown(shutdownCtx))
	})

	walletKey := actor.NewServiceKey[
		wallet.WalletMsg, wallet.WalletResp,
	]("send-oor-deadline-test-wallet")
	walletRef := walletKey.Spawn(
		system, "send-oor-deadline-test-wallet", testWallet,
	)

	sessionHash := chainhash.HashH([]byte("send-oor-deadline-session"))
	blockingActor := &blockingSendOORActor{
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		completed: make(chan struct{}),
		response: &oor.StartTransferResponse{
			SessionID: oor.SessionID(sessionHash),
		},
	}
	oorKey := oor.NewServiceKey()
	oorKey.Spawn(system, "send-oor-deadline-test-actor", blockingActor)

	walletReady := make(chan struct{})
	close(walletReady)

	server := &Server{
		cfg:         &Config{},
		log:         btclog.Disabled,
		walletReady: walletReady,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey:        operatorKey.PubKey().SerializeCompressed(),
				VtxoExitDelay: exitDelay,
				DustLimit:     1,
			},
		}),
		actorSystem: system,
		vtxoStore:   vtxoStore,
		walletRef:   fn.Some(walletRef),
	}

	rpcServer := NewRPCServer(server)
	recipient := sendOORPolicyRecipient(
		t, recipientKey.PubKey(), operatorKey.PubKey(),
		exitDelay, amountSat,
	)

	waitCtx, cancel := context.WithTimeout(
		context.Background(), 50*time.Millisecond,
	)
	defer cancel()

	_, err = rpcServer.SendOOR(waitCtx, &daemonrpc.SendOORRequest{
		Recipient: recipient,
	})
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))

	require.Eventually(t, func() bool {
		select {
		case <-blockingActor.started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	require.Empty(t, testWallet.unlockBatches())

	close(blockingActor.release)
	require.Eventually(t, func() bool {
		select {
		case <-blockingActor.completed:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	require.Empty(t, testWallet.unlockBatches())
}

// TestSubmittedOORCleanupDefersCustomInputRelease verifies custom-input
// double-use reservations are not released while a detached OOR actor future is
// still in flight after the RPC caller stopped waiting.
func TestSubmittedOORCleanupDefersCustomInputRelease(t *testing.T) {
	t.Parallel()

	rpcServer := &RPCServer{
		server: &Server{
			log: btclog.Disabled,
		},
		customInputLocks: make(map[wire.OutPoint]struct{}),
	}

	op := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("send-oor-custom-in-flight")),
		Index: 0,
	}

	release, err := rpcServer.reserveCustomInputs([]wire.OutPoint{op})
	require.NoError(t, err)

	promise := actor.NewPromise[oor.ActorResp]()
	rpcServer.cleanupSubmittedOORStart(
		context.Background(), promise.Future(), nil, release,
	)

	_, err = rpcServer.reserveCustomInputs([]wire.OutPoint{op})
	require.ErrorContains(t, err, "already reserved")

	sessionHash := chainhash.HashH([]byte("send-oor-custom-complete"))
	promise.Complete(fn.Ok[oor.ActorResp](&oor.StartTransferResponse{
		SessionID: oor.SessionID(sessionHash),
	}))

	require.Eventually(t, func() bool {
		release2, err := rpcServer.reserveCustomInputs(
			[]wire.OutPoint{op},
		)
		if err != nil {
			return false
		}

		defer release2()

		return true
	}, time.Second, 10*time.Millisecond)
}

// TestSubmittedOORCleanupTimeoutReleasesCustomInput verifies the detached OOR
// cleanup waiter is bounded even if the actor future never completes.
func TestSubmittedOORCleanupTimeoutReleasesCustomInput(t *testing.T) {
	t.Parallel()

	rpcServer := &RPCServer{
		server: &Server{
			log: btclog.Disabled,
		},
		customInputLocks: make(map[wire.OutPoint]struct{}),
	}

	op := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("send-oor-custom-timeout")),
		Index: 0,
	}

	release, err := rpcServer.reserveCustomInputs([]wire.OutPoint{op})
	require.NoError(t, err)

	promise := actor.NewPromise[oor.ActorResp]()
	rpcServer.cleanupSubmittedOORStartWithTimeout(
		context.Background(), promise.Future(), nil, release,
		10*time.Millisecond,
	)

	_, err = rpcServer.reserveCustomInputs([]wire.OutPoint{op})
	require.ErrorContains(t, err, "already reserved")

	require.Eventually(t, func() bool {
		release2, err := rpcServer.reserveCustomInputs(
			[]wire.OutPoint{op},
		)
		if err != nil {
			return false
		}

		defer release2()

		return true
	}, time.Second, 10*time.Millisecond)
}

func TestIsAwaitContextError(t *testing.T) {
	t.Parallel()

	deadlineCtx, cancel := context.WithTimeout(
		context.Background(), time.Nanosecond,
	)
	defer cancel()
	<-deadlineCtx.Done()

	require.True(t, isAwaitContextError(
		deadlineCtx, context.DeadlineExceeded,
	))
	require.True(t, isAwaitContextError(
		deadlineCtx, context.Canceled,
	))
	require.False(t, isAwaitContextError(
		context.Background(), context.Canceled,
	))
	require.False(t, isAwaitContextError(
		deadlineCtx, errors.New("actor failed"),
	))
}

func newSendOORTestStores(t *testing.T) (
	*db.VTXOPersistenceStore, actor.DeliveryStore) {

	t.Helper()

	sqlDB := db.NewTestDB(t)
	roundDB := db.NewTransactionExecutor(
		sqlDB.BaseDB,
		func(tx *sql.Tx) db.RoundStore {
			return sqlDB.WithTx(tx)
		},
		btclog.Disabled,
	)

	vtxoStore := db.NewVTXOPersistenceStore(
		roundDB, clock.NewDefaultClock(),
	)

	deliveryStore, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB, sqlDB.Backend(), clock.NewDefaultClock(),
		btclog.Disabled,
	)
	require.NoError(t, err)

	type txAwareDeliveryStore = actordelivery.TxAwareActorDeliveryStore
	txAwareStore, ok := deliveryStore.(*txAwareDeliveryStore)
	require.True(t, ok)

	return vtxoStore, txAwareStore.Store
}

func newSendOORTestVTXO(t *testing.T, operatorKey *btcec.PublicKey,
	hashByte byte, amount btcutil.Amount) *vtxo.Descriptor {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay uint32 = 10

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		clientKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
	require.NoError(t, err)

	pkScript, err := template.PkScript()
	require.NoError(t, err)

	tapScript, err := arkscript.VTXOTapScript(
		clientKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	var outpointHash chainhash.Hash
	outpointHash[0] = hashByte

	var commitmentTxID chainhash.Hash
	commitmentTxID[0] = hashByte
	commitmentTxID[1] = 0xc0

	return &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  outpointHash,
			Index: uint32(hashByte),
		},
		Amount:         amount,
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Index: uint32(hashByte),
			},
		},
		OperatorKey:    operatorKey,
		TapScript:      tapScript,
		RoundID:        fmt.Sprintf("send-oor-round-%x", hashByte),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    1000,
		RelativeExpiry: exitDelay,
		CreatedHeight:  500,
		Status:         vtxo.VTXOStatusLive,
	}
}

func selectedVTXOFromDescriptor(desc *vtxo.Descriptor) wallet.SelectedVTXO {
	return wallet.SelectedVTXO{
		Outpoint: desc.Outpoint,
		Amount:   desc.Amount,
		PkScript: desc.PkScript,
	}
}

func sendOORPolicyRecipient(t *testing.T, ownerKey,
	operatorKey *btcec.PublicKey, exitDelay uint32,
	amountSat int64) *daemonrpc.Output {

	t.Helper()

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return &daemonrpc.Output{
		Destination: &daemonrpc.Output_PolicyTemplate{
			PolicyTemplate: policyTemplate,
		},
		AmountSat:          amountSat,
		VtxoPolicyTemplate: policyTemplate,
	}
}
