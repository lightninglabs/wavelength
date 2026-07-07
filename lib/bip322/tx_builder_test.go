package bip322

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestBuildToSpendMatchesBIP322Vectors asserts to_spend construction matches
// the BIP-322 test vectors.
func TestBuildToSpendMatchesBIP322Vectors(t *testing.T) {
	t.Parallel()

	challengeScript, err := hex.DecodeString(
		"00142b05d564e6a7a33c087f16e0f730d1440123799d",
	)
	require.NoError(t, err)

	emptyMessageHashHex := "c90c269c4f8fcbe6880f72a721ddfbf1914268a79" +
		"4cbb21cfafee13770ae19f1"
	helloWorldHashHex := "f0eb03b1a75ac6d9847f55c624a99169b5dccba2a3" +
		"1f5b23bea77ba270de0a7a"
	emptyToSpendTxID := "c5680aa69bb8d860bf82d4e9cd3504b55dde018de765" +
		"a91bb566283c545a99a7"
	helloWorldToSpendTxID := "b79d196740ad5217771c1098fc4a4b51e0535c32236" +
		"c71f1ea4d61a2d603352b"

	testCases := []struct {
		name           string
		messageHashHex string
		wantTxID       string
	}{
		{
			name:           "empty message",
			messageHashHex: emptyMessageHashHex,
			wantTxID:       emptyToSpendTxID,
		},
		{
			name:           "hello world",
			messageHashHex: helloWorldHashHex,
			wantTxID:       helloWorldToSpendTxID,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			messageHashBytes, err := hex.DecodeString(
				tc.messageHashHex,
			)
			require.NoError(t, err)
			require.Len(t, messageHashBytes, 32)

			var messageHash [32]byte
			copy(messageHash[:], messageHashBytes)

			tx, err := BuildToSpend(messageHash, challengeScript)
			require.NoError(t, err)

			require.Equal(t, tc.wantTxID, tx.TxHash().String())
			require.Equal(t, int32(0), tx.Version)
			require.Equal(t, uint32(0), tx.LockTime)
			require.Len(t, tx.TxIn, 1)
			require.Len(t, tx.TxOut, 1)
			require.Equal(t, uint32(0), tx.TxIn[0].Sequence)
			require.Equal(
				t, uint32(0xffffffff),
				tx.TxIn[0].PreviousOutPoint.Index,
			)
			require.Equal(t, int64(0), tx.TxOut[0].Value)
			require.Equal(t, challengeScript, tx.TxOut[0].PkScript)
			require.Equal(
				t, "0020"+tc.messageHashHex,
				hex.EncodeToString(tx.TxIn[0].SignatureScript),
			)
		})
	}
}

// TestBuildToSignMatchesBIP322Vectors asserts to_sign construction matches the
// BIP-322 test vectors when using the published witness data.
func TestBuildToSignMatchesBIP322Vectors(t *testing.T) {
	t.Parallel()

	challengeScript, err := hex.DecodeString(
		"00142b05d564e6a7a33c087f16e0f730d1440123799d",
	)
	require.NoError(t, err)

	emptyMessageHashHex := "c90c269c4f8fcbe6880f72a721ddfbf1914268a79" +
		"4cbb21cfafee13770ae19f1"
	helloWorldHashHex := "f0eb03b1a75ac6d9847f55c624a99169b5dccba2a3" +
		"1f5b23bea77ba270de0a7a"
	emptyWitnessSigHex := "30440220336801010aaf657d79662cac98a990a43ac6f3" +
		"76af2c84f8f76401ccb9d0231602201693a4e683db4a91944ca5cb11" +
		"527840366daf583a2c695fccf8e93483b52e3401"
	helloWorldWitnessSigHex := "304402206517c8637a7bfc3a154edcba6196d64b" +
		"bd5b73" +
		"955cb7da7d1626bcdde466c364022022bf10d19fc0bb69b" +
		"4596e306b" +
		"362acaa835293cf693bb176f7324b531f5afec01"
	witnessPubHex := "02c7f12003196442943d8588e01aee840423cc54fc1521526a" +
		"3b" +
		"85c2b0cbd58872"
	emptyToSignTxID := "1e9654e951a5ba44c8604c4de6c67fd78a27e81dcadcfe1e" +
		"df638ba3aaebaed6"
	helloWorldToSignTxID := "88737ae86f2077145f93cc4b153ae9a1cb8d56afa511" +
		"988c" +
		"149c5c8c9d93bddf"

	testCases := []struct {
		name           string
		messageHashHex string
		witnessSigHex  string
		witnessPubHex  string
		wantTxID       string
	}{
		{
			name:           "empty message",
			messageHashHex: emptyMessageHashHex,
			witnessSigHex:  emptyWitnessSigHex,
			witnessPubHex:  witnessPubHex,
			wantTxID:       emptyToSignTxID,
		},
		{
			name:           "hello world",
			messageHashHex: helloWorldHashHex,
			witnessSigHex:  helloWorldWitnessSigHex,
			witnessPubHex:  witnessPubHex,
			wantTxID:       helloWorldToSignTxID,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			messageHashBytes, err := hex.DecodeString(
				tc.messageHashHex,
			)
			require.NoError(t, err)
			require.Len(t, messageHashBytes, 32)

			var messageHash [32]byte
			copy(messageHash[:], messageHashBytes)

			toSpend, err := BuildToSpend(
				messageHash, challengeScript,
			)
			require.NoError(t, err)

			witnessSig, err := hex.DecodeString(tc.witnessSigHex)
			require.NoError(t, err)

			witnessPub, err := hex.DecodeString(tc.witnessPubHex)
			require.NoError(t, err)

			witness := wire.TxWitness{witnessSig, witnessPub}

			toSign, err := BuildToSignTx(
				toSpend, WithToSignWitness(witness),
			)
			require.NoError(t, err)

			require.Equal(t, tc.wantTxID, toSign.TxHash().String())
			require.Equal(t, int32(0), toSign.Version)
			require.Equal(t, uint32(0), toSign.LockTime)
			require.Len(t, toSign.TxIn, 1)
			require.Len(t, toSign.TxOut, 1)
			require.Equal(t, uint32(0), toSign.TxIn[0].Sequence)
			require.Equal(
				t, toSpend.TxHash(),
				toSign.TxIn[0].PreviousOutPoint.Hash,
			)
			require.Equal(
				t, uint32(0),
				toSign.TxIn[0].PreviousOutPoint.Index,
			)
			require.Equal(t, witness, toSign.TxIn[0].Witness)
			require.Equal(t, int64(0), toSign.TxOut[0].Value)
			require.Equal(
				t, []byte{txscript.OP_RETURN},
				toSign.TxOut[0].PkScript,
			)
		})
	}
}

