package unroll

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// wedgeableRegistryRef is a self-ref double that models a registry mailbox
// with no room. Tell parks the caller until the test ends, which is what the
// real mailbox does when it is full; TryTell reports the configured error
// until the test lets a send through.
type wedgeableRegistryRef struct {
	mu sync.Mutex

	// tryErr is returned by the next TryTell. Once failures runs out it
	// is cleared, so the send after that succeeds.
	tryErr error

	// failures is how many more times TryTell reports tryErr.
	failures int

	// tryCalls counts every TryTell, delivered or not.
	tryCalls int

	// delivered holds the messages TryTell accepted.
	delivered []RegistryMsg
}

// ID identifies the double in log lines.
func (w *wedgeableRegistryRef) ID() string {
	return "wedged-registry"
}

// Tell blocks forever, standing in for a send into a mailbox that nothing is
// draining. Any caller that reaches this has reintroduced the bug.
func (w *wedgeableRegistryRef) Tell(ctx context.Context, _ RegistryMsg) error {
	<-ctx.Done()

	return ctx.Err()
}

// TryTell reports the configured failure until the test's budget runs out,
// then accepts the message.
func (w *wedgeableRegistryRef) TryTell(_ context.Context,
	msg RegistryMsg) error {

	w.mu.Lock()
	defer w.mu.Unlock()

	w.tryCalls++

	if w.failures > 0 {
		w.failures--

		return w.tryErr
	}

	w.delivered = append(w.delivered, msg)

	return nil
}

// counts returns the number of attempted and delivered sends.
func (w *wedgeableRegistryRef) counts() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.tryCalls, len(w.delivered)
}

// newSelfTellRegistry builds the bare registry behavior these tests need. The
// self-tell path touches no registry state, so nothing else has to be wired.
func newSelfTellRegistry(ref *wedgeableRegistryRef) *registryBehavior {
	return &registryBehavior{
		log:     btclog.Disabled,
		selfRef: ref,
	}
}

// TestRegistryRequestPersistDoesNotBlock is the deadlock regression test.
// requestPersist runs on the registry's own receive goroutine, so a blocking
// send into its own full mailbox waits for room that only the parked
// goroutine could make: the registry wedges until shutdown. The call has to
// return whether or not the mailbox had room.
func TestRegistryRequestPersistDoesNotBlock(t *testing.T) {
	t.Parallel()

	ref := &wedgeableRegistryRef{
		tryErr: actor.ErrMailboxFull,

		// Fail every attempt for the duration of the test, so nothing
		// but a non-blocking send can let this call return.
		failures: 1 << 30,
	}
	r := newSelfTellRegistry(ref)

	returned := make(chan struct{})
	go func() {
		defer close(returned)

		r.requestPersist(wire.OutPoint{Index: 7}, 0)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("requestPersist parked on a full self-mailbox")
	}
}

// TestRegistrySelfTellRetriesUntilDelivered pins the other half of the
// contract: not blocking must not mean dropping. The registry's persistence
// messages are its own follow-up work, and nothing else re-derives them, so a
// send that finds no room has to come back for it.
func TestRegistrySelfTellRetriesUntilDelivered(t *testing.T) {
	t.Parallel()

	ref := &wedgeableRegistryRef{
		tryErr:   actor.ErrMailboxFull,
		failures: 2,
	}
	r := newSelfTellRegistry(ref)

	r.requestPersist(wire.OutPoint{Index: 9}, 3)

	require.Eventually(t, func() bool {
		_, delivered := ref.counts()

		return delivered == 1
	}, 5*time.Second, 10*time.Millisecond)

	attempts, _ := ref.counts()
	require.Equal(t, 3, attempts)

	ref.mu.Lock()
	defer ref.mu.Unlock()

	// The retry has to carry the original message, not a fresh one: the
	// attempt count drives the store's backoff.
	msg, ok := ref.delivered[0].(*persistActiveRecordMsg)
	require.True(t, ok)
	require.Equal(t, 3, msg.Attempt)
	require.Equal(t, uint32(9), msg.Outpoint.Index)
}

// TestRegistrySelfTellStopsOnTerminal verifies that a registry which is gone
// for good ends the retry chain. No delay repairs a terminated actor, so
// retrying one just burns timers until the process exits.
func TestRegistrySelfTellStopsOnTerminal(t *testing.T) {
	t.Parallel()

	ref := &wedgeableRegistryRef{
		tryErr:   actor.ErrActorTerminated,
		failures: 1 << 30,
	}
	r := newSelfTellRegistry(ref)

	r.requestPersist(wire.OutPoint{Index: 11}, 0)

	// Well past several retry delays, so a live chain would have shown up
	// as further attempts.
	time.Sleep(10 * selfTellRetry)

	attempts, delivered := ref.counts()
	require.Equal(t, 1, attempts)
	require.Zero(t, delivered)
}
