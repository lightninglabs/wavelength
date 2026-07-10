package round

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockRoundStore implements RoundStore using mock.Mock for testing.
type MockRoundStore struct {
	mock.Mock
}

func (m *MockRoundStore) CommitState(ctx context.Context, round *Round,
	state ClientState) error {

	args := m.Called(ctx, round, state)

	return args.Error(0)
}

//nolint:forcetypeassert
func (m *MockRoundStore) FetchState(ctx context.Context, roundID RoundID) (
	*Round, ClientState, error) {

	args := m.Called(ctx, roundID)

	var round *Round
	if args.Get(0) != nil {
		round = args.Get(0).(*Round)
	}

	var state ClientState
	if args.Get(1) != nil {
		state = args.Get(1).(ClientState)
	}

	return round, state, args.Error(2)
}

//nolint:forcetypeassert
func (m *MockRoundStore) LookupRoundByCommitmentTx(ctx context.Context,
	txid chainhash.Hash) (*Round, error) {

	args := m.Called(ctx, txid)

	var round *Round
	if args.Get(0) != nil {
		round = args.Get(0).(*Round)
	}

	return round, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockRoundStore) ListActiveRounds(ctx context.Context) ([]*Round,
	error) {

	args := m.Called(ctx)

	var rounds []*Round
	if args.Get(0) != nil {
		rounds = args.Get(0).([]*Round)
	}

	return rounds, args.Error(1)
}

func (m *MockRoundStore) FinalizeRound(ctx context.Context, roundID RoundID,
	txid chainhash.Hash, confInfo ConfInfo) error {

	args := m.Called(ctx, roundID, txid, confInfo)

	return args.Error(0)
}

// Compile-time check that MockRoundStore implements RoundStore.
var _ RoundStore = (*MockRoundStore)(nil)

// MockVTXOStore implements VTXOStore using mock.Mock for testing.
type MockVTXOStore struct {
	mock.Mock
}

func (m *MockVTXOStore) SaveVTXOs(ctx context.Context,
	vtxos []*ClientVTXO) error {

	args := m.Called(ctx, vtxos)

	return args.Error(0)
}

//nolint:forcetypeassert
func (m *MockVTXOStore) ListVTXOs(ctx context.Context) ([]*ClientVTXO, error) {
	args := m.Called(ctx)

	var vtxos []*ClientVTXO
	if args.Get(0) != nil {
		vtxos = args.Get(0).([]*ClientVTXO)
	}

	return vtxos, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockVTXOStore) GetVTXO(ctx context.Context, outpoint wire.OutPoint) (
	*ClientVTXO, error) {

	args := m.Called(ctx, outpoint)

	var vtxo *ClientVTXO
	if args.Get(0) != nil {
		vtxo = args.Get(0).(*ClientVTXO)
	}

	return vtxo, args.Error(1)
}

func (m *MockVTXOStore) MarkVTXOSpent(ctx context.Context,
	outpoint wire.OutPoint) error {

	args := m.Called(ctx, outpoint)

	return args.Error(0)
}

// Compile-time check that MockVTXOStore implements VTXOStore.
var _ VTXOStore = (*MockVTXOStore)(nil)

// mockOwnedScriptChecker is an OwnedScriptChecker backed by a set of
// known owned pkScripts.
type mockOwnedScriptChecker struct {
	owned map[string]bool
}

func newMockOwnedScriptChecker(ownedScripts ...[]byte) *mockOwnedScriptChecker {
	m := &mockOwnedScriptChecker{owned: make(map[string]bool)}
	for _, s := range ownedScripts {
		m.owned[string(s)] = true
	}

	return m
}

func (m *mockOwnedScriptChecker) IsOwnedScript(_ context.Context,
	pkScript []byte) fn.Result[bool] {

	return fn.Ok(m.owned[string(pkScript)])
}

var _ OwnedScriptChecker = (*mockOwnedScriptChecker)(nil)

// MockClientWallet implements ClientWallet (input.MuSig2Signer + input.Signer)
// using mock.Mock for testing.
type MockClientWallet struct {
	mock.Mock
}

//nolint:forcetypeassert
func (m *MockClientWallet) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	for _, call := range m.ExpectedCalls {
		if call.Method != "DeriveNextKey" || len(call.Arguments) < 2 {
			continue
		}

		expectedFamily, ok := call.Arguments.Get(1).(keychain.KeyFamily)
		if ok && expectedFamily == family {
			args := m.Called(ctx, family)

			var keyDesc *keychain.KeyDescriptor
			if args.Get(0) != nil {
				keyDesc = args.Get(0).(*keychain.KeyDescriptor)
			}

			return keyDesc, args.Error(1)
		}
	}

	if family == types.VTXOSigningKeyFamily {
		privKey, err := btcec.NewPrivateKey()
		if err != nil {
			return nil, err
		}

		return &keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: family,
			},
		}, nil
	}

	args := m.Called(ctx, family)

	var keyDesc *keychain.KeyDescriptor
	if args.Get(0) != nil {
		keyDesc = args.Get(0).(*keychain.KeyDescriptor)
	}

	return keyDesc, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockClientWallet) MuSig2CreateSession(version input.MuSig2Version,
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

func (m *MockClientWallet) MuSig2RegisterNonces(sessionID input.MuSig2SessionID,
	nonces [][musig2.PubNonceSize]byte) (bool, error) {

	args := m.Called(sessionID, nonces)

	return args.Bool(0), args.Error(1)
}

func (m *MockClientWallet) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	nonce [musig2.PubNonceSize]byte,
) error {

	args := m.Called(sessionID, nonce)

	return args.Error(0)
}

//nolint:forcetypeassert
func (m *MockClientWallet) MuSig2GetCombinedNonce(
	sessionID input.MuSig2SessionID) ([musig2.PubNonceSize]byte, error) {

	args := m.Called(sessionID)

	var nonce [musig2.PubNonceSize]byte
	if args.Get(0) != nil {
		nonce = args.Get(0).([musig2.PubNonceSize]byte)
	}

	return nonce, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockClientWallet) MuSig2Sign(sessionID input.MuSig2SessionID,
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
func (m *MockClientWallet) MuSig2CombineSig(sessionID input.MuSig2SessionID,
	otherPartials []*musig2.PartialSignature) (*schnorr.Signature, bool,
	error) {

	args := m.Called(sessionID, otherPartials)

	var sig *schnorr.Signature
	if args.Get(0) != nil {
		sig = args.Get(0).(*schnorr.Signature)
	}

	return sig, args.Bool(1), args.Error(2)
}

func (m *MockClientWallet) MuSig2Cleanup(
	sessionID input.MuSig2SessionID) error {

	args := m.Called(sessionID)

	return args.Error(0)
}

//nolint:forcetypeassert
func (m *MockClientWallet) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	args := m.Called(tx, signDesc)

	var sig input.Signature
	if args.Get(0) != nil {
		sig = args.Get(0).(input.Signature)
	}

	return sig, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockClientWallet) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	args := m.Called(tx, signDesc)

	var script *input.Script
	if args.Get(0) != nil {
		script = args.Get(0).(*input.Script)
	}

	return script, args.Error(1)
}

// Compile-time check that MockClientWallet implements ClientWallet.
var _ ClientWallet = (*MockClientWallet)(nil)

// boardingTestHarness is the central test harness housing all common setup,
// mocks, fixtures, and helper functions for boarding FSM tests.
//
//nolint:containedctx
type boardingTestHarness struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc

	// Mocks for FSM dependencies.
	roundStore *MockRoundStore
	vtxoStore  *MockVTXOStore
	wallet     *MockClientWallet

	// Environment for FSM.
	env *ClientEnvironment

	// Real cryptographic keys for signature testing.
	clientPrivKey   *btcec.PrivateKey
	clientPubKey    *btcec.PublicKey
	operatorPrivKey *btcec.PrivateKey
	operatorPubKey  *btcec.PublicKey

	// forfeitPubKey is the operator's dedicated per-round forfeit penalty
	// key, distinct from the operator identity key. The forfeit-tx penalty
	// output is a BIP-86 key-spend to this key.
	forfeitPubKey *btcec.PublicKey

	// Runtime state tracking for assertions.
	currentState   ClientState
	lastTransition *ClientStateTransition
	outboxMessages []ClientOutMsg
}

