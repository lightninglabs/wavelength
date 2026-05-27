package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestAttachExternalTaprootScriptSignaturesRejectsUnexpectedPubKey verifies an
// external signature must come from a key required by the custom spend path.
func TestAttachExternalTaprootScriptSignaturesRejectsUnexpectedPubKey(
	t *testing.T) {

	t.Parallel()

	_, requiredKey := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x01}, 32))
	_, unexpectedKey := btcec.PrivKeyFromBytes(
		bytes.Repeat(
			[]byte{0x02}, 32,
		),
	)
	witnessScript := []byte{0x51}

	input := &TransferInput{
		CustomSpend: &arkscript.SpendPath{
			SpendInfo: &arkscript.SpendInfo{
				WitnessScript: witnessScript,
			},
		},
		CustomSpendKeys: []*btcec.PublicKey{
			requiredKey,
		},
		ExternalSignatures: []ExternalTaprootScriptSignature{
			{
				PubKey:        unexpectedKey,
				WitnessScript: witnessScript,
				Signature: []byte{
					0x01,
				},
			},
		},
	}

	err := attachExternalTaprootScriptSignatures(input, &psbt.PInput{})
	require.ErrorContains(t, err, "pubkey is not required")
}
