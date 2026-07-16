package round

import (
	"context"
	"fmt"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/bip322"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// realSchnorrWallet wraps MockClientWallet with real Schnorr signing
// for taproot key-path and script-path spends. This enables
// end-to-end BIP-322 verification in tests while still using the
// mock for unneeded MuSig2 interface methods.
type realSchnorrWallet struct {
	*MockClientWallet

	// keys maps compressed pubkey bytes to private keys.
	keys map[[33]byte]*btcec.PrivateKey
}

// newRealSchnorrWallet creates a signing wallet backed by the
// provided private keys. All other ClientWallet methods delegate to
// the embedded mock.
func newRealSchnorrWallet(mockWallet *MockClientWallet,
	keys ...*btcec.PrivateKey) *realSchnorrWallet {

	keyMap := make(map[[33]byte]*btcec.PrivateKey, len(keys))
	for _, k := range keys {
		var kb [33]byte
		copy(kb[:], k.PubKey().SerializeCompressed())
		keyMap[kb] = k
	}

	return &realSchnorrWallet{
		MockClientWallet: mockWallet,
		keys:             keyMap,
	}
}

// SignOutputRaw produces real Schnorr signatures for taproot
// key-path (BIP-86) and script-path spends.
func (w *realSchnorrWallet) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	pubBytes := signDesc.KeyDesc.PubKey.SerializeCompressed()

	var kb [33]byte
	copy(kb[:], pubBytes)

	privKey, ok := w.keys[kb]
	if !ok {
		return nil, fmt.Errorf("realSchnorrWallet: no key for "+
			"pubkey %x", pubBytes)
	}

	switch signDesc.SignMethod {
	case input.TaprootKeySpendBIP0086SignMethod:
		// BIP-86 key-path: tweak the private key with just
		// the pubkey (no script tree).
		tweakedKey := txscript.TweakTaprootPrivKey(
			*privKey, nil,
		)

		sigHash, err := txscript.CalcTaprootSignatureHash(
			signDesc.SigHashes, signDesc.HashType, tx,
			signDesc.InputIndex, signDesc.PrevOutputFetcher,
		)
		if err != nil {
			return nil, fmt.Errorf("calc taproot sighash: %w", err)
		}

		return schnorr.Sign(tweakedKey, sigHash)

	case input.TaprootScriptSpendSignMethod:
		// Script-path: sign with the raw private key using
		// the leaf hash in the sighash computation.
		leaf := txscript.NewBaseTapLeaf(
			signDesc.WitnessScript,
		)

		sigHash, err := txscript.CalcTapscriptSignaturehash(
			signDesc.SigHashes, signDesc.HashType, tx,
			signDesc.InputIndex, signDesc.PrevOutputFetcher, leaf,
		)
		if err != nil {
			return nil, fmt.Errorf("calc tapscript sighash: %w",
				err)
		}

		return schnorr.Sign(privKey, sigHash)

	default:
		return nil, fmt.Errorf("unsupported sign method: %v",
			signDesc.SignMethod)
	}
}

// joinAuthTestFixture bundles all test data needed for
// buildJoinRoundAuth tests.
//
//nolint:containedctx
type joinAuthTestFixture struct {
	ctx context.Context

	// Real key pairs.
	clientPrivKey     *btcec.PrivateKey
	operatorPrivKey   *btcec.PrivateKey
	identifierPrivKey *btcec.PrivateKey

	// Signing wallet and environment.
	wallet        *realSchnorrWallet
	env           *ClientEnvironment
	signingHeight uint32
}

// newJoinAuthTestFixture creates a fixture with real keys and a
// signing wallet configured for BIP-322 auth tests.
func newJoinAuthTestFixture(t *testing.T) *joinAuthTestFixture {
	t.Helper()

	clientPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	identifierPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	mockWallet := &MockClientWallet{}
	signingWallet := newRealSchnorrWallet(
		mockWallet, clientPrivKey, identifierPrivKey,
	)

	const (
		testStartHeight   uint32 = 100
		testSigningHeight uint32 = 125
	)

	env := &ClientEnvironment{
		Wallet:      signingWallet,
		ChainParams: &chaincfg.RegressionNetParams,
		Log:         btclog.Disabled,
		StartHeight: testStartHeight,
		QueryBestHeight: func(_ context.Context) (uint32, error) {
			return testSigningHeight, nil
		},
	}

	return &joinAuthTestFixture{
		ctx:               t.Context(),
		clientPrivKey:     clientPrivKey,
		operatorPrivKey:   operatorPrivKey,
		identifierPrivKey: identifierPrivKey,
		wallet:            signingWallet,
		env:               env,
		signingHeight:     testSigningHeight,
	}
}

