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
	"github.com/btcsuite/btcd/txscript/v2"
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
	err      error
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
	err := a.err
	a.mu.Unlock()
	if err != nil {
		return fn.Err[oor.ActorResp](err)
	}

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

// TestSendOORReservesExactManagedCustomInputs verifies that outpoint-only
// custom inputs use the wallet's durable spend reservation path, select
// precisely the caller-named VTXOs, and retain ordinary recipient fanout.
func TestSendOORReservesExactManagedCustomInputs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKeyA, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKeyB, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		inputAmount     = btcutil.Amount(744)
		recipientAmount = int64(inputAmount)
		totalAmount     = 2 * inputAmount
		floor           = btcutil.Amount(700)
		exitDelay       = uint32(10)
	)

	vtxoStore, _, _ := newSendOORTestStores(t)
	input1, _ := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x61, inputAmount,
	)
	input2, _ := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x62, inputAmount,
	)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, input1))
	require.NoError(t, vtxoStore.SaveVTXO(ctx, input2))

	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{{
			selectedVTXOFromDescriptor(input2),
			selectedVTXOFromDescriptor(input1),
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
		"send-oor-exact-test-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "send-oor-exact-test-wallet", testWallet,
	)

	sessionHash := chainhash.HashH([]byte("send-oor-exact-session"))
	oorActor := &capturingSendOORActor{
		response: &oor.StartTransferResponse{
			SessionID: oor.SessionID(sessionHash),
		},
	}
	oorKey := oor.NewServiceKey()
	oorKey.Spawn(system, "send-oor-exact-test-actor", oorActor)

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
				DustLimit:     int64(floor),
			},
		}),
		actorSystem: system,
		vtxoStore:   vtxoStore,
		walletRef:   fn.Some(walletRef),
	}

	recipientA := sendOORPolicyRecipient(
		t, recipientKeyA.PubKey(), operatorKey.PubKey(), exitDelay,
		recipientAmount,
	)
	recipientB := sendOORPolicyRecipient(
		t, recipientKeyB.PubKey(), operatorKey.PubKey(), exitDelay,
		recipientAmount,
	)
	requestedOutpoints := []wire.OutPoint{
		input2.Outpoint, input1.Outpoint,
	}

	resp, err := NewRPCServer(server).SendOOR(
		ctx, &waverpc.SendOORRequest{
			Recipients: []*waverpc.Output{
				recipientA, recipientB,
			},
			CustomInputs: []*waverpc.CustomOORInput{
				{Outpoint: requestedOutpoints[0].String()},
				{Outpoint: requestedOutpoints[1].String()},
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", resp.GetStatus())
	require.Len(t, resp.GetRecipientOutpoints(), 2)

	selectReqs := testWallet.selectionRequests()
	require.Len(t, selectReqs, 1)
	require.Equal(t, totalAmount, selectReqs[0].TargetAmount)
	require.Equal(t, floor, selectReqs[0].MinChangeAmount)
	require.Equal(t, requestedOutpoints, selectReqs[0].Outpoints)

	requests := oorActor.capturedRequests()
	require.Len(t, requests, 1)
	require.Len(t, requests[0].Inputs, 2)
	require.Len(t, requests[0].Recipients, 2)
	require.Equal(
		t, requestedOutpoints[0], requests[0].Inputs[0].VTXO.Outpoint,
	)
	require.Equal(
		t, requestedOutpoints[1], requests[0].Inputs[1].VTXO.Outpoint,
	)
}

// TestClassifyCustomOORInputsRejectsMixedModes verifies a request cannot
// silently bypass durable wallet reservation by mixing a managed outpoint with
// an explicitly described custom-policy input.
func TestClassifyCustomOORInputsRejectsMixedModes(t *testing.T) {
	t.Parallel()

	op1 := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("managed-custom-input")),
		Index: 1,
	}
	op2 := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("explicit-custom-input")),
		Index: 2,
	}

	_, _, err := classifyCustomOORInputs([]*waverpc.CustomOORInput{
		{Outpoint: op1.String()},
		{
			Outpoint:  op2.String(),
			AmountSat: 1000,
		},
	})
	require.ErrorContains(t, err, "cannot mix outpoint-only managed")
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

