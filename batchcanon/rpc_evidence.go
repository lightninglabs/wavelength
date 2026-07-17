package batchcanon

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
)

// EvidenceFromAncestryPaths authenticates the canonicality evidence carried
// by indexer ancestry paths. Multiple tree fragments may name the same
// commitment, but their shared evidence must be byte-for-byte consistent.
func EvidenceFromAncestryPaths(paths []*arkrpc.AncestryPath) ([]BatchEvidence,
	error) {

	evidence := make([]BatchEvidence, 0, len(paths))
	seen := make(map[chainhash.Hash]int, len(paths))
	for i, path := range paths {
		item, err := evidenceFromAncestryPath(path)
		if err != nil {
			return nil, fmt.Errorf("ancestry path %d: %w", i, err)
		}

		if existing, ok := seen[item.BatchTxID]; ok {
			if !equalBatchEvidence(evidence[existing], item) {
				return nil, fmt.Errorf("ancestry path %d "+
					"conflicts with evidence for "+
					"commitment %s", i, item.BatchTxID)
			}

			continue
		}

		seen[item.BatchTxID] = len(evidence)
		evidence = append(evidence, item)
	}

	return evidence, nil
}

// evidenceFromAncestryPath binds the transport fields to the serialized
// transaction, its selected tree output, and every actual input prevout.
func evidenceFromAncestryPath(path *arkrpc.AncestryPath) (BatchEvidence,
	error) {

	if path == nil {
		return BatchEvidence{}, fmt.Errorf("path must be provided")
	}

	commitmentTxID, err := arkrpc.AncestryCommitmentTxID(path)
	if err != nil {
		return BatchEvidence{}, err
	}

	treePath := path.GetTreePath()
	if treePath == nil || treePath.GetBatchOutpoint() == nil ||
		treePath.GetBatchOutput() == nil {
		return BatchEvidence{}, fmt.Errorf("tree path batch output " +
			"evidence must be provided")
	}

	batchOutpoint := treePath.GetBatchOutpoint()
	if len(batchOutpoint.GetTxid()) != chainhash.HashSize ||
		!bytes.Equal(batchOutpoint.GetTxid(), commitmentTxID[:]) {
		return BatchEvidence{}, fmt.Errorf("tree path batch outpoint " +
			"does not match commitment txid")
	}

	rawTx := path.GetCommitmentTx()
	reader := bytes.NewReader(rawTx)
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(reader); err != nil {
		return BatchEvidence{}, fmt.Errorf("decode commitment "+
			"transaction: %w", err)
	}
	if reader.Len() != 0 {
		return BatchEvidence{}, fmt.Errorf("commitment transaction "+
			"has %d trailing bytes", reader.Len())
	}
	if tx.TxHash() != commitmentTxID {
		return BatchEvidence{}, fmt.Errorf("commitment transaction " +
			"hash does not match commitment txid")
	}

	outputIndex := batchOutpoint.GetVout()
	if uint64(outputIndex) >= uint64(len(tx.TxOut)) {
		return BatchEvidence{}, fmt.Errorf("commitment output index "+
			"%d is out of range", outputIndex)
	}

	claimedOutput := treePath.GetBatchOutput()
	actualOutput := tx.TxOut[outputIndex]
	if claimedOutput.GetValue() < 0 ||
		claimedOutput.GetValue() != actualOutput.Value ||
		!bytes.Equal(
			claimedOutput.GetPkScript(), actualOutput.PkScript,
		) {
		return BatchEvidence{}, fmt.Errorf("tree path batch output "+
			"does not match commitment transaction output %d",
			outputIndex)
	}

	inputEvidence := path.GetCommitmentInputs()
	if len(inputEvidence) != len(tx.TxIn) {
		return BatchEvidence{}, fmt.Errorf("commitment transaction "+
			"has %d inputs, evidence has %d", len(tx.TxIn),
			len(inputEvidence))
	}

	consumedInputs := make([]ConsumedInput, len(inputEvidence))
	for i, input := range inputEvidence {
		if input == nil || input.GetOutpoint() == nil ||
			input.GetPrevOut() == nil {
			return BatchEvidence{}, fmt.Errorf("commitment input "+
				"%d evidence must be provided", i)
		}

		outpoint, err := rpcOutpoint(input.GetOutpoint())
		if err != nil {
			return BatchEvidence{}, fmt.Errorf("commitment input "+
				"%d: %w", i, err)
		}
		if outpoint != tx.TxIn[i].PreviousOutPoint {
			return BatchEvidence{}, fmt.Errorf("commitment input "+
				"%d outpoint does not match transaction input",
				i)
		}

		prevOut := input.GetPrevOut()
		if prevOut.GetValue() < 0 {
			return BatchEvidence{}, fmt.Errorf("commitment input "+
				"%d has negative previous output value", i)
		}
		if len(prevOut.GetPkScript()) == 0 {
			return BatchEvidence{}, fmt.Errorf("commitment input "+
				"%d has no previous output pkScript", i)
		}

		consumedInputs[i] = ConsumedInput{
			Outpoint: outpoint,
			Value:    prevOut.GetValue(),
			PkScript: bytes.Clone(prevOut.GetPkScript()),
		}
	}

	evidence := BatchEvidence{
		BatchTxID:            commitmentTxID,
		BatchTx:              bytes.Clone(rawTx),
		BatchOutputIndex:     outputIndex,
		ConfirmationPkScript: bytes.Clone(actualOutput.PkScript),
		CSVExpiryDelta:       path.GetCommitmentCsvExpiryDelta(),
		ConsumedInputs:       consumedInputs,
	}
	if evidence.CSVExpiryDelta <= 0 {
		return BatchEvidence{}, fmt.Errorf("commitment CSV expiry " +
			"delta must be positive")
	}
	if err := evidence.Validate(); err != nil {
		return BatchEvidence{}, err
	}

	return evidence, nil
}

// rpcOutpoint decodes the raw-hash outpoint encoding used by arkrpc.
func rpcOutpoint(outpoint *arkrpc.OutPoint) (wire.OutPoint, error) {
	if len(outpoint.GetTxid()) != chainhash.HashSize {
		return wire.OutPoint{}, fmt.Errorf("invalid outpoint txid "+
			"length %d", len(outpoint.GetTxid()))
	}

	var txid chainhash.Hash
	copy(txid[:], outpoint.GetTxid())

	return wire.OutPoint{
		Hash:  txid,
		Index: outpoint.GetVout(),
	}, nil
}

// equalBatchEvidence compares immutable evidence from duplicate ancestry
// fragments while ignoring slice ownership.
func equalBatchEvidence(a, b BatchEvidence) bool {
	if a.BatchTxID != b.BatchTxID ||
		a.BatchOutputIndex != b.BatchOutputIndex ||
		a.CSVExpiryDelta != b.CSVExpiryDelta ||
		!bytes.Equal(a.BatchTx, b.BatchTx) ||
		!bytes.Equal(a.ConfirmationPkScript, b.ConfirmationPkScript) ||
		len(a.ConsumedInputs) != len(b.ConsumedInputs) {
		return false
	}

	for i := range a.ConsumedInputs {
		aInput := a.ConsumedInputs[i]
		bInput := b.ConsumedInputs[i]
		if aInput.Outpoint != bInput.Outpoint ||
			aInput.Value != bInput.Value ||
			!bytes.Equal(
				aInput.PkScript, bInput.PkScript,
			) {
			return false
		}
	}

	return true
}
