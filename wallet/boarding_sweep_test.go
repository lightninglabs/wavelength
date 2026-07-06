package wallet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// testBoardingSweepWallet is a deterministic SweepSigner test double.
type testBoardingSweepWallet struct{}

// NewWalletPkScript returns a deterministic destination script.
func (w *testBoardingSweepWallet) NewWalletPkScript(context.Context) ([]byte,
	error) {

	return []byte{txscript.OP_TRUE}, nil
}

// SignOutputRaw returns a dummy schnorr signature.
func (w *testBoardingSweepWallet) SignOutputRaw(*wire.MsgTx,
	*input.SignDescriptor) (input.Signature, error) {

	return testBoardingSweepSignature{}, nil
}

// ComputeInputScript is unused by the boarding timeout sweep helper.
func (w *testBoardingSweepWallet) ComputeInputScript(*wire.MsgTx,
	*input.SignDescriptor) (*input.Script, error) {

	return nil, fmt.Errorf("unused")
}

// MuSig2CreateSession is unused by the boarding timeout sweep helper.
func (w *testBoardingSweepWallet) MuSig2CreateSession(input.MuSig2Version,
	keychain.KeyLocator, []*btcec.PublicKey, *input.MuSig2Tweaks,
	[][musig2.PubNonceSize]byte, *musig2.Nonces) (*input.MuSig2SessionInfo,
	error) {

	return nil, fmt.Errorf("unused")
}

// MuSig2RegisterNonces is unused by the boarding timeout sweep helper.
func (w *testBoardingSweepWallet) MuSig2RegisterNonces(input.MuSig2SessionID,
	[][musig2.PubNonceSize]byte) (bool, error) {

	return false, fmt.Errorf("unused")
}

// MuSig2RegisterCombinedNonce is unused by the boarding timeout sweep helper.
func (w *testBoardingSweepWallet) MuSig2RegisterCombinedNonce(
	input.MuSig2SessionID, [musig2.PubNonceSize]byte) error {

	return fmt.Errorf("unused")
}

// MuSig2GetCombinedNonce is unused by the boarding timeout sweep helper.
func (w *testBoardingSweepWallet) MuSig2GetCombinedNonce(
	input.MuSig2SessionID) ([musig2.PubNonceSize]byte, error) {

	return [musig2.PubNonceSize]byte{}, fmt.Errorf("unused")
}

// MuSig2Sign is unused by the boarding timeout sweep helper.
func (w *testBoardingSweepWallet) MuSig2Sign(input.MuSig2SessionID,
	[sha256.Size]byte, bool) (*musig2.PartialSignature, error) {

	return nil, fmt.Errorf("unused")
}

// MuSig2CombineSig is unused by the boarding timeout sweep helper.
func (w *testBoardingSweepWallet) MuSig2CombineSig(input.MuSig2SessionID,
	[]*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	return nil, false, fmt.Errorf("unused")
}

// MuSig2Cleanup is unused by the boarding timeout sweep helper.
func (w *testBoardingSweepWallet) MuSig2Cleanup(input.MuSig2SessionID) error {
	return nil
}

// testBoardingSweepSignature is a fixed-size dummy signature.
type testBoardingSweepSignature struct{}

// Serialize returns a deterministic signature blob.
func (s testBoardingSweepSignature) Serialize() []byte {
	return bytes.Repeat([]byte{1}, 64)
}

// Verify always succeeds for the test signature.
func (s testBoardingSweepSignature) Verify([]byte, *btcec.PublicKey) bool {
	return true
}