// TestBuildToSignRejectsUnsupportedVersion asserts to_sign versions other than
// 0 and 2 are rejected.
func TestBuildToSignRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()

	var messageHash [32]byte

	toSpend, err := BuildToSpend(messageHash, []byte{txscript.OP_TRUE})
	require.NoError(t, err)

	_, err = BuildToSignTx(toSpend, WithToSignVersion(1))
	require.Error(t, err)
}

// TestBuildToSignAcceptsVersionTwo asserts version 2 is accepted for timelock
// use cases.
func TestBuildToSignAcceptsVersionTwo(t *testing.T) {
	t.Parallel()

	var messageHash [32]byte

	toSpend, err := BuildToSpend(messageHash, []byte{txscript.OP_TRUE})
	require.NoError(t, err)

	toSign, err := BuildToSignTx(
		toSpend, WithToSignVersion(2), WithToSignLockTime(500),
		WithToSignSequence(12),
	)
	require.NoError(t, err)
	require.Equal(t, int32(2), toSign.Version)
	require.Equal(t, uint32(500), toSign.LockTime)
	require.Equal(t, uint32(12), toSign.TxIn[0].Sequence)
}

// TestBuildToSignAddsProofOfFundsInputs asserts proof-of-funds inputs are
// appended to to_sign after the required to_spend input.
func TestBuildToSignAddsProofOfFundsInputs(t *testing.T) {
	t.Parallel()

	var messageHash [32]byte

	toSpend, err := BuildToSpend(messageHash, []byte{txscript.OP_TRUE})
	require.NoError(t, err)

	additionalPrevOut := wire.OutPoint{
		Hash: chainhash.Hash{
			0x01, 0x02, 0x03, 0x04,
		},
		Index: 7,
	}
	additionalSigScript := []byte{txscript.OP_TRUE}
	additionalWitness := wire.TxWitness{
		[]byte{
			0xaa,
		},
		[]byte{
			0xbb,
		},
	}

	toSign, err := BuildToSignTx(
		toSpend,
		WithToSignAdditionalInputs(
			AdditionalInput{
				PreviousOutPoint: additionalPrevOut,
				Sequence:         42,
				SignatureScript:  additionalSigScript,
				Witness:          additionalWitness,
			},
		),
	)
	require.NoError(t, err)
	require.Len(t, toSign.TxIn, 2)
	require.Equal(t, toSpend.TxHash(), toSign.TxIn[0].PreviousOutPoint.Hash)
	require.Equal(t, uint32(0), toSign.TxIn[0].PreviousOutPoint.Index)

	require.Equal(t, additionalPrevOut, toSign.TxIn[1].PreviousOutPoint)
	require.Equal(t, uint32(42), toSign.TxIn[1].Sequence)
	require.Equal(t, additionalSigScript, toSign.TxIn[1].SignatureScript)
	require.Equal(t, additionalWitness, toSign.TxIn[1].Witness)
}
