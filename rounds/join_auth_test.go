package rounds

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/bip322"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// joinAuthProofInput describes one timeout-path ownership proof input used in
// test join-auth signatures.
type joinAuthProofInput struct {
	// Outpoint is the UTXO/VTXO outpoint proven by this input.
	Outpoint wire.OutPoint

	// PrevOut is the previous output metadata for this input.
	PrevOut *wire.TxOut

	// Tapscript is the tapscript tree used to derive timeout spend data.
	Tapscript *waddrmgr.Tapscript

	// KeyDesc identifies the signing key for timeout-path signatures.
	KeyDesc keychain.KeyDescriptor

	// Sequence is the CSV-compatible nSequence used for this input.
	Sequence uint32

	// Signer produces timeout-path signatures for this input.
	Signer input.Signer
}

// TestValidateJoinRequestAuthBoardingValid asserts join auth is accepted for a
// valid boarding ownership proof.
func TestValidateJoinRequestAuthBoardingValid(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.env.DisableJoinRequestAuth = false

	clientPub, clientSigner := testutils.CreateKey(70)
	outpoint := wire.OutPoint{
		Hash:  [32]byte{0x41},
		Index: 0,
	}
	const exitDelay uint32 = 144

	h.setupBoardingInputValidationOnly(&outpoint, clientPub, exitDelay, 12)

	req := &types.JoinRoundRequest{
		BoardingReqs: []*types.BoardingRequest{
			{
				Outpoint:    &outpoint,
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   exitDelay,
			},
		},
	}

	tapscript, err := scripts.VTXOTapScript(
		clientPub, h.operatorPub, exitDelay,
	)
	require.NoError(t, err)

	pkScript := buildExpectedPkScript(
		t, clientPub, h.operatorPub, exitDelay,
	)
	req.Auth = buildTestJoinAuth(
		t, req, 500, []joinAuthProofInput{
			{
				Outpoint: outpoint,
				PrevOut: &wire.TxOut{
					Value:    100_000,
					PkScript: pkScript,
				},
				Tapscript: tapscript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: clientPub,
				},
				Sequence: exitDelay,
				Signer:   clientSigner,
			},
		},
	)

	_, err = ValidateJoinRequestAtHeight(t.Context(), h.env, req, 500)
	require.NoError(t, err)
}

// TestValidateJoinRequestAuthRejectsExpired asserts stale join-auth windows are
// rejected.
func TestValidateJoinRequestAuthRejectsExpired(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.env.DisableJoinRequestAuth = false

	clientPub, clientSigner := testutils.CreateKey(71)
	outpoint := wire.OutPoint{
		Hash:  [32]byte{0x42},
		Index: 0,
	}
	const exitDelay uint32 = 144

	h.setupBoardingInputValidationOnly(&outpoint, clientPub, exitDelay, 12)

	req := &types.JoinRoundRequest{
		BoardingReqs: []*types.BoardingRequest{
			{
				Outpoint:    &outpoint,
				ClientKey:   clientPub,
				OperatorKey: h.operatorPub,
				ExitDelay:   exitDelay,
			},
		},
	}

	tapscript, err := scripts.VTXOTapScript(
		clientPub, h.operatorPub, exitDelay,
	)
	require.NoError(t, err)

	pkScript := buildExpectedPkScript(
		t, clientPub, h.operatorPub, exitDelay,
	)
	req.Auth = buildTestJoinAuth(
		t, req, 100, []joinAuthProofInput{
			{
				Outpoint: outpoint,
				PrevOut: &wire.TxOut{
					Value:    100_000,
					PkScript: pkScript,
				},
				Tapscript: tapscript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: clientPub,
				},
				Sequence: exitDelay,
				Signer:   clientSigner,
			},
		},
	)

	_, err = ValidateJoinRequestAtHeight(t.Context(), h.env, req, 500)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expired")
}

// TestValidateJoinRequestAuthForfeitValid asserts join auth accepts forfeit
// ownership proofs.
func TestValidateJoinRequestAuthForfeitValid(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.env.DisableJoinRequestAuth = false

	clientPub, clientSigner := testutils.CreateKey(72)
	outpoint := wire.OutPoint{
		Hash:  [32]byte{0x43},
		Index: 1,
	}

	vtxo := h.setupValidForfeitVTXO(&outpoint, clientPub, h.roundID)
	require.NotNil(t, vtxo)
	require.NotNil(t, vtxo.Descriptor)

	const vtxoExitDelay uint32 = 144
	tapscript, err := scripts.VTXOTapScript(
		clientPub, h.operatorPub, vtxoExitDelay,
	)
	require.NoError(t, err)

	req := &types.JoinRoundRequest{
		ForfeitReqs: []*types.ForfeitRequest{
			{VTXOOutpoint: &outpoint},
		},
	}
	req.Auth = buildTestJoinAuth(
		t, req, 500, []joinAuthProofInput{
			{
				Outpoint: outpoint,
				PrevOut: &wire.TxOut{
					Value:    int64(vtxo.Descriptor.Amount),
					PkScript: vtxo.Descriptor.PkScript,
				},
				Tapscript: tapscript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: clientPub,
				},
				Sequence: vtxoExitDelay,
				Signer:   clientSigner,
			},
		},
	)

	_, err = ValidateJoinRequestAtHeight(t.Context(), h.env, req, 500)
	require.NoError(t, err)
}

