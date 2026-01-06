package rounds

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// commonMockSetup contains mocks and static test data shared between FSM
// and actor tests. This reduces code duplication. Only roundID is kept
// separate as it changes across actor tests.
type commonMockSetup struct {
	t *testing.T

	// Operator keys for test identities (static across tests).
	operatorPub    *btcec.PublicKey
	operatorSigner input.Signer

	// Mocks (testify/mock.Mock based).
	boardingLocker   *mockBoardingInputLocker
	chainSource      *mockChainSource
	feeEstimator     *chainfee.MockEstimator
	walletController *mockWalletController
	roundStore       *mockRoundStore
}

// newCommonMockSetup creates a new common mock setup with default
// configuration and deterministic operator keys.
func newCommonMockSetup(t *testing.T) *commonMockSetup {
	t.Helper()

	// Generate deterministic test keys.
	operatorPub, operatorSigner := testutils.CreateKey(1)

	// Create mocks without default expectations.
	mockLocker := &mockBoardingInputLocker{}
	mockChainSrc := &mockChainSource{}
	mockFeeEstimator := &chainfee.MockEstimator{}
	mockWalletController := newMockWalletController(operatorSigner)
	mockRoundStore := &mockRoundStore{}

	m := &commonMockSetup{
		t:                t,
		operatorPub:      operatorPub,
		operatorSigner:   operatorSigner,
		boardingLocker:   mockLocker,
		chainSource:      mockChainSrc,
		feeEstimator:     mockFeeEstimator,
		walletController: mockWalletController,
		roundStore:       mockRoundStore,
	}

	// Register cleanup to automatically assert mock expectations.
	t.Cleanup(func() {
		m.assertMockExpectations()
	})

	return m
}

// fsmTestHarness is the central test harness housing all common setup,
// mocks, fixtures, and helper functions for round FSM tests.
type fsmTestHarness struct {
	*testing.T
	*commonMockSetup

	// roundID for the FSM under test.
	roundID RoundID

	// Environment for FSM.
	env *Environment

	// fsm is the state machine instance under test.
	fsm *StateMachine

	// outboxMessages accumulates outbox events from the last sendEvent
	// call.
	outboxMessages []OutboxEvent
}

