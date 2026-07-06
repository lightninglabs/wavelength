package vtxo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockVTXOStore implements VTXOStore using mock.Mock for testing.
type MockVTXOStore struct {
	mock.Mock
}

func (m *MockVTXOStore) SaveVTXO(ctx context.Context, vtxo *Descriptor) error {
	args := m.Called(ctx, vtxo)

	return args.Error(0)
}

//nolint:forcetypeassert
func (m *MockVTXOStore) GetVTXO(ctx context.Context, outpoint wire.OutPoint) (
	*Descriptor, error) {

	args := m.Called(ctx, outpoint)
	var vtxo *Descriptor
	if args.Get(0) != nil {
		vtxo = args.Get(0).(*Descriptor)
	}

	return vtxo, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockVTXOStore) ListLiveVTXOs(ctx context.Context) ([]*Descriptor,
	error) {

	args := m.Called(ctx)
	var vtxos []*Descriptor
	if args.Get(0) != nil {
		vtxos = args.Get(0).([]*Descriptor)
	}

	return vtxos, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockVTXOStore) ListVTXOsByStatus(ctx context.Context,
	status VTXOStatus) ([]*Descriptor, error) {

	args := m.Called(ctx, status)
	var vtxos []*Descriptor
	if args.Get(0) != nil {
		vtxos = args.Get(0).([]*Descriptor)
	}

	return vtxos, args.Error(1)
}

// ListSelectionCandidatesByStatus derives the selection projection from the
// mocked ListVTXOsByStatus expectation, so existing tests keep stubbing a
// single listing surface for both the full and the projected reads.
func (m *MockVTXOStore) ListSelectionCandidatesByStatus(ctx context.Context,
	status VTXOStatus) ([]SelectedVTXO, error) {

	descs, err := m.ListVTXOsByStatus(ctx, status)
	if err != nil {
		return nil, err
	}

	candidates := make([]SelectedVTXO, 0, len(descs))
	for _, desc := range descs {
		candidates = append(candidates, SelectedVTXO{
			Outpoint: desc.Outpoint,
			Amount:   desc.Amount,
			PkScript: desc.PkScript,
		})
	}

	return candidates, nil
}

func (m *MockVTXOStore) UpdateVTXOStatus(ctx context.Context,
	outpoint wire.OutPoint, status VTXOStatus) error {

	args := m.Called(ctx, outpoint, status)

	return args.Error(0)
}

func (m *MockVTXOStore) UpdateVTXOStatusReleasingReservation(
	ctx context.Context, outpoint wire.OutPoint, status VTXOStatus) error {

	args := m.Called(ctx, outpoint, status)

	return args.Error(0)
}

func (m *MockVTXOStore) MarkForfeited(ctx context.Context,
	outpoint wire.OutPoint, forfeitTxID chainhash.Hash) error {

	args := m.Called(ctx, outpoint, forfeitTxID)

	return args.Error(0)
}

func (m *MockVTXOStore) DeleteVTXO(ctx context.Context,
	outpoint wire.OutPoint) error {

	args := m.Called(ctx, outpoint)

	return args.Error(0)
}

func (m *MockVTXOStore) MarkForfeiting(ctx context.Context,
	outpoint wire.OutPoint, roundID string, forfeitTx *wire.MsgTx) error {

	args := m.Called(ctx, outpoint, roundID, forfeitTx)

	return args.Error(0)
}

//nolint:forcetypeassert
func (m *MockVTXOStore) GetForfeitTx(ctx context.Context,
	outpoint wire.OutPoint) (*wire.MsgTx, error) {

	args := m.Called(ctx, outpoint)
	var tx *wire.MsgTx
	if args.Get(0) != nil {
		tx = args.Get(0).(*wire.MsgTx)
	}

	return tx, args.Error(1)
}

// Compile-time check that MockVTXOStore implements VTXOStore.
var _ VTXOStore = (*MockVTXOStore)(nil)

// MockVTXOWallet implements VTXOWallet using mock.Mock for testing. This
// includes all methods from input.Signer which embeds input.MuSig2Signer.
type MockVTXOWallet struct {
	mock.Mock
}

//nolint:forcetypeassert
func (m *MockVTXOWallet) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	args := m.Called(tx, signDesc)
	var sig input.Signature
	if args.Get(0) != nil {
		sig = args.Get(0).(input.Signature)
	}

	return sig, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockVTXOWallet) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	args := m.Called(tx, signDesc)
	var script *input.Script
	if args.Get(0) != nil {
		script = args.Get(0).(*input.Script)
	}

	return script, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockVTXOWallet) MuSig2CreateSession(version input.MuSig2Version,
	keyLoc keychain.KeyLocator, signers []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, otherNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	args := m.Called(
		version, keyLoc, signers, tweaks, otherNonces, localNonces,
	)
	var info *input.MuSig2SessionInfo
	if args.Get(0) != nil {
		info = args.Get(0).(*input.MuSig2SessionInfo)
	}

	return info, args.Error(1)
}