// buildTestJoinAuth builds a full-format BIP-322 join-auth payload for tests.
func buildTestJoinAuth(t *testing.T, req *types.JoinRoundRequest,
	validUntil uint32,
	proofInputs []joinAuthProofInput) *types.JoinRoundAuth {

	t.Helper()
	require.NotEmpty(t, proofInputs)

	if req.Identifier == nil {
		req.Identifier = proofInputs[0].KeyDesc.PubKey
	}
	require.NotNil(t, req.Identifier)

	message, err := types.JoinRoundAuthMessage(req)
	require.NoError(t, err)

	challengeScript, err := bip322.JoinRoundMessageChallenge(req.Identifier)
	require.NoError(t, err)

	messageHash := bip322.MessageHash(message)
	toSpend, err := bip322.BuildToSpend(
		messageHash, challengeScript,
	)
	require.NoError(t, err)

	additionalInputs := make([]bip322.AdditionalInput, 0, len(proofInputs))
	for i := 0; i < len(proofInputs); i++ {
		proofInput := proofInputs[i]
		additionalInput := bip322.AdditionalInput{
			PreviousOutPoint: proofInput.Outpoint,
			Sequence:         proofInput.Sequence,
			WitnessUtxo:      proofInput.PrevOut,
		}
		additionalInputs = append(additionalInputs, additionalInput)
	}

	toSign, err := bip322.BuildToSignTx(
		toSpend,
		bip322.WithToSignVersion(2),
		bip322.WithBlockWindow(bip322.BlockWindow{
			ValidFromBlock:  0,
			ValidUntilBlock: validUntil,
		}),
		bip322.WithToSignAdditionalInputs(additionalInputs...),
	)
	require.NoError(t, err)

	packet, err := psbt.NewFromUnsignedTx(toSign)
	require.NoError(t, err)
	require.NoError(t, packet.SanityCheck())

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(proofInputs)+1)
	prevOuts[toSign.TxIn[0].PreviousOutPoint] = toSpend.TxOut[0]
	for i := 0; i < len(proofInputs); i++ {
		proofInput := proofInputs[i]
		prevOuts[proofInput.Outpoint] = proofInput.PrevOut
	}

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(toSign, prevFetcher)

	signJoinAuthMessageInput(t, toSign, toSpend, proofInputs[0], sigHashes,
		prevFetcher)

	for i := 0; i < len(proofInputs); i++ {
		proofInput := proofInputs[i]
		inputIndex := i + 1

		spendInfo, err := scripts.NewVTXOSpendInfo(
			proofInput.Tapscript, scripts.VTXOTimeoutPathLeaf,
		)
		require.NoError(t, err)

		signDesc := scripts.VTXOSignDesc(
			proofInput.KeyDesc, proofInput.PrevOut, sigHashes,
			prevFetcher, inputIndex, spendInfo,
		)

		witness, err := scripts.VTXOTimeoutSpendWitness(
			proofInput.Signer, signDesc, toSign,
		)
		require.NoError(t, err)

		toSign.TxIn[inputIndex].Witness = witness
	}

	sig := &bip322.Sig{ToSign: toSign}
	rawSig, err := sig.Encode()
	require.NoError(t, err)

	return &types.JoinRoundAuth{
		Message:   message,
		Signature: rawSig,
	}
}

// signJoinAuthMessageInput signs to_sign input 0 with the identifier key so
// the proof is tied to the request challenge script.
func signJoinAuthMessageInput(t *testing.T, toSign *wire.MsgTx,
	toSpend *wire.MsgTx, proofInput joinAuthProofInput,
	sigHashes *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) {

	t.Helper()

	signDesc := &input.SignDescriptor{
		KeyDesc:           proofInput.KeyDesc,
		Output:            toSpend.TxOut[0],
		HashType:          txscript.SigHashDefault,
		InputIndex:        0,
		SignMethod:        input.TaprootKeySpendBIP0086SignMethod,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		TapTweak:          []byte{},
	}

	sig, err := proofInput.Signer.SignOutputRaw(toSign, signDesc)
	require.NoError(t, err)

	toSign.TxIn[0].Witness = wire.TxWitness{
		sig.Serialize(),
	}
}