// newTestHarness creates a new test harness with default mock configuration.
func newTestHarness(t *testing.T) *boardingTestHarness {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	clientPrivKey, clientPubKey := generateTestKeyPair(t)
	operatorPrivKey, operatorPubKey := generateTestKeyPair(t)

	roundStore := &MockRoundStore{}
	vtxoStore := &MockVTXOStore{}
	wallet := &MockClientWallet{}

	// The forfeit penalty key is delivered per round (no longer a global
	// operator term); derive a dedicated key distinct from the operator
	// identity key.
	_, forfeitPubKey := generateTestKeyPair(t)

	terms := &types.OperatorTerms{
		PubKey:            operatorPubKey,
		VTXOExitDelay:     144, // 1 day in blocks.
		DustLimit:         546,
		MinBoardingAmount: 10000,
		MaxVTXOAmount:     100000000, // 1 BTC.
		FeeRate:           10,
		MinConfirmations:  3,
	}

	// Use a mock start height for testing.
	const testStartHeight uint32 = 100

	// Default max operator fee for tests: 100,000 sats (0.001 BTC).
	// This is generous to avoid test brittleness when multiple intents
	// are used.
	const defaultMaxOperatorFee = btcutil.Amount(100000)

	env := NewClientEnvironment(
		roundStore, vtxoStore, wallet, terms,
		&chaincfg.RegressionNetParams, defaultMaxOperatorFee,
		btclog.Disabled, testStartHeight, nil, 2*time.Minute,
	)
	env.DisableJoinRequestAuth = true

	// The join-round transition always derives a fresh identifier key,
	// so wire up a default mock that returns a valid key descriptor.
	identifierPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	wallet.On(
		"DeriveNextKey", mock.Anything,
		joinRoundAuthIdentifierKeyFamily,
	).Return(&keychain.KeyDescriptor{
		PubKey: identifierPrivKey.PubKey(),
	}, nil)

	h := &boardingTestHarness{
		t:               t,
		ctx:             ctx,
		cancel:          cancel,
		roundStore:      roundStore,
		vtxoStore:       vtxoStore,
		wallet:          wallet,
		env:             env,
		clientPrivKey:   clientPrivKey,
		clientPubKey:    clientPubKey,
		operatorPrivKey: operatorPrivKey,
		operatorPubKey:  operatorPubKey,
		forfeitPubKey:   forfeitPubKey,
		currentState:    &Idle{},
		outboxMessages:  make([]ClientOutMsg, 0),
	}

	t.Cleanup(func() {
		cancel()
	})

	return h
}

// withState sets the current state for the harness.
func (h *boardingTestHarness) withState(
	state ClientState) *boardingTestHarness {

	h.currentState = state

	return h
}

// sendEvent sends an event to the current state, captures the transition, and
// updates internal state tracking for subsequent assertions.
func (h *boardingTestHarness) sendEvent(event ClientEvent) (
	*ClientStateTransition, error) {

	h.t.Helper()

	transition, err := h.currentState.ProcessEvent(h.ctx, event, h.env)
	if err != nil {
		return nil, err
	}

	h.lastTransition = transition

	if transition != nil {
		if nextState, ok := transition.NextState.(ClientState); ok {
			h.currentState = nextState
		}

		transition.NewEvents.WhenSome(func(emitted ClientEmittedEvent) {
			h.outboxMessages = append(
				h.outboxMessages, emitted.Outbox...,
			)
		})
	}

	return transition, nil
}

// assertStateType asserts the current state is of the expected type and
// returns it cast to that type.
func assertStateType[T ClientState](h *boardingTestHarness) T {
	h.t.Helper()

	state, ok := h.currentState.(T)
	require.True(h.t, ok, "current state is not of expected type")

	return state
}

func (h *boardingTestHarness) newTestOutpoint() wire.OutPoint {
	h.t.Helper()

	var hash chainhash.Hash
	_, err := rand.Read(hash[:])
	require.NoError(h.t, err)

	return wire.OutPoint{
		Hash:  hash,
		Index: 0,
	}
}

func (h *boardingTestHarness) newTestBoardingAddress() wallet.BoardingAddress {
	h.t.Helper()

	// ~1 day in blocks.
	exitDelay := uint32(144)

	// Build the tapscript for the boarding address.
	tapscript, err := arkscript.VTXOTapScript(
		h.clientPubKey, h.operatorPubKey, exitDelay,
	)
	require.NoError(h.t, err)

	// Compute the taproot key and create the address.
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(h.t, err)

	address, err := btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(h.t, err)

	return wallet.BoardingAddress{
		Address:   address,
		Tapscript: tapscript,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(
					wallet.BoardingKeyFamily,
				),
				Index: 0,
			},
		},
		OperatorKey: h.operatorPubKey,
		ExitDelay:   exitDelay,
	}
}

func (h *boardingTestHarness) newTestBoardingIntent() BoardingIntent {
	h.t.Helper()

	outpoint := h.newTestOutpoint()
	address := h.newTestBoardingAddress()

	return BoardingIntent{
		BoardingIntent: wallet.BoardingIntent{
			Address:  address,
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: 100,
				OutPoint:   outpoint,
				Amount:     btcutil.Amount(50000),
			},
			Status: wallet.BoardingStatusConfirmed,
		},
		Request: types.BoardingRequest{
			Outpoint:    &outpoint,
			ClientKey:   h.clientPubKey,
			OperatorKey: h.operatorPubKey,
		},
	}
}

// newTestVTXORequestForIntent creates a VTXORequest that corresponds to a
// boarding intent. This is what the client would request as output for their
// boarding input.
func (h *boardingTestHarness) newTestVTXORequestForIntent(
	intent BoardingIntent) types.VTXORequest {

	h.t.Helper()

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.NoError(h.t, err)

	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
	require.NoError(h.t, err)

	pkScript, err := template.PkScript()
	require.NoError(h.t, err)

	signingKey := keychain.KeyDescriptor{
		PubKey: h.clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: types.VTXOSigningKeyFamily,
			Index:  0,
		},
	}

	return types.VTXORequest{
		Amount:         intent.ChainInfo.Amount,
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		Expiry:         testExitDelay,
		ClientKey:      h.clientPubKey,
		OwnerKey: keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  0,
			},
		},
		OperatorKey: h.operatorPubKey,
		SigningKey:  signingKey,
	}
}

// newTestCommitmentTx creates a commitment transaction with the given inputs.
// Uses version 3 for BIP 431 ephemeral anchor compatibility.
func (h *boardingTestHarness) newTestCommitmentTx(
	intents []BoardingIntent) *psbt.Packet {

	h.t.Helper()

	tx := wire.NewMsgTx(3)

	for _, intent := range intents {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: intent.Outpoint,
		})
	}

	tx.AddTxOut(&wire.TxOut{
		Value:    100000,
		PkScript: make([]byte, 34),
	})

	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_TRUE, txscript.OP_RETURN},
	})

	// Create PSBT with WitnessUtxo for each input.
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(h.t, err)

	for i, intent := range intents {
		addr := intent.Address.Address
		pkScript := addr.ScriptAddress()
		amt := intent.ChainInfo.Amount

		packet.Inputs[i] = psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    int64(amt),
				PkScript: pkScript,
			},
		}
	}

	return packet
}

func generateTestKeyPair(t *testing.T) (*btcec.PrivateKey, *btcec.PublicKey) {
	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return privKey, privKey.PubKey()
}

