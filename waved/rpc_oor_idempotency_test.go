package waved

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type selectionReq = wallet.SelectAndLockVTXOsRequest

type sendOORTestWallet struct {
	mu sync.Mutex

	selections [][]wallet.SelectedVTXO
	unlocks    [][]wire.OutPoint
	selectReqs []*selectionReq
	selects    int
}

func (w *sendOORTestWallet) Receive(_ context.Context,
	msg wallet.WalletMsg) fn.Result[wallet.WalletResp] {

	w.mu.Lock()
	defer w.mu.Unlock()

	switch msg := msg.(type) {
	case *wallet.SelectAndLockVTXOsRequest:
		w.selects++
		reqCopy := *msg
		w.selectReqs = append(w.selectReqs, &reqCopy)

		if len(w.selections) == 0 {
			return fn.Err[wallet.WalletResp](
				fmt.Errorf("unexpected select for %d sats",
					msg.TargetAmount),
			)
		}

		selected := append(
			[]wallet.SelectedVTXO(nil), w.selections[0]...,
		)
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
		w.unlocks = append(
			w.unlocks,
			append(
				[]wire.OutPoint(nil), msg.Outpoints...,
			),
		)

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
		batches = append(
			batches,
			append(
				[]wire.OutPoint(nil), w.unlocks[i]...,
			),
		)
	}

	return batches
}

func (w *sendOORTestWallet) selectCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.selects
}

func (w *sendOORTestWallet) selectionRequests() []*selectionReq {
	w.mu.Lock()
	defer w.mu.Unlock()

	requests := make(
		[]*selectionReq, 0, len(w.selectReqs),
	)
	for _, req := range w.selectReqs {
		reqCopy := *req
		requests = append(requests, &reqCopy)
	}

	return requests
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

type capturingSendOORActor struct {
	mu       sync.Mutex
	requests []*oor.StartTransferRequest
	response *oor.StartTransferResponse
}

func (a *capturingSendOORActor) Receive(_ context.Context,
	msg oor.OORDurableMsg) fn.Result[oor.ActorResp] {

	req, ok := msg.(*oor.StartTransferRequest)
	if !ok {
		return fn.Err[oor.ActorResp](
			fmt.Errorf("unexpected OOR message %T", msg),
		)
	}

	reqCopy := *req
	reqCopy.Inputs = append([]oor.TransferInput(nil), req.Inputs...)
	reqCopy.Recipients = append(
		[]oortx.RecipientOutput(nil), req.Recipients...,
	)

	a.mu.Lock()
	a.requests = append(a.requests, &reqCopy)
	resp := a.response
	a.mu.Unlock()

	return fn.Ok[oor.ActorResp](resp)
}

func (a *capturingSendOORActor) capturedRequests() []*oor.StartTransferRequest {
	a.mu.Lock()
	defer a.mu.Unlock()

	requests := make([]*oor.StartTransferRequest, 0, len(a.requests))
	for _, req := range a.requests {
		reqCopy := *req
		reqCopy.Inputs = append([]oor.TransferInput(nil), req.Inputs...)
		reqCopy.Recipients = append(
			[]oortx.RecipientOutput(nil), req.Recipients...,
		)
		requests = append(requests, &reqCopy)
	}

	return requests
}

// TestSendOORRejectsRecipientBelowFloorBeforeWalletSelection verifies the
// daemon enforces the operator's VTXO floor before it selects wallet inputs or
// submits work to the OOR actor. This is the daemon-side guard behind
// `wavecli ark send oor`: a caller that asks to create a below-floor
// recipient VTXO must fail synchronously instead of leaving the receiver with a
// live VTXO they cannot later spend cooperatively.
func TestSendOORRejectsRecipientBelowFloorBeforeWalletSelection(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		dustLimit = int64(1000)
		amountSat = int64(999)
		exitDelay = uint32(10)
	)

	walletReady := make(chan struct{})
	close(walletReady)

	server := &Server{
		cfg:         &Config{},
		log:         btclog.Disabled,
		walletReady: walletReady,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: operatorKey.
					PubKey().
					SerializeCompressed(),
				VtxoExitDelay: exitDelay,
				DustLimit:     dustLimit,
			},
		}),
	}

	rpcServer := NewRPCServer(server)
	recipient := sendOORPolicyRecipient(
		t, recipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)

	_, err = rpcServer.SendOOR(t.Context(), &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{recipient},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(
		t, err, "amount 999 below operator min_vtxo_amount_sat 1000",
	)
}

