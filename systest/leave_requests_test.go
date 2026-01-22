//go:build systest

package systest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/taproot-assets/proof"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestLeaveRequestCreatesWalletUTXO verifies that leave requests are included
// in the commitment transaction and that the wallet detects the on-chain
// output after broadcast.
func TestLeaveRequestCreatesWalletUTXO(t *testing.T) {
	ParallelN(t)

	f := NewBoardingWalletFixture(t)
	ctx := f.Context()

	rpc, err := f.Harness.Harness.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(rpc.Shutdown)

	leaveAddrResp := f.CreateBoardingAddress(144)
	leaveAddrs := f.GetActiveAddresses()
	leaveAddr := leaveAddrs[leaveAddrResp.Address.String()]
	require.NotNil(t, leaveAddr)

	leavePkScript, err := txscript.PayToAddrScript(
		leaveAddr.Address,
	)
	require.NoError(t, err)

	leaveAmount := btcutil.Amount(100_000)
	fee := btcutil.Amount(1_000)
	minChange := btcutil.Amount(1_000)

	utxo, inputAmount := pickSpendableUtxo(
		t, rpc, leaveAmount+fee+minChange,
	)
	outpoint, inputPkScript := utxoOutpoint(
		t, utxo,
	)
	prevTx := fetchPrevTx(t, rpc, outpoint.Hash)

	confHeight, confHash := currentBlockInfo(t, rpc)

	intent := &wallet.BoardingIntent{
		Address:  *leaveAddr,
		Outpoint: outpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: confHeight,
			ConfHash:   confHash,
			ConfTx:     prevTx.MsgTx(),
			OutPoint:   outpoint,
			Amount:     inputAmount,
			TxProof:    fn.None[proof.TxProof](),
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	server := newLeaveServerConn(
		t,
		rpc,
		&wire.TxOut{
			Value:    int64(leaveAmount),
			PkScript: leavePkScript,
		},
		outpoint,
		inputAmount,
		inputPkScript,
		fee,
	)
	serverRef := registerServerConn(t, f.Harness, server)

	operatorKey := GenerateOperatorKey(t)
	terms := &types.OperatorTerms{
		PubKey:            operatorKey,
		BoardingExitDelay: 144,
		VTXOExitDelay:     144,
		ForfeitScript:     []byte{txscript.OP_TRUE},
		SweepKey:          operatorKey,
		SweepDelay:        288,
		DustLimit:         btcutil.Amount(546),
		MinBoardingAmount: btcutil.Amount(1),
		FeeRate:           btcutil.Amount(1),
		MinConfirmations:  1,
	}

	roundActor, roundRef := newRoundClientActor(
		t, f, serverRef, terms, inputAmount,
	)
	server.setRoundRef(roundRef)
	require.NoError(t, roundActor.Start(ctx))

	sendWalletBoardingConfirmed(t, ctx, roundRef, intent)

	sendLeaveRequest(
		t, ctx, roundRef,
		&wire.TxOut{
			Value:    int64(leaveAmount),
			PkScript: leavePkScript,
		},
	)

	sendRegistrationRequested(t, ctx, roundRef)

	waitForServerBroadcast(t, server)

	f.Harness.Harness.Generate(1)
	f.Harness.WaitForLNDSync()

	require.Eventually(t, func() bool {
		balance := f.GetBalance()
		if balance.UtxoCount != 1 {
			return false
		}
		return balance.TotalBalance == leaveAmount
	}, 60*time.Second, 500*time.Millisecond)

	assertRoundNotFailed(t, ctx, roundRef)
}

// stubRoundStore implements round.RoundStore with no-op behavior for systests.
type stubRoundStore struct{}

// CommitState is a no-op for the stub round store.
func (s *stubRoundStore) CommitState(_ context.Context,
	_ *round.Round, _ round.ClientState) error {

	return nil
}

// FetchState returns an error since the stub does not persist rounds.
func (s *stubRoundStore) FetchState(_ context.Context,
	_ round.RoundID) (*round.Round, round.ClientState, error) {

	return nil, nil, fmt.Errorf("round state not found")
}

// LookupRoundByCommitmentTx returns an error since the stub is empty.
func (s *stubRoundStore) LookupRoundByCommitmentTx(
	_ context.Context, _ chainhash.Hash) (*round.Round, error) {

	return nil, fmt.Errorf("round not found by commitment tx")
}

// ListActiveRounds returns no active rounds for the stub store.
func (s *stubRoundStore) ListActiveRounds(
	_ context.Context) ([]*round.Round, error) {

	return nil, nil
}

// FinalizeRound is a no-op for the stub round store.
func (s *stubRoundStore) FinalizeRound(_ context.Context,
	_ round.RoundID, _ chainhash.Hash, _ round.ConfInfo) error {

	return nil
}

// stubVTXOStore implements round.VTXOStore with no-op behavior for systests.
type stubVTXOStore struct{}

// SaveVTXOs is a no-op for the stub VTXO store.
func (s *stubVTXOStore) SaveVTXOs(
	_ context.Context, _ []*round.ClientVTXO) error {

	return nil
}

// ListVTXOs returns an empty list for the stub VTXO store.
func (s *stubVTXOStore) ListVTXOs(
	_ context.Context) ([]*round.ClientVTXO, error) {

	return nil, nil
}

// GetVTXO returns an error since the stub does not persist VTXOs.
func (s *stubVTXOStore) GetVTXO(
	_ context.Context, _ wire.OutPoint) (*round.ClientVTXO, error) {

	return nil, fmt.Errorf("VTXO not found")
}

// MarkVTXOSpent is a no-op for the stub VTXO store.
func (s *stubVTXOStore) MarkVTXOSpent(
	_ context.Context, _ wire.OutPoint) error {

	return nil
}

// stubClientWallet is a minimal ClientWallet implementation for tests that
// never reach signing operations.
type stubClientWallet struct{}

var errUnsupported = fmt.Errorf("signing not supported in systest stub")

// MuSig2CreateSession returns an error for the stub wallet.
func (s *stubClientWallet) MuSig2CreateSession(_ input.MuSig2Version,
	_ keychain.KeyLocator, _ []*btcec.PublicKey, _ *input.MuSig2Tweaks,
	_ [][musig2.PubNonceSize]byte, _ *musig2.Nonces,
) (*input.MuSig2SessionInfo, error) {

	return nil, errUnsupported
}

// MuSig2RegisterNonces returns an error for the stub wallet.
func (s *stubClientWallet) MuSig2RegisterNonces(_ input.MuSig2SessionID,
	_ [][musig2.PubNonceSize]byte) (bool, error) {

	return false, errUnsupported
}

// MuSig2RegisterCombinedNonce returns an error for the stub wallet.
func (s *stubClientWallet) MuSig2RegisterCombinedNonce(
	_ input.MuSig2SessionID, _ [musig2.PubNonceSize]byte) error {

	return errUnsupported
}

// MuSig2GetCombinedNonce returns an error for the stub wallet.
func (s *stubClientWallet) MuSig2GetCombinedNonce(
	_ input.MuSig2SessionID) ([musig2.PubNonceSize]byte, error) {

	return [musig2.PubNonceSize]byte{}, errUnsupported
}

// MuSig2Sign returns an error for the stub wallet.
func (s *stubClientWallet) MuSig2Sign(_ input.MuSig2SessionID,
	_ [sha256.Size]byte, _ bool) (*musig2.PartialSignature, error) {

	return nil, errUnsupported
}

// MuSig2CombineSig returns an error for the stub wallet.
func (s *stubClientWallet) MuSig2CombineSig(_ input.MuSig2SessionID,
	_ []*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	return nil, false, errUnsupported
}

// MuSig2Cleanup returns an error for the stub wallet.
func (s *stubClientWallet) MuSig2Cleanup(
	_ input.MuSig2SessionID) error {

	return errUnsupported
}

// SignOutputRaw returns an error for the stub wallet.
func (s *stubClientWallet) SignOutputRaw(
	_ *wire.MsgTx, _ *input.SignDescriptor) (input.Signature, error) {

	return nil, errUnsupported
}

// ComputeInputScript returns an error for the stub wallet.
func (s *stubClientWallet) ComputeInputScript(_ *wire.MsgTx,
	_ *input.SignDescriptor) (*input.Script, error) {

	return nil, errUnsupported
}

// DeriveNextKey returns an error for the stub wallet.
func (s *stubClientWallet) DeriveNextKey(_ context.Context,
	_ keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	return nil, errUnsupported
}

// leaveServerConn is a fake server that builds and broadcasts a commitment
// transaction containing the requested leave output.
type leaveServerConn struct {
	t *testing.T

	rpc *rpcclient.Client

	roundRef actor.TellOnlyRef[round.ClientMsg]

	leaveOutput   *wire.TxOut
	inputOutpoint wire.OutPoint
	inputValue    btcutil.Amount
	inputPkScript []byte
	fee           btcutil.Amount

	broadcasted chan struct{}
	errChan     chan error
	roundID     fn.Option[round.RoundID]
}

// newLeaveServerConn returns a configured fake server connection.
func newLeaveServerConn(t *testing.T, rpc *rpcclient.Client,
	leaveOutput *wire.TxOut, inputOutpoint wire.OutPoint,
	inputValue btcutil.Amount, inputPkScript []byte,
	fee btcutil.Amount) *leaveServerConn {

	return &leaveServerConn{
		t:             t,
		rpc:           rpc,
		leaveOutput:   leaveOutput,
		inputOutpoint: inputOutpoint,
		inputValue:    inputValue,
		inputPkScript: inputPkScript,
		fee:           fee,
		broadcasted:   make(chan struct{}, 1),
		errChan:       make(chan error, 1),
		roundID:       fn.None[round.RoundID](),
	}
}

// Receive handles outbound client messages and emulates server responses.
func (s *leaveServerConn) Receive(ctx context.Context,
	msg serverconn.ServerConnMsg) fn.Result[serverconn.ServerConnResp] {

	req, ok := msg.(*serverconn.SendClientEventRequest)
	if !ok {
		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{Success: true},
		)
	}

	joinReq, ok := req.Message.(*round.JoinRoundRequest)
	if !ok {
		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{Success: true},
		)
	}

	if len(joinReq.LeaveRequests) != 1 {
		s.errChan <- fmt.Errorf("expected 1 leave request, got %d",
			len(joinReq.LeaveRequests))
		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{Success: false},
		)
	}

	leaveReq := joinReq.LeaveRequests[0]
	if leaveReq.Output == nil {
		s.errChan <- fmt.Errorf("leave request output is nil")
		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{Success: false},
		)
	}
	if leaveReq.Output.Value != s.leaveOutput.Value {
		s.errChan <- fmt.Errorf("leave output amount mismatch")
		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{Success: false},
		)
	}
	if !bytes.Equal(leaveReq.Output.PkScript, s.leaveOutput.PkScript) {
		s.errChan <- fmt.Errorf("leave output script mismatch")
		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{Success: false},
		)
	}

	commitTx, err := s.buildCommitmentTx()
	if err != nil {
		s.errChan <- fmt.Errorf("build commitment tx: %w", err)
		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{Success: false},
		)
	}

	packet, err := psbt.NewFromUnsignedTx(commitTx)
	if err != nil {
		s.errChan <- fmt.Errorf("create psbt: %w", err)
		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{Success: false},
		)
	}
	packet.Inputs = []psbt.PInput{{
		WitnessUtxo: &wire.TxOut{
			Value:    int64(s.inputValue),
			PkScript: s.inputPkScript,
		},
	}}

	roundID := s.roundID.UnwrapOr(round.RoundID(uuid.New()))
	s.roundID = fn.Some(roundID)

	s.roundRef.Tell(ctx, &round.ServerMessageNotification{
		Message: &round.RoundJoined{
			RoundID: roundID,
			AcceptedBoardingOutpoints: []wire.OutPoint{
				s.inputOutpoint,
			},
		},
	})

	s.roundRef.Tell(ctx, &round.ServerMessageNotification{
		Message: &round.CommitmentTxBuilt{
			RoundID:       roundID,
			Tx:            packet,
			VTXOTreePaths: map[int]*tree.Tree{},
		},
	})

	if err := s.broadcastCommitmentTx(commitTx); err != nil {
		s.errChan <- fmt.Errorf("broadcast commitment: %w", err)
		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{Success: false},
		)
	}

	select {
	case s.broadcasted <- struct{}{}:
	default:
	}

	return fn.Ok[serverconn.ServerConnResp](
		&serverconn.SendClientEventResponse{Success: true},
	)
}

