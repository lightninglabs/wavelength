package bip322

import (
	"encoding/base64"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestFullSigEncodeDecodeRoundTrip asserts full signatures round-trip through
// the raw (and, for the single-input case, base64) encodings while preserving
// version, lock time, and every per-input field.
func TestFullSigEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()

	singleInput := wire.NewMsgTx(2)
	singleInput.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 3,
		},
		Sequence: 7,
		Witness: wire.TxWitness{
			[]byte{0x01, 0x02},
		},
	})
	singleInput.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_RETURN},
	})

	proofInputs := wire.NewMsgTx(2)
	proofInputs.LockTime = 123
	proofInputs.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x01, 0x02, 0x03, 0x04},
			Index: 0,
		},
		Sequence:        11,
		SignatureScript: []byte{txscript.OP_TRUE},
		Witness: wire.TxWitness{
			[]byte{0x01, 0x02},
		},
	})
	proofInputs.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0xaa, 0xbb, 0xcc, 0xdd},
			Index: 9,
		},
		Sequence:        22,
		SignatureScript: []byte{txscript.OP_FALSE},
		Witness: wire.TxWitness{
			[]byte{0xaa},
			[]byte{0xbb},
		},
	})
	proofInputs.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_RETURN},
	})

	tests := []struct {
		name        string
		toSign      *wire.MsgTx
		checkBase64 bool
	}{
		{
			name:        "single input",
			toSign:      singleInput,
			checkBase64: true,
		},
		{
			name:   "proof of funds inputs",
			toSign: proofInputs,
		},
	}

	for i := 0; i < len(tests); i++ {
		tc := tests[i]

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			orig := &Sig{
				ToSign: tc.toSign,
			}

			raw, err := orig.Encode()
			require.NoError(t, err)
			require.NotEmpty(t, raw)

			decoded, err := DecodeSig(raw)
			require.NoError(t, err)
			requireSigEqual(t, orig, decoded)

			if !tc.checkBase64 {
				return
			}

			b64, err := orig.EncodeBase64()
			require.NoError(t, err)
			require.NotEmpty(t, b64)

			decodedB64, err := DecodeSigBase64(b64)
			require.NoError(t, err)
			requireSigEqual(t, orig, decodedB64)
		})
	}
}

// requireSigEqual asserts a decoded full-format signature preserves the
// original transaction. The non-witness txid commits to version, lock time,
// outpoints, sequences, and signature scripts; witnesses (which the txid
// excludes) are compared per input.
func requireSigEqual(t *testing.T, orig, decoded *Sig) {
	t.Helper()

	require.Equal(t, orig.ToSign.TxHash(), decoded.ToSign.TxHash())
	require.Len(t, decoded.ToSign.TxIn, len(orig.ToSign.TxIn))

	for j := range orig.ToSign.TxIn {
		require.Equal(
			t, orig.ToSign.TxIn[j].Witness,
			decoded.ToSign.TxIn[j].Witness,
		)
	}
}

// TestDecodeSigRejectsEmptyBytes asserts empty payloads are rejected.
func TestDecodeSigRejectsEmptyBytes(t *testing.T) {
	t.Parallel()

	_, err := DecodeSig(nil)
	require.Error(t, err)
}

// TestDecodeSigRejectsNonTxPayload asserts witness-stack payloads are rejected
// because this package only supports full-format signatures.
func TestDecodeSigRejectsNonTxPayload(t *testing.T) {
	t.Parallel()

	simpleFormatB64 := "AkcwRAIgZRfIY3p7/DoVTty6YZbWS71bc5Vct9p9Fia83eRm" +
		"w2QCICK/ENGfwLtptFluMGs2KsqoNSk89pO7F29zJLUx9a/sASECx/EgA" +
		"xlkQpQ9hYjgGu6EBCPMVPwVIVJqO4XCsMvViHI="

	raw, err := base64.StdEncoding.DecodeString(
		simpleFormatB64,
	)
	require.NoError(t, err)

	_, err = DecodeSig(raw)
	require.Error(t, err)
}

// TestDecodeSigRejectsTrailingBytes asserts extra trailing bytes are rejected.
func TestDecodeSigRejectsTrailingBytes(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 1,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_RETURN},
	})

	sig := &Sig{
		ToSign: tx,
	}

	raw, err := sig.Encode()
	require.NoError(t, err)

	raw = append(raw, 0x00)

	_, err = DecodeSig(raw)
	require.Error(t, err)
}

// TestDecodeSigBase64RejectsInvalidBase64 asserts invalid base64 is rejected
// before parser dispatch.
func TestDecodeSigBase64RejectsInvalidBase64(t *testing.T) {
	t.Parallel()

	invalid := base64.StdEncoding.EncodeToString([]byte{0x01, 0x02})
	invalid = invalid[:len(invalid)-1]

	_, err := DecodeSigBase64(invalid)
	require.Error(t, err)
}
