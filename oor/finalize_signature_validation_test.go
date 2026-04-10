package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func encodeFinalScriptWitness(t *testing.T, items ...[]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	err := wire.WriteVarInt(&buf, 0, uint64(len(items)))
	require.NoError(t, err)

	for i := range items {
		err = wire.WriteVarBytes(&buf, 0, items[i])
		require.NoError(t, err)
	}

	return buf.Bytes()
}

func TestParseFinalScriptWitnessRejectsOversizedCount(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := wire.WriteVarInt(&buf, 0, maxFinalWitnessItems+1)
	require.NoError(t, err)

	_, err = parseFinalScriptWitness(buf.Bytes())
	require.ErrorContains(t, err, "exceeds max")
}

func TestParseFinalScriptWitnessRejectsTrailingBytes(t *testing.T) {
	t.Parallel()

	raw := encodeFinalScriptWitness(t, []byte("sig"))
	raw = append(raw, 0x01, 0x02)

	_, err := parseFinalScriptWitness(raw)
	require.ErrorContains(t, err, "trailing bytes")
}