// TestSendOORRejectsExplicitCustomInputsWithMultipleRecipients verifies the
// daemon keeps custom-spend sends single-recipient. These inputs carry
// per-input signing material, unlike outpoint-only managed exact inputs.
func TestSendOORRejectsExplicitCustomInputsWithMultipleRecipients(
	t *testing.T) {

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
	customOutpoint := wire.OutPoint{
		Hash: chainhash.HashH(
			[]byte("explicit-custom-multi-recipient"),
		),
		Index: 0,
	}

	_, err = NewRPCServer(server).SendOOR(
		t.Context(), &waverpc.SendOORRequest{
			Recipients: []*waverpc.Output{
				recipientA,
				recipientB,
			},
			DryRun: true,
			CustomInputs: []*waverpc.CustomOORInput{{
				Outpoint:  customOutpoint.String(),
				AmountSat: amountSat,
			}},
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(
		t, err, "explicit custom inputs require exactly one recipient",
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

	recipientKeyA, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKeyB, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		amountA        = int64(4000)
		amountB        = int64(6000)
		totalAmount    = amountA + amountB
		inputAmount    = totalAmount + 2000
		exitDelay      = uint32(10)
		idempotencyKey = "rpc-send-oor-idempotency-key"
	)

	vtxoStore, deliveryStore, registryStore := newSendOORTestStores(t)

	firstDesc, clientKey := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x31, btcutil.Amount(inputAmount),
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
	changeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	rpcServer.oorChangeRecipientOverride = func(_ context.Context,
		change btcutil.Amount) (oortx.RecipientOutput, error) {

		pkScript, err := txscript.PayToTaprootScript(changeKey.PubKey())
		if err != nil {
			return oortx.RecipientOutput{}, err
		}

		return oortx.RecipientOutput{
			PkScript: pkScript,
			Value:    change,
		}, nil
	}
	recipientA := sendOORPolicyRecipient(
		t, recipientKeyA.PubKey(), operatorKey.PubKey(), exitDelay,
		amountA,
	)
	recipientB := sendOORPolicyRecipient(
		t, recipientKeyB.PubKey(), operatorKey.PubKey(), exitDelay,
		amountB,
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
		Recipients:     []*waverpc.Output{recipientA, recipientB},
		IdempotencyKey: idempotencyKey,
	})
	require.NoError(t, err)
	require.Equal(t, "submitted", firstResp.Status)
	require.NotEmpty(t, firstResp.SessionId)
	require.Len(t, firstResp.RecipientOutpoints, 2)
	require.NotEqual(
		t, firstResp.RecipientOutpoints[0],
		firstResp.RecipientOutpoints[1],
	)
	require.NotContains(
		t, firstResp.RecipientOutpoints, firstResp.SessionId+":0",
	)
	require.Empty(t, testWallet.unlockBatches())
	selectReqs := testWallet.selectionRequests()
	require.Len(t, selectReqs, 1)
	require.Equal(
		t, btcutil.Amount(totalAmount), selectReqs[0].TargetAmount,
	)
	require.Equal(t, btcutil.Amount(1), selectReqs[0].MinChangeAmount)

	// A sender can ingest its own OOR change under the same session id. The
	// incoming lifecycle must advance independently without hiding the
	// outgoing recipient proof needed to prove a keyed replay.
	sessionID, err := chainhash.NewHashFromStr(firstResp.SessionId)
	require.NoError(t, err)

	outgoingRecord, err := registryStore.GetSession(ctx, *sessionID)
	require.NoError(t, err)
	attempt, err := registryStore.GetDispatchAttemptByIdempotencyKey(
		ctx, idempotencyKey,
	)
	require.NoError(t, err)
	require.NotEmpty(t, attempt.RequestData)
	proofRecipients, err := oor.OutgoingReplayRecipients(
		attempt.RequestData,
	)
	require.NoError(t, err)
	require.Len(t, proofRecipients, 2)
	for _, recipient := range proofRecipients {
		require.Contains(
			t, firstResp.RecipientOutpoints, fmt.Sprintf("%s:%d",
				firstResp.SessionId, recipient.OutputIndex),
		)
	}

	// The real terminal outgoing bridge intentionally emits a minimal
	// snapshot with no Ark PSBT. Model that persisted update and verify the
	// database keeps the earlier artifact-bearing proof while status
	// advances.
	require.NoError(
		t,
		registryStore.UpsertSession(
			ctx, db.OORSessionRegistryRecord{
				SessionID:       *sessionID,
				ActorID:         outgoingRecord.ActorID,
				Direction:       db.OORSessionDirectionOutgoing,
				Phase:           "completed",
				IdempotencyKey:  idempotencyKey,
				Status:          db.OORSessionStatusCompleted,
				SnapshotData:    []byte{0xff},
				SnapshotVersion: 5,
				FlowVersion:     outgoingRecord.FlowVersion,
				CreatedAt:       outgoingRecord.CreatedAt,
			},
		),
	)

	terminalRecord, err := registryStore.GetSession(ctx, *sessionID)
	require.NoError(t, err)
	require.Equal(t, []byte{0xff}, terminalRecord.SnapshotData)
	terminalAttempt, err :=
		registryStore.GetDispatchAttemptByIdempotencyKey(
			ctx, idempotencyKey,
		)
	require.NoError(t, err)
	require.Equal(t, attempt.RequestData, terminalAttempt.RequestData)

	require.NoError(
		t,
		registryStore.UpsertSession(
			ctx, db.OORSessionRegistryRecord{
				SessionID:       *sessionID,
				ActorID:         outgoingRecord.ActorID,
				Direction:       db.OORSessionDirectionIncoming,
				Phase:           "completed",
				Status:          db.OORSessionStatusCompleted,
				SnapshotData:    []byte{0x02},
				SnapshotVersion: 1,
				FlowVersion:     outgoingRecord.FlowVersion,
				CreatedAt:       outgoingRecord.CreatedAt,
			},
		),
	)

	// Model response loss followed by process restart. A fresh RPC server
	// reads only the durable attempt. Reordering the same recipient
	// multiset must return the same outpoints in the caller's new order
	// without a second wallet selection or transport send.
	restartedRPCServer := NewRPCServer(server)
	secondResp, err := restartedRPCServer.SendOOR(
		ctx, &waverpc.SendOORRequest{
			Recipients: []*waverpc.Output{
				recipientB, recipientA,
			},
			IdempotencyKey: idempotencyKey,
			AdmissionDeadlineUnixNanos: time.Now().
				Add(-time.Second).
				UnixNano(),
			ExistingOnly: true,
		},
	)
	require.NoError(t, err)
	require.Equal(t, firstResp.SessionId, secondResp.SessionId)
	require.Equal(
		t, []string{
			firstResp.RecipientOutpoints[1],
			firstResp.RecipientOutpoints[0],
		}, secondResp.RecipientOutpoints,
	)
	require.Equal(t, 1, testWallet.selectCount())
	require.Empty(t, testWallet.unlockBatches())

	amountMismatch := sendOORPolicyRecipient(
		t, recipientKeyA.PubKey(), operatorKey.PubKey(), exitDelay,
		amountA+1,
	)
	_, err = restartedRPCServer.SendOOR(
		ctx, &waverpc.SendOORRequest{
			Recipients: []*waverpc.Output{
				amountMismatch, recipientB,
			},
			IdempotencyKey: idempotencyKey,
			ExistingOnly:   true,
		},
	)
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	otherRecipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	scriptMismatch := sendOORPolicyRecipient(
		t, otherRecipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
		amountA,
	)
	_, err = restartedRPCServer.SendOOR(
		ctx, &waverpc.SendOORRequest{
			Recipients: []*waverpc.Output{
				scriptMismatch, recipientB,
			},
			IdempotencyKey: idempotencyKey,
			ExistingOnly:   true,
		},
	)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.Equal(t, 1, testWallet.selectCount())

	_, err = restartedRPCServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients:     []*waverpc.Output{recipientA},
		IdempotencyKey: "missing-existing-only-key",
		ExistingOnly:   true,
	})
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Equal(t, 1, testWallet.selectCount())

	_, err = restartedRPCServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients:     []*waverpc.Output{recipientA},
		IdempotencyKey: "expired-admission-key",
		AdmissionDeadlineUnixNanos: time.Now().
			Add(-time.Second).
			UnixNano(),
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, 1, testWallet.selectCount())
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
		t, operatorKey.PubKey(), 0x30, btcutil.Amount(amountSat),
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
		"send-oor-existing-session-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "send-oor-existing-session-wallet", testWallet,
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

