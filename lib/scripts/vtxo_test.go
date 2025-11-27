package scripts

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/lib/closure"
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
	exitDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 100,
	}
	outputAmt := btcutil.Amount(500000)

	// Create a VTXO output. We will attempt to spend this via the various
	// paths.
	//
	// Start by creating the VTXO script using closures.
	vtxoScript := NewDefaultVtxoScript(clientPub, operatorPub, exitDelay)

	// Now use that to create the pkscript for the output.
	taprootKey, _, err := vtxoScript.TapTree()
	require.NoError(t, err)
	vtxoPkScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(t, err)

	// From the VTXO script, we can already derive all the spend info.
	exitSpendInfo, err := VtxoExitSpendInfo(vtxoScript)
	require.NoError(t, err)

	collabSpendInfo, err := VtxoCollabSpendInfo(vtxoScript)
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

		sig, err := SignVtxoCollabInput(
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

		witness, err := VtxoCollabSpendWitness(
			clientSig, operatorSig, collabSpendInfo,
		)
		require.NoError(t, err)

		return witness
	}

	exitWitness := func(tx *wire.MsgTx) wire.TxWitness {
		sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

		signDesc := VTXOSignDesc(
			keychain.KeyDescriptor{PubKey: clientPub},
			vtxoOutput, sigHashes, prevOutFetcher, inputIndex,
			exitSpendInfo,
		)

		witness, err := VtxoExitSpendWitness(
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
			// The script should be spendable via the collab
			// (collaborative) path at any time if both client and
			// server cooperate.
			name: "valid collab (collaborative) spend",
			witnessGen: func(tx *wire.MsgTx) wire.TxWitness {
				return collabWitness(tx)
			},
			valid:       true,
			useSequence: wire.MaxTxInSequenceNum,
		},
		{
			// The script should be spendable by the client after
			// the CSV delay via the exit path.
			name: "valid exit (timeout) path spend",
			witnessGen: func(tx *wire.MsgTx) wire.TxWitness {
				return exitWitness(tx)
			},
			valid:       true,
			useSequence: exitDelay.Value,
		},
		{
			name: "invalid exit spend before csv delay",
			witnessGen: func(tx *wire.MsgTx) wire.TxWitness {
				return exitWitness(tx)
			},
			valid:       false,
			useSequence: exitDelay.Value - 1,
		},
		{
			name: "invalid collab spend missing operator " +
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
			name: "invalid collab spend signature order swapped",
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
			name: "client uses wrong key to sign collab spend",
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

				witness, err := VtxoCollabSpendWitness(
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

// TestVtxoTapKey tests the VtxoTapKey helper function that computes the
// taproot output key for a VTXO.
func TestVtxoTapKey(t *testing.T) {
	t.Parallel()

	clientPub, _ := testutils.CreateKey(1)
	operatorPub, _ := testutils.CreateKey(2)
	exitDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 100,
	}

	t.Run("computes valid taproot key", func(t *testing.T) {
		outputKey, err := VtxoTapKey(clientPub, operatorPub, exitDelay)
		require.NoError(t, err)
		require.NotNil(t, outputKey)

		// Key should be a valid 32-byte x-only pubkey.
		require.Len(t, outputKey.SerializeCompressed(), 33)
	})

	t.Run("matches manual computation", func(t *testing.T) {
		// Compute using helper.
		outputKey1, err := VtxoTapKey(clientPub, operatorPub, exitDelay)
		require.NoError(t, err)

		// Compute manually using NewDefaultVtxoScript.
		vtxoScript := NewDefaultVtxoScript(clientPub, operatorPub, exitDelay)

		taprootKey, tree, err := vtxoScript.TapTree()
		require.NoError(t, err)
		_ = tree // tree is used for verification

		// Both methods should produce the same key.
		require.Equal(t, outputKey1, taprootKey)
	})

	t.Run("creates valid P2TR script", func(t *testing.T) {
		outputKey, err := VtxoTapKey(clientPub, operatorPub, exitDelay)
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

			key1, err := VtxoTapKey(
				clientPub, operatorPub, exitDelay,
			)
			require.NoError(t, err)

			// Different client.
			key2, err := VtxoTapKey(
				otherClientPub, operatorPub, exitDelay,
			)
			require.NoError(t, err)
			require.NotEqual(t, key1, key2)

			// Different operator.
			key3, err := VtxoTapKey(
				clientPub, otherOperatorPub, exitDelay,
			)
			require.NoError(t, err)
			require.NotEqual(t, key1, key3)

			// Different exit delay.
			differentDelay := closure.RelativeLocktime{
				Type:  closure.LocktimeTypeBlock,
				Value: 200,
			}
			key4, err := VtxoTapKey(
				clientPub, operatorPub, differentDelay,
			)
			require.NoError(t, err)
			require.NotEqual(t, key1, key4)
		})

	t.Run("deterministic computation", func(t *testing.T) {
		// Same inputs should always produce same output.
		key1, err := VtxoTapKey(clientPub, operatorPub, exitDelay)
		require.NoError(t, err)

		key2, err := VtxoTapKey(clientPub, operatorPub, exitDelay)
		require.NoError(t, err)

		require.Equal(t, key1, key2)
	})
}

// TestCustomVTXOSpending tests spending VTXO outputs with non-default closure
// configurations, including CSVMultisigClosure (multi-key exit), exit-only
// VTXOs, and VTXOs with multiple exit paths.
func TestCustomVTXOSpending(t *testing.T) {
	t.Parallel()

	outputAmt := btcutil.Amount(500000)

	// Helper to create a VTXO output and test spending it.
	testVTXOSpend := func(t *testing.T, vtxoScript *closure.TapscriptsVtxoScript,
		closureIdx int, witnessGen func(
			spendInfo *VTXOSpendData,
			output *wire.TxOut,
			tx *wire.MsgTx,
			prevFetcher txscript.PrevOutputFetcher,
		) wire.TxWitness, sequence uint32, shouldSucceed bool) {

		// Create taproot output from the script.
		taprootKey, _, err := vtxoScript.TapTree()
		require.NoError(t, err)

		vtxoPkScript, err := txscript.PayToTaprootScript(taprootKey)
		require.NoError(t, err)

		vtxoOutput := &wire.TxOut{
			Value:    int64(outputAmt),
			PkScript: vtxoPkScript,
		}

		prevOut := wire.OutPoint{
			Hash:  [32]byte{1, 2, 3, 4, 5, 6, 7, 8},
			Index: 0,
		}

		prevOutFetcher := txscript.NewMultiPrevOutFetcher(
			map[wire.OutPoint]*wire.TxOut{prevOut: vtxoOutput},
		)

		// Get spend info for the specific closure.
		spendInfo, err := NewVtxoSpendInfo(vtxoScript, closureIdx)
		require.NoError(t, err)

		// Create spending transaction.
		spendTx := wire.NewMsgTx(2)
		spendTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: prevOut,
			Sequence:         sequence,
		})
		spendTx.AddTxOut(&wire.TxOut{
			Value:    int64(outputAmt) - 1000,
			PkScript: []byte{0x51, 0x20},
		})

		// Generate witness. Pass vtxoOutput for correct signing.
		witness := witnessGen(spendInfo, vtxoOutput, spendTx, prevOutFetcher)
		spendTx.TxIn[0].Witness = witness

		// Validate the script.
		hashCache := txscript.NewTxSigHashes(spendTx, prevOutFetcher)
		engine, err := txscript.NewEngine(
			vtxoPkScript, spendTx, 0,
			txscript.StandardVerifyFlags, nil, hashCache,
			int64(outputAmt), prevOutFetcher,
		)
		require.NoError(t, err)

		err = engine.Execute()
		if shouldSucceed {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}

	t.Run("CSVMultisigClosure 2-of-2 exit", func(t *testing.T) {
		// Create keys for a 2-of-2 multisig exit.
		client1Pub, client1Signer := testutils.CreateKey(10)
		client2Pub, client2Signer := testutils.CreateKey(11)
		operatorPub, operatorSigner := testutils.CreateKey(12)

		exitDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 100,
		}

		// VTXO with CSVMultisigClosure (2-of-2 for exit) + MultisigClosure
		// for collab.
		vtxoScript := &closure.TapscriptsVtxoScript{
			Closures: []closure.Closure{
				// Exit: requires both clients after delay.
				&closure.CSVMultisigClosure{
					MultisigClosure: closure.MultisigClosure{
						PubKeys: []*btcec.PublicKey{
							client1Pub, client2Pub,
						},
						Type: closure.MultisigTypeChecksig,
					},
					Locktime: exitDelay,
				},
				// Collab: all three parties.
				&closure.MultisigClosure{
					PubKeys: []*btcec.PublicKey{
						client1Pub, client2Pub, operatorPub,
					},
					Type: closure.MultisigTypeChecksig,
				},
			},
		}

		t.Run("valid 2-of-2 exit spend", func(t *testing.T) {
			testVTXOSpend(t, vtxoScript, 0,
				func(spendInfo *VTXOSpendData, output *wire.TxOut,
					tx *wire.MsgTx,
					prevFetcher txscript.PrevOutputFetcher,
				) wire.TxWitness {

					sigHashes := txscript.NewTxSigHashes(
						tx, prevFetcher,
					)

					// Sign with client1.
					signDesc1 := VTXOSignDesc(
						keychain.KeyDescriptor{
							PubKey: client1Pub,
						},
						output,
						sigHashes, prevFetcher, 0, spendInfo,
					)
					sig1, err := client1Signer.SignOutputRaw(
						tx, signDesc1,
					)
					require.NoError(t, err)

					// Sign with client2.
					signDesc2 := VTXOSignDesc(
						keychain.KeyDescriptor{
							PubKey: client2Pub,
						},
						output,
						sigHashes, prevFetcher, 0, spendInfo,
					)
					sig2, err := client2Signer.SignOutputRaw(
						tx, signDesc2,
					)
					require.NoError(t, err)

					// Build witness: sig2, sig1, script,
					// control.
					return wire.TxWitness{
						sig2.Serialize(),
						sig1.Serialize(),
						spendInfo.WitnessScript,
						spendInfo.ControlBlock,
					}
				},
				exitDelay.Value, true,
			)
		})

		t.Run("valid 3-of-3 collab spend", func(t *testing.T) {
			testVTXOSpend(t, vtxoScript, 1,
				func(spendInfo *VTXOSpendData, output *wire.TxOut,
					tx *wire.MsgTx,
					prevFetcher txscript.PrevOutputFetcher,
				) wire.TxWitness {

					sigHashes := txscript.NewTxSigHashes(
						tx, prevFetcher,
					)

					signers := []struct {
						pub    *btcec.PublicKey
						signer input.Signer
					}{
						{client1Pub, client1Signer},
						{client2Pub, client2Signer},
						{operatorPub, operatorSigner},
					}

					sigs := make([][]byte, len(signers))
					for i, s := range signers {
						signDesc := VTXOSignDesc(
							keychain.KeyDescriptor{
								PubKey: s.pub,
							},
							output,
							sigHashes, prevFetcher, 0,
							spendInfo,
						)
						sig, err := s.signer.SignOutputRaw(
							tx, signDesc,
						)
						require.NoError(t, err)
						sigs[i] = sig.Serialize()
					}

					// Witness: sig3, sig2, sig1, script,
					// control.
					return wire.TxWitness{
						sigs[2], sigs[1], sigs[0],
						spendInfo.WitnessScript,
						spendInfo.ControlBlock,
					}
				},
				wire.MaxTxInSequenceNum, true,
			)
		})
	})

	t.Run("exit-only VTXO (no collab path)", func(t *testing.T) {
		clientPub, clientSigner := testutils.CreateKey(20)

		exitDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 50,
		}

		// VTXO with only a single exit closure.
		vtxoScript := &closure.TapscriptsVtxoScript{
			Closures: []closure.Closure{
				&closure.CSVSigClosure{
					PubKey:   clientPub,
					Locktime: exitDelay,
				},
			},
		}

		t.Run("valid exit spend", func(t *testing.T) {
			testVTXOSpend(t, vtxoScript, 0,
				func(spendInfo *VTXOSpendData, output *wire.TxOut,
					tx *wire.MsgTx,
					prevFetcher txscript.PrevOutputFetcher,
				) wire.TxWitness {

					sigHashes := txscript.NewTxSigHashes(
						tx, prevFetcher,
					)

					signDesc := VTXOSignDesc(
						keychain.KeyDescriptor{
							PubKey: clientPub,
						},
						output,
						sigHashes, prevFetcher, 0, spendInfo,
					)

					witness, err := VtxoExitSpendWitness(
						clientSigner, signDesc, tx,
					)
					require.NoError(t, err)

					return witness
				},
				exitDelay.Value, true,
			)
		})

		t.Run("invalid exit spend before delay", func(t *testing.T) {
			testVTXOSpend(t, vtxoScript, 0,
				func(spendInfo *VTXOSpendData, output *wire.TxOut,
					tx *wire.MsgTx,
					prevFetcher txscript.PrevOutputFetcher,
				) wire.TxWitness {

					sigHashes := txscript.NewTxSigHashes(
						tx, prevFetcher,
					)

					signDesc := VTXOSignDesc(
						keychain.KeyDescriptor{
							PubKey: clientPub,
						},
						output,
						sigHashes, prevFetcher, 0, spendInfo,
					)

					witness, err := VtxoExitSpendWitness(
						clientSigner, signDesc, tx,
					)
					require.NoError(t, err)

					return witness
				},
				exitDelay.Value-1, false,
			)
		})

		t.Run("no collab closure returns error", func(t *testing.T) {
			_, err := VtxoCollabSpendInfo(vtxoScript)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrNoCollabClosure)
		})
	})

	t.Run("multiple exit closures with different delays", func(t *testing.T) {
		clientPub, clientSigner := testutils.CreateKey(30)
		operatorPub, operatorSigner := testutils.CreateKey(31)

		shortDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 10,
		}
		longDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 100,
		}

		// VTXO with two exit paths: short delay and long delay.
		vtxoScript := &closure.TapscriptsVtxoScript{
			Closures: []closure.Closure{
				// Short exit.
				&closure.CSVSigClosure{
					PubKey:   clientPub,
					Locktime: shortDelay,
				},
				// Long exit.
				&closure.CSVSigClosure{
					PubKey:   clientPub,
					Locktime: longDelay,
				},
				// Collab path.
				&closure.MultisigClosure{
					PubKeys: []*btcec.PublicKey{
						clientPub, operatorPub,
					},
					Type: closure.MultisigTypeChecksig,
				},
			},
		}

		t.Run("valid short delay exit", func(t *testing.T) {
			testVTXOSpend(t, vtxoScript, 0,
				func(spendInfo *VTXOSpendData, output *wire.TxOut,
					tx *wire.MsgTx,
					prevFetcher txscript.PrevOutputFetcher,
				) wire.TxWitness {

					sigHashes := txscript.NewTxSigHashes(
						tx, prevFetcher,
					)

					signDesc := VTXOSignDesc(
						keychain.KeyDescriptor{
							PubKey: clientPub,
						},
						output,
						sigHashes, prevFetcher, 0, spendInfo,
					)

					witness, err := VtxoExitSpendWitness(
						clientSigner, signDesc, tx,
					)
					require.NoError(t, err)

					return witness
				},
				shortDelay.Value, true,
			)
		})

		t.Run("valid long delay exit", func(t *testing.T) {
			testVTXOSpend(t, vtxoScript, 1,
				func(spendInfo *VTXOSpendData, output *wire.TxOut,
					tx *wire.MsgTx,
					prevFetcher txscript.PrevOutputFetcher,
				) wire.TxWitness {

					sigHashes := txscript.NewTxSigHashes(
						tx, prevFetcher,
					)

					signDesc := VTXOSignDesc(
						keychain.KeyDescriptor{
							PubKey: clientPub,
						},
						output,
						sigHashes, prevFetcher, 0, spendInfo,
					)

					witness, err := VtxoExitSpendWitness(
						clientSigner, signDesc, tx,
					)
					require.NoError(t, err)

					return witness
				},
				longDelay.Value, true,
			)
		})

		t.Run("valid collab spend", func(t *testing.T) {
			testVTXOSpend(t, vtxoScript, 2,
				func(spendInfo *VTXOSpendData, output *wire.TxOut,
					tx *wire.MsgTx,
					prevFetcher txscript.PrevOutputFetcher,
				) wire.TxWitness {

					sigHashes := txscript.NewTxSigHashes(
						tx, prevFetcher,
					)

					// Sign with client.
					clientSignDesc := VTXOSignDesc(
						keychain.KeyDescriptor{
							PubKey: clientPub,
						},
						output,
						sigHashes, prevFetcher, 0, spendInfo,
					)
					clientSig, err := clientSigner.SignOutputRaw(
						tx, clientSignDesc,
					)
					require.NoError(t, err)

					// Sign with operator.
					opSignDesc := VTXOSignDesc(
						keychain.KeyDescriptor{
							PubKey: operatorPub,
						},
						output,
						sigHashes, prevFetcher, 0, spendInfo,
					)
					opSig, err := operatorSigner.SignOutputRaw(
						tx, opSignDesc,
					)
					require.NoError(t, err)

					witness, err := VtxoCollabSpendWitness(
						clientSig, opSig, spendInfo,
					)
					require.NoError(t, err)

					return witness
				},
				wire.MaxTxInSequenceNum, true,
			)
		})

		t.Run("VtxoExitSpendInfo returns first exit", func(t *testing.T) {
			spendInfo, err := VtxoExitSpendInfo(vtxoScript)
			require.NoError(t, err)

			// Should get the first exit closure (short delay).
			firstSpendInfo, err := NewVtxoSpendInfo(vtxoScript, 0)
			require.NoError(t, err)

			require.Equal(t, firstSpendInfo.WitnessScript,
				spendInfo.WitnessScript)
		})
	})
}
