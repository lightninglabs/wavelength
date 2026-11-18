package arkchannel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChannelChanges reports only committed progress for the subscribed ID,
// coalesces bursts, and releases listeners without owning a worker lifecycle.
func TestChannelChanges(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	terms := testTerms(t, KindPromotion)
	changed, stop := coordinator.subscribeChannelChanges(terms.ID)
	defer stop()
	second, stopSecond := coordinator.subscribeChannelChanges(terms.ID)
	defer stopSecond()
	other, stopOther := coordinator.subscribeChannelChanges(ID{99})
	defer stopOther()

	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	require.Len(t, second, 1)
	require.Empty(t, other)

	binding := &BindVTXO{Binding: testBinding(terms)}
	_, _, err = coordinator.Apply(t.Context(), terms.ID, binding)
	require.NoError(t, err)
	require.Len(t, changed, 1, "progress must coalesce")
	<-changed
	<-second

	_, _, err = coordinator.Apply(t.Context(), terms.ID, binding)
	require.NoError(t, err)
	require.Empty(t, changed, "idempotent replay is not progress")
	require.Empty(t, second)

	_, _, err = coordinator.Apply(t.Context(), terms.ID, &ChannelActive{})
	require.Error(t, err)
	require.Empty(t, changed, "a rejected event cannot wake a replay")

	stop()
	stop()
	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &BackingSigned{
			Backing: testBacking(t, terms, binding.Binding),
		},
	)
	require.NoError(t, err)
	require.Empty(t, changed)
	require.Len(t, second, 1)
	require.Empty(t, other)
	stopSecond()
	stopOther()
	require.Empty(t, coordinator.changes)
}

// TestChannelChangesConcurrentUnsubscribe joins notification with listener
// removal without closing a channel that a committing writer can still use.
func TestChannelChangesConcurrentUnsubscribe(t *testing.T) {
	t.Parallel()

	coordinator, err := NewCoordinator(newMemoryStore())
	require.NoError(t, err)
	id := ID{1}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1_000 {
			coordinator.notifyChannelChanged(id)
		}
	}()
	for range 1_000 {
		_, stop := coordinator.subscribeChannelChanges(id)
		stop()
		stop()
	}
	<-done
	require.Empty(t, coordinator.changes)
}