// newBoardingIntent creates a boarding intent backed by real tapscript
// keys so that the BIP-322 timeout-path witness can be verified.
func (f *joinAuthTestFixture) newBoardingIntent(t *testing.T,
	amount btcutil.Amount) BoardingIntent {

	t.Helper()

	exitDelay := uint32(144)

	tapscript, err := arkscript.VTXOTapScript(
		f.clientPrivKey.PubKey(), f.operatorPrivKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	taprootKey, err := tapscript.TaprootKey()
	require.NoError(t, err)

	address, err := btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x01,
			0x02,
			0x03,
		},
		Index: 0,
	}

	return BoardingIntent{
		BoardingIntent: wallet.BoardingIntent{
			Address: wallet.BoardingAddress{
				Address:   address,
				Tapscript: tapscript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: f.clientPrivKey.PubKey(),
				},
				OperatorKey: f.operatorPrivKey.PubKey(),
				ExitDelay:   exitDelay,
			},
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				Amount: amount,
			},
		},
		Request: types.BoardingRequest{
			Outpoint: &outpoint,
			PolicyTemplate: func() []byte {
				policy := stdTpl(
					t, f.clientPrivKey.PubKey(),
					f.operatorPrivKey.PubKey(), exitDelay,
				)

				return policy
			}(),
		},
	}
}

// newVTXORequest creates a fully-populated VTXORequest with all
// fields required for TLV encoding.
func (f *joinAuthTestFixture) newVTXORequest(t *testing.T,
	amount btcutil.Amount) types.VTXORequest {

	t.Helper()

	expiry := uint32(288)

	tapScript, err := arkscript.VTXOTapScript(
		f.clientPrivKey.PubKey(), f.operatorPrivKey.PubKey(), expiry,
	)
	require.NoError(t, err)

	taprootKey, err := tapScript.TaprootKey()
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(t, err)

	// Generate a distinct signing key for the VTXO.
	signingKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		f.clientPrivKey.PubKey(), f.operatorPrivKey.PubKey(), expiry,
	)
	require.NoError(t, err)

	return types.VTXORequest{
		Amount:         amount,
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		Expiry:         expiry,
		ClientKey:      f.clientPrivKey.PubKey(),
		OperatorKey:    f.operatorPrivKey.PubKey(),
		SigningKey: keychain.KeyDescriptor{
			PubKey: signingKey.PubKey(),
		},
	}
}

// identifierKeyDesc returns the key descriptor for the identifier key.
func (f *joinAuthTestFixture) identifierKeyDesc() keychain.KeyDescriptor {
	return keychain.KeyDescriptor{
		PubKey: f.identifierPrivKey.PubKey(),
	}
}

// validateAuth decodes the auth payload and runs BIP-322 verification with
// the supplied proof prevouts.
func (f *joinAuthTestFixture) validateAuth(t *testing.T,
	auth *types.JoinRoundAuth,
	proofPrevOuts map[wire.OutPoint]*wire.TxOut) bip322.VerificationResult {

	t.Helper()

	sig, err := bip322.DecodeSig(auth.Signature)
	require.NoError(t, err)

	challenge, err := bip322.JoinRoundMessageChallenge(
		f.identifierPrivKey.PubKey(),
	)
	require.NoError(t, err)

	intent := &bip322.Intent{
		Payload:    auth.Message,
		ValidFrom:  auth.ValidFrom,
		ValidUntil: auth.ValidUntil,
	}

	intentMessage, err := intent.SigningMessage()
	require.NoError(t, err)

	return bip322.ValidateAuthPkg(&bip322.AuthPkg{
		Message:          intentMessage,
		MessageChallenge: challenge,
		Sig:              sig,
		ProofPrevOutputs: proofPrevOuts,
	})
}