func (m *MockVTXOWallet) MuSig2RegisterNonces(sessionID input.MuSig2SessionID,
	nonces [][musig2.PubNonceSize]byte) (bool, error) {

	args := m.Called(sessionID, nonces)

	return args.Bool(0), args.Error(1)
}

func (m *MockVTXOWallet) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	nonce [musig2.PubNonceSize]byte) error {

	args := m.Called(sessionID, nonce)

	return args.Error(0)
}

//nolint:forcetypeassert
func (m *MockVTXOWallet) MuSig2GetCombinedNonce(
	sessionID input.MuSig2SessionID) ([musig2.PubNonceSize]byte, error) {

	args := m.Called(sessionID)
	var nonce [musig2.PubNonceSize]byte
	if args.Get(0) != nil {
		nonce = args.Get(0).([musig2.PubNonceSize]byte)
	}

	return nonce, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockVTXOWallet) MuSig2Sign(sessionID input.MuSig2SessionID,
	message [sha256.Size]byte, cleanup bool) (*musig2.PartialSignature,
	error) {

	args := m.Called(sessionID, message, cleanup)
	var sig *musig2.PartialSignature
	if args.Get(0) != nil {
		sig = args.Get(0).(*musig2.PartialSignature)
	}

	return sig, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockVTXOWallet) MuSig2CombineSig(sessionID input.MuSig2SessionID,
	partials []*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	args := m.Called(sessionID, partials)
	var sig *schnorr.Signature
	if args.Get(0) != nil {
		sig = args.Get(0).(*schnorr.Signature)
	}

	return sig, args.Bool(1), args.Error(2)
}

func (m *MockVTXOWallet) MuSig2Cleanup(sessionID input.MuSig2SessionID) error {
	args := m.Called(sessionID)

	return args.Error(0)
}

// Compile-time check that MockVTXOWallet implements VTXOWallet.
var _ VTXOWallet = (*MockVTXOWallet)(nil)

// vtxoTestHarness provides common test utilities for VTXO FSM testing.
//
//nolint:containedctx
type vtxoTestHarness struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc

	// Mocks for FSM dependencies.
	store  *MockVTXOStore
	wallet *MockVTXOWallet

	// Environment for FSM.
	env *VTXOEnvironment

	// Cryptographic keys.
	clientPrivKey   *btcec.PrivateKey
	clientPubKey    *btcec.PublicKey
	operatorPrivKey *btcec.PrivateKey
	operatorPubKey  *btcec.PublicKey

	// Runtime state tracking.
	currentState   VTXOState
	lastTransition *VTXOStateTransition
	outboxMessages []VTXOOutMsg
}

// newVTXOTestHarness creates a new test harness with default configuration.
func newVTXOTestHarness(t *testing.T) *vtxoTestHarness {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	clientPrivKey, clientPubKey := generateTestKeyPair(t)
	operatorPrivKey, operatorPubKey := generateTestKeyPair(t)

	store := &MockVTXOStore{}
	wallet := &MockVTXOWallet{}

	env := NewVTXOEnvironment(
		"test-vtxo", store, wallet, DefaultExpiryConfig(),
		&chaincfg.RegressionNetParams, nil,
	)

	h := &vtxoTestHarness{
		t:               t,
		ctx:             ctx,
		cancel:          cancel,
		store:           store,
		wallet:          wallet,
		env:             env,
		clientPrivKey:   clientPrivKey,
		clientPubKey:    clientPubKey,
		operatorPrivKey: operatorPrivKey,
		operatorPubKey:  operatorPubKey,
		outboxMessages:  make([]VTXOOutMsg, 0),
	}

	t.Cleanup(func() {
		cancel()
	})

	return h
}

// withState sets the current state for the harness.
func (h *vtxoTestHarness) withState(state VTXOState) *vtxoTestHarness {
	h.currentState = state

	return h
}

// withExpiryConfig sets a custom expiry config for testing.
func (h *vtxoTestHarness) withExpiryConfig(cfg *ExpiryConfig) *vtxoTestHarness {
	h.env.ExpiryConfig = cfg

	return h
}