const (
	testExitDelay = uint32(144) // 1 day in blocks.

	// testConnectorDustValue is the on-chain dust value of a connector
	// output used in test fixtures. Mirrors the operator-published
	// `DustLimit` of 546 sats configured in `newTestEnvironment`.
	testConnectorDustValue = int64(546)

	// testForfeitVTXOValue is the local VTXO amount used by forfeit-tx
	// fixtures.
	testForfeitVTXOValue = int64(50000)
)

// newTestVTXOTree creates a minimal valid VTXT tree for testing by
// delegating to lib/tree.NewTree() which handles all internal construction
// including proper Taproot tweaking and node structure.
func (h *boardingTestHarness) newTestVTXOTree(numLeaves int) (*tree.Tree,
	[]tree.LeafDescriptor) {

	h.t.Helper()

	const leafAmount = btcutil.Amount(50000)

	leaves := make([]tree.LeafDescriptor, numLeaves)
	for i := 0; i < numLeaves; i++ {
		vtxoScript, err := txscript.PayToTaprootScript(h.clientPubKey)
		require.NoError(h.t, err)

		leaves[i] = tree.LeafDescriptor{
			PkScript:    vtxoScript,
			Amount:      leafAmount,
			CoSignerKey: h.clientPubKey,
		}
	}

	batchOutpoint := h.newTestOutpoint()

	batchPkScript, err := txscript.PayToTaprootScript(h.operatorPubKey)
	require.NoError(h.t, err)

	// Batch output value must equal the sum of all leaf amounts.
	batchOutput := &wire.TxOut{
		Value:    int64(leafAmount) * int64(numLeaves),
		PkScript: batchPkScript,
	}

	sweepRoot := sha256.Sum256([]byte("test-sweep-root"))

	vtxtTree, err := tree.NewTree(
		batchOutpoint, batchOutput, leaves, h.operatorPubKey,
		sweepRoot[:], 2,
	)
	require.NoError(h.t, err)

	return vtxtTree, leaves
}

// newTestVTXOTreeForIntents creates a VTXT tree configured for the given VTXO
// requests, with total amount calculated from the VTXOs.
func (h *boardingTestHarness) newTestVTXOTreeForIntents(
	vtxoReqs []types.VTXORequest) *tree.Tree {

	h.t.Helper()

	var totalAmount btcutil.Amount
	leaves := make([]tree.LeafDescriptor, len(vtxoReqs))
	for i, vtxoReq := range vtxoReqs {
		pkScript, err := vtxoReq.EffectivePkScript()
		require.NoError(h.t, err)

		leaves[i] = tree.LeafDescriptor{
			PkScript:    pkScript,
			Amount:      vtxoReq.Amount,
			CoSignerKey: h.clientPubKey,
		}
		totalAmount += vtxoReq.Amount
	}

	batchOutpoint := h.newTestOutpoint()

	batchPkScript, err := txscript.PayToTaprootScript(h.operatorPubKey)
	require.NoError(h.t, err)

	batchOutput := &wire.TxOut{
		Value:    int64(totalAmount),
		PkScript: batchPkScript,
	}

	sweepRoot := sha256.Sum256([]byte("test-sweep-root"))

	vtxtTree, err := tree.NewTree(
		batchOutpoint, batchOutput, leaves, h.operatorPubKey,
		sweepRoot[:], 2,
	)
	require.NoError(h.t, err)

	return vtxtTree
}

// leafDescriptorsFromTree reconstructs the leaf descriptors of a VTXO tree
// from its leaf nodes. bindTreeToCommitment uses it to rebuild a tree rooted
// at a different batch outpoint: each leaf's non-anchor output supplies the
// script and amount, and the single non-operator cosigner is the leaf owner
// (signing) key.
func (h *boardingTestHarness) leafDescriptorsFromTree(
	t *tree.Tree) []tree.LeafDescriptor {

	h.t.Helper()

	leafNodes := t.Root.GetLeafNodes()
	leaves := make([]tree.LeafDescriptor, 0, len(leafNodes))
	for _, leaf := range leafNodes {
		require.GreaterOrEqual(h.t, len(leaf.Outputs), 1)
		out := leaf.Outputs[0]

		var coSigner *btcec.PublicKey
		for _, cs := range leaf.CoSigners {
			if !cs.IsEqual(h.operatorPubKey) {
				coSigner = cs

				break
			}
		}
		require.NotNil(
			h.t, coSigner, "leaf has no non-operator cosigner",
		)

		leaves = append(leaves, tree.LeafDescriptor{
			PkScript:    out.PkScript,
			Amount:      btcutil.Amount(out.Value),
			CoSignerKey: coSigner,
		})
	}

	return leaves
}

// bindTreeToCommitment makes vtxtTree commitment-bound, mirroring how the
// honest operator constructs a round: it builds a commitment tx over the given
// boarding intents whose first output is the tree's batch output (the canonical
// tree-root taproot script plus the summed leaf value), then rebuilds vtxtTree
// in place so its root spends that exact output. After it returns, the returned
// PSBT and the (now bound) vtxtTree satisfy validateVTXOTreeBinding. The
// returned PSBT carries WitnessUtxo for every input, which is required for
// correct Taproot sighash computation in downstream signing fixtures.
//
// Replacing the batch outpoint changes the root tx hash and cascades to every
// child input, so the tree is rebuilt from its leaves rather than patched in
// place — the caller's *tree.Tree pointer is updated via assignment so events
// generated afterwards (nonces, signatures) use the bound tree.
//
// extraOutputs are appended after the batch output and the anchor (e.g. leave
// outputs). They must be supplied here rather than appended to the returned
// PSBT, because appending after the fact would change the commitment txid and
// break the binding the tree was just rebuilt against.
func (h *boardingTestHarness) bindTreeToCommitment(intents []BoardingIntent,
	vtxtTree *tree.Tree, extraOutputs ...*wire.TxOut) *psbt.Packet {

	h.t.Helper()

	// The batch output script depends only on the tree's cosigner set and
	// sweep root, not on the outpoint, so it can be computed before the
	// commitment txid is known. Recompute it exactly as the binding
	// validation does.
	finalKey, err := tree.ComputeFinalKey(
		vtxtTree.Root.CoSigners, vtxtTree.SweepTapscriptRoot,
	)
	require.NoError(h.t, err)
	rootScript, err := txscript.PayToTaprootScript(finalKey)
	require.NoError(h.t, err)
	batchOutput := &wire.TxOut{
		Value:    vtxtTree.BatchOutput.Value,
		PkScript: rootScript,
	}

	// Build the unsigned commitment tx: boarding inputs, the batch output
	// at index 0, and the ephemeral anchor.
	tx := wire.NewMsgTx(3)
	for _, intent := range intents {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: intent.Outpoint,
		})
	}
	tx.AddTxOut(batchOutput)
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_TRUE, txscript.OP_RETURN},
	})
	for _, out := range extraOutputs {
		tx.AddTxOut(out)
	}

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(h.t, err)
	for i, intent := range intents {
		addr := intent.Address.Address
		packet.Inputs[i] = psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    int64(intent.ChainInfo.Amount),
				PkScript: addr.ScriptAddress(),
			},
		}
	}

	// Rebuild the tree rooted at the real commitment output and adopt it
	// into the caller's pointer.
	txid := tx.TxHash()
	leaves := h.leafDescriptorsFromTree(vtxtTree)
	rebuilt, err := tree.NewTree(
		wire.OutPoint{
			Hash:  txid,
			Index: 0,
		},
		batchOutput,
		leaves,
		h.operatorPubKey,
		vtxtTree.SweepTapscriptRoot,
		2,
	)
	require.NoError(h.t, err)
	*vtxtTree = *rebuilt

	return packet
}