// verifyAuth decodes the auth and validates it against the BIP-322
// validation engine.
func (f *joinAuthTestFixture) verifyAuth(t *testing.T,
	auth *types.JoinRoundAuth,
	proofPrevOuts map[wire.OutPoint]*wire.TxOut) {

	t.Helper()

	result := f.validateAuth(t, auth, proofPrevOuts)

	require.Equal(
		t, bip322.VerificationStateValid, result.State, "BIP-322 "+
			"validation failed: %s", result.Reason,
	)
}

// TestBuildJoinRoundAuthBoardingOnly verifies that buildJoinRoundAuth
// produces a BIP-322 auth package that passes full script validation
// when a single boarding intent is present.
func TestBuildJoinRoundAuthBoardingOnly(t *testing.T) {
	t.Parallel()

	f := newJoinAuthTestFixture(t)

	boardingAmount := btcutil.Amount(50000)
	intent := f.newBoardingIntent(t, boardingAmount)

	vtxoReqs := []types.VTXORequest{
		f.newVTXORequest(t, 49000),
	}
	intents := Intents{
		Boarding: []BoardingIntent{
			intent,
		},
		VTXOs: vtxoReqs,
	}

	auth, err := buildJoinRoundAuth(
		f.ctx, f.env, f.identifierKeyDesc(), intents, vtxoReqs, nil,
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, auth)
	require.NotEmpty(t, auth.Message)
	require.NotEmpty(t, auth.Signature)

	// Ensure join-auth validity metadata is carried in the auth
	// payload, not encoded in to_sign input 0 lock metadata.
	require.Equal(t, f.signingHeight, auth.ValidFrom)
	require.Equal(
		t, joinAuthValidUntil(f.signingHeight), auth.ValidUntil,
	)

	sig, err := bip322.DecodeSig(auth.Signature)
	require.NoError(t, err)

	require.Equal(t, int32(2), sig.ToSign.Version)
	require.Equal(t, uint32(0), sig.ToSign.LockTime)
	require.NotEmpty(t, sig.ToSign.TxIn)
	require.Equal(t, uint32(0), sig.ToSign.TxIn[0].Sequence)

	// Build the proof prevouts needed by the validator. Each
	// boarding UTXO must be supplied so the script engine can
	// verify the timeout-path witness.
	pkScript, err := txscript.PayToAddrScript(
		intent.Address.Address,
	)
	require.NoError(t, err)

	proofPrevOuts := map[wire.OutPoint]*wire.TxOut{
		intent.Outpoint: {
			Value:    int64(boardingAmount),
			PkScript: pkScript,
		},
	}

	f.verifyAuth(t, auth, proofPrevOuts)
}

// TestBuildJoinRoundAuthRejectsTamperedSig verifies that round-level auth
// verification rejects payloads whose signature transaction was modified
// after signing.
func TestBuildJoinRoundAuthRejectsTamperedSig(t *testing.T) {
	t.Parallel()

	f := newJoinAuthTestFixture(t)

	boardingAmount := btcutil.Amount(50000)
	intent := f.newBoardingIntent(t, boardingAmount)

	vtxoReqs := []types.VTXORequest{
		f.newVTXORequest(t, 49000),
	}
	intents := Intents{
		Boarding: []BoardingIntent{
			intent,
		},
		VTXOs: vtxoReqs,
	}

	auth, err := buildJoinRoundAuth(
		f.ctx, f.env, f.identifierKeyDesc(), intents, vtxoReqs, nil,
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, auth)

	pkScript, err := txscript.PayToAddrScript(intent.Address.Address)
	require.NoError(t, err)

	proofPrevOuts := map[wire.OutPoint]*wire.TxOut{
		intent.Outpoint: {
			Value:    int64(boardingAmount),
			PkScript: pkScript,
		},
	}

	// Establish baseline validity before tampering.
	f.verifyAuth(t, auth, proofPrevOuts)

	sig, err := bip322.DecodeSig(auth.Signature)
	require.NoError(t, err)
	require.NotEmpty(t, sig.ToSign.TxIn)
	require.NotEmpty(t, sig.ToSign.TxIn[0].Witness)
	require.NotEmpty(t, sig.ToSign.TxIn[0].Witness[0])

	// Flip one byte in the witness signature, then re-encode into wire
	// format to simulate an in-transit payload mutation.
	sig.ToSign.TxIn[0].Witness[0][0] ^= 0x01

	tamperedRawSig, err := sig.Encode()
	require.NoError(t, err)

	tamperedAuth := &types.JoinRoundAuth{
		Message:    append([]byte(nil), auth.Message...),
		ValidFrom:  auth.ValidFrom,
		ValidUntil: auth.ValidUntil,
		Signature:  tamperedRawSig,
	}

	result := f.validateAuth(t, tamperedAuth, proofPrevOuts)
	require.Equal(t, bip322.VerificationStateInvalid, result.State)

	// Mutating intent metadata without re-signing must also fail.
	tamperedIntent := &types.JoinRoundAuth{
		Message:    append([]byte(nil), auth.Message...),
		ValidFrom:  auth.ValidFrom,
		ValidUntil: auth.ValidUntil + 1,
		Signature:  append([]byte(nil), auth.Signature...),
	}

	intentResult := f.validateAuth(t, tamperedIntent, proofPrevOuts)
	require.Equal(t, bip322.VerificationStateInvalid, intentResult.State)
}