// TestSendOORSubmitsMultipleRecipients verifies the daemon maps one
// multi-recipient RPC request into one OOR actor request. This pins the public
// RPC surface that later batching code uses: wallet selection targets the sum
// of all requested outputs, the actor receives every requested recipient in one
// package, and the response reports outpoints in request-recipient order even
// though the underlying Ark transaction uses canonical output ordering.
func TestSendOORSubmitsMultipleRecipients(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKeyA, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKeyB, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		amountA   = int64(11_000)
		amountB   = int64(13_000)
		exitDelay = uint32(10)
	)
	totalAmount := btcutil.Amount(amountA + amountB)

	vtxoStore, _, _ := newSendOORTestStores(t)
	desc, _ := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x51, totalAmount,
	)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))

	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{{
			selectedVTXOFromDescriptor(desc),
		}},
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
	](
		"send-oor-many-test-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "send-oor-many-test-wallet", testWallet,
	)

	sessionHash := chainhash.HashH([]byte("send-oor-many-session"))
	oorActor := &capturingSendOORActor{
		response: &oor.StartTransferResponse{
			SessionID: oor.SessionID(sessionHash),
		},
	}
	oorKey := oor.NewServiceKey()
	oorKey.Spawn(system, "send-oor-many-test-actor", oorActor)

	walletReady := make(chan struct{})
	close(walletReady)

	server := &Server{
		cfg:         &Config{},
		log:         btclog.Disabled,
		walletReady: walletReady,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: operatorKey.
					PubKey().
					SerializeCompressed(),
				VtxoExitDelay: exitDelay,
				DustLimit:     1,
			},
		}),
		actorSystem: system,
		vtxoStore:   vtxoStore,
		walletRef:   fn.Some(walletRef),
	}

	rpcServer := NewRPCServer(server)
	recipientA := sendOORPolicyRecipient(
		t, recipientKeyA.PubKey(), operatorKey.PubKey(), exitDelay,
		amountA,
	)
	recipientB := sendOORPolicyRecipient(
		t, recipientKeyB.PubKey(), operatorKey.PubKey(), exitDelay,
		amountB,
	)

	resp, err := rpcServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{
			recipientA,
			recipientB,
		},
	})
	require.NoError(t, err)
	require.Equal(t, "submitted", resp.GetStatus())
	require.Equal(t, sessionHash.String(), resp.GetSessionId())
	require.Len(t, resp.GetRecipientOutpoints(), 2)

	requests := oorActor.capturedRequests()
	require.Len(t, requests, 1)
	require.Len(t, requests[0].Recipients, 2)
	require.Empty(t, requests[0].IdempotencyKey)
	require.Len(t, requests[0].Inputs, 1)
	require.Equal(t, desc.Outpoint, requests[0].Inputs[0].VTXO.Outpoint)

	for i, recipient := range requests[0].Recipients {
		outpoint, err := oortx.RecipientOutPoint(
			sessionHash, requests[0].Recipients, recipient,
		)
		require.NoError(t, err)
		require.Equal(t, outpoint.String(), resp.RecipientOutpoints[i])
	}

	selectReqs := testWallet.selectionRequests()
	require.Len(t, selectReqs, 1)
	require.Equal(t, totalAmount, selectReqs[0].TargetAmount)
	require.Equal(t, btcutil.Amount(1), selectReqs[0].MinChangeAmount)
	require.Empty(t, testWallet.unlockBatches())
}