// newTestHarness creates a new test harness with default configuration.
// It initializes and starts a new state machine for testing.
func newTestHarness(t *testing.T) *fsmTestHarness {
	t.Helper()

	roundID, err := NewRoundID()
	require.NoError(t, err)

	// Create common mock setup.
	common := newCommonMockSetup(t)

	operatorKey := keychain.KeyDescriptor{
		PubKey: common.operatorPub,
	}

	env := Environment{
		RoundID:             roundID,
		ChainParams:         &chaincfg.RegressionNetParams,
		BoardingInputLocker: common.boardingLocker,
		ChainSource:         common.chainSource,
		FeeEstimator:        common.feeEstimator,
		Log:                 btclog.Disabled,
		WalletController:    common.walletController,
		RoundStore:          common.roundStore,
		ConfTarget:          6,
		MinConfs:            1,
		Terms: &batch.Terms{
			OperatorKey:                   operatorKey,
			BoardingExitDelay:             100,
			BoardingExitDelaySafetyMargin: 6,
			MinBoardingConfirmations:      1,
			MaxVTXOsPerTree:               1024,
			SignatureCollectionTimeout:    30 * time.Second,
			TreeRadix:                     4,
		},
	}

	fsmCfg := StateMachineCfg{
		InitialState: &CreatedState{},
		Env:          &env,
		Logger:       btclog.Disabled,
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(t.Context())

	h := &fsmTestHarness{
		T:               t,
		commonMockSetup: common,
		roundID:         roundID,
		env:             &env,
		fsm:             &fsm,
		outboxMessages:  make([]OutboxEvent, 0),
	}

	return h
}

// setupPermissiveMocks sets up permissive `.Maybe()` expectations on all mocks.
// This is useful for tests that don't need precise mock control.
func (c *commonMockSetup) setupPermissiveMocks() {
	c.t.Helper()

	// Set up permissive boarding locker expectations.
	c.boardingLocker.On("Lock", mock.Anything, mock.Anything,
		mock.Anything).Return(nil).Maybe()
	c.boardingLocker.On("Unlock", mock.Anything, mock.Anything,
		mock.Anything).Return(nil).Maybe()
	c.boardingLocker.On("IsLocked", mock.Anything, mock.Anything).
		Return(false, RoundID{}, nil).Maybe()

	// Set up permissive fee estimator expectation.
	c.feeEstimator.On("EstimateFeePerKW", uint32(6)).
		Return(chainfee.SatPerKWeight(1000), nil).Maybe()

	// Set up permissive wallet controller expectation.
	c.walletController.On("FundPsbt", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, mock.Anything).
		Return(int32(-1), nil).Maybe()
}

// allowBoardingInput sets up the boarding locker mock to allow the given
// outpoint to be used. This sets up IsLocked to return false (not locked) and
// Lock to succeed. If roundID is provided, Lock expectation is also set up;
// otherwise only IsLocked is configured.
func (c *commonMockSetup) allowBoardingInput(outpoint *wire.OutPoint,
	roundID ...RoundID) {

	c.t.Helper()

	c.boardingLocker.On("IsLocked", mock.Anything, outpoint).
		Return(false, RoundID{}, nil)

	// If roundID provided, also set up Lock expectation.
	if len(roundID) > 0 {
		c.boardingLocker.On(
			"Lock", mock.Anything, outpoint, roundID[0],
		).Return(nil)
	}
}

// lockBoardingInput sets up the boarding locker mock to indicate the given
// outpoint is already locked by another round.
func (c *commonMockSetup) lockBoardingInput(outpoint *wire.OutPoint,
	lockedBy RoundID) {

	c.t.Helper()

	c.boardingLocker.On("IsLocked", mock.Anything, outpoint).
		Return(true, lockedBy, nil)
}

// mockBoardingUTXO sets up a ChainSource mock for a boarding UTXO with the
// specified parameters.
func (c *commonMockSetup) mockBoardingUTXO(outpoint wire.OutPoint,
	clientKey *btcec.PublicKey, exitDelay uint32, confirmations int64) {

	c.t.Helper()

	expectedPkScript := buildExpectedPkScript(
		c.t, clientKey, c.operatorPub, exitDelay,
	)

	utxo := &UTXO{
		Output: &wire.TxOut{
			Value:    100000,
			PkScript: expectedPkScript,
		},
		Confirmations: confirmations,
	}
	c.chainSource.On("GetUTXO", outpoint).Return(utxo, nil)
}

// setupValidBoardingInput sets up both the boarding locker (allowed + lock)
// and chain source (valid UTXO) mocks for a boarding input that should pass
// validation.
func (c *commonMockSetup) setupValidBoardingInput(outpoint *wire.OutPoint,
	clientKey *btcec.PublicKey, exitDelay uint32, confirmations int64,
	roundID RoundID) {

	c.t.Helper()

	c.allowBoardingInput(outpoint, roundID)
	c.mockBoardingUTXO(*outpoint, clientKey, exitDelay, confirmations)
}

// setupBoardingInputValidationOnly sets up mocks for boarding input validation
// without setting up lock expectations. This is useful for tests that want to
// control lock behavior explicitly (e.g., testing lock failures).
func (c *commonMockSetup) setupBoardingInputValidationOnly(
	outpoint *wire.OutPoint, clientKey *btcec.PublicKey, exitDelay uint32,
	confirmations int64) {

	c.t.Helper()

	c.allowBoardingInput(outpoint)
	c.mockBoardingUTXO(*outpoint, clientKey, exitDelay, confirmations)
}

// expectFailedLock sets up the boarding locker mock to expect a lock call that
// fails with the given error.
func (c *commonMockSetup) expectFailedLock(outpoint *wire.OutPoint,
	roundID RoundID, err error) {

	c.t.Helper()

	c.boardingLocker.On("Lock", mock.Anything, outpoint, roundID).
		Return(err).Once()
}

// setupBatchBuildingMocks sets up the mocks needed for successful batch
// building (fee estimation and PSBT funding). This should be called before
// sealing a round that will build a batch.
func (c *commonMockSetup) setupBatchBuildingMocks() {
	c.t.Helper()

	c.feeEstimator.On("EstimateFeePerKW", uint32(6)).
		Return(chainfee.SatPerKWeight(1000), nil).Once()
	c.walletController.On("FundPsbt", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, mock.Anything).
		Return(int32(-1), nil).Once()
}

// setupBatchBuildingFailure sets up the mocks for batch building to fail with
// the given error. This is useful for testing failure scenarios.
func (c *commonMockSetup) setupBatchBuildingFailure(err error) {
	c.t.Helper()

	c.feeEstimator.On("EstimateFeePerKW", uint32(6)).
		Return(chainfee.SatPerKWeight(1000), nil).Once()
	c.walletController.On("FundPsbt", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, mock.Anything).
		Return(int32(0), err).Once()
}

// setupCompleteRegistrationFlow sets up all mocks needed for a client to
// successfully join and proceed through batch building. This combines:
// - IsLocked check (not locked) and Lock
// - Boarding input validation (UTXO)
// - Batch building mocks (fee + funding).
func (c *commonMockSetup) setupCompleteRegistrationFlow(
	outpoint *wire.OutPoint, clientKey *btcec.PublicKey, exitDelay uint32,
	confirmations int64, roundID RoundID) {

	c.t.Helper()

	c.allowBoardingInput(outpoint, roundID)
	c.mockBoardingUTXO(*outpoint, clientKey, exitDelay, confirmations)
	c.setupBatchBuildingMocks()
}

// expectRoundFinalized sets up the round store mock to expect a PersistRound
// call that succeeds and for the wallet to finalise the PSBT.
func (c *commonMockSetup) expectRoundFinalized(tx *wire.MsgTx) {
	c.t.Helper()

	c.walletController.On("FinalizePsbt", mock.Anything, mock.Anything).
		Return(tx, nil).Once()

	c.roundStore.On("PersistRound", mock.Anything, mock.Anything).
		Return(nil).Once()
}

// setupBoardingInputWithUnlock sets up mocks for a boarding input that will
// be unlocked later (used in failure test scenarios). This includes IsLocked,
// Lock, Unlock, and UTXO mocks.
func (c *commonMockSetup) setupBoardingInputWithUnlock(outpoint *wire.OutPoint,
	clientKey *btcec.PublicKey, exitDelay uint32, confirmations int64,
	roundID RoundID) {

	c.t.Helper()

	c.allowBoardingInput(outpoint, roundID)
	c.boardingLocker.On("Unlock", mock.Anything, outpoint, roundID).
		Return(nil).Once()
	c.mockBoardingUTXO(*outpoint, clientKey, exitDelay, confirmations)
}

// assertMockExpectations asserts that all mocks received their expected calls.
// This should be called at the end of each test to verify mock expectations.
func (c *commonMockSetup) assertMockExpectations() {
	c.t.Helper()

	c.boardingLocker.AssertExpectations(c.t)
	c.chainSource.AssertExpectations(c.t)
	c.feeEstimator.AssertExpectations(c.t)
	c.walletController.AssertExpectations(c.t)
	c.roundStore.AssertExpectations(c.t)
}

// buildExpectedPkScript builds the expected PkScript for a boarding input.
func buildExpectedPkScript(t *testing.T, clientKey *btcec.PublicKey,
	operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	// Build the expected tapscript using the scripts package.
	tapscript, err := scripts.VTXOTapScript(
		clientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	// Build the P2TR script from the tapscript.
	outputKey, err := tapscript.TaprootKey()
	require.NoError(t, err)

	pkScript, err := input.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	return pkScript
}

// sendEvent sends an event to the state machine and accumulates outbox
// messages. The state machine executor automatically handles dispatching
// internal events, so this method simply awaits the result and captures
// the accumulated outbox events.
func (h *fsmTestHarness) sendEvent(event Event) error {
	h.Helper()

	// Use AskEvent to send the event and wait for all state transitions
	// (including those triggered by internal events) to complete.
	future := h.fsm.AskEvent(h.Context(), event)
	result := future.Await(h.Context())

	// Extract outbox events or return the error.
	outbox, err := result.Unpack()
	if err != nil {
		return err
	}

	// Accumulate the outbox messages from this event.
	h.outboxMessages = append(h.outboxMessages, outbox...)

	return nil
}

// clearOutbox clears the captured outbox messages. Useful between multiple
// event sends when testing specific sequences.
//
//nolint:unused
func (h *fsmTestHarness) clearOutbox() {
	h.outboxMessages = nil
}

// assertStateType asserts the current state is of the expected type and
// returns it cast to that type.
func assertStateType[T State](h *fsmTestHarness) T {
	h.Helper()

	currentState, err := h.fsm.CurrentState()
	require.NoError(h, err, "failed to query current state")

	state, ok := currentState.(T)
	require.True(h, ok, "current state is not of expected type %T, got "+
		"%T", *new(T), currentState)

	return state
}

// assertOutboxLen asserts that exactly n outbox messages were emitted.
func (h *fsmTestHarness) assertOutboxLen(n int) {
	h.Helper()

	require.Len(h, h.outboxMessages, n)
}

// assertOutboxMessageType asserts that the outbox contains a message of the
// given type at the specified index and returns it cast to that type.
func assertOutboxMessageType[T OutboxEvent](h *fsmTestHarness,
	index int) T {

	h.Helper()

	require.Greater(h, len(h.outboxMessages), index)

	msg, ok := h.outboxMessages[index].(T)
	require.True(h, ok)

	return msg
}

// assertOutboxContains asserts that the outbox contains at least one message
// of the given type and returns the first match.
//
//nolint:unused
func assertOutboxContains[T OutboxEvent](h *fsmTestHarness) T {
	h.Helper()

	var (
		result T
		found  bool
	)

	for _, msg := range h.outboxMessages {
		if typed, ok := msg.(T); ok {
			result = typed
			found = true
			break
		}
	}

	if !found {
		require.Failf(
			h,
			"outbox missing message",
			"expected outbox to contain %T",
			result,
		)
	}

	return result
}

// mockBoardingInputLocker is a mock implementation of BoardingInputLocker for
// testing using testify/mock.
type mockBoardingInputLocker struct {
	mock.Mock
}

// Lock is a mock implementation of BoardingInputLocker.Lock.
func (m *mockBoardingInputLocker) Lock(ctx context.Context,
	outpoint *wire.OutPoint, roundID RoundID) error {

	args := m.Called(ctx, outpoint, roundID)

	return args.Error(0)
}

// Unlock is a mock implementation of BoardingInputLocker.Unlock.
func (m *mockBoardingInputLocker) Unlock(ctx context.Context,
	outpoint *wire.OutPoint, roundID RoundID) error {

	args := m.Called(ctx, outpoint, roundID)

	return args.Error(0)
}

// IsLocked is a mock implementation of BoardingInputLocker.IsLocked.
func (m *mockBoardingInputLocker) IsLocked(ctx context.Context,
	outpoint *wire.OutPoint) (bool, RoundID, error) {

	args := m.Called(ctx, outpoint)

	var roundID RoundID
	if args.Get(1) != nil {
		roundID = args.Get(1).(RoundID) //nolint:forcetypeassert
	}

	return args.Bool(0), roundID, args.Error(2)
}

// mockChainSource mocks the ChainSource interface for testing.
type mockChainSource struct {
	mock.Mock
}

// GetUTXO is a mock implementation of ChainSource.GetUTXO.
func (m *mockChainSource) GetUTXO(outpoint wire.OutPoint) (*UTXO, error) {
	args := m.Called(outpoint)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*UTXO), args.Error(1) //nolint:forcetypeassert
}

// clientHarness helps simulate a client in tests. It tracks the client's keys
// and can generate boarding and VTXO requests for FSM-level tests.
type clientHarness struct {
	t *testing.T

	// Client identity.
	clientID ClientID

	// Primary boarding key and signer (for creating valid signatures).
	boardingKey    *btcec.PublicKey
	boardingSigner input.Signer

	// Key index for generating new keys.
	nextKeyIndex int32

	// Default values for requests.
	operatorKey *btcec.PublicKey
	exitDelay   uint32
	expiry      uint32

	// submittedBoardingReqs stores boarding requests submitted via
	// createJoinRequest so they can be used later for signature creation.
	submittedBoardingReqs []*types.BoardingRequest
}

// newClientHarness creates a new client harness for testing.
func newClientHarness(t *testing.T, clientID ClientID, baseKeyIndex int32,
	operatorKey *btcec.PublicKey, exitDelay, expiry uint32) *clientHarness {

	t.Helper()

	boardingKey, boardingSigner := testutils.CreateKey(baseKeyIndex)

	return &clientHarness{
		t:              t,
		clientID:       clientID,
		boardingKey:    boardingKey,
		boardingSigner: boardingSigner,
		nextKeyIndex:   baseKeyIndex + 1,
		operatorKey:    operatorKey,
		exitDelay:      exitDelay,
		expiry:         expiry,
	}
}

// createBoardingRequest creates a BoardingRequest for this client using the
// boarding key and default operator key and exit delay.
func (c *clientHarness) createBoardingRequest(
	outpoint *wire.OutPoint) *types.BoardingRequest {

	c.t.Helper()

	return &types.BoardingRequest{
		Outpoint:    outpoint,
		ClientKey:   c.boardingKey,
		OperatorKey: c.operatorKey,
		ExitDelay:   c.exitDelay,
	}
}

// createJoinRequest creates a ClientJoinRequestEvent from the provided
// boarding requests. This is used for FSM-level tests. The boarding requests
// are stored so they can be used later for signature creation.
func (c *clientHarness) createJoinRequest(
	boardingReqs []*types.BoardingRequest) *ClientJoinRequestEvent {

	c.t.Helper()

	// Store the boarding requests for later signature creation.
	c.submittedBoardingReqs = append(
		c.submittedBoardingReqs, boardingReqs...,
	)

	return &ClientJoinRequestEvent{
		ClientID: c.clientID,
		Request: &types.JoinRoundRequest{
			BoardingReqs: boardingReqs,
		},
	}
}

// mockWalletController is a mock implementation of WalletController for
// testing.
type mockWalletController struct {
	mock.Mock

	input.Signer
}

// newMockWalletController creates a new mock wallet controller with the
// provided private key for signing.
func newMockWalletController(signer input.Signer) *mockWalletController {
	return &mockWalletController{
		Signer: signer,
	}
}

// FundPsbt is a mock implementation of WalletController.FundPsbt.
func (m *mockWalletController) FundPsbt(ctx context.Context,
	packet *psbt.Packet, minConfs int32,
	feeRate chainfee.SatPerKWeight,
	account string) (int32, error) {

	args := m.Called(ctx, packet, minConfs, feeRate, account)

	return args.Get(0).(int32), args.Error(1) //nolint:forcetypeassert
}

// FinalizePsbt is a mock implementation of WalletController.FinalizePsbt.
func (m *mockWalletController) FinalizePsbt(ctx context.Context,
	packet *psbt.Packet) (*wire.MsgTx, error) {

	args := m.Called(ctx, packet)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*wire.MsgTx), args.Error(1) //nolint:forcetypeassert
}

