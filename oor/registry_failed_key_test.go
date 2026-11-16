package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// TestOORRegistryFailedSessionRetainsKey verifies an attemptless failed
// session can be rebuilt only by its original keyed caller.
func TestOORRegistryFailedSessionRetainsKey(t *testing.T) {
	t.Parallel()

	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	client, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	input := newTestTransferInput(
		t, client, operator.PubKey(), wire.OutPoint{
			Hash: [32]byte{0x84},
		},
		btcutil.Amount(10_000),
	)
	req := &StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operator.PubKey(), CSVDelay: 10,
		},
		Inputs: []TransferInput{
			input,
		},
		Recipients: []oortx.RecipientOutput{{
			PkScript: newTestTaprootPkScript(t, client.PubKey()),
			Value:    input.VTXO.Amount,
		}},
		IdempotencyKey: "original-caller",
	}
	session, _, err := NewSessionWithIdempotencyKey(
		t.Context(), req.Policy, req.Inputs, req.Recipients,
		req.IdempotencyKey, EnvConfig{},
	)
	require.NoError(t, err)
	t.Cleanup(session.FSM.Stop)
	outgoing := clientdb.OORSessionDirectionOutgoing

	for _, key := range []string{"different-caller", "original-caller"} {
		t.Run(key, func(t *testing.T) {
			store := newFakeRegistryStore()
			record := clientdb.OORSessionRegistryRecord{
				SessionID:      chainHashOf(session.ID),
				ActorID:        ActorIDForSession(session.ID),
				Direction:      outgoing,
				Phase:          "failed",
				IdempotencyKey: "original-caller",
				Status:         clientdb.OORSessionStatusFailed,
			}
			require.NoError(
				t,
				store.UpsertSession(
					t.Context(), record,
				),
			)
			b, rec := newTestRegistryBehavior(store)
			b.active[session.ID] = &OORSessionActor{stop: func() {
				rec.mu.Lock()
				defer rec.mu.Unlock()

				rec.stops++
			}}
			request := *req
			request.IdempotencyKey = key
			result := b.handleStartTransfer(t.Context(), &request)
			if key != req.IdempotencyKey {
				require.ErrorIs(
					t, result.Err(),
					ErrIdempotencyKeyConflict,
				)
				require.Zero(t, rec.stopCount())
				require.Zero(t, rec.spawns)
				require.Empty(t, rec.recorded())

				return
			}

			require.True(t, result.IsOk(), result.Err())
			value := result.UnwrapOr(nil)
			response, ok := value.(*StartTransferResponse)
			require.True(t, ok)
			require.False(t, response.Existing)
			require.Equal(t, session.ID, response.SessionID)
			require.Equal(t, 1, rec.stopCount())
			require.Equal(t, 1, rec.spawns)
			require.Len(t, rec.recorded(), 1)
			require.Equal(t, session.ID, b.pendingOutgoingKeys[key])
		})
	}
}
