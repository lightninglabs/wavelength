package rounds

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing/route"
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
	vtxoStore        *mockVTXOStore
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
	mockVTXOStore := &mockVTXOStore{}

	// Allow confirmation bookkeeping calls by default.
	mockRoundStore.On(
		"MarkRoundConfirmed", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything,
	).Return(nil).Maybe()
	mockVTXOStore.On(
		"MarkVTXOsLive", mock.Anything, mock.Anything,
	).Return(nil).Maybe()

	m := &commonMockSetup{
		t:                t,
		operatorPub:      operatorPub,
		operatorSigner:   operatorSigner,
		boardingLocker:   mockLocker,
		chainSource:      mockChainSrc,
		feeEstimator:     mockFeeEstimator,
		walletController: mockWalletController,
		roundStore:       mockRoundStore,
		vtxoStore:        mockVTXOStore,
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
// It initializes and starts a new state machine for testing. If an initial
// state is provided, the FSM will start in that state; otherwise it starts
// in CreatedState.
func newTestHarness(t *testing.T, initialState ...State) *fsmTestHarness {
	t.Helper()

	roundID, err := NewRoundID()
	require.NoError(t, err)

	// Create common mock setup.
	common := newCommonMockSetup(t)

	operatorKey := keychain.KeyDescriptor{
		PubKey: common.operatorPub,
	}

	// Generate a sweep key for VTXO trees.
	sweepKey, _ := testutils.CreateKey(2)

	env := Environment{
		RoundID:             roundID,
		ChainParams:         &chaincfg.RegressionNetParams,
		BoardingInputLocker: common.boardingLocker,
		ChainSource:         common.chainSource,
		FeeEstimator:        common.feeEstimator,
		Log:                 btclog.Disabled,
		WalletController:    common.walletController,
		RoundStore:          common.roundStore,
		VTXOStore:           common.vtxoStore,
		ConfTarget:          6,
		MinConfs:            1,
		Terms: &batch.Terms{
			OperatorKey:                   operatorKey,
			SweepKey:                      keychain.KeyDescriptor{PubKey: sweepKey},
			SweepDelay:                    288,
			BoardingExitDelay:             100,
			BoardingExitDelaySafetyMargin: 6,
			MinBoardingConfirmations:      1,
			MaxVTXOsPerTree:               1024,
			SignatureCollectionTimeout:    30 * time.Second,
			TreeRadix:                     4,
			MinVTXOAmount:                 1000,
			MaxVTXOAmount:                 100000000,
			VTXOExitDelay:                 100,
		},
	}

	// Determine initial state: use provided state or default to CreatedState.
	var startState State = &CreatedState{}
	if len(initialState) > 0 {
		startState = initialState[0]
	}

	fsmCfg := StateMachineCfg{
		InitialState: startState,
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

// expectPersistVTXOs sets up the VTXO store mock to expect a PersistVTXOs call
// with VTXOs matching the provided pkScripts.
func (c *commonMockSetup) expectPersistVTXOs(pkScripts ...[]byte) {
	c.t.Helper()

	expected := make(map[string]int)
	for _, pk := range pkScripts {
		expected[hex.EncodeToString(pk)]++
	}

	c.vtxoStore.On(
		"PersistVTXOs", mock.Anything,
		mock.MatchedBy(func(v []*VTXO) bool {
			if len(v) != len(pkScripts) {
				return false
			}

			actual := make(map[string]int)
			for _, item := range v {
				if item == nil || item.Descriptor == nil {
					return false
				}

				actual[hex.EncodeToString(
					item.Descriptor.PkScript,
				)]++
			}

			for key, expectedCount := range expected {
				if actual[key] != expectedCount {
					return false
				}
			}

			return true
		}),
	).Return(nil).Once()
}

// setActiveRounds configures the round store mock to return the given rounds
// when LoadPendingRounds is called.
func (c *commonMockSetup) setActiveRounds(rounds []*Round) {
	c.t.Helper()

	c.roundStore.On("LoadPendingRounds", mock.Anything).
		Return(rounds, nil).Once()
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
	c.vtxoStore.AssertExpectations(c.t)
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

// buildTestClientRegistration creates a simple ClientRegistration for testing.
// This is useful when starting tests directly in RegistrationState or later
// states without going through the full join flow.
func buildTestClientRegistration(clientID ClientID,
	boardingInputs ...*BoardingInput) *ClientRegistration {

	return &ClientRegistration{
		ClientID:       clientID,
		BoardingInputs: boardingInputs,
	}
}

// vtxoNoncesStateOpts configures buildAwaitingVTXONoncesState.
type vtxoNoncesStateOpts struct {
	// withVTXOs marks this client as having VTXODescriptors.
	withVTXOs bool

	// alreadySubmitted marks this client as having already submitted.
	alreadySubmitted bool

	// boardingInputs are boarding inputs to attach to the registration.
	boardingInputs []*BoardingInput
}

// buildAwaitingVTXONoncesState creates an AwaitingVTXONoncesState for testing.
// The opts map keys are client IDs, values configure each client's state.
func buildAwaitingVTXONoncesState(
	opts map[ClientID]vtxoNoncesStateOpts) *AwaitingVTXONoncesState {

	regs := make(map[ClientID]*ClientRegistration)
	submitted := make(map[ClientID]struct{})
	keyIdx := int32(200)

	for clientID, clientOpts := range opts {
		reg := &ClientRegistration{
			ClientID:       clientID,
			BoardingInputs: clientOpts.boardingInputs,
		}

		if clientOpts.withVTXOs {
			testKey, _ := testutils.CreateKey(keyIdx)
			keyIdx++
			keyVertex := route.NewVertex(testKey)
			vtxoDescs := map[SigningKeyHex]*tree.VTXODescriptor{
				keyVertex: {
					CoSignerKey: testKey,
				},
			}
			reg.VTXODescriptors = vtxoDescs

			if clientOpts.alreadySubmitted {
				submitted[clientID] = struct{}{}
			}
		}

		regs[clientID] = reg
	}

	return &AwaitingVTXONoncesState{
		ClientRegistrations: regs,
		PSBT: &psbt.Packet{
			UnsignedTx: wire.NewMsgTx(2),
		},
		VTXOTrees:            map[int]*tree.Tree{},
		TreeSignCoordinators: map[int]*batch.TreeSignCoordinator{},
		ClientsWithNonces:    submitted,
	}
}

// buildAwaitingVTXOSignaturesState creates an AwaitingVTXOSignaturesState for
// testing. The opts map keys are client IDs, values configure each client.
func buildAwaitingVTXOSignaturesState(
	opts map[ClientID]vtxoNoncesStateOpts) *AwaitingVTXOSignaturesState {

	regs := make(map[ClientID]*ClientRegistration)
	submitted := make(map[ClientID]struct{})
	keyIdx := int32(300)

	for clientID, clientOpts := range opts {
		reg := &ClientRegistration{
			ClientID:       clientID,
			BoardingInputs: clientOpts.boardingInputs,
		}

		if clientOpts.withVTXOs {
			testKey, _ := testutils.CreateKey(keyIdx)
			keyIdx++
			keyVertex := route.NewVertex(testKey)
			vtxoDescs := map[SigningKeyHex]*tree.VTXODescriptor{
				keyVertex: {
					CoSignerKey: testKey,
				},
			}
			reg.VTXODescriptors = vtxoDescs

			if clientOpts.alreadySubmitted {
				submitted[clientID] = struct{}{}
			}
		}

		regs[clientID] = reg
	}

	return &AwaitingVTXOSignaturesState{
		ClientRegistrations: regs,
		PSBT: &psbt.Packet{
			UnsignedTx: wire.NewMsgTx(2),
		},
		VTXOTrees:             map[int]*tree.Tree{},
		TreeSignCoordinators:  map[int]*batch.TreeSignCoordinator{},
		ClientsWithSignatures: submitted,
	}
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

// getClientBatchInfo returns the ClientBatchInfo for the given client from the
// captured outbox messages, or nil if not found.
func (h *fsmTestHarness) getClientBatchInfo(
	clientID ClientID) *ClientBatchInfo {

	h.Helper()

	for _, msg := range h.outboxMessages {
		batchInfo, ok := msg.(*ClientBatchInfo)
		if !ok {
			continue
		}

		if batchInfo.Client == clientID {
			return batchInfo
		}
	}

	return nil
}

// getClientVTXOAggNonces returns the ClientVTXOAggNonces message for the given
// client from the captured outbox messages, or nil if not found.
func (h *fsmTestHarness) getClientVTXOAggNonces(
	clientID ClientID) *ClientVTXOAggNonces {

	h.Helper()

	for _, msg := range h.outboxMessages {
		nonces, ok := msg.(*ClientVTXOAggNonces)
		if !ok {
			continue
		}

		if nonces.Client == clientID {
			return nonces
		}
	}

	return nil
}

// getClientVTXOAggSigs returns the ClientVTXOAggSigs message for the given
// client from the captured outbox messages, or nil if not found.
func (h *fsmTestHarness) getClientVTXOAggSigs(
	clientID ClientID) *ClientVTXOAggSigs {

	h.Helper()

	for _, msg := range h.outboxMessages {
		sigs, ok := msg.(*ClientVTXOAggSigs)
		if !ok {
			continue
		}

		if sigs.Client == clientID {
			return sigs
		}
	}

	return nil
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

	// vtxoKeys stores the signing key descriptors for VTXO requests keyed
	// by their hex-encoded public key.
	vtxoKeys map[SigningKeyHex]*keychain.KeyDescriptor

	// vtxoMuSigSigners stores the MuSig2 signer for each VTXO signing key.
	vtxoMuSigSigners map[SigningKeyHex]input.MuSig2Signer

	// vtxoSessions caches the MuSig2 signing sessions for each signing
	// key. These sessions must be reused between nonce registration and
	// signature generation to avoid nonce reuse.
	vtxoSessions map[SigningKeyHex]*clientMuSigSession

	// vtxoDescriptors stores the built VTXO descriptors per signing key.
	vtxoDescriptors map[SigningKeyHex]*tree.VTXODescriptor

	// vtxoKeyOrder preserves insertion order of signing keys to allow
	// deterministic iteration in tests.
	vtxoKeyOrder []SigningKeyHex
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
		vtxoKeys:       make(map[SigningKeyHex]*keychain.KeyDescriptor),
		vtxoMuSigSigners: make(
			map[SigningKeyHex]input.MuSig2Signer,
		),
		vtxoSessions: make(map[SigningKeyHex]*clientMuSigSession),
		vtxoDescriptors: make(
			map[SigningKeyHex]*tree.VTXODescriptor,
		),
		vtxoKeyOrder: make([]SigningKeyHex, 0),
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

// createVTXORequest creates a VTXORequest with a fresh signing key and stores
// the signing material for later nonce/signature creation.
func (c *clientHarness) createVTXORequest(
	amount btcutil.Amount) *types.VTXORequest {

	c.t.Helper()

	signingKey, signingSigner := testutils.CreateKey(c.nextKeyIndex)
	c.nextKeyIndex++

	musigSigner, ok := signingSigner.(input.MuSig2Signer)
	require.True(c.t, ok, "signer must implement MuSig2Signer")

	keyDesc := &keychain.KeyDescriptor{
		PubKey: signingKey,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamilyMultiSig,
			Index:  uint32(c.nextKeyIndex),
		},
	}

	keyVertex := route.NewVertex(signingKey)
	c.vtxoKeys[keyVertex] = keyDesc
	c.vtxoMuSigSigners[keyVertex] = musigSigner

	desc, err := tree.NewVTXODescriptor(
		amount, signingKey, c.operatorKey, c.expiry,
	)
	require.NoError(c.t, err, "failed to build vtxo descriptor")

	c.vtxoDescriptors[keyVertex] = desc
	c.vtxoKeyOrder = append(c.vtxoKeyOrder, keyVertex)

	return &types.VTXORequest{
		Amount:      amount,
		PkScript:    desc.PkScript,
		Expiry:      c.expiry,
		ClientKey:   signingKey,
		OperatorKey: c.operatorKey,
		SigningKey:  *keyDesc,
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

// createJoinRequestWithVTXOs creates a ClientJoinRequestEvent containing both
// boarding requests and VTXO requests.
func (c *clientHarness) createJoinRequestWithVTXOs(
	boardingReqs []*types.BoardingRequest,
	vtxoReqs []*types.VTXORequest) *ClientJoinRequestEvent {

	c.t.Helper()

	// Store boarding requests for later boarding signature creation.
	c.submittedBoardingReqs = append(
		c.submittedBoardingReqs, boardingReqs...,
	)

	return &ClientJoinRequestEvent{
		ClientID: c.clientID,
		Request: &types.JoinRoundRequest{
			BoardingReqs: boardingReqs,
			VTXOReqs:     vtxoReqs,
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

// LoadPendingRounds is a mock implementation of RoundStore.LoadPendingRounds.
func (m *mockRoundStore) LoadPendingRounds(ctx context.Context) ([]*Round,
	error) {

	args := m.Called(ctx)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*Round), args.Error(1) //nolint:forcetypeassert
}

// MarkRoundConfirmed is a mock implementation of RoundStore.MarkRoundConfirmed.
func (m *mockRoundStore) MarkRoundConfirmed(ctx context.Context,
	roundID RoundID, blockHeight int32,
	blockHash chainhash.Hash) error {

	if len(m.ExpectedCalls) == 0 {
		return nil
	}

	args := m.Called(ctx, roundID, blockHeight, blockHash)

	return args.Error(0)
}

// mockVTXOStore is a mock implementation of VTXOStore for testing.
type mockVTXOStore struct {
	mock.Mock
}

// PersistVTXOs is a mock implementation of VTXOStore.PersistVTXOs.
func (m *mockVTXOStore) PersistVTXOs(ctx context.Context,
	vtxos []*VTXO) error {

	args := m.Called(ctx, vtxos)

	return args.Error(0)
}

// MarkVTXOsLive is a mock implementation of VTXOStore.MarkVTXOsLive.
func (m *mockVTXOStore) MarkVTXOsLive(ctx context.Context,
	roundID RoundID) error {

	args := m.Called(ctx, roundID)

	return args.Error(0)
}

// clientMuSigSession holds the MuSig2 signing sessions for a client's VTXO
// signing key across all relevant transactions.
type clientMuSigSession struct {
	keyDesc  *keychain.KeyDescriptor
	signer   input.MuSig2Signer
	sessions []*tree.SignerSession
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

// vtxoSigningKeys returns the stored VTXO signing keys in insertion order.
func (c *clientHarness) vtxoSigningKeys() []SigningKeyHex {
	keys := make([]SigningKeyHex, len(c.vtxoKeyOrder))
	copy(keys, c.vtxoKeyOrder)

	return keys
}

// vtxoPkScripts returns the pkScripts for all VTXO requests made by this
// client in insertion order.
func (c *clientHarness) vtxoPkScripts() [][]byte {
	scripts := make([][]byte, 0, len(c.vtxoKeyOrder))
	for _, key := range c.vtxoKeyOrder {
		desc := c.vtxoDescriptors[key]
		if desc == nil {
			continue
		}

		pk := make([]byte, len(desc.PkScript))
		copy(pk, desc.PkScript)
		scripts = append(scripts, pk)
	}

	return scripts
}

// buildOrGetMuSigSession builds MuSig2 signing sessions for the provided tree
// paths if they have not been created yet for the signing key.
func (c *clientHarness) buildOrGetMuSigSession(keyHex SigningKeyHex,
	treePaths map[int]*tree.Tree) *clientMuSigSession {

	c.t.Helper()

	keyDesc, hasKey := c.vtxoKeys[keyHex]
	signer, hasSigner := c.vtxoMuSigSigners[keyHex]

	require.True(c.t, hasKey, "no key descriptor for %x", keyHex)
	require.True(c.t, hasSigner, "no signer for %x", keyHex)

	session, ok := c.vtxoSessions[keyHex]
	if !ok {
		session = &clientMuSigSession{
			keyDesc:  keyDesc,
			signer:   signer,
			sessions: nil,
		}
		c.vtxoSessions[keyHex] = session
	}

	// If we already built sessions, return early.
	if len(session.sessions) > 0 {
		return session
	}

	for _, treePath := range treePaths {
		extracted, err := treePath.ExtractPathForCoSigners(
			keyDesc.PubKey,
		)
		require.NoError(c.t, err, "extract path for cosigner")
		if extracted == nil {
			continue
		}

		prevOutFetcher, err := extracted.Root.PrevOutputFetcher(
			extracted.BatchOutput,
		)
		require.NoError(c.t, err, "failed to build prevout fetcher")

		signerSession, err := tree.NewSignerSession(
			signer, keyDesc, extracted.SweepTapscriptRoot,
			prevOutFetcher, extracted.Root,
		)
		require.NoError(c.t, err, "failed to create signer session")

		session.sessions = append(session.sessions, signerSession)
	}

	require.NotEmpty(c.t, session.sessions,
		"no signer sessions created for key %x", keyHex)

	return session
}

// createVTXONoncesEvent builds a ClientVTXONoncesEvent with fresh nonces for
// the client's VTXO signing key.
func (c *clientHarness) createVTXONoncesEvent(keyHex SigningKeyHex,
	treePaths map[int]*tree.Tree) *ClientVTXONoncesEvent {

	c.t.Helper()

	session := c.buildOrGetMuSigSession(
		keyHex, treePaths,
	)

	nonces := make(map[tree.TxID]tree.Musig2PubNonce)
	for _, signerSession := range session.sessions {
		for txid, nonce := range signerSession.GetNonces() {
			nonces[txid] = nonce
		}
	}

	return &ClientVTXONoncesEvent{
		ClientID: c.clientID,
		Nonces: map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce{
			keyHex: nonces,
		},
	}
}

// createVTXONoncesEventAll builds a ClientVTXONoncesEvent containing nonces
// for all stored signing keys in a single message.
func (c *clientHarness) createVTXONoncesEventAll(
	treePaths map[int]*tree.Tree) *ClientVTXONoncesEvent {

	c.t.Helper()

	noncesByKey := make(
		map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce,
	)

	for _, keyHex := range c.vtxoSigningKeys() {
		session := c.buildOrGetMuSigSession(
			keyHex, treePaths,
		)

		nonces := make(map[tree.TxID]tree.Musig2PubNonce)
		for _, signerSession := range session.sessions {
			for txid, nonce := range signerSession.GetNonces() {
				nonces[txid] = nonce
			}
		}

		noncesByKey[keyHex] = nonces
	}

	return &ClientVTXONoncesEvent{
		ClientID: c.clientID,
		Nonces:   noncesByKey,
	}
}

// createVTXOPartialSigsEvent registers the aggregated nonces and generates the
// client's partial signatures for all relevant transactions.
func (c *clientHarness) createVTXOPartialSigsEvent(
	keyHex SigningKeyHex, treePaths map[int]*tree.Tree,
	aggNonces map[tree.TxID]tree.Musig2PubNonce,
) *ClientVTXOPartialSigsEvent {

	c.t.Helper()

	session := c.buildOrGetMuSigSession(
		keyHex, treePaths,
	)

	for _, signerSession := range session.sessions {
		err := signerSession.RegisterAggNonces(aggNonces)
		require.NoError(c.t, err, "failed to register agg nonce")
	}

	sigs := make(map[tree.TxID]*musig2.PartialSignature)
	for _, signerSession := range session.sessions {
		partialSigs, err := signerSession.Signatures(false)
		require.NoError(c.t, err, "failed to create partial sigs")

		for txid, sig := range partialSigs {
			sigs[txid] = sig
		}
	}

	sigsByKey := map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature{
		keyHex: sigs,
	}

	return &ClientVTXOPartialSigsEvent{
		ClientID:   c.clientID,
		Signatures: sigsByKey,
	}
}

// createVTXOPartialSigsEventAll registers aggregated nonces and generates
// partial signatures for all signing keys in one message.
func (c *clientHarness) createVTXOPartialSigsEventAll(
	treePaths map[int]*tree.Tree,
	aggNonces map[tree.TxID]tree.Musig2PubNonce,
) *ClientVTXOPartialSigsEvent {

	c.t.Helper()

	sigsByKey := make(
		map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature,
	)

	for _, keyHex := range c.vtxoSigningKeys() {
		session := c.buildOrGetMuSigSession(
			keyHex, treePaths,
		)

		for _, signerSession := range session.sessions {
			err := signerSession.RegisterAggNonces(aggNonces)
			require.NoError(
				c.t, err, "failed to register agg nonce",
			)
		}

		sigs := make(map[tree.TxID]*musig2.PartialSignature)
		for _, signerSession := range session.sessions {
			partialSigs, err := signerSession.Signatures(false)
			require.NoError(
				c.t, err, "failed to create partial sigs",
			)

			for txid, sig := range partialSigs {
				sigs[txid] = sig
			}
		}

		sigsByKey[keyHex] = sigs
	}

	return &ClientVTXOPartialSigsEvent{
		ClientID:   c.clientID,
		Signatures: sigsByKey,
	}
}