// sendEvent sends an event to the current state and captures the transition.
func (h *vtxoTestHarness) sendEvent(event VTXOEvent) (*VTXOStateTransition,
	error) {

	h.t.Helper()

	transition, err := h.currentState.ProcessEvent(h.ctx, event, h.env)
	if err != nil {
		return nil, err
	}

	h.lastTransition = transition

	if transition != nil {
		if nextState, ok := transition.NextState.(VTXOState); ok {
			h.currentState = nextState
		}

		transition.NewEvents.WhenSome(func(e VTXOEmittedEvent) {
			h.outboxMessages = append(
				h.outboxMessages, e.Outbox...,
			)
		})
	}

	return transition, nil
}

// newTestOutpoint creates a random test outpoint.
func (h *vtxoTestHarness) newTestOutpoint() wire.OutPoint {
	h.t.Helper()

	var hash chainhash.Hash
	_, err := rand.Read(hash[:])
	require.NoError(h.t, err)

	return wire.OutPoint{Hash: hash, Index: 0}
}

// newTestDescriptor creates a VTXO descriptor for testing.
func (h *vtxoTestHarness) newTestDescriptor() *Descriptor {
	h.t.Helper()

	outpoint := h.newTestOutpoint()

	tapscript, err := arkscript.VTXOTapScript(
		h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.NoError(h.t, err)

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.NoError(h.t, err)

	return &Descriptor{
		Outpoint:       outpoint,
		Amount:         btcutil.Amount(50000),
		PolicyTemplate: policyTemplate,
		ClientKey: keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  0,
			},
		},
		OperatorKey:    h.operatorPubKey,
		TapScript:      tapscript,
		BatchExpiry:    int32(testBatchExpiryBlocks),
		RelativeExpiry: testExitDelay,
		Ancestry: []Ancestry{
			{
				TreeDepth: 2,
			},
		},
		CreatedHeight: 100,
		Status:        VTXOStatusLive,
	}
}

// newBlockEpochEvent creates a BlockEpochEvent at the given height.
func (h *vtxoTestHarness) newBlockEpochEvent(height int32) *BlockEpochEvent {
	h.t.Helper()

	var hash chainhash.Hash
	_, err := rand.Read(hash[:])
	require.NoError(h.t, err)

	return &BlockEpochEvent{
		Height: height,
		Hash:   hash,
	}
}

// assertCurrentState asserts the current state is of the expected type.
func assertState[T VTXOState](h *vtxoTestHarness) T {
	h.t.Helper()

	state, ok := h.currentState.(T)
	require.True(
		h.t, ok, "expected state type %T, got %T", *new(T),
		h.currentState,
	)

	return state
}

// assertOutboxContains checks if the outbox contains a message of the given
// type.
func assertOutboxContains[T VTXOOutMsg](h *vtxoTestHarness) T {
	h.t.Helper()

	for _, msg := range h.outboxMessages {
		if typed, ok := msg.(T); ok {
			return typed
		}
	}

	var zero T
	h.t.Fatalf("outbox does not contain message of type %T", zero)

	return zero
}

// assertOutboxLacks asserts the outbox contains no message of the given type.
func assertOutboxLacks[T VTXOOutMsg](h *vtxoTestHarness) {
	h.t.Helper()

	for _, msg := range h.outboxMessages {
		if _, ok := msg.(T); ok {
			var zero T
			h.t.Fatalf("outbox unexpectedly contains message "+
				"of type %T", zero)
		}
	}
}

// setupMockWalletForSigning configures the wallet mock to return valid
// signatures for forfeit tx signing.
func (h *vtxoTestHarness) setupMockWalletForSigning() {
	h.t.Helper()

	// Generate a real signature using the client's private key.
	msgHash := sha256.Sum256([]byte("test-forfeit-sig"))
	sig, err := schnorr.Sign(h.clientPrivKey, msgHash[:])
	require.NoError(h.t, err)

	h.wallet.On("SignOutputRaw", mock.Anything, mock.Anything).Return(
		sig, nil,
	)
}

func generateTestKeyPair(t *testing.T) (*btcec.PrivateKey, *btcec.PublicKey) {
	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return privKey, privKey.PubKey()
}

const (
	// testExitDelay is the CSV delay for tests (~1 day in blocks).
	testExitDelay = uint32(144)

	// testBatchExpiryBlocks is a mock batch expiry at block 1000.
	testBatchExpiryBlocks = 1000
)

// realVTXOSigner wraps LND's MockSigner to provide real cryptographic signing
// for VTXO forfeit transactions. Despite its name, input.MockSigner performs
// actual Schnorr signing operations using real private keys.
type realVTXOSigner struct {
	*input.MockSigner
}