// TestSendOORReturnsPendingKeyedSessionBeforeAttemptCommit verifies a same-key
// retry can receive the in-memory admission winner before the child's first
// transaction makes its immutable attempt visible. The RPC returns the stable
// session handle with no unproven outpoints and releases its fresh selection.
func TestSendOORReturnsPendingKeyedSessionBeforeAttemptCommit(t *testing.T) {
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

	vtxoStore, _, registryStore := newSendOORTestStores(t)
	desc, _ := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x35, btcutil.Amount(amountSat),
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
		"send-oor-pending-key-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "send-oor-pending-key-wallet", testWallet,
	)

	sessionHash := chainhash.HashH([]byte("pending-keyed-oor-session"))
	oorActor := &capturingSendOORActor{
		response: &oor.StartTransferResponse{
			SessionID: oor.SessionID(sessionHash),
			Existing:  true,
		},
	}
	oor.NewServiceKey().Spawn(
		system, "send-oor-pending-key-actor", oorActor,
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
		actorSystem:     system,
		vtxoStore:       vtxoStore,
		oorSessionStore: registryStore,
		walletRef:       fn.Some(walletRef),
	}

	recipient := sendOORPolicyRecipient(
		t, recipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)
	resp, err := NewRPCServer(server).SendOOR(
		ctx, &waverpc.SendOORRequest{
			Recipients:     []*waverpc.Output{recipient},
			IdempotencyKey: "pending-key",
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", resp.Status)
	require.Equal(t, sessionHash.String(), resp.SessionId)
	require.Empty(t, resp.RecipientOutpoints)
	require.Equal(t, 1, testWallet.selectCount())
	require.Eventually(t, func() bool {
		batches := testWallet.unlockBatches()

		return len(batches) == 1 && len(batches[0]) == 1 &&
			batches[0][0] == desc.Outpoint
	}, 5*time.Second, 50*time.Millisecond)
}

// TestSendOORRejectsDifferentKeyForExistingSession verifies the daemon never
// reports success under a caller key that an existing deterministic session
// does not retain, and releases the inputs selected for the rejected attempt.
func TestSendOORRejectsDifferentKeyForExistingSession(t *testing.T) {
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
		actorSystem:     system,
		vtxoStore:       vtxoStore,
		oorSessionStore: registryStore,
		walletRef:       fn.Some(walletRef),
	}

	rpcServer := NewRPCServer(server)
	recipient := sendOORPolicyRecipient(
		t, recipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)

	firstResp, err := rpcServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients:     []*waverpc.Output{recipient},
		IdempotencyKey: "first-caller-key",
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
		Recipients:     []*waverpc.Output{recipient},
		IdempotencyKey: "second-caller-key",
	})
	require.Nil(t, secondResp)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.Equal(t, 2, testWallet.selectCount())

	require.Eventually(t, func() bool {
		batches := testWallet.unlockBatches()
		if len(batches) != 1 {
			return false
		}

		return len(batches[0]) == 1 &&
			batches[0][0] == desc.Outpoint
	}, 5*time.Second, 50*time.Millisecond)

	firstRecord, err := registryStore.GetDispatchAttemptByIdempotencyKey(
		ctx, "first-caller-key",
	)
	require.NoError(t, err)
	require.Equal(
		t, firstResp.SessionId,
		oor.SessionID(firstRecord.SessionID).String(),
	)

	_, err = registryStore.GetDispatchAttemptByIdempotencyKey(
		ctx, "second-caller-key",
	)
	require.ErrorIs(t, err, db.ErrOORDispatchAttemptNotFound)
}

