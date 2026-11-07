package oor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/stretchr/testify/require"
)

// stoppedSessionStore keeps a real native child's final database read alive
// after cancellation. The registry's own mailbox remains usable.
type stoppedSessionStore struct {
	actor.TxAwareDeliveryStore
	mailbox   string
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	once      sync.Once
}

// PeekNextMessage blocks the selected child's actual mailbox worker across
// cancellation, then performs its final store access after the test releases
// it.
func (s *stoppedSessionStore) PeekNextMessage(ctx context.Context,
	mailbox string) (*actor.LeasedMessage, error) {

	if mailbox == s.mailbox {
		s.once.Do(func() {
			close(s.entered)
			<-ctx.Done()
			close(s.cancelled)
			<-s.release
		})
	}

	return s.TxAwareDeliveryStore.PeekNextMessage(ctx, mailbox)
}

// TestRegistryJoinRetainsRemovedNativeSession proves an already-cancelled child
// remains owned even without membership in the active session map.
func TestRegistryJoinRetainsRemovedNativeSession(t *testing.T) {
	base, ok := newTestDeliveryStore(t).(actor.TxAwareDeliveryStore)
	require.True(t, ok)
	sid := oorSessionID(0x92)
	store := &stoppedSessionStore{
		TxAwareDeliveryStore: base,
		mailbox:              ActorIDForSession(sid),
		entered:              make(chan struct{}), cancelled: make(
			chan struct{},
		),
		release: make(chan struct{}),
	}
	var release sync.Once
	unblock := func() {
		release.Do(func() { close(store.release) })
	}
	t.Cleanup(unblock)
	registry, err := NewOORRegistryActor(OORRegistryConfig{
		RegistryStore: newFakeRegistryStore(), DeliveryStore: store,
		ReservationStore: &countingReservationStore{},
		ServerConn:       fakeServerConnRef{}, Signer: testSigner(t),
		IncomingHandler: &fakeRecipientFilter{owned: true},
		PackageStore:    &fakePackageStore{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { unblock(); registry.StopAndWait() })

	// The registry supplies lifetime ownership even after its reap path has
	// dropped the child's active-map handle. No message processing is
	// needed to reach the native worker's final database access.
	cfg := registry.behavior.childConfig(
		sid, clientdb.OORSessionDirectionIncoming,
	)
	child, err := NewOORSessionActor(cfg)
	require.NoError(t, err)
	<-store.entered
	child.Stop()
	<-store.cancelled
	joined := make(chan struct{})
	go func() {
		registry.StopAndWait()
		close(joined)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, registry.registry.Wait(ctx))
	select {
	case <-joined:
		t.Fatal("registry released a child with a live database access")

	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("registry did not join the released session")
	}
}