// TestSendOORRejectsTooManyRecipients verifies the daemon rejects oversized
// OOR fanout before resolving scripts or selecting wallet inputs. The OOR
// actor also has request-size limits, but the RPC layer does enough per
// recipient work that it needs its own cheap boundary guard.
func TestSendOORRejectsTooManyRecipients(t *testing.T) {
	t.Parallel()

	recipients := make([]*waverpc.Output, maxOORRecipients+1)
	for i := range recipients {
		recipients[i] = &waverpc.Output{
			AmountSat: 1,
		}
	}

	_, err := sendOORRequestRecipients(&waverpc.SendOORRequest{
		Recipients: recipients,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "too many recipients")
}

// TestSendOORRejectsDuplicateRecipientOutputs verifies the daemon rejects
// request entries that would map to the same canonical OOR output. Without
// this guard the transaction builder cannot map request-order recipients back
// to distinct outpoints after canonical output sorting.
func TestSendOORRejectsDuplicateRecipientOutputs(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		amountSat = int64(10_000)
		exitDelay = uint32(10)
	)

	walletReady := make(chan struct{})
	close(walletReady)

	server := &Server{
		cfg:         &Config{},
		log:         btclog.Disabled,
		walletReady: walletReady,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: operatorKey.
					PubKey().
					SerializeCompressed(),
				VtxoExitDelay: exitDelay,
				DustLimit:     1,
			},
		}),
	}

	recipient := sendOORPolicyRecipient(
		t, recipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)

	_, err = NewRPCServer(server).SendOOR(
		t.Context(), &waverpc.SendOORRequest{
			Recipients: []*waverpc.Output{
				recipient,
				recipient,
			},
			DryRun: true,
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "recipient 1 duplicates recipient 0")
}

