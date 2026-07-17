package batchcanon

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// TestAdmissionTokenTracksReadyGenerationAndRevision proves that admission
// remains closed until every watched subject contributes to Ready(g), and that
// a later canonicality change invalidates the issued token before a critical
// side effect can use it.
func TestAdmissionTokenTracksReadyGenerationAndRevision(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0xa7)
	input := testOutpoint(0xa8, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xa7},
		ConsumedInputs:       []ConsumedInput{ci(input)},
	})

	query := func() *QueryLineageResponse {
		resp, err := h.mgrRef.Ask(
			t.Context(), &QueryLineageRequest{
				BatchTxIDs: []chainhash.Hash{txid},
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)
		lineage, ok := resp.(*QueryLineageResponse)
		require.True(t, ok)

		return lineage
	}

	// Merely arming every watch is not Ready(g): no subject has supplied a
	// current observation yet.
	result := query()
	require.Equal(t, LineageReconciling, result.Availability)
	require.Nil(t, result.Token)

	// The confirmation alone is still incomplete because the actual input
	// spend has not been observed for this generation.
	h.fireConfirmed(t, txid, 101, testBatchTxid(0xb1))
	result = query()
	require.Equal(t, LineageReconciling, result.Availability)
	require.Nil(t, result.Token)

	// The batch's own spend supplies the final subject observation.
	// Ready(g) is installed and a revision-bound token can now be issued.
	h.fireSpend(t, input, txid, 101)
	result = query()
	require.Equal(t, AvailableProvisional, result.Availability)
	require.NotNil(t, result.Token)
	require.Len(t, result.Token.Lineage, 1)
	token := *result.Token

	validation, err := h.mgrRef.Ask(
		t.Context(), &ValidateAdmissionRequest{Token: token},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)
	valid, ok := validation.(*ValidateAdmissionResponse)
	require.True(t, ok)
	require.True(t, valid.Valid)
	require.Equal(t, AvailableProvisional, valid.Availability)

	// A reorg changes semantic availability and the durable revision. The
	// old token is stale and cannot cross a point of no return.
	h.fireConfReorged(t, txid)
	validation, err = h.mgrRef.Ask(
		t.Context(), &ValidateAdmissionRequest{Token: token},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)
	valid, ok = validation.(*ValidateAdmissionResponse)
	require.True(t, ok)
	require.False(t, valid.Valid)
	require.Equal(t, LimboReorg, valid.Availability)
}

// TestAdmissionFailsClosedOnObservationPersistenceError proves the manager's
// in-memory overlay cannot issue or validate a token from an old durable
// usable row after a newer chain observation failed to commit.
func TestAdmissionFailsClosedOnObservationPersistenceError(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0xb7)
	input := testOutpoint(0xb8, 0)
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xb7},
		ConsumedInputs:       []ConsumedInput{ci(input)},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0xc1))
	h.fireSpend(t, input, txid, 101)

	query := func() *QueryLineageResponse {
		resp, err := h.mgrRef.Ask(
			t.Context(), &QueryLineageRequest{
				BatchTxIDs: []chainhash.Hash{txid},
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)
		queryResp, ok := resp.(*QueryLineageResponse)
		require.True(t, ok)

		return queryResp
	}

	admitted := query()
	require.Equal(t, AvailableProvisional, admitted.Availability)
	require.NotNil(t, admitted.Token)
	oldToken := *admitted.Token

	h.store.setApplyError(errors.New("injected durable write failure"))
	h.fireConfReorged(t, txid)

	// The SQL-equivalent fake still contains the previously usable row, but
	// the serialized manager observed a newer event and must fail closed.
	durable, err := h.store.GetBatch(t.Context(), txid)
	require.NoError(t, err)
	require.Equal(t, StateProvisional, durable.State)
	require.True(t, durable.Ready())
	blocked := query()
	require.Equal(t, LineageReconciling, blocked.Availability)
	require.Nil(t, blocked.Token)

	resp, err := h.mgrRef.Ask(
		t.Context(), &ValidateAdmissionRequest{Token: oldToken},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)
	validation, ok := resp.(*ValidateAdmissionResponse)
	require.True(t, ok)
	require.False(t, validation.Valid)
	require.Equal(t, LineageReconciling, validation.Availability)

	// A later full-snapshot write can safely recover without replaying the
	// failed operation. It persists all retained in-memory observations and
	// issues a different revision-bound token.
	h.store.setApplyError(nil)
	h.fireConfirmed(t, txid, 102, testBatchTxid(0xc2))
	recovered := query()
	require.Equal(t, AvailableProvisional, recovered.Availability)
	require.NotNil(t, recovered.Token)
	require.NotEqual(
		t, oldToken.Lineage[0].Revision,
		recovered.Token.Lineage[0].Revision,
	)
}
