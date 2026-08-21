package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newTestOperatorFundedInput builds a leased float input the way the daemon
// does: policy template and owner leaf come from the lease, no client key.
func newTestOperatorFundedInput(t *testing.T, floatKey *btcec.PrivateKey,
	operatorKey *btcec.PublicKey, outpoint wire.OutPoint,
	amount btcutil.Amount) TransferInput {

	t.Helper()

	const exitDelay = uint32(10)
	policyBytes, err := arkscript.EncodeStandardVTXOTemplate(
		floatKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	template, err := arkscript.DecodePolicyTemplate(policyBytes)
	require.NoError(t, err)
	pkScript, err := template.PkScript()
	require.NoError(t, err)

	ownerLeafPolicy, err := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				floatKey.PubKey(),
				operatorKey,
			},
		},
	}.Encode()
	require.NoError(t, err)

	return TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint:       outpoint,
			Amount:         amount,
			PkScript:       pkScript,
			PolicyTemplate: policyBytes,
			OperatorKey:    operatorKey,
			RelativeExpiry: exitDelay,
		},
		VTXOPolicyTemplate: policyBytes,
		OwnerLeafPolicy:    ownerLeafPolicy,
		OperatorFunded:     true,
	}
}

// signFloatOwnerLegForTest models the operator adding the float-owner leg to
// its own funded input's checkpoint during cosign.
func signFloatOwnerLegForTest(t *testing.T, floatKey *btcec.PrivateKey,
	in *TransferInput, checkpoint *psbt.Packet) {

	t.Helper()

	spendPath, err := in.EffectiveSpendPath()
	require.NoError(t, err)

	prevOut := &wire.TxOut{
		Value:    int64(in.VTXO.Amount),
		PkScript: in.VTXO.PkScript,
	}
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(checkpoint.UnsignedTx, prevFetcher)
	signDesc := spendPath.SpendInfo.BuildSignDescriptor(
		keychain.KeyDescriptor{
			PubKey: floatKey.PubKey(),
		},
		prevOut,
		sigHashes,
		prevFetcher,
		0,
	)

	signer := input.NewMockSigner([]*btcec.PrivateKey{floatKey}, nil)
	sig, err := signer.SignOutputRaw(checkpoint.UnsignedTx, signDesc)
	require.NoError(t, err)

	require.NoError(
		t,
		psbtutil.AddTaprootScriptSpendSig(
			&checkpoint.Inputs[0], floatKey.PubKey(),
			spendPath.WitnessScript, sig.Serialize(),
			signDesc.HashType,
		),
	)
}

// TestOperatorFundedInputValidate pins the marked input's contract: the lease
// supplies everything and no local key material may sneak in.
func TestOperatorFundedInputValidate(t *testing.T) {
	t.Parallel()

	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	valid := newTestOperatorFundedInput(
		t, floatKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x0f},
			Index: 0,
		},
		25_000,
	)
	require.NoError(t, valid.Validate())
	require.NotEmpty(t, valid.OwnerLeafScript)

	spendPath, err := valid.EffectiveSpendPath()
	require.NoError(t, err)
	require.NotEmpty(t, spendPath.WitnessScript)

	tests := []struct {
		name    string
		mutate  func(*TransferInput)
		wantErr string
	}{
		{
			name: "client key rejected",
			mutate: func(in *TransferInput) {
				in.VTXO.ClientKey.PubKey = floatKey.PubKey()
			},
			wantErr: "cannot carry a client key",
		},
		{
			name: "missing policy template",
			mutate: func(in *TransferInput) {
				in.VTXOPolicyTemplate = nil
			},
			wantErr: "requires a policy template",
		},
		{
			name: "missing owner leaf policy",
			mutate: func(in *TransferInput) {
				in.OwnerLeafPolicy = nil
			},
			wantErr: "requires an owner leaf policy",
		},
		{
			name: "missing operator key",
			mutate: func(in *TransferInput) {
				in.VTXO.OperatorKey = nil
			},
			wantErr: "requires an operator key",
		},
		{
			name: "custom spend rejected",
			mutate: func(in *TransferInput) {
				in.CustomSpend = &arkscript.SpendPath{}
			},
			wantErr: "cannot use a custom spend path",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			in := newTestOperatorFundedInput(
				t, floatKey, operatorKey.PubKey(),
				wire.OutPoint{
					Hash:  [32]byte{0x0f},
					Index: 0,
				},
				25_000,
			)
			test.mutate(&in)
			require.ErrorContains(
				t, in.Validate(), test.wantErr,
			)
		})
	}
}