// mockRoundStore is a mock implementation of RoundStore for testing.
type mockRoundStore struct {
	mock.Mock
}

// PersistRound is a mock implementation of RoundStore.PersistRound.
func (m *mockRoundStore) PersistRound(ctx context.Context,
	round *Round) error {

	args := m.Called(ctx, round)

	return args.Error(0)
}

// createBoardingSignaturesEvent creates a ClientBoardingSignaturesEvent with
// real signatures for the given boarding inputs. The client signs each input
// using the tapscript collaborative spend path.
func (c *clientHarness) createBoardingSignaturesEvent(
	state *AwaitingBoardingSigsState) *ClientBoardingSignaturesEvent {

	c.t.Helper()

	// Get the client's registration to find their boarding inputs.
	reg, exists := state.ClientRegistrations[c.clientID]
	require.True(c.t, exists, "client %s not registered", c.clientID)

	// Build a prevout fetcher from the PSBT's WitnessUtxo fields.
	tx := state.PSBT.UnsignedTx
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, pIn := range state.PSBT.Inputs {
		if pIn.WitnessUtxo != nil {
			prevOutFetcher.AddPrevOut(
				tx.TxIn[i].PreviousOutPoint, pIn.WitnessUtxo,
			)
		}
	}

	// Create signature hashes for the transaction.
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	// Sign each boarding input.
	var sigs []*types.BoardingInputSignature
	for _, bi := range reg.BoardingInputs {
		// Find the input index in the PSBT that matches this outpoint.
		inputIdx := -1
		for i, txIn := range tx.TxIn {
			if txIn.PreviousOutPoint == *bi.Outpoint {
				inputIdx = i
				break
			}
		}
		require.NotEqual(c.t, -1, inputIdx,
			"boarding input not found in PSBT")

		// Get the spend info for the collaborative path.
		spendInfo, err := scripts.NewVTXOSpendInfo(
			bi.Tapscript, scripts.VTXOCollabPathLeaf,
		)
		require.NoError(c.t, err, "failed to get spend info")

		// Get the prevout for this input.
		prevOut := state.PSBT.Inputs[inputIdx].WitnessUtxo
		require.NotNil(c.t, prevOut, "missing WitnessUtxo for input")

		// Create the key descriptor for signing.
		keyDesc := keychain.KeyDescriptor{
			PubKey: c.boardingKey,
		}

		// Sign the input using the collaborative spend path.
		sig, err := scripts.SignVTXOCollabInput(
			c.boardingSigner, tx, inputIdx, spendInfo,
			&keyDesc, prevOut, sigHashes, prevOutFetcher,
		)
		require.NoError(c.t, err, "failed to sign boarding input")

		// Convert to schnorr.Signature.
		schnorrSig, err := schnorr.ParseSignature(sig.Serialize())
		require.NoError(c.t, err, "failed to parse signature")

		sigs = append(sigs, &types.BoardingInputSignature{
			InputIndex:      inputIdx,
			Outpoint:        *bi.Outpoint,
			ClientSignature: schnorrSig,
		})
	}

	return &ClientBoardingSignaturesEvent{
		ClientID:   c.clientID,
		Signatures: sigs,
	}
}