// testBoardingSweepIntent builds a confirmed boarding intent whose timeout
// policy matches the stored output script.
func testBoardingSweepIntent(t *testing.T, amount btcutil.Amount,
	confHeight int32, exitDelay uint32) BoardingIntent {

	t.Helper()

	clientPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	tapscript, err := arkscript.VTXOTapScript(
		clientPrivKey.PubKey(), operatorPrivKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	taprootKey := txscript.ComputeTaprootOutputKey(
		&arkscript.ARKNUMSKey, tapscript.RootHash,
	)
	address, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)

	confTx := wire.NewMsgTx(arktx.TxVersion)
	confTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x01},
			Index: 0,
		},
	})
	confTx.AddTxOut(&wire.TxOut{
		Value:    int64(amount),
		PkScript: pkScript,
	})

	outpoint := wire.OutPoint{
		Hash:  confTx.TxHash(),
		Index: 0,
	}

	return BoardingIntent{
		Address: BoardingAddress{
			Address:     address,
			Tapscript:   tapscript,
			OperatorKey: operatorPrivKey.PubKey(),
			ExitDelay:   exitDelay,
			KeyDesc: keychain.KeyDescriptor{
				PubKey: clientPrivKey.PubKey(),
			},
		},
		Outpoint: outpoint,
		ChainInfo: BoardingChainInfo{
			ConfHeight: confHeight,
			ConfTx:     confTx,
			OutPoint:   outpoint,
			Amount:     amount,
		},
		Status: BoardingStatusConfirmed,
	}
}

// TestBuildBoardingSweepTx verifies the timeout sweep builder spends multiple
// boarding outpoints through the CSV path and pays one wallet output after
// fees.
func TestBuildBoardingSweepTx(t *testing.T) {
	t.Parallel()

	const (
		amountSat         = btcutil.Amount(50_000)
		feeRateSatPerByte = int64(2)
		exitDelay         = uint32(10)
	)

	intent1 := testBoardingSweepIntent(t, amountSat, 100, exitDelay)
	intent2 := testBoardingSweepIntent(t, amountSat*2, 100, exitDelay)

	sweep, err := buildBoardingSweepTx(
		&testBoardingSweepWallet{}, []BoardingIntent{
			intent1, intent2,
		}, []byte{txscript.OP_TRUE}, feeRateSatPerByte,
	)
	require.NoError(t, err)
	require.NotNil(t, sweep)

	require.Equal(
		t, btcutil.Amount(feeRateSatPerByte*sweep.VBytes), sweep.Fee,
	)

	tx := sweep.Tx
	require.Equal(t, int32(arktx.TxVersion), tx.Version)
	require.Len(t, tx.TxIn, 2)
	require.Len(t, tx.TxOut, 2)
	require.Equal(t, intent1.Outpoint, tx.TxIn[0].PreviousOutPoint)
	require.Equal(t, intent2.Outpoint, tx.TxIn[1].PreviousOutPoint)
	require.Equal(
		t, blockchain.LockTimeToSequence(false, exitDelay),
		tx.TxIn[0].Sequence,
	)
	require.NotEmpty(t, tx.TxIn[0].Witness)
	require.NotEmpty(t, tx.TxIn[1].Witness)
	require.Equal(
		t, int64(amountSat*3-sweep.Fee)-boardingSweepAnchorValue,
		tx.TxOut[0].Value,
	)
	require.Equal(t, []byte{txscript.OP_TRUE}, tx.TxOut[0].PkScript)

	// The P2A anchor must be the last output. The value is intentionally
	// above the BIP-433 P2A dust threshold (240 sats) rather than zero —
	// a zero-value (ephemeral) anchor combined with this parent's
	// non-zero fee would be rejected by the ephemeral-dust rule. We
	// compare the pkScript directly instead of using arktx.IsAnchorOutput
	// because that helper gates on Value == 0 (it identifies the
	// ephemeral-anchor pattern, not every P2A output).
	require.Equal(t, boardingSweepAnchorValue, tx.TxOut[1].Value)
	require.Equal(
		t, arkscript.AnchorPkScript, tx.TxOut[1].PkScript,
	)
}

// TestBoardingSweepTargetOutputRejectsMismatchedTx verifies that a persisted
// intent cannot pair one confirmation transaction with another txid.
func TestBoardingSweepTargetOutputRejectsMismatchedTx(t *testing.T) {
	t.Parallel()

	intent := testBoardingSweepIntent(t, 50_000, 100, 10)
	intent.Outpoint.Hash[0] ^= 0x01

	_, err := boardingSweepTargetOutput(intent)
	require.ErrorContains(t, err, "confirmation tx mismatch")
}

// TestBoardingSweepTargetOutputRejectsMismatchedScript verifies the target
// output must still pay the persisted boarding address.
func TestBoardingSweepTargetOutputRejectsMismatchedScript(t *testing.T) {
	t.Parallel()

	intent := testBoardingSweepIntent(t, 50_000, 100, 10)
	intent.ChainInfo.ConfTx.TxOut[0].PkScript = []byte{txscript.OP_TRUE}
	intent.Outpoint.Hash = intent.ChainInfo.ConfTx.TxHash()

	_, err := boardingSweepTargetOutput(intent)
	require.ErrorContains(t, err, "pkscript mismatch")
}