// TestOperatorFundedSigningSkipsLocalKey proves both local signing sites skip
// the leased float input while the operator's checkpoint signature on it is
// still verified, and an extra float-owner leg is tolerated.
func TestOperatorFundedSigningSkipsLocalKey(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, operatorKey.PubKey(), wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			10_000,
		),
		newTestOperatorFundedInput(
			t, floatKey, operatorKey.PubKey(), wire.OutPoint{
				Hash:  [32]byte{0x02},
				Index: 0,
			},
			25_000,
		),
	}
	outputs := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    35_000,
	}}

	arkPSBT, checkpoints, err := BuildSubmitPackage(policy, inputs, outputs)
	require.NoError(t, err)

	// The operator cosigns every checkpoint and adds the float-owner leg
	// on its own funded input.
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)
	require.NoError(
		t, coSignCheckpointPSBTsForTest(
			operatorSigner, inputs, checkpoints,
		),
	)
	signFloatOwnerLegForTest(t, floatKey, &inputs[1], checkpoints[1])

	// Missing the operator leg must still fail for the float input, so
	// strip it and confirm before signing for real.
	stripped, err := psbtutil.Parse(mustSerializePSBT(t, checkpoints[1]))
	require.NoError(t, err)
	stripped.Inputs[0].TaprootScriptSpendSig = nil
	err = SignCheckpointPSBTs(
		input.NewMockSigner(
			[]*btcec.PrivateKey{clientKey}, nil,
		),
		inputs,
		[]*psbt.Packet{checkpoints[0], stripped},
	)
	require.ErrorContains(t, err, "missing taproot script spend signature")

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	require.NoError(
		t, SignCheckpointPSBTs(
			clientSigner, inputs, checkpoints,
		),
	)

	// The wallet input gains the client signature; the float input keeps
	// exactly the operator-provided legs.
	require.Len(t, checkpoints[0].Inputs[0].TaprootScriptSpendSig, 2)
	require.Len(t, checkpoints[1].Inputs[0].TaprootScriptSpendSig, 2)
	clientXOnly := schnorr.SerializePubKey(clientKey.PubKey())
	for _, sigRec := range checkpoints[1].Inputs[0].TaprootScriptSpendSig {
		require.NotEqual(t, clientXOnly, sigRec.XOnlyPubKey)
	}

	// Ark signing skips the float input entirely.
	require.NoError(
		t, SignArkPSBT(
			clientSigner, arkPSBT, checkpoints, inputs,
		),
	)

	floatCheckpointTxid := checkpoints[1].UnsignedTx.TxHash()
	for idx, txIn := range arkPSBT.UnsignedTx.TxIn {
		sigs := arkPSBT.Inputs[idx].TaprootScriptSpendSig
		if txIn.PreviousOutPoint.Hash == floatCheckpointTxid {
			require.Empty(t, sigs)

			continue
		}

		require.Len(t, sigs, 1)
	}
}

// TestNormalizeCheckpointOwnerLeavesSkipsOperatorFunded proves the float
// input's lease-supplied owner leaf survives owner-leaf normalization under a
// different session operator key.
func TestNormalizeCheckpointOwnerLeavesSkipsOperatorFunded(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	sessionOperatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	in := newTestOperatorFundedInput(
		t, floatKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x0f},
			Index: 1,
		},
		25_000,
	)
	require.NoError(t, in.Validate())
	wantLeafScript := append([]byte(nil), in.OwnerLeafScript...)
	wantLeafPolicy := append([]byte(nil), in.OwnerLeafPolicy...)

	inputs := []TransferInput{in}
	require.NoError(
		t,
		NormalizeCheckpointOwnerLeaves(
			arkscript.CheckpointPolicy{
				OperatorKey: sessionOperatorKey.PubKey(),
				CSVDelay:    10,
			},
			inputs,
		),
	)

	require.Equal(t, wantLeafScript, inputs[0].OwnerLeafScript)
	require.Equal(t, wantLeafPolicy, inputs[0].OwnerLeafPolicy)
}

// TestOperatorFundedSnapshotRoundTrip proves the marker and the lease-derived
// signing context survive the durable snapshot encoding.
func TestOperatorFundedSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	in := newTestOperatorFundedInput(
		t, floatKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x0f},
			Index: 2,
		},
		25_000,
	)
	require.NoError(t, in.Validate())

	snap, err := in.ToSnapshot()
	require.NoError(t, err)
	require.True(t, snap.OperatorFunded)
	require.Empty(t, snap.ClientPubKey)

	encoded, err := encodeTransferInputSnapshot(snap)
	require.NoError(t, err)
	decoded, err := decodeTransferInputSnapshot(encoded)
	require.NoError(t, err)
	require.True(t, decoded.OperatorFunded)

	restored, err := TransferInputFromSnapshot(decoded)
	require.NoError(t, err)
	require.True(t, restored.OperatorFunded)
	require.Nil(t, restored.VTXO.ClientKey.PubKey)
	require.Equal(t, in.VTXO.PkScript, restored.VTXO.PkScript)
	require.Equal(t, in.OwnerLeafScript, restored.OwnerLeafScript)
	require.NoError(t, restored.Validate())

	// The restored input still resolves the float collab spend path for
	// operator-signature verification.
	spendPath, err := restored.EffectiveSpendPath()
	require.NoError(t, err)
	require.NotEmpty(t, spendPath.WitnessScript)

	// A pkscript-less marked snapshot must fail closed rather than
	// resurrect an unsignable input.
	broken := *decoded
	broken.PkScript = nil
	_, err = TransferInputFromSnapshot(&broken)
	require.ErrorContains(t, err, "requires a stored pkscript")
}

// mustSerializePSBT round-trips a PSBT through its wire encoding so tests can
// mutate an independent copy.
func mustSerializePSBT(t *testing.T, packet *psbt.Packet) []byte {
	t.Helper()

	raw, err := psbtutil.Serialize(packet)
	require.NoError(t, err)

	return raw
}