// newRealVTXOSigner creates a signer that produces real Schnorr signatures
// using the provided private key.
func newRealVTXOSigner(privKey *btcec.PrivateKey) *realVTXOSigner {
	return &realVTXOSigner{
		MockSigner: input.NewMockSigner(
			[]*btcec.PrivateKey{privKey}, nil,
		),
	}
}

// Compile-time check that realVTXOSigner implements VTXOWallet.
var _ VTXOWallet = (*realVTXOSigner)(nil)

// realVTXOSigningHarness extends vtxoTestHarness with real signing capabilities
// for testing forfeit signature validity. Unlike mock-based testing, signatures
// produced here are cryptographically valid and can be verified using
// txscript.NewEngine.
//
//nolint:containedctx
type realVTXOSigningHarness struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc

	// Store mock for FSM dependencies.
	store *MockVTXOStore

	// Real signers for client and operator.
	clientSigner   *realVTXOSigner
	operatorSigner *realVTXOSigner

	// Environment for FSM with real client signer.
	env *VTXOEnvironment

	// Cryptographic keys.
	clientPrivKey   *btcec.PrivateKey
	clientPubKey    *btcec.PublicKey
	operatorPrivKey *btcec.PrivateKey
	operatorPubKey  *btcec.PublicKey

	// Runtime state tracking.
	currentState   VTXOState
	lastTransition *VTXOStateTransition
	outboxMessages []VTXOOutMsg
}

// newRealVTXOSigningHarness creates a test harness with real cryptographic
// signing for forfeit transactions.
func newRealVTXOSigningHarness(t *testing.T) *realVTXOSigningHarness {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	clientPrivKey, clientPubKey := generateTestKeyPair(t)
	operatorPrivKey, operatorPubKey := generateTestKeyPair(t)

	store := &MockVTXOStore{}

	clientSigner := newRealVTXOSigner(clientPrivKey)
	operatorSigner := newRealVTXOSigner(operatorPrivKey)

	env := NewVTXOEnvironment(
		"test-vtxo-real-signing", store, clientSigner,
		DefaultExpiryConfig(), &chaincfg.RegressionNetParams, nil,
	)

	h := &realVTXOSigningHarness{
		t:               t,
		ctx:             ctx,
		cancel:          cancel,
		store:           store,
		clientSigner:    clientSigner,
		operatorSigner:  operatorSigner,
		env:             env,
		clientPrivKey:   clientPrivKey,
		clientPubKey:    clientPubKey,
		operatorPrivKey: operatorPrivKey,
		operatorPubKey:  operatorPubKey,
		outboxMessages:  make([]VTXOOutMsg, 0),
	}

	t.Cleanup(func() {
		cancel()
	})

	return h
}

// withState sets the current state for the harness.
func (h *realVTXOSigningHarness) withState(
	state VTXOState) *realVTXOSigningHarness {

	h.currentState = state

	return h
}

// sendEvent sends an event to the current state and captures the transition.
func (h *realVTXOSigningHarness) sendEvent(event VTXOEvent) (
	*VTXOStateTransition, error) {

	h.t.Helper()

	transition, err := h.currentState.ProcessEvent(h.ctx, event, h.env)
	if err != nil {
		return nil, err
	}

	h.lastTransition = transition

	if transition != nil {
		if nextState, ok := transition.NextState.(VTXOState); ok {
			h.currentState = nextState
		}

		transition.NewEvents.WhenSome(func(e VTXOEmittedEvent) {
			h.outboxMessages = append(
				h.outboxMessages, e.Outbox...,
			)
		})
	}

	return transition, nil
}

// newTestOutpoint creates a random test outpoint.
func (h *realVTXOSigningHarness) newTestOutpoint() wire.OutPoint {
	h.t.Helper()

	var hash chainhash.Hash
	_, err := rand.Read(hash[:])
	require.NoError(h.t, err)

	return wire.OutPoint{Hash: hash, Index: 0}
}

// newTestDescriptor creates a VTXO descriptor for testing with real tapscript.
func (h *realVTXOSigningHarness) newTestDescriptor() *Descriptor {
	h.t.Helper()

	outpoint := h.newTestOutpoint()

	tapscript, err := arkscript.VTXOTapScript(
		h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.NoError(h.t, err)

	// Compute the P2TR pkScript for the VTXO output.
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(h.t, err)

	pkScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(h.t, err)

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.NoError(h.t, err)

	return &Descriptor{
		Outpoint:       outpoint,
		Amount:         btcutil.Amount(50000),
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		ClientKey: keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  0,
			},
		},
		OperatorKey:    h.operatorPubKey,
		TapScript:      tapscript,
		BatchExpiry:    int32(testBatchExpiryBlocks),
		RelativeExpiry: testExitDelay,
		Ancestry: []Ancestry{
			{
				TreeDepth: 2,
			},
		},
		CreatedHeight: 100,
		Status:        VTXOStatusLive,
	}
}