// newCommitmentTxBuiltEvent creates a CommitmentTxBuilt event simulating what
// the server sends after building the commitment transaction with all boarding
// inputs and the batch VTXT tree output. The transaction is returned as a PSBT
// with WitnessUtxo populated for all inputs, which is required for correct
// Taproot sighash computation. The tree is bound to the commitment tx so it
// passes the round's VTXO-tree commitment-binding validation.
func (h *boardingTestHarness) newCommitmentTxBuiltEvent(roundID RoundID,
	intents []BoardingIntent, vtxtTree *tree.Tree) *CommitmentTxBuilt {

	h.t.Helper()

	packet := h.bindTreeToCommitment(intents, vtxtTree)

	return &CommitmentTxBuilt{
		RoundID: roundID,
		Tx:      packet,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		SweepKey:   h.operatorPubKey,
		SweepDelay: 1008, // ~1 week in blocks, must exceed exit delay.
		ForfeitKey: h.forfeitPubKey,
	}
}

// newNoncesAggregatedEvent creates aggregated nonces for all tree nodes,
// simulating what the server sends after collecting and combining nonces from
// all round participants for the MuSig2 signing protocol.
func (h *boardingTestHarness) newNoncesAggregatedEvent(roundID RoundID,
	vtxtTree *tree.Tree) *NoncesAggregated {

	h.t.Helper()

	aggNonces := make(map[tree.TxID]tree.Musig2PubNonce)
	err := vtxtTree.Root.ForEach(func(node *tree.Node) error {
		txid, err := node.TXID()
		if err != nil {
			return err
		}

		var nonce tree.Musig2PubNonce
		_, err = rand.Read(nonce[:])
		if err != nil {
			return err
		}

		aggNonces[txid] = nonce

		return nil
	})
	require.NoError(h.t, err)

	return &NoncesAggregated{
		RoundID:   roundID,
		AggNonces: aggNonces,
	}
}

// newOperatorSignedEvent creates operator signatures for the tree, simulating
// what the server sends after combining all partial signatures into final
// Schnorr signatures for each tree node.
//
//nolint:unused
func (h *boardingTestHarness) newOperatorSignedEvent(roundID RoundID,
	vtxtTree *tree.Tree) *OperatorSigned {

	h.t.Helper()

	// Build a map of signatures keyed by transaction ID.
	sigs := make(map[tree.TxID]*schnorr.Signature)
	err := vtxtTree.Root.ForEach(func(node *tree.Node) error {
		txid, err := node.TXID()
		if err != nil {
			return err
		}

		sigBytes := make([]byte, schnorr.SignatureSize)
		_, err = rand.Read(sigBytes)
		if err != nil {
			return err
		}

		sig, err := schnorr.ParseSignature(sigBytes)
		if err != nil {
			// Use a dummy signature for tests.
			sig = &schnorr.Signature{}
		}

		sigs[txid] = sig

		return nil
	})
	require.NoError(h.t, err)

	return &OperatorSigned{
		RoundID: roundID,
		AggSigs: sigs,
	}
}

// newCommitmentTxReceivedState creates a CommitmentTxReceivedState ready for
// validation testing with a pre-built commitment transaction and VTXT tree.
func (h *boardingTestHarness) newCommitmentTxReceivedState(roundID RoundID,
	intents []BoardingIntent) *CommitmentTxReceivedState {

	h.t.Helper()

	// Generate VTXO requests from intents.
	vtxoReqs := make([]types.VTXORequest, len(intents))
	var totalAmount btcutil.Amount
	for i, intent := range intents {
		vtxoReqs[i] = h.newTestVTXORequestForIntent(intent)
		totalAmount += intent.ChainInfo.Amount
	}

	vtxtTree := h.newTestVTXOTreeForIntents(vtxoReqs)
	commitmentTx := h.newTestCommitmentTx(intents)

	return &CommitmentTxReceivedState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		TxID:         commitmentTx.UnsignedTx.TxHash(),
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: intents,
			VTXOs:    vtxoReqs,
		},
		ClientTrees: make(map[SignerKey]*tree.Tree),
	}
}

// newCommitmentTxValidatedState creates a CommitmentTxValidatedState ready
// for nonce generation, with boarding input indices pre-mapped for efficient
// input signature placement during the signing phase.
func (h *boardingTestHarness) newCommitmentTxValidatedState(roundID RoundID,
	intents []BoardingIntent) *CommitmentTxValidatedState {

	h.t.Helper()

	// Generate VTXO requests from intents.
	vtxoReqs := make([]types.VTXORequest, len(intents))
	var totalAmount btcutil.Amount
	for i, intent := range intents {
		vtxoReqs[i] = h.newTestVTXORequestForIntent(intent)
		totalAmount += intent.ChainInfo.Amount
	}

	vtxtTree := h.newTestVTXOTreeForIntents(vtxoReqs)
	commitmentTx := h.newTestCommitmentTx(intents)

	boardingInputIndices := make(map[wire.OutPoint]int)
	for i, intent := range intents {
		boardingInputIndices[intent.Outpoint] = i
	}

	// Populate ClientTrees by extracting sub-trees for each signer key
	// in the VTXOs. This simulates what happens during commitment tx
	// validation when ValidatePath is called.
	clientTrees := make(map[SignerKey]*tree.Tree)
	for _, vtxoReq := range vtxoReqs {
		signerKey := NewSignerKey(vtxoReq.SigningKey.PubKey)

		// The client tree is a subtree extracted from the VTXT
		// tree that corresponds to this signer's VTXO. For test
		// purposes, we use the full tree as the client tree.
		clientTrees[signerKey] = vtxtTree
	}

	return &CommitmentTxValidatedState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: intents,
			VTXOs:    vtxoReqs,
		},
		ClientTrees:          clientTrees,
		BoardingInputIndices: boardingInputIndices,
	}
}

// MockSignerSession is a mock implementation of tree.SignerSession for
// testing error injection in the MuSig2 signing flow without requiring
// real cryptographic operations.
type MockSignerSession struct {
	mock.Mock

	pubKey *btcec.PublicKey
}

// NewMockSignerSession creates a new MockSignerSession with the given public
// key.
func NewMockSignerSession(pubKey *btcec.PublicKey) *MockSignerSession {
	return &MockSignerSession{
		pubKey: pubKey,
	}
}

//nolint:forcetypeassert
func (m *MockSignerSession) GetNonces() (map[string]tree.Musig2PubNonce,
	error) {

	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(map[string]tree.Musig2PubNonce), args.Error(1)
}

func (m *MockSignerSession) RegisterNonces(
	nonces map[string][]tree.Musig2PubNonce) error {

	args := m.Called(nonces)

	return args.Error(0)
}

//nolint:forcetypeassert
func (m *MockSignerSession) Signatures(cleanup bool) (
	map[string]*musig2.PartialSignature, error) {

	args := m.Called(cleanup)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(map[string]*musig2.PartialSignature), args.Error(1)
}

func (m *MockSignerSession) PubKey() *btcec.PublicKey {
	return m.pubKey
}

//nolint:unused
func (h *boardingTestHarness) newBoardingConfirmedEvent(
	commitmentTx *wire.MsgTx, blockHeight int32,
	blockHash chainhash.Hash) *BoardingConfirmed {

	h.t.Helper()

	return &BoardingConfirmed{
		TxID:          commitmentTx.TxHash(),
		BlockHeight:   blockHeight,
		BlockHash:     blockHash,
		Confirmations: 6,
	}
}

//nolint:unused
func (h *boardingTestHarness) newBoardingFailedEvent(reason string,
	recoverable bool) *BoardingFailed {

	h.t.Helper()

	return &BoardingFailed{
		Reason:      reason,
		Recoverable: recoverable,
	}
}