// TestBuildJoinRoundAuthWithForfeit verifies that buildJoinRoundAuth
// produces a valid BIP-322 package when both boarding and forfeit
// inputs are present.
func TestBuildJoinRoundAuthWithForfeit(t *testing.T) {
	t.Parallel()

	f := newJoinAuthTestFixture(t)

	// Set up the VTXO store mock for forfeit lookups.
	vtxoStore := &MockVTXOStore{}
	f.env.VTXOStore = vtxoStore

	// Create a boarding intent.
	boardingAmount := btcutil.Amount(50000)
	intent := f.newBoardingIntent(t, boardingAmount)

	// Create a VTXO to forfeit. The VTXO uses the same keys as
	// the boarding intent for simplicity; the important thing is
	// that the script engine can verify both witnesses.
	vtxoExpiry := uint32(288)
	vtxoOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
		},
		Index: 1,
	}
	vtxoAmount := btcutil.Amount(30000)

	vtxoTapscript, err := arkscript.VTXOTapScript(
		f.clientPrivKey.PubKey(), f.operatorPrivKey.PubKey(),
		vtxoExpiry,
	)
	require.NoError(t, err)

	vtxoTaprootKey, err := vtxoTapscript.TaprootKey()
	require.NoError(t, err)

	vtxoPkScript, err := txscript.PayToTaprootScript(
		vtxoTaprootKey,
	)
	require.NoError(t, err)

	vtxo := &ClientVTXO{
		Outpoint: vtxoOutpoint,
		Amount:   vtxoAmount,
		PolicyTemplate: func() []byte {
			policy, err := arkscript.EncodeStandardVTXOTemplate(
				f.clientPrivKey.PubKey(),
				f.operatorPrivKey.PubKey(), vtxoExpiry,
			)
			require.NoError(t, err)

			return policy
		}(),
		PkScript: vtxoPkScript,
		Expiry:   vtxoExpiry,
		OwnerKey: keychain.KeyDescriptor{
			PubKey: f.clientPrivKey.PubKey(),
		},
		OperatorKey: f.operatorPrivKey.PubKey(),
	}

	vtxoStore.On(
		"GetVTXO", mock.Anything, vtxoOutpoint,
	).Return(vtxo, nil)

	vtxoReqs := []types.VTXORequest{
		f.newVTXORequest(t, 70000),
	}
	intents := Intents{
		Boarding: []BoardingIntent{
			intent,
		},
		VTXOs: vtxoReqs,
	}

	forfeitReqs := []*types.ForfeitRequest{{
		VTXOOutpoint: &vtxoOutpoint,
	}}

	auth, err := buildJoinRoundAuth(
		f.ctx, f.env, f.identifierKeyDesc(), intents, vtxoReqs,
		forfeitReqs, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, auth)

	// Build proof prevouts for both the boarding and forfeit
	// inputs.
	boardingPkScript, err := txscript.PayToAddrScript(
		intent.Address.Address,
	)
	require.NoError(t, err)

	proofPrevOuts := map[wire.OutPoint]*wire.TxOut{
		intent.Outpoint: {
			Value:    int64(boardingAmount),
			PkScript: boardingPkScript,
		},
		vtxoOutpoint: {
			Value:    int64(vtxoAmount),
			PkScript: vtxoPkScript,
		},
	}

	f.verifyAuth(t, auth, proofPrevOuts)
}