// setRoundRef stores the round actor reference for server callbacks.
func (s *leaveServerConn) setRoundRef(
	roundRef actor.TellOnlyRef[round.ClientMsg]) {

	s.roundRef = roundRef
}

// buildCommitmentTx constructs a commitment transaction spending the input
// outpoint and paying the leave output plus change.
func (s *leaveServerConn) buildCommitmentTx() (*wire.MsgTx, error) {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: s.inputOutpoint,
	})

	leaveValue := btcutil.Amount(s.leaveOutput.Value)
	changeValue := s.inputValue - leaveValue - s.fee
	if changeValue < 0 {
		return nil, fmt.Errorf("insufficient input for leave output")
	}

	tx.AddTxOut(s.leaveOutput)

	if changeValue > 0 {
		changeAddr, err := s.rpc.GetNewAddress("")
		if err != nil {
			return nil, fmt.Errorf("get change address: %w", err)
		}

		changeScript, err := txscript.PayToAddrScript(changeAddr)
		if err != nil {
			return nil, fmt.Errorf("build change script: %w", err)
		}

		tx.AddTxOut(&wire.TxOut{
			Value:    int64(changeValue),
			PkScript: changeScript,
		})
	}

	return tx, nil
}

// broadcastCommitmentTx signs and broadcasts the commitment transaction.
func (s *leaveServerConn) broadcastCommitmentTx(tx *wire.MsgTx) error {
	signedTx, complete, err := s.rpc.SignRawTransactionWithWallet(tx)
	if err != nil {
		return fmt.Errorf("sign raw transaction: %w", err)
	}
	if !complete {
		return fmt.Errorf("sign raw transaction incomplete")
	}

	_, err = s.rpc.SendRawTransaction(signedTx, false)
	if err != nil {
		return fmt.Errorf("send raw transaction: %w", err)
	}

	return nil
}