// setupMockWalletForMuSig2 configures all MuSig2 mock methods needed for tree
// signing flows: session creation, nonce registration (both individual and
// combined), and partial signature generation.
func (h *boardingTestHarness) setupMockWalletForMuSig2() {
	h.t.Helper()

	// Session creation mock.
	var sessionID input.MuSig2SessionID
	_, err := rand.Read(sessionID[:])
	require.NoError(h.t, err)

	var pubNonce [musig2.PubNonceSize]byte
	_, err = rand.Read(pubNonce[:])
	require.NoError(h.t, err)

	sessionInfo := &input.MuSig2SessionInfo{
		SessionID:   sessionID,
		PublicNonce: pubNonce,
		CombinedKey: h.clientPubKey,
	}

	h.wallet.On(
		"MuSig2CreateSession", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything,
	).Return(sessionInfo, nil)

	// Nonce registration mocks (individual and combined/aggregated).
	h.wallet.On(
		"MuSig2RegisterNonces", mock.Anything, mock.Anything,
	).Return(true, nil)

	h.wallet.On(
		"MuSig2RegisterCombinedNonce", mock.Anything, mock.Anything,
	).Return(nil)

	// Signing mock.
	var scalarBytes [32]byte
	_, err = rand.Read(scalarBytes[:])
	require.NoError(h.t, err)

	var scalar btcec.ModNScalar
	scalar.SetBytes(&scalarBytes)

	partialSig := &musig2.PartialSignature{
		S: &scalar,
	}

	h.wallet.On(
		"MuSig2Sign", mock.Anything, mock.Anything, mock.Anything,
	).Return(partialSig, nil)

	// Batch signing performs explicit cleanup after every worker returns so
	// partial failures cannot leave a subset of sessions live.
	h.wallet.On("MuSig2Cleanup", mock.Anything).Return(nil)
}

func (h *boardingTestHarness) setupMockWalletForBoardingSigning() {
	h.t.Helper()

	sig := &schnorr.Signature{}

	h.wallet.On("SignOutputRaw", mock.Anything, mock.Anything).Return(
		sig, nil,
	)
}

func (h *boardingTestHarness) setupMockRoundStoreForCheckpoint() {
	h.t.Helper()

	h.roundStore.On(
		"CommitState",
		mock.Anything, // ctx
		mock.Anything, // round
		mock.Anything, // state
	).Return(nil)
}

func (h *boardingTestHarness) setupMockVTXOStoreForSave() {
	h.t.Helper()

	h.vtxoStore.On(
		"SaveVTXOs",
		mock.Anything, // ctx
		mock.Anything, // vtxos
	).Return(nil)
}

//nolint:unused
func (h *boardingTestHarness) newNoncesSentState(roundID RoundID,
	intents []BoardingIntent) *NoncesSentState {

	h.t.Helper()

	// Generate VTXO requests from intents.
	vtxoReqs := make([]types.VTXORequest, len(intents))
	var totalAmount btcutil.Amount
	for i, intent := range intents {
		vtxoReqs[i] = h.newTestVTXORequestForIntent(intent)
		totalAmount += intent.ChainInfo.Amount
	}

	vtxtTree := h.newTestVTXOTreeForIntents(vtxoReqs)
	commitmentTx := h.newTestCommitmentTx(intents)

	boardingInputIndices := make(map[wire.OutPoint]int)
	for i, intent := range intents {
		boardingInputIndices[intent.Outpoint] = i
	}

	musig2Sessions := make(map[SignerKey]*tree.SignerSession)

	return &NoncesSentState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: intents,
			VTXOs:    vtxoReqs,
		},
		ClientTrees:          make(map[SignerKey]*tree.Tree),
		Musig2Sessions:       musig2Sessions,
		BoardingInputIndices: boardingInputIndices,
	}
}

//nolint:unused
func (h *boardingTestHarness) newNoncesAggregatedState(roundID RoundID,
	intents []BoardingIntent) *NoncesAggregatedState {

	h.t.Helper()

	// Generate VTXO requests from intents.
	vtxoReqs := make([]types.VTXORequest, len(intents))
	var totalAmount btcutil.Amount
	for i, intent := range intents {
		vtxoReqs[i] = h.newTestVTXORequestForIntent(intent)
		totalAmount += intent.ChainInfo.Amount
	}

	vtxtTree := h.newTestVTXOTreeForIntents(vtxoReqs)
	commitmentTx := h.newTestCommitmentTx(intents)

	boardingInputIndices := make(map[wire.OutPoint]int)
	for i, intent := range intents {
		boardingInputIndices[intent.Outpoint] = i
	}

	aggNonces := make(map[tree.TxID]tree.Musig2PubNonce)
	err := vtxtTree.Root.ForEach(func(node *tree.Node) error {
		txid, err := node.TXID()
		if err != nil {
			return err
		}

		var nonce tree.Musig2PubNonce
		_, err = rand.Read(nonce[:])
		if err != nil {
			return err
		}

		aggNonces[txid] = nonce

		return nil
	})
	require.NoError(h.t, err)

	return &NoncesAggregatedState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: intents,
			VTXOs:    vtxoReqs,
		},
		ClientTrees:          make(map[SignerKey]*tree.Tree),
		Musig2Sessions:       make(map[SignerKey]*tree.SignerSession),
		AggNonces:            aggNonces,
		BoardingInputIndices: boardingInputIndices,
	}
}

//nolint:unused
func (h *boardingTestHarness) newPartialSigsSentState(roundID RoundID,
	intents []BoardingIntent) *PartialSigsSentState {

	h.t.Helper()

	// Generate VTXO requests from intents.
	vtxoReqs := make([]types.VTXORequest, len(intents))
	var totalAmount btcutil.Amount
	for i, intent := range intents {
		vtxoReqs[i] = h.newTestVTXORequestForIntent(intent)
		totalAmount += intent.ChainInfo.Amount
	}

	vtxtTree := h.newTestVTXOTreeForIntents(vtxoReqs)
	commitmentTx := h.newTestCommitmentTx(intents)

	boardingInputIndices := make(map[wire.OutPoint]int)
	for i, intent := range intents {
		boardingInputIndices[intent.Outpoint] = i
	}

	return &PartialSigsSentState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: intents,
			VTXOs:    vtxoReqs,
		},
		ClientTrees:          make(map[SignerKey]*tree.Tree),
		Musig2Sessions:       make(map[SignerKey]*tree.SignerSession),
		BoardingInputIndices: boardingInputIndices,
	}
}

func (h *boardingTestHarness) newInputSigSentState(roundID RoundID,
	intents []BoardingIntent) *InputSigSentState {

	h.t.Helper()

	// Generate VTXO requests from intents.
	vtxoReqs := make([]types.VTXORequest, len(intents))
	var totalAmount btcutil.Amount
	for i, intent := range intents {
		vtxoReqs[i] = h.newTestVTXORequestForIntent(intent)
		totalAmount += intent.ChainInfo.Amount
	}

	vtxtTree := h.newTestVTXOTreeForIntents(vtxoReqs)
	commitmentTx := h.newTestCommitmentTx(intents)

	inputSigs := make([]*types.BoardingInputSignature, len(intents))
	for i, intent := range intents {
		sigBytes := make([]byte, schnorr.SignatureSize)
		_, err := rand.Read(sigBytes)
		require.NoError(h.t, err)

		// Parse the random bytes as a signature, or use a dummy if
		// invalid.
		sig, err := schnorr.ParseSignature(sigBytes)
		if err != nil {
			sig = &schnorr.Signature{}
		}

		inputSigs[i] = &types.BoardingInputSignature{
			InputIndex:      i,
			Outpoint:        intent.Outpoint,
			ClientSignature: sig,
		}
	}

	clientTrees := make(map[SignerKey]*tree.Tree)
	for _, vtxoReq := range vtxoReqs {
		signerKey := NewSignerKey(vtxoReq.SigningKey.PubKey)
		clientTrees[signerKey] = vtxtTree
	}

	return &InputSigSentState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: intents,
			VTXOs:    vtxoReqs,
		},
		ClientTrees: clientTrees,
		InputSigs:   inputSigs,
	}
}