// TestSendOORRejectsCustomInputsWithMultipleRecipients verifies the daemon
// keeps custom-input sends single-recipient. Custom inputs carry per-input
// signing material for specialized spend paths, so widening them should happen
// as a separate protocol change rather than accidentally piggybacking on
// wallet-selected fanout.
func TestSendOORRejectsCustomInputsWithMultipleRecipients(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKeyA, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKeyB, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		amountSat = int64(10_000)
		exitDelay = uint32(10)
	)

	walletReady := make(chan struct{})
	close(walletReady)

	server := &Server{
		cfg:         &Config{},
		log:         btclog.Disabled,
		walletReady: walletReady,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: operatorKey.
					PubKey().
					SerializeCompressed(),
				VtxoExitDelay: exitDelay,
				DustLimit:     1,
			},
		}),
	}

	recipientA := sendOORPolicyRecipient(
		t, recipientKeyA.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)
	recipientB := sendOORPolicyRecipient(
		t, recipientKeyB.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)

	_, err = NewRPCServer(server).SendOOR(
		t.Context(), &waverpc.SendOORRequest{
			Recipients: []*waverpc.Output{
				recipientA,
				recipientB,
			},
			DryRun: true,
			CustomInputs: []*waverpc.CustomOORInput{{
				Outpoint: "00:0",
			}},
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(
		t, err, "custom inputs require exactly one recipient",
	)
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

	vtxoStore, deliveryStore, registryStore := newSendOORTestStores(t)

	firstDesc, clientKey := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x31, btcutil.Amount(amountSat),
	)

	require.NoError(t, vtxoStore.SaveVTXO(ctx, firstDesc))

	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{
			{
				selectedVTXOFromDescriptor(firstDesc),
			},
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
	](
		"send-oor-test-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "send-oor-test-wallet", testWallet,
	)

	signer := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	packageStore, reservationStore := newSendOORChildStores(t)
	oorRegistry, err := oor.NewOORRegistryActor(oor.OORRegistryConfig{
		Log:              fn.Some[btclog.Logger](btclog.Disabled),
		Signer:           signer,
		IncomingHandler:  noopOORHandler{},
		RegistryStore:    registryStore,
		DeliveryStore:    deliveryStore,
		ServerConn:       &fakeOORServerConn{},
		PackageStore:     packageStore,
		ReservationStore: reservationStore,
		ActorSystem:      system,
	})
	require.NoError(t, err)
	defer oorRegistry.Stop()

	walletReady := make(chan struct{})
	close(walletReady)

	server := &Server{
		cfg:         &Config{},
		log:         btclog.Disabled,
		walletReady: walletReady,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: operatorKey.
					PubKey().
					SerializeCompressed(),
				VtxoExitDelay: exitDelay,
				DustLimit:     1,
			},
		}),
		actorSystem:     system,
		vtxoStore:       vtxoStore,
		walletRef:       fn.Some(walletRef),
		oorSessionStore: registryStore,
	}

	rpcServer := NewRPCServer(server)
	recipient := sendOORPolicyRecipient(
		t, recipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)

	// A failed session row carrying the same key must not dedup the send:
	// the pre-flight lookup skips failed sessions, so the first call below
	// still admits a fresh session.
	failedSession := chainhash.HashH([]byte("send-oor-failed-session"))
	require.NoError(
		t,
		registryStore.UpsertSession(
			ctx, db.OORSessionRegistryRecord{
				SessionID:       failedSession,
				ActorID:         "actor-failed",
				Direction:       db.OORSessionDirectionOutgoing,
				Phase:           "failed",
				IdempotencyKey:  idempotencyKey,
				Status:          db.OORSessionStatusFailed,
				SnapshotData:    []byte{0x01},
				SnapshotVersion: 1,
			},
		),
	)

	firstResp, err := rpcServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients:     []*waverpc.Output{recipient},
		IdempotencyKey: idempotencyKey,
	})
	require.NoError(t, err)
	require.Equal(t, "submitted", firstResp.Status)
	require.NotEmpty(t, firstResp.SessionId)
	require.Equal(
		t, []string{
			firstResp.SessionId + ":0",
		},
		firstResp.RecipientOutpoints,
	)
	require.Empty(t, testWallet.unlockBatches())
	selectReqs := testWallet.selectionRequests()
	require.Len(t, selectReqs, 1)
	require.Equal(t, btcutil.Amount(amountSat), selectReqs[0].TargetAmount)
	require.Equal(t, btcutil.Amount(1), selectReqs[0].MinChangeAmount)

	secondResp, err := rpcServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients:     []*waverpc.Output{recipient},
		IdempotencyKey: idempotencyKey,
	})
	require.NoError(t, err)
	require.Equal(t, firstResp.SessionId, secondResp.SessionId)
	require.Empty(t, secondResp.RecipientOutpoints)
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

	vtxoStore, deliveryStore, registryStore := newSendOORTestStores(t)

	desc, clientKey := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x31, btcutil.Amount(amountSat),
	)

	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))

	selectedVTXO := selectedVTXOFromDescriptor(desc)
	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{
			{
				selectedVTXO,
			},
			{
				selectedVTXO,
			},
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
	](
		"send-oor-test-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "send-oor-test-wallet", testWallet,
	)

	signer := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	packageStore, reservationStore := newSendOORChildStores(t)
	oorRegistry, err := oor.NewOORRegistryActor(oor.OORRegistryConfig{
		Log:              fn.Some[btclog.Logger](btclog.Disabled),
		Signer:           signer,
		IncomingHandler:  noopOORHandler{},
		RegistryStore:    registryStore,
		DeliveryStore:    deliveryStore,
		ServerConn:       &fakeOORServerConn{},
		PackageStore:     packageStore,
		ReservationStore: reservationStore,
		ActorSystem:      system,
	})
	require.NoError(t, err)
	defer oorRegistry.Stop()

	walletReady := make(chan struct{})
	close(walletReady)

	server := &Server{
		cfg:         &Config{},
		log:         btclog.Disabled,
		walletReady: walletReady,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: operatorKey.
					PubKey().
					SerializeCompressed(),
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
		t, recipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)

	firstResp, err := rpcServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{recipient},
	})
	require.NoError(t, err)
	require.Equal(t, "submitted", firstResp.Status)
	require.NotEmpty(t, firstResp.SessionId)
	require.Equal(
		t, []string{
			firstResp.SessionId + ":0",
		},
		firstResp.RecipientOutpoints,
	)
	require.Empty(t, testWallet.unlockBatches())

	secondResp, err := rpcServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{recipient},
	})
	require.NoError(t, err)
	require.Equal(t, firstResp.SessionId, secondResp.SessionId)
	require.Equal(
		t, firstResp.RecipientOutpoints, secondResp.RecipientOutpoints,
	)
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

