package rounds

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/bip322"
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

	// OperatorKey is the operator/cosigner key used to construct the
	// VTXOPolicy for deriving spend info.
	OperatorKey *btcec.PublicKey

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

	tapscript, err := arkscript.VTXOTapScript(
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
				Tapscript:   tapscript,
				OperatorKey: h.operatorPub,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: clientPub,
				},
				Sequence: exitDelay,
				Signer:   clientSigner,
			},
		},
	)

	_, err = ValidateJoinRequestAtHeight(t.Context(), h.env, req, 500, 0)
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

	tapscript, err := arkscript.VTXOTapScript(
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
				Tapscript:   tapscript,
				OperatorKey: h.operatorPub,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: clientPub,
				},
				Sequence: exitDelay,
				Signer:   clientSigner,
			},
		},
	)

	_, err = ValidateJoinRequestAtHeight(t.Context(), h.env, req, 500, 0)
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
	tapscript, err := arkscript.VTXOTapScript(
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
				Tapscript:   tapscript,
				OperatorKey: h.operatorPub,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: clientPub,
				},
				Sequence: vtxoExitDelay,
				Signer:   clientSigner,
			},
		},
	)

	_, err = ValidateJoinRequestAtHeight(t.Context(), h.env, req, 500, 0)
	require.NoError(t, err)
}