// waitForServerBroadcast waits for the fake server to broadcast a commitment
// transaction.
func waitForServerBroadcast(t *testing.T, server *leaveServerConn) {
	t.Helper()

	select {
	case err := <-server.errChan:
		require.NoError(t, err)
	case <-server.broadcasted:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for commitment broadcast")
	}
}

// registerServerConn registers the fake server with the actor system.
func registerServerConn(t *testing.T, h *SysTestHarness,
	server *leaveServerConn) actor.ActorRef[
	serverconn.ServerConnMsg, serverconn.ServerConnResp] {

	t.Helper()

	name := fmt.Sprintf("leave-server-%s",
		strings.ReplaceAll(t.Name(), "/", "-"))
	key := actor.NewServiceKey[
		serverconn.ServerConnMsg, serverconn.ServerConnResp,
	](name)

	return actor.RegisterWithSystem(h.ActorSystem(), name, key, server)
}

// newRoundClientActor creates and registers a round client actor.
func newRoundClientActor(t *testing.T, f *BoardingWalletFixture,
	serverRef actor.TellOnlyRef[serverconn.ServerConnMsg],
	terms *types.OperatorTerms, maxOperatorFee btcutil.Amount,
) (*round.RoundClientActor,
	actor.ActorRef[round.ClientMsg, round.ClientResp]) {

	t.Helper()

	cfg := &round.RoundClientConfig{
		Name:           "leave-round-client",
		Logger:         f.Harness.SubLogger(round.Subsystem),
		Wallet:         &stubClientWallet{},
		RoundStore:     &stubRoundStore{},
		VTXOStore:      &stubVTXOStore{},
		OperatorTerms:  terms,
		ServerConn:     serverRef,
		ChainSource:    f.ChainSource,
		WalletActor:    f.Wallet,
		ChainParams:    f.Harness.ChainParams(),
		MaxOperatorFee: maxOperatorFee,
		ActorSystem:    f.Harness.ActorSystem(),
	}

	actorResult := round.NewRoundClientActor(cfg)
	actorVal, err := actorResult.Unpack()
	require.NoError(t, err)

	name := fmt.Sprintf("round-client-%s",
		strings.ReplaceAll(t.Name(), "/", "-"))
	key := actor.NewServiceKey[
		round.ClientMsg, round.ClientResp,
	](name)
	roundRef := actor.RegisterWithSystem(
		f.Harness.ActorSystem(), name, key, actorVal,
	)
	cfg.SelfRef = roundRef

	return actorVal, roundRef
}