// TestBuildJoinRoundAuthForfeitOnly verifies auth generation when
// there are no boarding intents and only forfeit inputs.
func TestBuildJoinRoundAuthForfeitOnly(t *testing.T) {
	t.Parallel()

	f := newJoinAuthTestFixture(t)

	vtxoStore := &MockVTXOStore{}
	f.env.VTXOStore = vtxoStore

	vtxoExpiry := uint32(288)
	vtxoOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xcc,
			0xdd,
		},
		Index: 0,
	}
	vtxoAmount := btcutil.Amount(40000)

	vtxoTapscript, err := arkscript.VTXOTapScript(
		f.clientPrivKey.PubKey(), f.operatorPrivKey.PubKey(),
		vtxoExpiry,
	)
	require.NoError(t, err)

	vtxoTaprootKey, err := vtxoTapscript.TaprootKey()
	require.NoError(t, err)

	vtxoPkScript, err := txscript.PayToTaprootScript(
		vtxoTaprootKey,
	)
	require.NoError(t, err)

	vtxo := &ClientVTXO{
		Outpoint: vtxoOutpoint,
		Amount:   vtxoAmount,
		PolicyTemplate: func() []byte {
			policy, err := arkscript.EncodeStandardVTXOTemplate(
				f.clientPrivKey.PubKey(),
				f.operatorPrivKey.PubKey(), vtxoExpiry,
			)
			require.NoError(t, err)

			return policy
		}(),
		PkScript: vtxoPkScript,
		Expiry:   vtxoExpiry,
		OwnerKey: keychain.KeyDescriptor{
			PubKey: f.clientPrivKey.PubKey(),
		},
		OperatorKey: f.operatorPrivKey.PubKey(),
	}

	vtxoStore.On(
		"GetVTXO", mock.Anything, vtxoOutpoint,
	).Return(vtxo, nil)

	vtxoReqs := []types.VTXORequest{
		f.newVTXORequest(t, 39000),
	}
	intents := Intents{
		VTXOs: vtxoReqs,
	}

	forfeitReqs := []*types.ForfeitRequest{{
		VTXOOutpoint: &vtxoOutpoint,
	}}

	auth, err := buildJoinRoundAuth(
		f.ctx, f.env, f.identifierKeyDesc(), intents, vtxoReqs,
		forfeitReqs, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, auth)

	proofPrevOuts := map[wire.OutPoint]*wire.TxOut{
		vtxoOutpoint: {
			Value:    int64(vtxoAmount),
			PkScript: vtxoPkScript,
		},
	}

	f.verifyAuth(t, auth, proofPrevOuts)
}

// TestComputeTotalForfeitAmountFallsBackToEmbedded verifies that the embedded
// Amount is used as a fallback when no VTXOStore is available (nil store).
func TestComputeTotalForfeitAmountFallsBackToEmbedded(t *testing.T) {
	t.Parallel()

	total, err := computeTotalForfeitAmount(
		t.Context(),
		nil,
		[]types.ForfeitRequest{{
			VTXOOutpoint: &wire.OutPoint{},
			Amount:       123,
		}, {
			VTXOOutpoint: &wire.OutPoint{
				Index: 1,
			},
			Amount: 456,
		}},
	)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(579), total)
}

