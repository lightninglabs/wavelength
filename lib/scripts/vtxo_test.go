package scripts

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestVTXOSpending tests spending VTXO outputs via both the collaborative
// multisig path and the unilateral CSV timeout path.
func TestVTXOSpending(t *testing.T) {
	t.Parallel()

	// Create keys for client and operator.
	clientPub, clientSigner := testutils.CreateKey(1)
	operatorPub, operatorSigner := testutils.CreateKey(2)
	wrongPub, wrongSigner := testutils.CreateKey(3)

	// Other output parameters.
	exitDelay := uint32(100)
	outputAmt := btcutil.Amount(500000)

	// Create a VTXO output. We will attempt to spend this via the various
	// paths.
	//
	// Start by creating the tapscript.
	vtxoTapScript, err := VTXOTapScript(
		clientPub, operatorPub, exitDelay,
	)
	require.NoError(t, err)

	// Now use that to create the pkscript for the output.
	taprootKey, err := vtxoTapScript.TaprootKey()
	require.NoError(t, err)
	vtxoPkScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(t, err)

	// From the tapscript, we can already derive all the spend info.
	timeoutSpendInfo, err := NewVTXOSpendInfo(
		vtxoTapScript, VTXOTimeoutPathLeaf,
	)
	require.NoError(t, err)

	collabSpendInfo, err := NewVTXOSpendInfo(
		vtxoTapScript, VTXOCollabPathLeaf,
	)
	require.NoError(t, err)

	// Create our output that we will be spending.
	vtxoOutput := &wire.TxOut{
		Value:    int64(outputAmt),
		PkScript: vtxoPkScript,
	}

	// The actual previous outpoint doesn't matter for script validation.
	// As long as our prevout fetcher returns the correct output when
	// queried with this prev out, we're good.
	prevOut := wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5, 6, 7, 8},
		Index: 0,
	}

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{prevOut: vtxoOutput},
	)

	// In our fake spend tx, we only add one input, so this will always
	// be the index we care about.
	const inputIndex = 0

	// We now create a fake, unsigned, transaction to spend this output.
	// We allow the tests to specify a sequence number to test the CSV path.
	createSpendTx := func(sequence uint32) *wire.MsgTx {
		spendTx := wire.NewMsgTx(2)

		spendTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: prevOut,
			Sequence:         sequence,
		})

		spendTx.AddTxOut(&wire.TxOut{
			// Subtract fee.
			Value: int64(outputAmt) - 1000,
			// Dummy taproot output.
			PkScript: []byte{0x51, 0x20},
		})

		return spendTx
	}

	signWith := func(signer input.Signer, pubKey *btcec.PublicKey,
		spendTx *wire.MsgTx, prevFetcher txscript.PrevOutputFetcher,
		sigHashes *txscript.TxSigHashes) input.Signature {

		sig, err := SignVTXOCollabInput(
			signer, spendTx, inputIndex, collabSpendInfo,
			&keychain.KeyDescriptor{PubKey: pubKey},
			vtxoOutput, sigHashes, prevFetcher,
		)
		require.NoError(t, err)

		return sig
	}

	signCollab := func(spendTx *wire.MsgTx) (input.Signature,
		input.Signature) {

		sigHashes := txscript.NewTxSigHashes(spendTx, prevOutFetcher)

		clientSig := signWith(
			clientSigner, clientPub, spendTx,
			prevOutFetcher, sigHashes,
		)

		operatorSig := signWith(
			operatorSigner, operatorPub, spendTx,
			prevOutFetcher, sigHashes,
		)

		return clientSig, operatorSig
	}

	collabWitness := func(spendTx *wire.MsgTx) wire.TxWitness {
		clientSig, operatorSig := signCollab(spendTx)

		witness, err := VTXOCollabSpendWitness(
			clientSig, operatorSig, collabSpendInfo,
		)
		require.NoError(t, err)

		return witness
	}

	timeoutWitness := func(tx *wire.MsgTx) wire.TxWitness {
		sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

		signDesc := VTXOSignDesc(
			keychain.KeyDescriptor{PubKey: clientPub},
			vtxoOutput, sigHashes, prevOutFetcher, inputIndex,
			timeoutSpendInfo,
		)

		witness, err := VTXOTimeoutSpendWitness(
			clientSigner, signDesc, tx,
		)
		require.NoError(t, err)

		return witness
	}

	testCases := []struct {
		name        string
		witnessGen  func(spendTx *wire.MsgTx) wire.TxWitness
		valid       bool
		useSequence uint32
	}{
		{
			// The script should be spendable via the collaborative
			// multisig path at any time if both client and
			// server cooperate.
			name: "valid collaborative multisig spend",
			witnessGen: func(tx *wire.MsgTx) wire.TxWitness {
				return collabWitness(tx)
			},
			valid:       true,
			useSequence: wire.MaxTxInSequenceNum,
		},
		{
			// The script should be spendable by the client after
			// the CSV delay.
			name: "valid unilateral timout path spend",
			witnessGen: func(tx *wire.MsgTx) wire.TxWitness {
				return timeoutWitness(tx)
			},
			valid:       true,
			useSequence: exitDelay,
		},
		{
			name: "invalid unilateral timeout spend before csv " +
				"delay",
			witnessGen: func(tx *wire.MsgTx) wire.TxWitness {
				return timeoutWitness(tx)
			},
			valid:       false,
			useSequence: exitDelay - 1,
		},
		{
			name: "invalid collaborative spend missing operator " +
				"signature",
			witnessGen: func(tx *wire.MsgTx) wire.TxWitness {
				clientSig, _ := signCollab(tx)

				return wire.TxWitness{
					clientSig.Serialize(),
					collabSpendInfo.WitnessScript,
					collabSpendInfo.ControlBlock,
				}
			},
			valid:       false,
			useSequence: 0,
		},
		{
			name: "invalid collaborative spend signature order " +
				"swapped",
			witnessGen: func(tx *wire.MsgTx) wire.TxWitness {
				clientSig, operatorSig := signCollab(tx)

				return wire.TxWitness{
					clientSig.Serialize(),
					operatorSig.Serialize(),
					collabSpendInfo.WitnessScript,
					collabSpendInfo.ControlBlock,
				}
			},
			valid:       false,
			useSequence: wire.MaxTxInSequenceNum,
		},
		{
			name: "client uses wrong key to sign collaborative " +
				"spend",
			witnessGen: func(tx *wire.MsgTx) wire.TxWitness {
				wrongSig := signWith(
					wrongSigner, wrongPub, tx,
					prevOutFetcher,
					txscript.NewTxSigHashes(
						tx, prevOutFetcher,
					),
				)
				operatorSig := signWith(
					operatorSigner, operatorPub, tx,
					prevOutFetcher,
					txscript.NewTxSigHashes(
						tx, prevOutFetcher,
					),
				)

				witness, err := VTXOCollabSpendWitness(
					wrongSig, operatorSig, collabSpendInfo,
				)
				require.NoError(t, err)

				return witness
			},
			valid:       false,
			useSequence: wire.MaxTxInSequenceNum,
		},
	}

	for i, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// Create the spending transaction with the appropriate
			// sequence.
			spendTx := createSpendTx(testCase.useSequence)

			// Generate the witness for this test case and mutate
			// the spend tx input.
			witness := testCase.witnessGen(spendTx)
			spendTx.TxIn[inputIndex].Witness = witness

			hashCache := txscript.NewTxSigHashes(
				spendTx, prevOutFetcher,
			)

			// Create engine to validate the script.
			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					// This is the actual script we want to
					// test spending from. The VM will use
					// this to decide witness form.
					vtxoPkScript,
					// This is the transaction that is going
					// to spend the output with the pkscript
					// above. So this tx must have a witness
					// that signs for that pkscript.
					spendTx,
					// The index of the specific input we
					// are validating. In this case, we
					// are using a pseudo spend tx that only
					// has one input.
					inputIndex,
					// Tells the VM which consensus/policy
					// rules to enforce.
					txscript.StandardVerifyFlags,
					// Optional cache of fully verified
					// sigs.
					nil,
					// Pre-computed sighash midsate values.
					hashCache,
					// The amount of the output being spent.
					// This is needed since Segwit v0 & v1
					// signatures commit to this amount.
					int64(outputAmt),
					// How the VM will fetch all the
					// previous outputs referenced by the
					// transaction being validated.
					prevOutFetcher,
				)
			}

			// Assert script execution result.
			testutils.AssertEngineExecution(
				t, i, testCase.valid, newEngine,
			)
		})
	}
}

