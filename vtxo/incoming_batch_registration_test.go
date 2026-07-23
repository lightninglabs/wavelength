package vtxo

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/stretchr/testify/require"
)

type recordingIncomingRegistrar struct {
	requests []*batchcanon.RegisterBatchRequest
	err      error
}

// RegisterBatch records one incoming lineage registration.
func (r *recordingIncomingRegistrar) RegisterBatch(_ context.Context,
	request *batchcanon.RegisterBatchRequest) error {

	if r.err != nil {
		return r.err
	}
	r.requests = append(r.requests, request)

	return nil
}

// TestRegisterIncomingBatchEvidence proves the descriptor outpoint is bound as
// a dependent and registration failures stop the receive path.
func TestRegisterIncomingBatchEvidence(t *testing.T) {
	t.Parallel()

	evidence := testIncomingEvidence(t)
	desc := &Descriptor{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0xaa,
			},
			Index: 3,
		},
		Ancestry: []Ancestry{{
			CommitmentTxID: evidence.BatchTxID,
		}},
	}
	registrar := &recordingIncomingRegistrar{}
	require.NoError(
		t,
		registerIncomingBatchEvidence(
			context.Background(), registrar, desc,
			[]batchcanon.BatchEvidence{evidence},
		),
	)
	require.Len(t, registrar.requests, 1)
	require.Equal(
		t, []wire.OutPoint{desc.Outpoint},
		registrar.requests[0].DependentVTXOs,
	)

	wantErr := errors.New("registration failed")
	err := registerIncomingBatchEvidence(
		context.Background(), &recordingIncomingRegistrar{
			err: wantErr,
		},
		desc,
		[]batchcanon.BatchEvidence{evidence},
	)
	require.ErrorIs(t, err, wantErr)
}

// testIncomingEvidence creates complete evidence for one commitment.
func testIncomingEvidence(t *testing.T) batchcanon.BatchEvidence {
	t.Helper()

	input := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
		},
		Index: 1,
	}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&input, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1_000, []byte{0x51, 0x20, 0x22}))

	var raw bytes.Buffer
	require.NoError(t, tx.Serialize(&raw))

	return batchcanon.BatchEvidence{
		BatchTxID:            tx.TxHash(),
		BatchTx:              raw.Bytes(),
		BatchOutputIndex:     0,
		ConfirmationPkScript: bytes.Clone(tx.TxOut[0].PkScript),
		CSVExpiryDelta:       144,
		ConsumedInputs: []batchcanon.ConsumedInput{{
			Outpoint: input,
			Value:    2_000,
			PkScript: []byte{
				0x51,
			},
		}},
	}
}
