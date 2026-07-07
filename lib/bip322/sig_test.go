package bip322

import (
	"encoding/base64"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestFullSigEncodeDecodeRoundTrip asserts full signatures round-trip through
// raw and base64 encodings.
func TestFullSigEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 3,
		},
		Sequence: 7,
		Witness: wire.TxWitness{
			[]byte{0x01, 0x02},
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_RETURN},
	})

	orig := &Sig{
		ToSign: tx,
	}

	raw, err := orig.Encode()
	require.NoError(t, err)
	require.NotEmpty(t, raw)

	decodedRaw, err := DecodeSig(raw)
	require.NoError(t, err)
	require.Equal(t, orig.ToSign.TxHash(), decodedRaw.ToSign.TxHash())
	require.Equal(
		t, orig.ToSign.TxIn[0].Witness,
		decodedRaw.ToSign.TxIn[0].Witness,
	)

	b64, err := orig.EncodeBase64()
	require.NoError(t, err)
	require.NotEmpty(t, b64)

	decodedB64, err := DecodeSigBase64(b64)
	require.NoError(t, err)
	require.Equal(t, orig.ToSign.TxHash(), decodedB64.ToSign.TxHash())
	require.Equal(
		t, orig.ToSign.TxIn[0].Witness,
		decodedB64.ToSign.TxIn[0].Witness,
	)
}

// TestFullSigEncodeDecodeRoundTripProofInputs asserts full signature
// serialization preserves additional proof-of-funds inputs.
func TestFullSigEncodeDecodeRoundTripProofInputs(t *testing.T) {
	t.Parallel()

	firstHash := chainhash.Hash{
		0x01, 0x02, 0x03, 0x04,
	}
	secondHash := chainhash.Hash{
		0xaa, 0xbb, 0xcc, 0xdd,
	}

	tx := wire.NewMsgTx(2)
	tx.LockTime = 123

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  firstHash,
			Index: 0,
		},
		Sequence:        11,
		SignatureScript: []byte{txscript.OP_TRUE},
		Witness: wire.TxWitness{
			[]byte{0x01, 0x02},
		},
	})

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  secondHash,
			Index: 9,
		},
		Sequence:        22,
		SignatureScript: []byte{txscript.OP_FALSE},
		Witness: wire.TxWitness{
			[]byte{0xaa},
			[]byte{0xbb},
		},
	})

	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_RETURN},
	})

	orig := &Sig{
		ToSign: tx,
	}

	raw, err := orig.Encode()
	require.NoError(t, err)

	decoded, err := DecodeSig(raw)
	require.NoError(t, err)
	require.Equal(t, int32(2), decoded.ToSign.Version)
	require.Equal(t, uint32(123), decoded.ToSign.LockTime)
	require.Len(t, decoded.ToSign.TxIn, 2)

	decodedFirst := decoded.ToSign.TxIn[0]
	decodedSecond := decoded.ToSign.TxIn[1]
	firstInput := tx.TxIn[0]
	secondInput := tx.TxIn[1]

	require.Equal(
		t, firstInput.PreviousOutPoint, decodedFirst.PreviousOutPoint,
	)
	require.Equal(t, firstInput.Sequence, decodedFirst.Sequence)
	require.Equal(
		t, firstInput.SignatureScript, decodedFirst.SignatureScript,
	)
	require.Equal(t, firstInput.Witness, decodedFirst.Witness)
	require.Equal(
		t, secondInput.PreviousOutPoint, decodedSecond.PreviousOutPoint,
	)
	require.Equal(t, secondInput.Sequence, decodedSecond.Sequence)
	require.Equal(
		t, secondInput.SignatureScript, decodedSecond.SignatureScript,
	)
	require.Equal(t, secondInput.Witness, decodedSecond.Witness)
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
