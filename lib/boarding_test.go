package lib

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestBoardingTxSpendValidation tests spending boarding transactions via both
// the collaborative multisig path and the unilateral CSV timeout path.
func TestBoardingTxSpendValidation(t *testing.T) {
	t.Parallel()

	// Generate test keys.
	clientPrivKey, clientPubKey := CreateKey(0)
	operatorPrivKey, operatorPubKey := CreateKey(1)
	wrongClientPrivKey, wrongClientPubKey := CreateKey(2)
	wrongOperatorPrivKey, wrongOperatorPubKey := CreateKey(3)

	// Create mock signers for client and operator.
	mockClientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientPrivKey}, nil,
	)
	mockOperatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorPrivKey}, nil,
	)
	mockWrongClientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{wrongClientPrivKey}, nil,
	)
	mockWrongOperatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{wrongOperatorPrivKey}, nil,
	)

	exitDelay := uint32(144)
	boardingAmount := btcutil.Amount(1000000)

	// Create the boarding tapscript.
	boardingTapScript, err := BoardingTapScript(
		clientPubKey, operatorPubKey, exitDelay,
	)
	require.NoError(t, err)

	// Get the taproot output key.
	taprootKey, err := boardingTapScript.TaprootKey()
	require.NoError(t, err)

	// Create the boarding output pkScript.
	boardingPkScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(t, err)

	// Helper to create a spending transaction. This transaction "spends"
	// the boarding output and is the tx that the signature will cover.
	createSpendTx := func(sequence uint32) *wire.MsgTx {
		fakePrevOut := &wire.OutPoint{
			Hash:  [32]byte{1, 2, 3, 4, 5, 6, 7, 8},
			Index: 0,
		}

		spendTx := wire.NewMsgTx(2)
		spendTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *fakePrevOut,
			Sequence:         sequence,
		})
		spendTx.AddTxOut(&wire.TxOut{
			// Subtract fee.
			Value: int64(boardingAmount) - 1000,
			// Dummy taproot output.
			PkScript: []byte{0x51, 0x20},
		})

		return spendTx
	}

	timeoutSpendInfo, err := NewBoardingTimeoutSpendInfo(boardingTapScript)
	require.NoError(t, err)
	collabSpendInfo, err := NewBoardingCollabSpendInfo(boardingTapScript)
	require.NoError(t, err)

	signCollab := func(signer input.Signer, pubKey *btcec.PublicKey,
		spendTx *wire.MsgTx, prevFetcher txscript.PrevOutputFetcher,
		sigHashes *txscript.TxSigHashes) (input.Signature, error) {

		sig, err := SignBoardingCollabInput(
			signer, spendTx, 0, collabSpendInfo,
			&keychain.KeyDescriptor{PubKey: pubKey},
			boardingAmount, boardingPkScript,
			sigHashes, prevFetcher,
		)
		if err != nil {
			return nil, err
		}

		return sig, nil
	}

	testCases := []struct {
		name        string
		witnessGen  func(spendTx *wire.MsgTx) (wire.TxWitness, error)
		valid       bool
		useSequence uint32
	}{
		{
			name: "valid timeout spend after CSV delay",
			witnessGen: func(spendTx *wire.MsgTx) (wire.TxWitness, error) {
				// Use sequence that satisfies CSV delay.
				prevFetcher := txscript.NewCannedPrevOutputFetcher(
					boardingPkScript, int64(boardingAmount),
				)
				sigHashes := txscript.NewTxSigHashes(spendTx, prevFetcher)
				signDesc, err := BoardingTimeoutSignDescriptor(
					&keychain.KeyDescriptor{PubKey: clientPubKey},
					boardingAmount, boardingPkScript, 0,
					sigHashes, prevFetcher, timeoutSpendInfo,
				)
				if err != nil {
					return nil, err
				}

				return BoardingTimoutSpendWitness(
					mockClientSigner, signDesc, spendTx,
				)
			},
			valid:       true,
			useSequence: exitDelay,
		},
		{
			name: "invalid timeout spend before CSV delay",
			witnessGen: func(spendTx *wire.MsgTx) (wire.TxWitness, error) {
				prevFetcher := txscript.NewCannedPrevOutputFetcher(
					boardingPkScript, int64(boardingAmount),
				)
				sigHashes := txscript.NewTxSigHashes(spendTx, prevFetcher)
				signDesc, err := BoardingTimeoutSignDescriptor(
					&keychain.KeyDescriptor{PubKey: clientPubKey},
					boardingAmount, boardingPkScript, 0,
					sigHashes, prevFetcher, timeoutSpendInfo,
				)
				if err != nil {
					return nil, err
				}

				return BoardingTimoutSpendWitness(
					mockClientSigner, signDesc, spendTx,
				)
			},
			valid:       false,
			useSequence: exitDelay - 1,
		},
		{
			name: "valid collaborative multisig spend",
			witnessGen: func(spendTx *wire.MsgTx) (wire.TxWitness, error) {
				prevFetcher := txscript.NewCannedPrevOutputFetcher(
					boardingPkScript, int64(boardingAmount),
				)
				sigHashes := txscript.NewTxSigHashes(spendTx, prevFetcher)

				clientSig, err := signCollab(
					mockClientSigner, clientPubKey, spendTx,
					prevFetcher, sigHashes,
				)
				if err != nil {
					return nil, err
				}

				operatorSig, err := signCollab(
					mockOperatorSigner, operatorPubKey, spendTx,
					prevFetcher, sigHashes,
				)
				if err != nil {
					return nil, err
				}

				return BoardingCollabWitness(
					operatorSig, clientSig, collabSpendInfo,
				)
			},
			valid:       true,
			useSequence: wire.MaxTxInSequenceNum,
		},
		{
			name: "invalid collaborative spend with swapped signatures",
			witnessGen: func(spendTx *wire.MsgTx) (wire.TxWitness, error) {
				prevFetcher := txscript.NewCannedPrevOutputFetcher(
					boardingPkScript, int64(boardingAmount),
				)
				sigHashes := txscript.NewTxSigHashes(spendTx, prevFetcher)

				clientSig, err := signCollab(
					mockClientSigner, clientPubKey, spendTx,
					prevFetcher, sigHashes,
				)
				if err != nil {
					return nil, err
				}

				operatorSig, err := signCollab(
					mockOperatorSigner, operatorPubKey, spendTx,
					prevFetcher, sigHashes,
				)
				if err != nil {
					return nil, err
				}

				return wire.TxWitness{
					clientSig.Serialize(),
					operatorSig.Serialize(),
					collabSpendInfo.WitnessScript,
					collabSpendInfo.ControlBlock,
				}, nil
			},
			valid:       false,
			useSequence: wire.MaxTxInSequenceNum,
		},
		{
			name: "invalid collaborative spend with wrong client signature",
			witnessGen: func(spendTx *wire.MsgTx) (wire.TxWitness, error) {
				prevFetcher := txscript.NewCannedPrevOutputFetcher(
					boardingPkScript, int64(boardingAmount),
				)
				sigHashes := txscript.NewTxSigHashes(spendTx, prevFetcher)

				operatorSig, err := signCollab(
					mockOperatorSigner, operatorPubKey, spendTx,
					prevFetcher, sigHashes,
				)
				if err != nil {
					return nil, err
				}

				wrongClientSig, err := signCollab(
					mockWrongClientSigner, wrongClientPubKey, spendTx,
					prevFetcher, sigHashes,
				)
				if err != nil {
					return nil, err
				}

				return BoardingCollabWitness(
					operatorSig, wrongClientSig, collabSpendInfo,
				)
			},
			valid:       false,
			useSequence: wire.MaxTxInSequenceNum,
		},
		{
			name: "invalid collaborative spend with wrong operator signature",
			witnessGen: func(spendTx *wire.MsgTx) (wire.TxWitness, error) {
				prevFetcher := txscript.NewCannedPrevOutputFetcher(
					boardingPkScript, int64(boardingAmount),
				)
				sigHashes := txscript.NewTxSigHashes(spendTx, prevFetcher)

				clientSig, err := signCollab(
					mockClientSigner, clientPubKey, spendTx,
					prevFetcher, sigHashes,
				)
				if err != nil {
					return nil, err
				}

				wrongOperatorSig, err := signCollab(
					mockWrongOperatorSigner, wrongOperatorPubKey,
					spendTx, prevFetcher, sigHashes,
				)
				if err != nil {
					return nil, err
				}

				return BoardingCollabWitness(
					wrongOperatorSig, clientSig, collabSpendInfo,
				)
			},
			valid:       false,
			useSequence: wire.MaxTxInSequenceNum,
		},
	}

	for i, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			// Create spending transaction with appropriate sequence.
			spendTx := createSpendTx(testCase.useSequence)

			// Generate witness for this test case.
			witness, err := testCase.witnessGen(spendTx)
			require.NoError(t, err)

			// Set witness on the transaction.
			spendTx.TxIn[0].Witness = witness

			// Create engine to validate the script.
			newEngine := func() (*txscript.Engine, error) {
				prevFetcher := txscript.NewCannedPrevOutputFetcher(
					boardingPkScript,
					int64(boardingAmount),
				)
				hashCache := txscript.NewTxSigHashes(
					spendTx, prevFetcher,
				)

				return txscript.NewEngine(
					boardingPkScript,
					spendTx, 0,
					txscript.StandardVerifyFlags,
					nil, hashCache,
					int64(boardingAmount),
					prevFetcher,
				)
			}

			// Assert script execution result.
			assertEngineExecution(t, i, testCase.valid, newEngine)
		})
	}
}
