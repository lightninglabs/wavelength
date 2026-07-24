//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreparedSendStoreRejectsExpiredIntent(t *testing.T) {
	t.Parallel()

	store := newPreparedSendStore()
	intent := &preparedSendIntent{
		kind: preparedSendInvoice,
	}

	id, err := store.put(intent)
	require.NoError(t, err)

	store.mu.Lock()
	store.intents[id].expiresAt = time.Now().Add(-time.Second)
	store.mu.Unlock()

	_, err = store.consume(id)
	require.ErrorIs(t, err, ErrInvalidSendIntent)

	store.mu.Lock()
	_, ok := store.intents[id]
	store.mu.Unlock()
	require.False(t, ok, "expired intents must be pruned on consume")
}

func TestPreparedSendStoreRejectsNilIntent(t *testing.T) {
	t.Parallel()

	store := newPreparedSendStore()

	id, err := store.put(nil)
	require.ErrorIs(t, err, ErrInvalidSendIntent)
	require.Empty(t, id)
}

// TestPrepareResponseUsesNegotiatedRoutingBudget verifies the response reports
// the routing allowance selected by the server.
func TestPrepareResponseUsesNegotiatedRoutingBudget(t *testing.T) {
	t.Parallel()

	resp := prepareResponseFromIntent(
		&preparedSendIntent{}, prepareSendPreview{
			routingFeeBudgetSat: 20,
		},
	)
	require.Equal(t, uint64(20), resp.GetRoutingFeeBudgetSat())
}