// TestComputeTotalForfeitAmountStoreOverridesEmbedded verifies that
// when a VTXOStore is provided, the store amount is used even if the
// embedded Amount field is set. This prevents callers from inflating
// the forfeit total.
func TestComputeTotalForfeitAmountStoreOverridesEmbedded(t *testing.T) {
	t.Parallel()

	outpoint1 := wire.OutPoint{Hash: chainhash.Hash{0x01}}
	outpoint2 := wire.OutPoint{Hash: chainhash.Hash{0x02}, Index: 1}

	store := &MockVTXOStore{}
	store.On("GetVTXO", mock.Anything, outpoint1).Return(
		&ClientVTXO{Amount: 100}, nil,
	)
	store.On("GetVTXO", mock.Anything, outpoint2).Return(
		&ClientVTXO{Amount: 200}, nil,
	)

	// The embedded amounts (999, 888) should be ignored when a store
	// is available. The store values (100, 200) should be used.
	total, err := computeTotalForfeitAmount(
		t.Context(),
		store,
		[]types.ForfeitRequest{
			{VTXOOutpoint: &outpoint1, Amount: 999},
			{VTXOOutpoint: &outpoint2, Amount: 888},
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, btcutil.Amount(300), total,
		"store amounts (100+200) should override embedded (999+888)",
	)
}

// TestSortedForfeitRequestsPreservesAmount verifies that the Amount
// field survives the sort.
func TestSortedForfeitRequestsPreservesAmount(t *testing.T) {
	t.Parallel()

	// Two forfeits with amounts, in reverse outpoint order.
	forfeits := []types.ForfeitRequest{
		{
			VTXOOutpoint: &wire.OutPoint{
				Hash: chainhash.Hash{
					0xff,
				},
				Index: 1,
			},
			Amount: 300,
		},
		{
			VTXOOutpoint: &wire.OutPoint{
				Hash: chainhash.Hash{
					0x01,
				},
				Index: 0,
			},
			Amount: 700,
		},
	}

	sorted, err := sortedForfeitRequests(forfeits)
	require.NoError(t, err)
	require.Len(t, sorted, 2)

	// After sorting, the 0x01 outpoint should come first.
	require.Equal(t,
		chainhash.Hash{0x01}, sorted[0].VTXOOutpoint.Hash,
	)
	require.Equal(t, btcutil.Amount(700), sorted[0].Amount)

	require.Equal(t,
		chainhash.Hash{0xff}, sorted[1].VTXOOutpoint.Hash,
	)
	require.Equal(t, btcutil.Amount(300), sorted[1].Amount)
}

// TestBuildJoinRoundAuthRejectsNoInputs verifies that
// buildJoinRoundAuth returns an error when no proof-of-funds inputs
// are provided.
func TestBuildJoinRoundAuthRejectsNoInputs(t *testing.T) {
	t.Parallel()

	f := newJoinAuthTestFixture(t)

	vtxoReqs := []types.VTXORequest{
		f.newVTXORequest(t, 10000),
	}
	intents := Intents{
		VTXOs: vtxoReqs,
	}

	_, err := buildJoinRoundAuth(
		f.ctx, f.env, f.identifierKeyDesc(), intents, vtxoReqs, nil,
		nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least one proof-of-funds")
}

// TestBuildJoinRoundAuthOperatorFundedVirtualChannelNoInputs verifies that a
// receive-channel intent can authenticate with only the identifier signature:
// the operator is funding the backing VTXO, so there are no client-owned
// proof-of-funds inputs to include.
func TestBuildJoinRoundAuthOperatorFundedVirtualChannelNoInputs(t *testing.T) {
	t.Parallel()

	f := newJoinAuthTestFixture(t)

	req := f.newVTXORequest(t, 101000)
	req.VirtualChannel = &types.VirtualChannelIntent{
		Capacity: btcutil.Amount(100000),
	}

	vtxoReqs := []types.VTXORequest{req}
	intents := Intents{
		VTXOs: vtxoReqs,
	}

	auth, err := buildJoinRoundAuth(
		f.ctx, f.env, f.identifierKeyDesc(), intents, vtxoReqs, nil,
		nil,
	)
	require.NoError(t, err)

	f.verifyAuth(t, auth, nil)
}

// TestBuildJoinRoundAuthRejectsMissingValidFromQuery verifies that
// buildJoinRoundAuth returns an error when the environment cannot query the
// current height for intent validity metadata.
func TestBuildJoinRoundAuthRejectsMissingValidFromQuery(t *testing.T) {
	t.Parallel()

	f := newJoinAuthTestFixture(t)
	f.env.QueryBestHeight = nil

	boardingAmount := btcutil.Amount(50000)
	intent := f.newBoardingIntent(t, boardingAmount)

	vtxoReqs := []types.VTXORequest{
		f.newVTXORequest(t, 49000),
	}
	intents := Intents{
		Boarding: []BoardingIntent{
			intent,
		},
		VTXOs: vtxoReqs,
	}

	_, err := buildJoinRoundAuth(
		f.ctx, f.env, f.identifierKeyDesc(), intents, vtxoReqs, nil,
		nil,
	)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		"valid-from query function must be provided",
	)
}