func (h *boardingTestHarness) assertOutboxContainsType(msgType string) {
	h.t.Helper()

	found := false
	for _, msg := range h.outboxMessages {
		typeName := fmt.Sprintf("%T", msg)
		if typeName == msgType || typeName == "*round."+msgType {
			found = true
			break
		}
	}
	require.True(
		h.t, found, "outbox does not contain message of type %s",
		msgType,
	)
}

//nolint:unused
func assertOutboxContains[T ClientEvent](h *boardingTestHarness) T {
	h.t.Helper()

	for _, msg := range h.outboxMessages {
		if typed, ok := msg.(T); ok {
			return typed
		}
	}

	var zero T
	h.t.Fatalf("outbox does not contain event of type %T", zero)

	return zero
}

func (h *boardingTestHarness) assertOutboxLen(expected int) {
	h.t.Helper()

	require.Len(
		h.t, h.outboxMessages, expected, "outbox has %d messages, "+
			"expected %d", len(h.outboxMessages), expected,
	)
}

// assertOutboxFirstType asserts that the first outbox message has the given
// concrete type. This is used to pin ordering invariants where a message must
// be emitted before any others (e.g. arming a timeout before dispatching
// forfeit requests).
func (h *boardingTestHarness) assertOutboxFirstType(msgType string) {
	h.t.Helper()

	require.NotEmpty(h.t, h.outboxMessages, "outbox is empty")

	typeName := fmt.Sprintf("%T", h.outboxMessages[0])
	require.Truef(
		h.t, typeName == msgType || typeName == "*round."+msgType,
		"first outbox message is %s, expected %s", typeName, msgType,
	)
}

func (h *boardingTestHarness) clearOutbox() {
	h.outboxMessages = nil
}

// assertTransitionEmitsInternalEvent verifies the transition emitted an
// internal event of the expected type, used for testing FSM event propagation.
func assertTransitionEmitsInternalEvent[T ClientEvent](h *boardingTestHarness,
	transition *ClientStateTransition) {

	h.t.Helper()

	require.True(
		h.t, transition.NewEvents.IsSome(),
		"transition should emit events",
	)

	var found bool
	transition.NewEvents.WhenSome(func(emitted ClientEmittedEvent) {
		for _, evt := range emitted.InternalEvent {
			if _, ok := evt.(T); ok {
				found = true
				break
			}
		}
	})

	var zero T
	require.True(
		h.t, found, "transition does not emit internal event of "+
			"type %T", zero,
	)
}

// realMuSig2Signer provides real MuSig2 cryptographic operations for testing
// instead of mocks, allowing tests to verify actual signature validity and
// protocol correctness.
type realMuSig2Signer struct {
	privKey       *btcec.PrivateKey
	sessions      map[input.MuSig2SessionID]*realSession
	nextSessionID int
}

// realSession maintains cryptographic state for an active MuSig2 signing
// session, tracking nonces and partial signatures until finalization.
type realSession struct {
	info           *input.MuSig2SessionInfo
	nonces         *musig2.Nonces
	musigSession   *musig2.Session
	allNoncesKnown bool
}

func newRealMuSig2Signer(privKey *btcec.PrivateKey) *realMuSig2Signer {
	return &realMuSig2Signer{
		privKey:  privKey,
		sessions: make(map[input.MuSig2SessionID]*realSession),
	}
}

// MuSig2CreateSession creates a new MuSig2 session with real nonce generation.
func (r *realMuSig2Signer) MuSig2CreateSession(version input.MuSig2Version,
	keyLoc keychain.KeyLocator, signers []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, otherNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	var nonces *musig2.Nonces
	var err error
	if localNonces != nil {
		nonces = localNonces
	} else {
		nonces, err = musig2.GenNonces(
			musig2.WithPublicKey(
				r.privKey.PubKey(),
			),
			musig2.WithNonceSecretKeyAux(r.privKey),
		)
		if err != nil {
			return nil, err
		}
	}

	var ctxOpts []musig2.ContextOption
	ctxOpts = append(ctxOpts, musig2.WithKnownSigners(signers))

	if tweaks != nil && len(tweaks.TaprootTweak) > 0 {
		twkCtx := musig2.WithTaprootTweakCtx(tweaks.TaprootTweak)
		ctxOpts = append(ctxOpts, twkCtx)
	}

	ctx, err := musig2.NewContext(r.privKey, true, ctxOpts...)
	if err != nil {
		return nil, err
	}

	musigSession, err := ctx.NewSession()
	if err != nil {
		return nil, err
	}

	sessionID := input.MuSig2SessionID{byte(r.nextSessionID)}
	r.nextSessionID++

	sess := &realSession{
		info: &input.MuSig2SessionInfo{
			SessionID:   sessionID,
			PublicNonce: nonces.PubNonce,
		},
		nonces:       nonces,
		musigSession: musigSession,
	}

	r.sessions[sessionID] = sess

	return sess.info, nil
}

// MuSig2RegisterNonces registers nonces from other signers, accumulating them
// until all participants have contributed their public nonces.
func (r *realMuSig2Signer) MuSig2RegisterNonces(sessionID input.MuSig2SessionID,
	nonces [][musig2.PubNonceSize]byte) (bool, error) {

	session, ok := r.sessions[sessionID]
	if !ok {
		return false, fmt.Errorf("session not found: %v", sessionID)
	}

	var haveAll bool
	var err error
	for _, nonce := range nonces {
		haveAll, err = session.musigSession.RegisterPubNonce(nonce)
		if err != nil {
			return false, err
		}
	}

	session.allNoncesKnown = haveAll

	return haveAll, nil
}

// MuSig2RegisterCombinedNonce registers a pre-aggregated combined nonce for
// the session. This is an alternative to MuSig2RegisterNonces and is used
// when a coordinator has already aggregated all individual nonces.
func (r *realMuSig2Signer) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	nonce [musig2.PubNonceSize]byte,
) error {

	session, ok := r.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session not found: %v", sessionID)
	}

	// Register the combined nonce as if it's from all other participants.
	haveAll, err := session.musigSession.RegisterPubNonce(nonce)
	if err != nil {
		return err
	}

	session.allNoncesKnown = haveAll

	return nil
}

// MuSig2GetCombinedNonce retrieves the combined nonce for the session. This
// will be available after all individual nonces have been registered or a
// combined nonce has been registered.
func (r *realMuSig2Signer) MuSig2GetCombinedNonce(
	sessionID input.MuSig2SessionID) ([musig2.PubNonceSize]byte, error) {

	session, ok := r.sessions[sessionID]
	if !ok {
		return [musig2.PubNonceSize]byte{}, fmt.Errorf("session not "+
			"found: %v", sessionID)
	}

	if !session.allNoncesKnown {
		return [musig2.PubNonceSize]byte{}, fmt.Errorf("combined " +
			"nonce not available: nonces not registered")
	}

	// The combined nonce is available after all nonces are registered.
	// Return the session's public nonce as a proxy.
	return session.nonces.PubNonce, nil
}

// MuSig2Sign creates a partial signature for the given message using
// the session's secret nonce. Requires all participant nonces to be
// registered first.
func (r *realMuSig2Signer) MuSig2Sign(sessionID input.MuSig2SessionID,
	msg [32]byte, cleanup bool) (*musig2.PartialSignature, error) {

	session, ok := r.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session not found: %v", sessionID)
	}

	if !session.allNoncesKnown {
		return nil, fmt.Errorf("not all nonces registered")
	}

	partialSig, err := session.musigSession.Sign(msg)
	if err != nil {
		return nil, err
	}

	return partialSig, nil
}