// createBoardingSignaturesFromPSBT creates a ClientBoardingSignaturesEvent
// using the PSBT received from ClientBatchInfo. This mimics the real client
// flow where the client uses their stored boarding requests and the received
// PSBT to create signatures.
func (c *clientHarness) createBoardingSignaturesFromPSBT(
	p *psbt.Packet) *ClientBoardingSignaturesEvent {

	c.t.Helper()

	require.NotEmpty(c.t, c.submittedBoardingReqs,
		"no boarding requests stored - call createJoinRequest first")

	// Build a prevout fetcher from the PSBT's WitnessUtxo fields.
	tx := p.UnsignedTx
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, pIn := range p.Inputs {
		if pIn.WitnessUtxo != nil {
			prevOutFetcher.AddPrevOut(
				tx.TxIn[i].PreviousOutPoint, pIn.WitnessUtxo,
			)
		}
	}

	// Create signature hashes for the transaction.
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	// Sign each boarding input using the stored boarding requests.
	var sigs []*types.BoardingInputSignature
	for _, boardReq := range c.submittedBoardingReqs {
		// Find the input index in the PSBT that matches this outpoint.
		inputIdx := -1
		for i, txIn := range tx.TxIn {
			if txIn.PreviousOutPoint == *boardReq.Outpoint {
				inputIdx = i

				break
			}
		}
		require.NotEqual(c.t, -1, inputIdx)

		// Build the tapscript from the boarding request parameters.
		tapscript, err := scripts.VTXOTapScript(
			boardReq.ClientKey, boardReq.OperatorKey,
			boardReq.ExitDelay,
		)
		require.NoError(c.t, err, "failed to create tapscript")

		// Get the spend info for the collaborative path.
		spendInfo, err := scripts.NewVTXOSpendInfo(
			tapscript, scripts.VTXOCollabPathLeaf,
		)
		require.NoError(c.t, err, "failed to get spend info")

		// Get the prevout for this input.
		prevOut := p.Inputs[inputIdx].WitnessUtxo
		require.NotNil(c.t, prevOut, "missing WitnessUtxo for input %d",
			inputIdx)

		// Create the key descriptor for signing.
		keyDesc := keychain.KeyDescriptor{
			PubKey: c.boardingKey,
		}

		// Sign the input using the collaborative spend path.
		sig, err := scripts.SignVTXOCollabInput(
			c.boardingSigner, tx, inputIdx, spendInfo,
			&keyDesc, prevOut, sigHashes, prevOutFetcher,
		)
		require.NoError(c.t, err, "failed to sign boarding input")

		// Convert to schnorr.Signature.
		schnorrSig, err := schnorr.ParseSignature(sig.Serialize())
		require.NoError(c.t, err, "failed to parse signature")

		sigs = append(sigs, &types.BoardingInputSignature{
			InputIndex:      inputIdx,
			Outpoint:        *boardReq.Outpoint,
			ClientSignature: schnorrSig,
		})
	}

	return &ClientBoardingSignaturesEvent{
		ClientID:   c.clientID,
		Signatures: sigs,
	}
}