// TestSendOORWaitCancelDoesNotUnlockSubmittedInputs verifies that once a
// detached OOR actor Ask has been submitted, caller cancellation does not
// release wallet-selected inputs while that actor work is still in flight.
func TestSendOORWaitCancelDoesNotUnlockSubmittedInputs(t *testing.T) {
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

	vtxoStore, _, _ := newSendOORTestStores(t)

	desc, _ := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x31, btcutil.Amount(amountSat),
	)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))

	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{
			{
				selectedVTXOFromDescriptor(desc),
			},
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
	](
		"send-oor-deadline-test-wallet",
	)
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
				Pubkey: operatorKey.
					PubKey().
					SerializeCompressed(),
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
		t, recipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)

	waitCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		_, err := rpcServer.SendOOR(
			waitCtx, &waverpc.SendOORRequest{
				Recipients: []*waverpc.Output{recipient},
			},
		)
		errChan <- err
	}()

	select {
	case <-blockingActor.started:
	case err := <-errChan:
		require.NoError(t, err)
		require.FailNow(t, "SendOOR returned before actor start")

	case <-time.After(time.Second):
		require.FailNow(t, "OOR actor did not start")
	}

	cancel()
	select {
	case err = <-errChan:
		require.Equal(t, codes.Canceled, status.Code(err))

	case <-time.After(time.Second):
		require.FailNow(t, "SendOOR did not observe caller cancel")
	}
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
	promise.Complete(
		fn.Ok[oor.ActorResp](
			&oor.StartTransferResponse{
				SessionID: oor.SessionID(sessionHash),
			},
		),
	)

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

// TestSubmittedOORCleanupTimeoutReleasesSelectedVTXOs verifies that when the
// detached OOR cleanup waiter times out, the wallet-selected VTXOs are still
// unlocked. The cleanupCtx is expired by the timeout, so the unlock must run on
// a fresh context or the wallet mailbox would reject the already-expired Tell
// and leave the VTXOs pinned.
func TestSubmittedOORCleanupTimeoutReleasesSelectedVTXOs(t *testing.T) {
	t.Parallel()

	testWallet := &sendOORTestWallet{}

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
	](
		"send-oor-vtxo-unlock-test-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "send-oor-vtxo-unlock-test-wallet", testWallet,
	)

	rpcServer := &RPCServer{
		server: &Server{
			log:       btclog.Disabled,
			walletRef: fn.Some(walletRef),
		},
		customInputLocks: make(map[wire.OutPoint]struct{}),
	}

	locked := &wallet.SelectAndLockVTXOsResponse{
		SelectedVTXOs: []wallet.SelectedVTXO{
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.HashH(
						[]byte("send-oor-vtxo-unlock"),
					),
					Index: 0,
				},
				Amount: 1000,
			},
		},
	}

	// The promise is never completed, forcing the cleanup waiter down the
	// timeout branch where cleanupCtx expires.
	promise := actor.NewPromise[oor.ActorResp]()
	rpcServer.cleanupSubmittedOORStartWithTimeout(
		context.Background(), promise.Future(), locked, nil,
		10*time.Millisecond,
	)

	require.Eventually(t, func() bool {
		return len(testWallet.unlockBatches()) == 1
	}, time.Second, 10*time.Millisecond)

	batches := testWallet.unlockBatches()
	require.Len(t, batches, 1)
	require.Equal(
		t, locked.SelectedVTXOs[0].Outpoint, batches[0][0],
	)
}

