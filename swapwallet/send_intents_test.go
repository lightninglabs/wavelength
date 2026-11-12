//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// TestPreparedSendStoreBoundsCount verifies lazy pruning work remains bounded
// by a fixed maximum number of live intents.
func TestPreparedSendStoreBoundsCount(t *testing.T) {
	t.Parallel()

	store := newPreparedSendStore()
	for range maxPreparedSendIntents {
		_, err := store.put(
			&preparedSendIntent{
				kind: preparedSendInvoice,
			},
		)
		require.NoError(t, err)
	}

	_, err := store.put(&preparedSendIntent{kind: preparedSendInvoice})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
}

// TestPreparedSendStoreBoundsBytes verifies retained caller strings cannot
// exceed the aggregate byte budget.
func TestPreparedSendStoreBoundsBytes(t *testing.T) {
	t.Parallel()

	store := newPreparedSendStore()
	_, err := store.put(&preparedSendIntent{
		kind:    preparedSendInvoice,
		invoice: strings.Repeat("x", maxPreparedSendBytes+1),
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Empty(t, store.intents)
	require.Zero(t, store.storedBytes)
}

// TestPreparedSendStoreBoundsNote verifies one caller note cannot consume the
// aggregate retained-byte budget.
func TestPreparedSendStoreBoundsNote(t *testing.T) {
	t.Parallel()

	store := newPreparedSendStore()
	_, err := store.put(&preparedSendIntent{
		kind: preparedSendInvoice,
		note: strings.Repeat("x", maxPreparedSendNoteBytes+1),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestPreparedSendStoreReleasesBytes verifies consume and expiration return
// retained-byte capacity to the store.
func TestPreparedSendStoreReleasesBytes(t *testing.T) {
	t.Parallel()

	store := newPreparedSendStore()
	intent := &preparedSendIntent{
		kind: preparedSendInvoice, note: "retained",
	}
	id, err := store.put(intent)
	require.NoError(t, err)
	require.Equal(t, len(intent.note), store.storedBytes)

	_, err = store.consume(id)
	require.NoError(t, err)
	require.Zero(t, store.storedBytes)
}

// TestPreparedSendStoreConcurrentAccounting proves concurrent admissions share
// one byte budget and consuming them returns all capacity exactly once.
func TestPreparedSendStoreConcurrentAccounting(t *testing.T) {
	t.Parallel()
	store := newPreparedSendStore()
	const workers = 64
	ids := make(chan string, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := store.put(&preparedSendIntent{
				invoice: strings.Repeat(
					"x", maxPreparedSendBytes/16,
				),
			})
			if err == nil {
				ids <- id

				return
			}
			require.Equal(
				t, codes.ResourceExhausted, status.Code(err),
			)
		}()
	}
	wg.Wait()
	close(ids)
	require.Len(t, store.intents, 16)
	require.Equal(t, maxPreparedSendBytes, store.storedBytes)
	for id := range ids {
		_, err := store.consume(id)
		require.NoError(t, err)
	}
	require.Zero(t, store.storedBytes)

	id, err := store.put(&preparedSendIntent{note: "expired"})
	require.NoError(t, err)
	store.intents[id].expiresAt = time.Now().Add(-time.Second)
	_, err = store.put(&preparedSendIntent{note: "new"})
	require.NoError(t, err)
	require.Equal(t, len("new"), store.storedBytes)
}