// TestSendOORRejectsNewKeyForDispatchedFailedSession verifies a terminal
// lifecycle cannot release the immutable dispatch identity and admit the same
// deterministic operation under a second key.
func TestSendOORRejectsNewKeyForDispatchedFailedSession(t *testing.T) {
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
		t, operatorKey.PubKey(), 0x32, btcutil.Amount(amountSat),
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
		"send-oor-failed-retry-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "send-oor-failed-retry-wallet", testWallet,
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
		actorSystem:     system,
		vtxoStore:       vtxoStore,
		oorSessionStore: registryStore,
		walletRef:       fn.Some(walletRef),
	}

	rpcServer := NewRPCServer(server)
	recipient := sendOORPolicyRecipient(
		t, recipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
		amountSat,
	)

	terms, err := server.fetchOperatorTerms(ctx)
	require.NoError(t, err)
	recipients, err := rpcServer.buildSendOORRecipients(
		ctx, []*waverpc.Output{recipient}, terms,
	)
	require.NoError(t, err)
	inputs, err := BuildTransferInputs(
		ctx, vtxoStore, []wire.OutPoint{desc.Outpoint},
	)
	require.NoError(t, err)

	failedSession, _, err := oor.NewSessionWithIdempotencyKey(
		ctx, arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(),
			CSVDelay:    exitDelay,
		}, inputs, recipients, "key-1", oor.EnvConfig{},
	)
	require.NoError(t, err)
	failedSession.FSM.Stop()

	failedID := chainhash.Hash(failedSession.ID)
	require.NoError(
		t,
		registryStore.UpsertSession(
			ctx, db.OORSessionRegistryRecord{
				SessionID: failedID,
				ActorID: oor.ActorIDForSession(
					failedSession.ID,
				),
				Direction:       db.OORSessionDirectionOutgoing,
				Phase:           "failed",
				IdempotencyKey:  "key-1",
				Status:          db.OORSessionStatusFailed,
				LastError:       "operator rejected",
				SnapshotData:    []byte{0xff},
				SnapshotVersion: 5,
				DispatchRequestData: []byte{
					0xee,
				},
			},
		),
	)

	packageStore, reservationStore := newSendOORChildStores(t)
	oorRegistry, err := oor.NewOORRegistryActor(
		oor.OORRegistryConfig{
			Log: fn.Some[btclog.Logger](
				btclog.Disabled,
			),
			Signer: input.NewMockSigner(
				[]*btcec.PrivateKey{clientKey}, nil,
			),
			IncomingHandler:  noopOORHandler{},
			RegistryStore:    registryStore,
			DeliveryStore:    deliveryStore,
			ServerConn:       &fakeOORServerConn{},
			PackageStore:     packageStore,
			ReservationStore: reservationStore,
			ActorSystem:      system,
		},
	)
	require.NoError(t, err)
	defer oorRegistry.Stop()

	failedReplay, err := rpcServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients:     []*waverpc.Output{recipient},
		IdempotencyKey: "key-1",
	})
	require.NoError(t, err)
	require.Equal(t, "failed", failedReplay.Status)
	require.Equal(t, failedSession.ID.String(), failedReplay.SessionId)
	require.Empty(t, failedReplay.RecipientOutpoints)
	require.Equal(t, 0, testWallet.selectCount())

	resp, err := rpcServer.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients:     []*waverpc.Output{recipient},
		IdempotencyKey: "key-2",
	})
	require.Nil(t, resp)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.Equal(t, 1, testWallet.selectCount())
	require.Eventually(t, func() bool {
		return len(testWallet.unlockBatches()) == 1
	}, 5*time.Second, 50*time.Millisecond)

	firstAttempt, err := registryStore.GetDispatchAttemptByIdempotencyKey(
		ctx, "key-1",
	)
	require.NoError(t, err)
	require.Equal(t, failedID, firstAttempt.SessionID)

	_, err = registryStore.GetDispatchAttemptByIdempotencyKey(ctx, "key-2")
	require.ErrorIs(t, err, db.ErrOORDispatchAttemptNotFound)
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

// TryTell delegates to Tell, which is all this double needs: no test
// drives the non-blocking path through it.
func (f *fakeOORServerConn) TryTell(ctx context.Context,
	msg serverconn.ServerConnMsg) error {

	return f.Tell(ctx, msg)
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