func TestIsAwaitContextError(t *testing.T) {
	t.Parallel()

	deadlineCtx, cancel := context.WithTimeout(
		context.Background(), time.Nanosecond,
	)
	defer cancel()
	<-deadlineCtx.Done()

	require.True(
		t, isAwaitContextError(
			deadlineCtx, context.DeadlineExceeded,
		),
	)
	require.True(t, isAwaitContextError(
		deadlineCtx, context.Canceled,
	))
	require.False(
		t,
		isAwaitContextError(
			context.Background(), context.Canceled,
		),
	)
	require.False(
		t,
		isAwaitContextError(
			deadlineCtx, errors.New("actor failed"),
		),
	)
}

// fakeOORServerConn is a no-op serverconn ref for OOR registry tests; the
// per-session actor only needs its ID for the durable outbox target.
type fakeOORServerConn struct{}

func (f *fakeOORServerConn) ID() string { return "fake-oor-serverconn" }

func (f *fakeOORServerConn) Tell(context.Context,
	serverconn.ServerConnMsg) error {

	return nil
}

// noopOORHandler is an oor.OutboxHandler stub used to satisfy the registry
// constructor's required-dep check in tests that exercise the RPC idempotency
// pre-flight rather than the incoming receive path.
type noopOORHandler struct{}

func (noopOORHandler) Handle(context.Context, oor.SessionID, oor.OutboxEvent) (
	[]oor.Event, error) {

	return nil, nil
}

// newSendOORChildStores builds the package and reservation stores the registry
// constructor now requires. The idempotency pre-flight tests do not drive these
// paths; the stores exist only to pass construction validation.
func newSendOORChildStores(t *testing.T) (oor.PackagePersistence,
	oor.ReservationStore) {

	t.Helper()

	sqlDB := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	clk := clock.NewDefaultClock()

	return dbStore.NewOORArtifactStore(clk),
		dbStore.NewSpendingReservationStore(clk)
}

func newSendOORTestStores(t *testing.T) (*db.VTXOPersistenceStore,
	actor.DeliveryStore, *db.OORSessionRegistryStoreDB) {

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

	dbStore := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	registryStore := dbStore.NewOORSessionRegistryStore(
		clock.NewDefaultClock(),
	)

	return vtxoStore, deliveryStore, registryStore
}

func newSendOORTestVTXO(t *testing.T, operatorKey *btcec.PublicKey,
	hashByte byte,
	amount btcutil.Amount) (*vtxo.Descriptor, *btcec.PrivateKey) {

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
	}, clientKey
}

func selectedVTXOFromDescriptor(desc *vtxo.Descriptor) wallet.SelectedVTXO {
	return wallet.SelectedVTXO{
		Outpoint: desc.Outpoint,
		Amount:   desc.Amount,
		PkScript: desc.PkScript,
	}
}

func sendOORPolicyRecipient(t *testing.T,
	ownerKey, operatorKey *btcec.PublicKey, exitDelay uint32,
	amountSat int64) *waverpc.Output {

	t.Helper()

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return &waverpc.Output{
		Destination: &waverpc.Output_PolicyTemplate{
			PolicyTemplate: policyTemplate,
		},
		AmountSat:          amountSat,
		VtxoPolicyTemplate: policyTemplate,
	}
}