// TestValidateJoinRequestAuthNegative covers rejection paths in
// validateJoinRequestAuth using table-driven subtests. Each subtest
// starts from a valid base state and mutates one aspect to trigger
// a specific error.
func TestValidateJoinRequestAuthNegative(t *testing.T) {
	t.Parallel()

	// Build shared base state: one boarding input with valid auth.
	clientPub, clientSigner := testutils.CreateKey(80)
	operatorPub, _ := testutils.CreateKey(81)

	outpointA := wire.OutPoint{
		Hash:  [32]byte{0x50},
		Index: 0,
	}
	const exitDelay uint32 = 144

	tapscript, err := arkscript.VTXOTapScript(
		clientPub, operatorPub, exitDelay,
	)
	require.NoError(t, err)

	pkScript := buildExpectedPkScript(
		t, clientPub, operatorPub, exitDelay,
	)

	prevOut := &wire.TxOut{
		Value:    100_000,
		PkScript: pkScript,
	}

	proofInput := joinAuthProofInput{
		Outpoint:    outpointA,
		PrevOut:     prevOut,
		Tapscript:   tapscript,
		OperatorKey: operatorPub,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: clientPub,
		},
		Sequence: exitDelay,
		Signer:   clientSigner,
	}

	boardingInput := &BoardingInput{
		Outpoint:  &outpointA,
		Tapscript: tapscript,
		Value:     100_000,
		PkScript:  pkScript,
		ClientKey: clientPub,
	}

	// makeValidReq builds a fresh valid request and auth payload.
	// Each subtest must call this to get an independent copy.
	makeValidReq := func(t *testing.T) *types.JoinRoundRequest {
		t.Helper()

		req := &types.JoinRoundRequest{
			BoardingReqs: []*types.BoardingRequest{
				{
					Outpoint:    &outpointA,
					ClientKey:   clientPub,
					OperatorKey: operatorPub,
					ExitDelay:   exitDelay,
				},
			},
		}
		req.Auth = buildTestJoinAuth(
			t, req, 500, []joinAuthProofInput{proofInput},
		)

		return req
	}

	// authNegativeCase describes a single negative test case for
	// validateJoinRequestAuth.
	type authNegativeCase struct {
		name string

		// mutate modifies the valid request to trigger a
		// specific error path.
		mutate func(t *testing.T, req *types.JoinRoundRequest)

		// boardingInputs overrides the default single boarding
		// input when set.
		boardingInputs []*BoardingInput

		// wantErr is the sentinel error expected, checked via
		// errors.Is. Nil when wantContains is used instead.
		wantErr error

		// wantContains is a substring expected in the error
		// message when wantErr is nil.
		wantContains string
	}

	cases := []authNegativeCase{
		{
			name: "auth_missing",
			mutate: func(_ *testing.T,
				req *types.JoinRoundRequest) {

				req.Auth = nil
			},
			wantErr: ErrJoinRequestAuthMissing,
		},

		{
			name: "identifier_missing",
			mutate: func(_ *testing.T,
				req *types.JoinRoundRequest) {

				req.Identifier = nil
			},
			wantErr: ErrJoinRequestIdentifierMissing,
		},

		{
			name: "message_empty",
			mutate: func(_ *testing.T,
				req *types.JoinRoundRequest) {

				req.Auth.Message = nil
			},
			wantContains: "message must be provided",
		},

		{
			name: "signature_empty",
			mutate: func(_ *testing.T,
				req *types.JoinRoundRequest) {

				req.Auth.Signature = nil
			},
			wantContains: "signature must be provided",
		},

		{
			name: "signature_too_large",
			mutate: func(_ *testing.T,
				req *types.JoinRoundRequest) {

				req.Auth.Signature = make(
					[]byte, joinAuthMaxSignatureSize+1,
				)
			},
			wantContains: "signature size",
		},

		{
			name: "message_mismatch",
			mutate: func(_ *testing.T,
				req *types.JoinRoundRequest) {

				// Flip a byte so the message no longer
				// matches the canonical encoding.
				req.Auth.Message[0] ^= 0xff
			},
			wantErr: ErrJoinRequestAuthMessageMismatch,
		},

		{
			name: "input_count_mismatch",
			mutate: func(t *testing.T,
				req *types.JoinRoundRequest) {

				t.Helper()

				// Build request with 2 boarding inputs
				// so the message is canonical for both.
				extra := wire.OutPoint{
					Hash:  [32]byte{0x51},
					Index: 0,
				}
				req.BoardingReqs = append(
					req.BoardingReqs,
					&types.BoardingRequest{
						Outpoint:    &extra,
						ClientKey:   clientPub,
						OperatorKey: operatorPub,
						ExitDelay:   exitDelay,
					},
				)

				// Sign auth for only 1 input so the
				// signature has fewer inputs than the
				// request expects.
				req.Auth = buildTestJoinAuth(
					t, req, 500,
					[]joinAuthProofInput{
						proofInput,
					},
				)
			},
			boardingInputs: []*BoardingInput{
				boardingInput,
				{
					Outpoint: &wire.OutPoint{
						Hash:  [32]byte{0x51},
						Index: 0,
					},
					Tapscript: tapscript,
					Value:     50_000,
					PkScript:  pkScript,
					ClientKey: clientPub,
				},
			},
			wantErr: ErrJoinRequestAuthInputCountMismatch,
		},

		{
			name: "input_order_mismatch",
			mutate: func(t *testing.T,
				req *types.JoinRoundRequest) {

				t.Helper()

				// Build request with [B, A] order so
				// the canonical message encodes [B, A].
				outpointB := wire.OutPoint{
					Hash:  [32]byte{0x52},
					Index: 0,
				}
				req.BoardingReqs = []*types.BoardingRequest{
					{
						Outpoint:    &outpointB,
						ClientKey:   clientPub,
						OperatorKey: operatorPub,
						ExitDelay:   exitDelay,
					},
					{
						Outpoint:    &outpointA,
						ClientKey:   clientPub,
						OperatorKey: operatorPub,
						ExitDelay:   exitDelay,
					},
				}

				// Sign auth with proof inputs in
				// [A, B] order so the signature's
				// inputs don't match the request's
				// [B, A] outpoint order.
				proofB := proofInput
				proofB.Outpoint = outpointB

				req.Auth = buildTestJoinAuth(
					t, req, 500,
					[]joinAuthProofInput{
						proofInput, proofB,
					},
				)
			},
			boardingInputs: []*BoardingInput{
				{
					Outpoint: &wire.OutPoint{
						Hash:  [32]byte{0x52},
						Index: 0,
					},
					Tapscript: tapscript,
					Value:     50_000,
					PkScript:  pkScript,
					ClientKey: clientPub,
				},
				boardingInput,
			},
			wantErr: ErrJoinRequestAuthInputOrderMismatch,
		},

		{
			name: "wrong_signer",
			mutate: func(t *testing.T,
				req *types.JoinRoundRequest) {

				t.Helper()

				// Sign the auth with a different key
				// than the one in the boarding input.
				attackerPub, attackerSigner :=
					testutils.CreateKey(82)

				attackerProof := joinAuthProofInput{
					Outpoint:    outpointA,
					PrevOut:     prevOut,
					Tapscript:   tapscript,
					OperatorKey: operatorPub,
					KeyDesc: keychain.KeyDescriptor{
						PubKey: attackerPub,
					},
					Sequence: exitDelay,
					Signer:   attackerSigner,
				}

				// Set attacker as identifier so the
				// message challenge uses their key.
				req.Identifier = attackerPub
				req.Auth = buildTestJoinAuth(
					t, req, 500,
					[]joinAuthProofInput{
						attackerProof,
					},
				)
			},
			wantContains: "join request auth verification",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := makeValidReq(t)
			tc.mutate(t, req)

			// Use override boarding inputs if provided,
			// otherwise use the default single input.
			inputs := []*BoardingInput{boardingInput}
			if tc.boardingInputs != nil {
				inputs = tc.boardingInputs
			}

			err := validateJoinRequestAuth(
				req, inputs, nil, 500,
			)
			require.Error(t, err)

			if tc.wantErr != nil {
				require.True(t,
					errors.Is(err, tc.wantErr),
					"expected %v, got: %v",
					tc.wantErr, err,
				)
			} else {
				require.Contains(t,
					err.Error(),
					tc.wantContains,
				)
			}
		})
	}
}

