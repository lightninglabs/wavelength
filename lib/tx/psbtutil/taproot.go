package psbtutil

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// AddTapLeafScript ensures the PSBT input includes the leaf script and
// control block for the collaborative VTXO leaf. If the leaf is already
// present the function is a no-op.
func AddTapLeafScript(in *psbt.PInput, spendInfo *arkscript.SpendInfo) error {
	if in == nil {
		return fmt.Errorf("psbt input must be provided")
	}

	if spendInfo == nil {
		return fmt.Errorf("spend info must be provided")
	}

	needle := &psbt.TaprootTapLeafScript{
		ControlBlock: spendInfo.ControlBlock,
		Script:       spendInfo.WitnessScript,
		LeafVersion:  txscript.BaseLeafVersion,
	}

	for i := range in.TaprootLeafScript {
		existing := in.TaprootLeafScript[i]
		if existing == nil {
			continue
		}

		if bytes.Equal(existing.ControlBlock, needle.ControlBlock) &&
			bytes.Equal(existing.Script, needle.Script) &&
			existing.LeafVersion == needle.LeafVersion {
			return nil
		}
	}

	in.TaprootLeafScript = append(in.TaprootLeafScript, needle)

	return nil
}

// AddTaprootScriptSpendSig adds or replaces a taproot script-path spend
// signature in the PSBT input, keyed by (x-only pubkey, leaf hash).
func AddTaprootScriptSpendSig(in *psbt.PInput, pubKey *btcec.PublicKey,
	leafScript []byte, sig []byte, sigHash txscript.SigHashType) error {

	switch {
	case in == nil:
		return fmt.Errorf("psbt input must be provided")

	case pubKey == nil:
		return fmt.Errorf("pubkey must be provided")

	case len(leafScript) == 0:
		return fmt.Errorf("leaf script must be provided")

	case len(sig) == 0:
		return fmt.Errorf("signature must be provided")
	}

	leafHash := txscript.NewBaseTapLeaf(leafScript).TapHash()
	leafHashBytes := make([]byte, 0, len(leafHash))
	leafHashBytes = append(leafHashBytes, leafHash[:]...)

	needle := &psbt.TaprootScriptSpendSig{
		XOnlyPubKey: schnorr.SerializePubKey(pubKey),
		LeafHash:    leafHashBytes,
		Signature:   sig,
		SigHash:     sigHash,
	}

	for i := range in.TaprootScriptSpendSig {
		existing := in.TaprootScriptSpendSig[i]
		if existing == nil {
			continue
		}

		if existing.EqualKey(needle) {
			in.TaprootScriptSpendSig[i] = needle

			return nil
		}
	}

	in.TaprootScriptSpendSig = append(
		in.TaprootScriptSpendSig, needle,
	)

	return nil
}