// MuSig2CombineSig combines all participants' partial signatures into a final
// Schnorr signature that validates against the aggregate public key.
func (r *realMuSig2Signer) MuSig2CombineSig(sessionID input.MuSig2SessionID,
	partialSigs []*musig2.PartialSignature) (*schnorr.Signature, bool,
	error) {

	session, ok := r.sessions[sessionID]
	if !ok {
		return nil, false, fmt.Errorf("session not found: %v",
			sessionID)
	}

	var haveAll bool
	for _, partialSig := range partialSigs {
		var err error
		haveAll, err = session.musigSession.CombineSig(partialSig)
		if err != nil {
			return nil, false, err
		}
	}

	if !haveAll {
		return nil, false, fmt.Errorf("not all partial signatures " +
			"provided")
	}

	finalSig := session.musigSession.FinalSig()
	if finalSig == nil {
		return nil, false, fmt.Errorf("final signature is invalid")
	}

	return finalSig, true, nil
}

func (r *realMuSig2Signer) MuSig2Cleanup(
	sessionID input.MuSig2SessionID) error {

	delete(r.sessions, sessionID)

	return nil
}

// Compile-time check that realMuSig2Signer implements input.MuSig2Signer.
var _ input.MuSig2Signer = (*realMuSig2Signer)(nil)

// realSigningTestHarness extends boardingTestHarness with real MuSig2
// signers for both client and operator roles, enabling full end-to-end
// signature testing.
type realSigningTestHarness struct {
	*boardingTestHarness

	clientSigner   *realMuSig2Signer
	operatorSigner *realMuSig2Signer
}

func newRealSigningTestHarness(t *testing.T) *realSigningTestHarness {
	t.Helper()

	base := newTestHarness(t)

	return &realSigningTestHarness{
		boardingTestHarness: base,
		clientSigner:        newRealMuSig2Signer(base.clientPrivKey),
		operatorSigner:      newRealMuSig2Signer(base.operatorPrivKey),
	}
}

// setupMockWalletForBoardingSigning configures the wallet mock to return valid
// Schnorr signatures for boarding inputs, using the client's real private key
// to ensure signature correctness.
func (h *realSigningTestHarness) setupMockWalletForBoardingSigning() {
	h.t.Helper()

	msgHash := sha256.Sum256([]byte("test-boarding-sig"))
	sig, err := schnorr.Sign(h.clientPrivKey, msgHash[:])
	require.NoError(h.t, err)

	h.wallet.On("SignOutputRaw", mock.Anything, mock.Anything).Return(
		sig, nil,
	)
}

func (h *realSigningTestHarness) setupMockRoundStoreForCommit() {
	h.t.Helper()

	h.roundStore.On(
		"CommitState", mock.Anything, mock.Anything, mock.Anything,
	).Return(nil)
}

// newTestBoardingIntentWithTapscript creates a boarding intent with real
// collaborative/unilateral spend paths for proper VTXO spending tests.
//
//nolint:ll
func (h *realSigningTestHarness) newTestBoardingIntentWithTapscript() BoardingIntent {
	h.t.Helper()

	outpoint := h.newTestOutpoint()

	// Create the VTXO tapscript with collaborative and timeout paths.
	tapscript, err := arkscript.VTXOTapScript(
		h.clientPubKey, h.operatorPubKey, testExitDelay,
	)
	require.NoError(h.t, err)

	// Derive the taproot output key from the tapscript.
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(h.t, err)

	// Create the bech32m address from the taproot key.
	addr, err := btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), &chaincfg.MainNetParams,
	)
	require.NoError(h.t, err)

	// Create signing key descriptor matching the boarding address.
	signingKey := keychain.KeyDescriptor{
		PubKey: h.clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(wallet.BoardingKeyFamily),
			Index:  0,
		},
	}

	// Build the complete boarding address with Tapscript.
	boardingAddr := wallet.BoardingAddress{
		Address:     addr,
		Tapscript:   tapscript,
		KeyDesc:     signingKey,
		OperatorKey: h.operatorPubKey,
		ExitDelay:   testExitDelay,
	}

	return BoardingIntent{
		BoardingIntent: wallet.BoardingIntent{
			Address:  boardingAddr,
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: 100,
				OutPoint:   outpoint,
				Amount:     btcutil.Amount(50000),
			},
			Status: wallet.BoardingStatusConfirmed,
		},
		Request: types.BoardingRequest{
			Outpoint:    &outpoint,
			ClientKey:   h.clientPubKey,
			OperatorKey: h.operatorPubKey,
		},
	}
}

// newCommitmentTxForIntents builds a commitment TX with inputs from boarding
// intents in the exact order needed for boardingInputIndices mapping. Returns
// a PSBT with WitnessUtxo populated for all inputs.
func (h *realSigningTestHarness) newCommitmentTxForIntents(
	intents []BoardingIntent, vtxtTree *tree.Tree) *psbt.Packet {

	h.t.Helper()

	tx := wire.NewMsgTx(3)

	// Add inputs from intents in order.
	for _, intent := range intents {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: intent.Outpoint,
		})
	}

	// Add batch output matching tree.
	tx.AddTxOut(vtxtTree.BatchOutput)

	// Add ephemeral anchor (BIP 431).
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_TRUE, txscript.OP_RETURN},
	})

	// Create PSBT with WitnessUtxo for each input.
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(h.t, err)

	for i, intent := range intents {
		addr := intent.Address.Address
		pkScript := addr.ScriptAddress()
		amt := intent.ChainInfo.Amount

		packet.Inputs[i] = psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    int64(amt),
				PkScript: pkScript,
			},
		}
	}

	return packet
}

