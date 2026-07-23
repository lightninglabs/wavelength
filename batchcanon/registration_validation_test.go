package batchcanon

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestValidateRegistrationBindsSerializedEvidence proves that registration
// cannot omit or substitute an actual transaction input and that the watched
// confirmation script is an output of the same transaction identity.
func TestValidateRegistrationBindsSerializedEvidence(t *testing.T) {
	t.Parallel()

	valid := validRegistrationRequest(t)
	require.NoError(t, validateRegistration(valid, false))

	tests := []struct {
		name   string
		mutate func(*RegisterBatchRequest)
		want   string
	}{
		{
			name: "missing serialized transaction",
			mutate: func(req *RegisterBatchRequest) {
				req.BatchTx = nil
			},
			want: "serialized batch transaction is required",
		},
		{
			name: "wrong transaction identity",
			mutate: func(req *RegisterBatchRequest) {
				req.BatchTxID[0] ^= 1
			},
			want: "hash does not match",
		},
		{
			name: "non-positive CSV expiry delta",
			mutate: func(req *RegisterBatchRequest) {
				req.CSVExpiryDelta = 0
			},
			want: "CSV expiry delta must be positive",
		},
		{
			name: "trailing transaction bytes",
			mutate: func(req *RegisterBatchRequest) {
				req.BatchTx = append(req.BatchTx, 0)
			},
			want: "trailing bytes",
		},
		{
			name: "output index out of range",
			mutate: func(req *RegisterBatchRequest) {
				req.BatchOutputIndex = 2
			},
			want: "output index 2 is out of range",
		},
		{
			name: "watch script is not selected output",
			mutate: func(req *RegisterBatchRequest) {
				req.ConfirmationPkScript = []byte{
					0x51,
				}
			},
			want: "does not match confirmation pkScript",
		},
		{
			name: "input subset",
			mutate: func(req *RegisterBatchRequest) {
				req.ConsumedInputs = req.ConsumedInputs[:1]
			},
			want: "transaction has 2 inputs, registration has 1",
		},
		{
			name: "input substitution",
			mutate: func(req *RegisterBatchRequest) {
				req.ConsumedInputs[1].Outpoint = testOutpoint(
					0xef, 9,
				)
			},
			want: "is not registered",
		},
		{
			name: "duplicate input",
			mutate: func(req *RegisterBatchRequest) {
				req.ConsumedInputs[1].Outpoint =
					req.ConsumedInputs[0].Outpoint
			},
			want: "is duplicated",
		},
		{
			name: "negative previous output value",
			mutate: func(req *RegisterBatchRequest) {
				req.ConsumedInputs[0].Value = -1
			},
			want: "has negative value",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			req := cloneRegistrationRequest(valid)
			test.mutate(req)
			require.ErrorContains(
				t, validateRegistration(req, false), test.want,
			)
		})
	}
}

// validRegistrationRequest creates a two-input transaction whose second
// output is the confirmation-watch subject.
func validRegistrationRequest(t *testing.T) *RegisterBatchRequest {
	t.Helper()

	inputA := testOutpoint(0xa1, 1)
	inputB := testOutpoint(0xb2, 2)
	watchScript := []byte{0x51, 0x20, 0xc3}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&inputA, nil, nil))
	tx.AddTxIn(wire.NewTxIn(&inputB, nil, nil))
	tx.AddTxOut(wire.NewTxOut(500, []byte{0x51}))
	tx.AddTxOut(wire.NewTxOut(1_000, watchScript))

	var raw bytes.Buffer
	require.NoError(t, tx.Serialize(&raw))

	return &RegisterBatchRequest{
		BatchTxID:            tx.TxHash(),
		BatchTx:              raw.Bytes(),
		BatchOutputIndex:     1,
		ConfirmationPkScript: watchScript,
		CSVExpiryDelta:       144,
		ConsumedInputs: []ConsumedInput{
			{
				Outpoint: inputA,
				Value:    700,
				PkScript: []byte{
					0x51,
				},
			},
			{
				Outpoint: inputB,
				Value:    900,
				PkScript: []byte{
					0x00,
					0x14,
					0x01,
				},
			},
		},
	}
}

// cloneRegistrationRequest makes each parallel table case independent.
func cloneRegistrationRequest(req *RegisterBatchRequest) *RegisterBatchRequest {
	clone := *req
	clone.BatchTx = append([]byte(nil), req.BatchTx...)
	clone.ConfirmationPkScript = append(
		[]byte(nil), req.ConfirmationPkScript...,
	)
	clone.ConsumedInputs = append(
		[]ConsumedInput(nil), req.ConsumedInputs...,
	)
	for i := range clone.ConsumedInputs {
		clone.ConsumedInputs[i].PkScript = append(
			[]byte(nil), req.ConsumedInputs[i].PkScript...,
		)
	}

	return &clone
}