// newTestConnectorOutput creates a P2TR connector output for forfeit testing.
func (h *realVTXOSigningHarness) newTestConnectorOutput() *wire.TxOut {
	h.t.Helper()

	// Connector is a simple P2TR key-path spend controlled by operator.
	pkScript, err := txscript.PayToTaprootScript(h.operatorPubKey)
	require.NoError(h.t, err)

	return &wire.TxOut{
		Value:    546, // Dust limit.
		PkScript: pkScript,
	}
}

// newServerForfeitScript creates a P2TR script for the server's penalty output.
func (h *realVTXOSigningHarness) newServerForfeitScript() []byte {
	h.t.Helper()

	pkScript, err := txscript.PayToTaprootScript(h.operatorPubKey)
	require.NoError(h.t, err)

	return pkScript
}

// assertStateReal asserts the current state is of the expected type.
func assertStateReal[T VTXOState](h *realVTXOSigningHarness) T {
	h.t.Helper()

	state, ok := h.currentState.(T)
	require.True(
		h.t, ok, "expected state type %T, got %T", *new(T),
		h.currentState,
	)

	return state
}

// assertOutboxContainsReal checks if the outbox contains a message of the
// given type.
func assertOutboxContainsReal[T VTXOOutMsg](h *realVTXOSigningHarness) T {
	h.t.Helper()

	for _, msg := range h.outboxMessages {
		if typed, ok := msg.(T); ok {
			return typed
		}
	}

	var zero T
	h.t.Fatalf("outbox does not contain message of type %T", zero)

	return zero
}

// mockRoundActorRef captures messages sent to the round actor for test
// verification. Used by manager relay tests.
type mockRoundActorRef struct {
	t        *testing.T
	messages []actormsg.RoundReceivable
	mu       sync.Mutex
}

// newMockRoundActorRef creates a new mock round actor ref.
func newMockRoundActorRef(t *testing.T) *mockRoundActorRef {
	return &mockRoundActorRef{
		t:        t,
		messages: make([]actormsg.RoundReceivable, 0),
	}
}

// ID returns the mock actor ID.
func (m *mockRoundActorRef) ID() string {
	return "mock-round-actor"
}

// Tell captures the message for test verification.
func (m *mockRoundActorRef) Tell(
	_ context.Context, msg actormsg.RoundReceivable,
) error {

	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)

	return nil
}

// getMessages returns all captured messages.
func (m *mockRoundActorRef) getMessages() []actormsg.RoundReceivable {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]actormsg.RoundReceivable, len(m.messages))
	copy(result, m.messages)

	return result
}

// mockManagerRef captures messages sent to the manager for test verification.
type mockManagerRef struct {
	t        *testing.T
	messages []ManagerMsg
	mu       sync.Mutex
}

func newMockManagerRef(t *testing.T) *mockManagerRef {
	return &mockManagerRef{
		t:        t,
		messages: make([]ManagerMsg, 0),
	}
}

func (m *mockManagerRef) ID() string {
	return "mock-manager"
}

func (m *mockManagerRef) Tell(_ context.Context, msg ManagerMsg) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)

	return nil
}

func (m *mockManagerRef) getMessages() []ManagerMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]ManagerMsg, len(m.messages))
	copy(result, m.messages)

	return result
}

// mockChainResolverRef captures expiring notifications for test verification.
type mockChainResolverRef struct {
	t        *testing.T
	messages []ExpiringNotification
	mu       sync.Mutex
}

// newMockChainResolverRef creates a new mock chain resolver ref.
func newMockChainResolverRef(t *testing.T) *mockChainResolverRef {
	return &mockChainResolverRef{
		t:        t,
		messages: make([]ExpiringNotification, 0),
	}
}

// ID returns the mock actor ID.
func (m *mockChainResolverRef) ID() string {
	return "mock-chain-resolver"
}

// Tell captures the message for test verification.
func (m *mockChainResolverRef) Tell(
	_ context.Context, msg ExpiringNotification,
) error {

	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)

	return nil
}

// getMessages returns all captured messages.
func (m *mockChainResolverRef) getMessages() []ExpiringNotification {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]ExpiringNotification, len(m.messages))
	copy(result, m.messages)

	return result
}