// sendWalletBoardingConfirmed sends a boarding confirmation into the round
// actor to seed the round inputs.
func sendWalletBoardingConfirmed(t *testing.T, ctx context.Context,
	roundRef actor.ActorRef[round.ClientMsg, round.ClientResp],
	intent *wallet.BoardingIntent,
) {

	t.Helper()

	msg := &round.WalletBoardingConfirmed{
		Intent: intent,
	}
	resp := roundRef.Ask(ctx, msg).Await(ctx)
	require.True(t, resp.IsOk())
}

// sendLeaveRequest submits a leave output to the round actor.
func sendLeaveRequest(t *testing.T, ctx context.Context,
	roundRef actor.ActorRef[round.ClientMsg, round.ClientResp],
	output *wire.TxOut,
) {

	t.Helper()

	msg := &round.RegisterLeaveRequestsRequest{
		Outputs: []*wire.TxOut{output},
	}
	resp := roundRef.Ask(ctx, msg).Await(ctx)
	require.True(t, resp.IsOk())
}

// sendRegistrationRequested prompts the round actor to emit a join request.
func sendRegistrationRequested(t *testing.T, ctx context.Context,
	roundRef actor.ActorRef[round.ClientMsg, round.ClientResp],
) {

	t.Helper()

	msg := &round.ServerMessageNotification{
		Message: &round.RegistrationRequested{},
	}
	resp := roundRef.Ask(ctx, msg).Await(ctx)
	require.True(t, resp.IsOk())
}