// TestBuildBoardingSweepTxRejectsTooManyInputs verifies one aggregate sweep
// cannot grow past the bounded input cap.
func TestBuildBoardingSweepTxRejectsTooManyInputs(t *testing.T) {
	t.Parallel()

	intents := make([]BoardingIntent, 0,
		defaultBoardingSweepMaxInputs+1)
	for i := 0; i <= defaultBoardingSweepMaxInputs; i++ {
		intent := testBoardingSweepIntent(t, 50_000, 100, 10)
		intent.ChainInfo.ConfTx.LockTime = uint32(i)
		intent.Outpoint.Hash = intent.ChainInfo.ConfTx.TxHash()
		intent.ChainInfo.OutPoint = intent.Outpoint
		intents = append(intents, intent)
	}

	_, err := buildBoardingSweepTx(
		&testBoardingSweepWallet{}, intents, []byte{txscript.OP_TRUE},
		2,
	)
	require.ErrorContains(t, err, "too many sweep inputs")
}

// TestBuildBoardingSweepTxRejectsExcessiveFee verifies the builder refuses
// sweeps whose absolute fee would burn too much of the selected input value.
func TestBuildBoardingSweepTxRejectsExcessiveFee(t *testing.T) {
	t.Parallel()

	intent := testBoardingSweepIntent(t, 50_000, 100, 10)

	_, err := buildBoardingSweepTx(
		&testBoardingSweepWallet{}, []BoardingIntent{intent},
		[]byte{txscript.OP_TRUE}, 10_000,
	)
	require.ErrorContains(t, err, "sweep fee")
	require.ErrorContains(t, err, "exceeds max")
}

// TestBoardingSweepMaturityHeight verifies the CSV maturity calculation.
func TestBoardingSweepMaturityHeight(t *testing.T) {
	t.Parallel()

	intent := testBoardingSweepIntent(t, 50_000, 144, 12)
	require.Equal(t, int32(156), boardingSweepMaturityHeight(intent))
}

// TestEstimateBoardingSweepVBytes verifies the aggregate sweep estimate grows
// by input count while sharing one output.
func TestEstimateBoardingSweepVBytes(t *testing.T) {
	t.Parallel()

	oneInput := estimateBoardingSweepVBytes(1)
	twoInputs := estimateBoardingSweepVBytes(2)

	require.Equal(t, int64(200), oneInput)
	require.Less(t, twoInputs, oneInput*2)
}

// TestBoardingSweepPkScript verifies sweep destination selection uses a wallet
// script by default and validates caller-provided addresses.
func TestBoardingSweepPkScript(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	signer := &testBoardingSweepWallet{}

	walletScript, err := boardingSweepPkScript(
		ctx, signer, &chaincfg.RegressionNetParams, "", true,
	)
	require.NoError(t, err)
	require.Equal(t, []byte{txscript.OP_TRUE}, walletScript)

	previewScript, err := boardingSweepPkScript(
		ctx, signer, &chaincfg.RegressionNetParams, "", false,
	)
	require.NoError(t, err)
	require.Len(t, previewScript, 34)

	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		bytes.Repeat(
			[]byte{2}, 20,
		),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	addrScript, err := boardingSweepPkScript(
		ctx, signer, &chaincfg.RegressionNetParams, addr.String(),
		false,
	)
	require.NoError(t, err)

	expectedScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	require.Equal(t, expectedScript, addrScript)

	mainnetAddr, err := btcaddr.NewAddressWitnessPubKeyHash(
		bytes.Repeat(
			[]byte{3}, 20,
		),
		&chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	mainnetAddrString := mainnetAddr.String()
	_, err = boardingSweepPkScript(
		ctx, signer, &chaincfg.RegressionNetParams, mainnetAddrString,
		false,
	)
	require.ErrorContains(t, err, "wrong network")
}

// Compile-time assertions that the test wallet satisfies SweepSigner.
var _ SweepSigner = (*testBoardingSweepWallet)(nil)
var _ input.Signature = testBoardingSweepSignature{}