// buildTestJoinAuth builds a full-format BIP-322 join-auth payload for tests.
func buildTestJoinAuth(t *testing.T, req *types.JoinRoundRequest,
	validUntil uint32,
	proofInputs []joinAuthProofInput) *types.JoinRoundAuth {

	t.Helper()
	require.NotEmpty(t, proofInputs)

	for i := range req.BoardingReqs {
		if len(req.BoardingReqs[i].PolicyTemplate) > 0 {
			continue
		}

		policy, err := arkscript.EncodeStandardVTXOTemplate(
			req.BoardingReqs[i].ClientKey,
			req.BoardingReqs[i].OperatorKey,
			req.BoardingReqs[i].ExitDelay,
		)
		require.NoErrorf(t, err, "boarding request %d policy", i)
		req.BoardingReqs[i].PolicyTemplate = policy
	}

	for i := range req.VTXOReqs {
		if len(req.VTXOReqs[i].PolicyTemplate) > 0 {
			continue
		}

		policy, err := arkscript.EncodeStandardVTXOTemplate(
			req.VTXOReqs[i].ClientKey,
			req.VTXOReqs[i].OperatorKey,
			req.VTXOReqs[i].Expiry,
		)
		require.NoErrorf(t, err, "vtxo request %d policy", i)
		req.VTXOReqs[i].PolicyTemplate = policy
	}

	if req.Identifier == nil {
		req.Identifier = proofInputs[0].KeyDesc.PubKey
	}
	require.NotNil(t, req.Identifier)

	message, err := types.JoinRoundAuthMessage(req)
	require.NoError(t, err)

	intent := &bip322.Intent{
		Payload:    message,
		ValidFrom:  0,
		ValidUntil: validUntil,
	}

	intentMessage, err := intent.SigningMessage()
	require.NoError(t, err)

	challengeScript, err := bip322.JoinRoundMessageChallenge(req.Identifier)
	require.NoError(t, err)

	messageHash := bip322.MessageHash(intentMessage)
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

		vtxoPolicy, err := arkscript.NewVTXOPolicy(
			proofInput.KeyDesc.PubKey,
			proofInput.OperatorKey,
			proofInput.Sequence,
		)
		require.NoError(t, err)

		spendInfo, err := vtxoPolicy.ExitSpendInfo()
		require.NoError(t, err)

		signDesc := spendInfo.BuildSignDescriptor(
			proofInput.KeyDesc, proofInput.PrevOut,
			sigHashes, prevFetcher, inputIndex,
		)

		witness, err := arkscript.VTXOTimeoutSpendWitness(
			proofInput.Signer, signDesc, toSign,
		)
		require.NoError(t, err)

		toSign.TxIn[inputIndex].Witness = witness
	}

	sig := &bip322.Sig{ToSign: toSign}
	rawSig, err := sig.Encode()
	require.NoError(t, err)

	return &types.JoinRoundAuth{
		Message:    message,
		ValidFrom:  0,
		ValidUntil: validUntil,
		Signature:  rawSig,
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