// generateValidTreeSignatures creates valid Schnorr signatures for nodes.
//
// This is a simplified implementation for testing. In production, signatures
// are generated by a coordinator (server) that combines client + operator
// partials. For tests, we simulate this by:
//
// 1. Creating manual MuSig2 sessions for each tree node.
// 2. Going through the full nonce exchange and signing protocol.
// 3. Combining partials to produce final Schnorr signatures.
func (h *realSigningTestHarness) generateValidTreeSignatures(
	vtxtTree *tree.Tree) (map[tree.TxID]*schnorr.Signature, error) {

	h.t.Helper()

	sweepRoot := vtxtTree.SweepTapscriptRoot

	// Collect all nodes with their txids and sighashes.
	type nodeInfo struct {
		txid    chainhash.Hash
		sigHash [32]byte
	}
	var nodes []nodeInfo

	fetcher, err := vtxtTree.Root.PrevOutputFetcher(vtxtTree.BatchOutput)
	if err != nil {
		return nil, fmt.Errorf("failed to create prev output "+
			"fetcher: %w", err)
	}

	err = vtxtTree.Root.ForEach(func(node *tree.Node) error {
		tx, txErr := node.ToTx()
		if txErr != nil {
			return txErr
		}

		sigHash, sigErr := computeTreeNodeSigHash(node, tx, fetcher)
		if sigErr != nil {
			return sigErr
		}

		nodes = append(nodes, nodeInfo{
			txid:    tx.TxHash(),
			sigHash: sigHash,
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to collect node info: %w", err)
	}

	// Reset signers for fresh sessions.
	h.clientSigner = newRealMuSig2Signer(h.clientPrivKey)
	h.operatorSigner = newRealMuSig2Signer(h.operatorPrivKey)

	// Process each node and build a map of signatures by txid.
	finalSigs := make(map[tree.TxID]*schnorr.Signature, len(nodes))
	for _, ni := range nodes {
		sig, sigErr := h.generateSignatureForNode(
			ni.sigHash, sweepRoot,
		)
		if sigErr != nil {
			return nil, fmt.Errorf("failed to sign node %s: %w",
				ni.txid, sigErr)
		}
		finalSigs[ni.txid] = sig
	}

	return finalSigs, nil
}

// generateSignatureForNode creates a complete MuSig2 signature for a single
// tree node by going through the full 2-party signing protocol.
func (h *realSigningTestHarness) generateSignatureForNode(sigHash [32]byte,
	sweepRoot []byte) (*schnorr.Signature, error) {

	cosigners := []*btcec.PublicKey{h.clientPubKey, h.operatorPubKey}

	// Generate nonces upfront for both parties.
	clientNonces, err := musig2.GenNonces(
		musig2.WithPublicKey(
			h.clientPrivKey.PubKey(),
		),
		musig2.WithNonceSecretKeyAux(h.clientPrivKey),
	)
	if err != nil {
		return nil, fmt.Errorf("client gen nonces: %w", err)
	}

	operatorNonces, err := musig2.GenNonces(
		musig2.WithPublicKey(
			h.operatorPrivKey.PubKey(),
		),
		musig2.WithNonceSecretKeyAux(h.operatorPrivKey),
	)
	if err != nil {
		return nil, fmt.Errorf("operator gen nonces: %w", err)
	}

	// Create context options for both parties.
	ctxOpts := []musig2.ContextOption{
		musig2.WithKnownSigners(cosigners),
	}
	if len(sweepRoot) > 0 {
		ctxOpts = append(ctxOpts, musig2.WithTaprootTweakCtx(sweepRoot))
	}

	// Create contexts for both parties.
	clientCtx, err := musig2.NewContext(h.clientPrivKey, true, ctxOpts...)
	if err != nil {
		return nil, fmt.Errorf("client context: %w", err)
	}

	operatorCtx, err := musig2.NewContext(
		h.operatorPrivKey, true, ctxOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("operator context: %w", err)
	}

	// Create sessions with pre-generated nonces.
	clientSession, err := clientCtx.NewSession(
		musig2.WithPreGeneratedNonce(clientNonces),
	)
	if err != nil {
		return nil, fmt.Errorf("client session: %w", err)
	}

	operatorSession, err := operatorCtx.NewSession(
		musig2.WithPreGeneratedNonce(operatorNonces),
	)
	if err != nil {
		return nil, fmt.Errorf("operator session: %w", err)
	}

	// Exchange and register nonces.
	haveAll, err := clientSession.RegisterPubNonce(operatorNonces.PubNonce)
	if err != nil {
		return nil, fmt.Errorf("client register operator nonce: %w",
			err)
	}
	if !haveAll {
		return nil, fmt.Errorf("client missing nonces after " +
			"registration")
	}

	haveAll, err = operatorSession.RegisterPubNonce(clientNonces.PubNonce)
	if err != nil {
		return nil, fmt.Errorf("operator register client nonce: %w",
			err)
	}
	if !haveAll {
		return nil, fmt.Errorf("operator missing nonces after " +
			"registration")
	}

	// Both parties sign the same message.
	clientPartial, err := clientSession.Sign(sigHash)
	if err != nil {
		return nil, fmt.Errorf("client sign: %w", err)
	}

	operatorPartial, err := operatorSession.Sign(sigHash)
	if err != nil {
		return nil, fmt.Errorf("operator sign: %w", err)
	}

	// Combine partial signatures on client side.
	// After Sign(), the client's own partial is already combined.
	haveAll, err = clientSession.CombineSig(operatorPartial)
	if err != nil {
		return nil, fmt.Errorf("combine operator partial: %w", err)
	}
	if !haveAll {
		return nil, fmt.Errorf("incomplete after combining operator " +
			"partial")
	}

	finalSig := clientSession.FinalSig()
	if finalSig == nil {
		// Try combining on operator side instead.
		haveAll, err = operatorSession.CombineSig(clientPartial)
		if err != nil {
			return nil, fmt.Errorf("combine client partial: %w",
				err)
		}
		if !haveAll {
			return nil, fmt.Errorf("incomplete after combining " +
				"client partial")
		}
		finalSig = operatorSession.FinalSig()
	}

	if finalSig == nil {
		return nil, fmt.Errorf("final signature is invalid")
	}

	return finalSig, nil
}

// computeTreeNodeSigHash computes the Taproot signature hash for a tree node.
func computeTreeNodeSigHash(node *tree.Node, tx *wire.MsgTx,
	fetcher txscript.PrevOutputFetcher) ([32]byte, error) {

	sigHashes := txscript.NewTxSigHashes(tx, fetcher)

	hashBytes, err := txscript.CalcTaprootSignatureHash(
		sigHashes, txscript.SigHashDefault, tx, 0, fetcher,
	)
	if err != nil {
		return [32]byte{}, err
	}

	var result [32]byte
	copy(result[:], hashBytes)

	return result, nil
}

// newTestForfeitTx creates a minimal valid forfeit transaction structure.
// The forfeit tx has 2 inputs (VTXO + connector) and 2 outputs (penalty +
// anchor).
func (h *boardingTestHarness) newTestForfeitTx(
	vtxoOutpoint, connectorOutpoint wire.OutPoint,
	serverForfeitScript []byte) *wire.MsgTx {

	h.t.Helper()

	tx := wire.NewMsgTx(2)

	// Input 0: VTXO being forfeited.
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: vtxoOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	// Input 1: Connector output.
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: connectorOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	// Output 0: Penalty to server (forfeited VTXO + connector dust).
	tx.AddTxOut(&wire.TxOut{
		Value:    testForfeitVTXOValue + testConnectorDustValue,
		PkScript: serverForfeitScript,
	})

	// Output 1: Standard P2A anchor (arkscript.AnchorPkScript).
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: arkscript.AnchorPkScript,
	})

	return tx
}

// newForfeitCollectingState creates a ForfeitSignaturesCollectingState with
// the given parameters for testing forfeit signature collection.
func (h *boardingTestHarness) newForfeitCollectingState(roundID RoundID,
	intents Intents,
	expectedForfeits map[wire.OutPoint]*ConnectorLeafInfo,
) *ForfeitSignaturesCollectingState {

	h.t.Helper()

	vtxtTree, _ := h.newTestVTXOTree(1)
	commitmentTx := h.newTestCommitmentTx(intents.Boarding)

	boardingInputIndices := make(map[wire.OutPoint]int)
	for i, intent := range intents.Boarding {
		boardingInputIndices[intent.Outpoint] = i
	}

	return &ForfeitSignaturesCollectingState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents:              intents,
		ForfeitKey:           h.forfeitPubKey,
		ClientTrees:          make(map[SignerKey]*tree.Tree),
		BoardingInputIndices: boardingInputIndices,
		ExpectedForfeits:     expectedForfeits,
		CollectedForfeits: make(
			map[wire.OutPoint]*ForfeitSignatureResponse,
		),
	}
}

// forfeitScript returns the BIP-86 key-spend penalty output script for the
// harness's per-round forfeit key, matching what the FSM derives from
// ForfeitKey when validating and building forfeit transactions.
func (h *boardingTestHarness) forfeitScript() []byte {
	h.t.Helper()

	script, err := forfeitPenaltyScript(h.forfeitPubKey)
	require.NoError(h.t, err)

	return script
}

// newMinimalVTXOTree creates a minimal valid VTXO tree for testing. This is
// used for refresh-only rounds where we need a valid tree structure but don't
// have specific VTXO requests from boarding intents.
func (h *boardingTestHarness) newMinimalVTXOTree() *tree.Tree {
	h.t.Helper()

	// Create a minimal tree with one leaf.
	const leafAmount = btcutil.Amount(50000)

	vtxoScript, err := txscript.PayToTaprootScript(h.clientPubKey)
	require.NoError(h.t, err)

	leaves := []tree.LeafDescriptor{
		{
			PkScript:    vtxoScript,
			Amount:      leafAmount,
			CoSignerKey: h.clientPubKey,
		},
	}

	batchOutpoint := h.newTestOutpoint()

	batchPkScript, err := txscript.PayToTaprootScript(h.operatorPubKey)
	require.NoError(h.t, err)

	batchOutput := &wire.TxOut{
		Value:    int64(leafAmount),
		PkScript: batchPkScript,
	}

	sweepRoot := sha256.Sum256([]byte("test-sweep-root"))

	vtxtTree, err := tree.NewTree(
		batchOutpoint, batchOutput, leaves, h.operatorPubKey,
		sweepRoot[:], 2,
	)
	require.NoError(h.t, err)

	return vtxtTree
}