// TestVTXOTapKey tests the VTXOTapKey helper function that computes the
// taproot output key for a VTXO.
func TestVTXOTapKey(t *testing.T) {
	t.Parallel()

	clientPub, _ := testutils.CreateKey(1)
	operatorPub, _ := testutils.CreateKey(2)
	exitDelay := uint32(100)

	t.Run("computes valid taproot key", func(t *testing.T) {
		outputKey, err := VTXOTapKey(clientPub, operatorPub, exitDelay)
		require.NoError(t, err)
		require.NotNil(t, outputKey)

		// Key should be a valid 32-byte x-only pubkey.
		require.Len(t, outputKey.SerializeCompressed(), 33)
	})

	t.Run("matches manual computation", func(t *testing.T) {
		// Compute using helper.
		outputKey1, err := VTXOTapKey(clientPub, operatorPub, exitDelay)
		require.NoError(t, err)

		// Compute manually using VTXOTapScript.
		vtxoTapScript, err := VTXOTapScript(
			clientPub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		tree := txscript.AssembleTaprootScriptTree(
			vtxoTapScript.Leaves...,
		)
		rootHash := tree.RootNode.TapHash()
		outputKey2 := txscript.ComputeTaprootOutputKey(
			vtxoTapScript.ControlBlock.InternalKey, rootHash[:],
		)

		// Both methods should produce the same key.
		require.Equal(t, outputKey1, outputKey2)
	})

	t.Run("creates valid P2TR script", func(t *testing.T) {
		outputKey, err := VTXOTapKey(clientPub, operatorPub, exitDelay)
		require.NoError(t, err)

		pkScript, err := txscript.PayToTaprootScript(outputKey)
		require.NoError(t, err)

		// Script should be valid taproot.
		require.True(t, txscript.IsPayToTaproot(pkScript))
		require.Len(t, pkScript, 34) // OP_1 + 32 bytes
	})

	t.Run("different parameters produce different keys",
		func(t *testing.T) {
			otherClientPub, _ := testutils.CreateKey(3)
			otherOperatorPub, _ := testutils.CreateKey(4)

			key1, err := VTXOTapKey(
				clientPub, operatorPub, exitDelay,
			)
			require.NoError(t, err)

			// Different client.
			key2, err := VTXOTapKey(
				otherClientPub, operatorPub, exitDelay,
			)
			require.NoError(t, err)
			require.NotEqual(t, key1, key2)

			// Different operator.
			key3, err := VTXOTapKey(
				clientPub, otherOperatorPub, exitDelay,
			)
			require.NoError(t, err)
			require.NotEqual(t, key1, key3)

			// Different exit delay.
			key4, err := VTXOTapKey(clientPub, operatorPub, 200)
			require.NoError(t, err)
			require.NotEqual(t, key1, key4)
		})

	t.Run("deterministic computation", func(t *testing.T) {
		// Same inputs should always produce same output.
		key1, err := VTXOTapKey(clientPub, operatorPub, exitDelay)
		require.NoError(t, err)

		key2, err := VTXOTapKey(clientPub, operatorPub, exitDelay)
		require.NoError(t, err)

		require.Equal(t, key1, key2)
	})
}
