package oor

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

type recordingBatchRegistrar struct {
	requests []*batchcanon.RegisterBatchRequest
	err      error
}

// RegisterBatch records one awaited registration for test assertions.
func (r *recordingBatchRegistrar) RegisterBatch(_ context.Context,
	request *batchcanon.RegisterBatchRequest) error {

	if r.err != nil {
		return r.err
	}

	r.requests = append(r.requests, request)

	return nil
}

// TestRegisterIncomingBatchEvidence proves same-batch recipient metadata is
// registered once with every dependent output before materialization.
func TestRegisterIncomingBatchEvidence(t *testing.T) {
	t.Parallel()

	evidence := testIncomingBatchEvidence(t, 0x44)
	sessionID := SessionID(chainhash.Hash{0xaa})
	registrar := &recordingBatchRegistrar{}
	matches := []IncomingMetadataMatch{
		incomingEvidenceMatch(1, evidence),
		incomingEvidenceMatch(3, evidence),
	}

	err := RegisterIncomingBatchEvidence(
		context.Background(), registrar, sessionID, matches,
	)
	require.NoError(t, err)
	require.Len(t, registrar.requests, 1)
	require.Equal(t, []wire.OutPoint{
		{
			Hash:  chainhash.Hash(sessionID),
			Index: 1,
		},
		{
			Hash:  chainhash.Hash(sessionID),
			Index: 3,
		},
	}, registrar.requests[0].DependentVTXOs)
}

// TestRegisterIncomingBatchEvidenceFailClosed rejects incomplete coverage and
// propagates registration failure before a VTXO can be persisted.
func TestRegisterIncomingBatchEvidenceFailClosed(t *testing.T) {
	t.Parallel()

	evidence := testIncomingBatchEvidence(t, 0x55)
	metadata := incomingEvidenceMatch(1, evidence)
	metadata.Metadata.Ancestry = append(
		metadata.Metadata.Ancestry, vtxo.Ancestry{
			CommitmentTxID: chainhash.Hash{0x99},
		},
	)

	err := RegisterIncomingBatchEvidence(
		context.Background(), &recordingBatchRegistrar{}, SessionID{},
		[]IncomingMetadataMatch{metadata},
	)
	require.ErrorContains(t, err, "covers 1 of 2")

	registrationErr := errors.New("watch arm failed")
	err = RegisterIncomingBatchEvidence(
		context.Background(), &recordingBatchRegistrar{
			err: registrationErr,
		}, SessionID{}, []IncomingMetadataMatch{
			incomingEvidenceMatch(1, evidence),
		},
	)
	require.ErrorIs(t, err, registrationErr)
}

// incomingEvidenceMatch builds one output's ancestry/evidence metadata.
func incomingEvidenceMatch(outputIndex uint32,
	evidence batchcanon.BatchEvidence) IncomingMetadataMatch {

	return IncomingMetadataMatch{
		OutputIndex: outputIndex,
		Metadata: IncomingVTXOMetadata{
			CommitmentTxID: evidence.BatchTxID,
			Ancestry: []vtxo.Ancestry{{
				CommitmentTxID: evidence.BatchTxID,
			}},
			BatchEvidence: []batchcanon.BatchEvidence{
				evidence,
			},
		},
	}
}

// testIncomingBatchEvidence creates complete evidence for one commitment.
func testIncomingBatchEvidence(t *testing.T,
	seed byte) batchcanon.BatchEvidence {

	t.Helper()

	input := wire.OutPoint{
		Hash: chainhash.Hash{
			seed,
		},
		Index: 2,
	}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&input, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1_000, []byte{0x51, seed}))

	var raw bytes.Buffer
	require.NoError(t, tx.Serialize(&raw))

	return batchcanon.BatchEvidence{
		BatchTxID:        tx.TxHash(),
		BatchTx:          raw.Bytes(),
		BatchOutputIndex: 0,
		ConfirmationPkScript: append(
			[]byte(nil), tx.TxOut[0].PkScript...,
		),
		CSVExpiryDelta: 144,
		ConsumedInputs: []batchcanon.ConsumedInput{{
			Outpoint: input,
			Value:    1_500,
			PkScript: []byte{
				0x51,
			},
		}},
	}
}
