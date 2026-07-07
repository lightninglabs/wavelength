package swaps

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightningnetwork/lnd/lntypes"
)

// preimageMatchesHash reports whether preimage hashes to paymentHash.
func preimageMatchesHash(preimage *lntypes.Preimage,
	paymentHash lntypes.Hash) bool {

	if preimage == nil {
		return false
	}

	return lntypes.Hash(sha256.Sum256(preimage[:])) ==
		paymentHash
}

// extractPreimageFromCheckpoint extracts a 32-byte preimage from a finalized
// checkpoint PSBT when the spend used the vHTLC claim path.
func extractPreimageFromCheckpoint(psbtBytes []byte) (*lntypes.Preimage,
	error) {

	pkt, err := psbt.NewFromRawBytes(
		bytes.NewReader(psbtBytes), false,
	)
	if err != nil {
		return nil, fmt.Errorf("parse checkpoint PSBT: %w", err)
	}

	if len(pkt.Inputs) == 0 {
		return nil, nil
	}

	inp := pkt.Inputs[0]

	if len(inp.FinalScriptWitness) > 0 {
		items, err := deserializeWitness(inp.FinalScriptWitness)
		if err != nil {
			return nil, fmt.Errorf("deserialize witness: %w", err)
		}

		if p := findPreimage(items); p != nil {
			return p, nil
		}
	}

	conditionWitness, err := arkscript.GetConditionWitnessPSBTInput(inp)
	switch {
	case err == nil:
		if p := findPreimage(conditionWitness); p != nil {
			return p, nil
		}

	case err != nil &&
		!errors.Is(err, arkscript.ErrConditionWitnessNotFound):
		return nil, fmt.Errorf("decode condition witness: %w", err)
	}

	for _, sig := range inp.TaprootScriptSpendSig {
		if len(sig.Signature) == lntypes.HashSize {
			var preimage lntypes.Preimage
			copy(preimage[:], sig.Signature)

			return &preimage, nil
		}
	}

	for _, leafScript := range inp.TaprootLeafScript {
		if len(leafScript.Script) == lntypes.HashSize {
			var preimage lntypes.Preimage
			copy(preimage[:], leafScript.Script)

			return &preimage, nil
		}
	}

	if pkt.UnsignedTx != nil &&
		len(pkt.UnsignedTx.TxIn) > 0 {

		if p := findPreimage(pkt.UnsignedTx.TxIn[0].Witness); p != nil {
			return p, nil
		}
	}

	return nil, nil
}

// findPreimage scans witness items for a 32-byte preimage candidate.
func findPreimage(items [][]byte) *lntypes.Preimage {
	for _, item := range items {
		if len(item) == lntypes.HashSize {
			var preimage lntypes.Preimage
			copy(preimage[:], item)

			return &preimage
		}
	}

	return nil
}

// deserializeWitness splits a serialized final witness into stack items.
func deserializeWitness(data []byte) ([][]byte, error) {
	r := bytes.NewReader(data)

	count, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, fmt.Errorf("read witness count: %w", err)
	}

	items := make([][]byte, 0, count)
	for i := uint64(0); i < count; i++ {
		itemLen, err := wire.ReadVarInt(r, 0)
		if err != nil {
			return nil, fmt.Errorf("read item %d length: %w", i,
				err)
		}

		item := make([]byte, itemLen)
		if _, err := r.Read(item); err != nil {
			return nil, fmt.Errorf("read item %d data: %w", i, err)
		}

		items = append(items, item)
	}

	return items, nil
}