// assertRoundNotFailed verifies no round is in ClientFailedState.
func assertRoundNotFailed(t *testing.T, ctx context.Context,
	roundRef actor.ActorRef[round.ClientMsg, round.ClientResp],
) {

	t.Helper()

	resp := roundRef.Ask(
		ctx, &round.GetClientStateRequest{},
	).Await(ctx)
	require.True(t, resp.IsOk())

	respVal, err := resp.Unpack()
	require.NoError(t, err)
	stateResp, ok := respVal.(*round.GetClientStateResponse)
	require.True(t, ok)

	for _, info := range stateResp.States {
		switch info.State.(type) {
		case *round.ClientFailedState:
			t.Fatal("round entered failed state")
		}
	}
}

// pickSpendableUtxo selects a bitcoind wallet UTXO with sufficient value.
func pickSpendableUtxo(t *testing.T, rpc *rpcclient.Client,
	minAmount btcutil.Amount) (*btcjson.ListUnspentResult,
	btcutil.Amount) {

	t.Helper()

	utxos, err := rpc.ListUnspent()
	require.NoError(t, err)

	for i := range utxos {
		amount := btcutil.Amount(
			utxos[i].Amount * btcutil.SatoshiPerBitcoin,
		)
		if amount >= minAmount {
			return &utxos[i], amount
		}
	}

	t.Fatal("no spendable UTXO available")

	return nil, 0
}

// utxoOutpoint builds a wire.OutPoint and pkScript from a listunspent entry.
func utxoOutpoint(t *testing.T, utxo *btcjson.ListUnspentResult,
) (wire.OutPoint, []byte) {

	t.Helper()

	hash, err := chainhash.NewHashFromStr(utxo.TxID)
	require.NoError(t, err)

	pkScript, err := hex.DecodeString(utxo.ScriptPubKey)
	require.NoError(t, err)

	return wire.OutPoint{
		Hash:  *hash,
		Index: utxo.Vout,
	}, pkScript
}

// fetchPrevTx fetches the previous transaction for the given hash.
func fetchPrevTx(t *testing.T, rpc *rpcclient.Client,
	hash chainhash.Hash) *btcutil.Tx {

	t.Helper()

	tx, err := rpc.GetRawTransaction(&hash)
	require.NoError(t, err)

	return tx
}

// currentBlockInfo returns the latest block height and hash.
func currentBlockInfo(t *testing.T,
	rpc *rpcclient.Client) (int32, chainhash.Hash) {

	t.Helper()

	height, err := rpc.GetBlockCount()
	require.NoError(t, err)

	hash, err := rpc.GetBlockHash(height)
	require.NoError(t, err)

	return int32(height), *hash
}
