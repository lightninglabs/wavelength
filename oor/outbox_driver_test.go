package oor

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// unknownOutboxEvent is an unrecognised outbox event for negative testing.
type unknownOutboxEvent struct{}

// OutboxType returns the type name.
func (e *unknownOutboxEvent) OutboxType() string {
	return "UnknownOutboxEvent"
}

// outboxSealed marks this as an OutboxEvent implementor.
func (e *unknownOutboxEvent) outboxSealed() {}

// unlockFailLocker is a test locker that always fails UnlockMany.
type unlockFailLocker struct {
	unlockErr error
}

// LockMany succeeds unconditionally.
func (l *unlockFailLocker) LockMany(_ context.Context, _ []wire.OutPoint,
	_ vtxo.LockOwner) error {

	return nil
}

// UnlockMany returns the configured error.
func (l *unlockFailLocker) UnlockMany(_ context.Context, _ []wire.OutPoint,
	_ vtxo.LockOwner) error {

	return l.unlockErr
}

// failingRecipientEventStore always returns an error on append.
type failingRecipientEventStore struct {
	err error
}

// AppendRecipientEvents returns the configured error.
func (s *failingRecipientEventStore) AppendRecipientEvents(
	_ context.Context, _ SessionID, _ *psbt.Packet,
	_ []clientoor.ArkRecipientOutput) error {

	return s.err
}

// ListRecipientEvents is a no-op implementation.
func (s *failingRecipientEventStore) ListRecipientEvents(
	_ context.Context, _ []byte, _ int64,
	_ int32) ([]*RecipientEvent, error) {

	return nil, nil
}

// nonAtomicSessionStore wraps a SessionStore but does not implement
// CoSignedAtomicStore, forcing the non-atomic code path in tests.
type nonAtomicSessionStore struct {
	SessionStore
}

// TestDriverRejectsUnknownOutboxEvent asserts the driver returns an error for
// unrecognised outbox event types.
func TestDriverRejectsUnknownOutboxEvent(t *testing.T) {
	t.Parallel()

	driver := NewDriver(DriverCfg{})

	_, err := driver.Handle(
		t.Context(), SessionID{}, &unknownOutboxEvent{},
	)
	require.ErrorContains(t, err, "unsupported outbox event type")
}

// TestDriverPropagatesUnlockFailures asserts unlock errors are surfaced to the
// caller.
func TestDriverPropagatesUnlockFailures(t *testing.T) {
	t.Parallel()

	driver := NewDriver(DriverCfg{
		Locker: &unlockFailLocker{
			unlockErr: errors.New("unlock failed"),
		},
	})

	_, err := driver.Handle(t.Context(), SessionID{1}, &UnlockInputsReq{
		Inputs: []wire.OutPoint{{Index: 1}},
	})
	require.ErrorContains(t, err, "unlock failed")
}

// TestDriverNotifyFailureReturnsFSMEvent asserts that recipient event store
// failures are surfaced as a NotifyRecipientsFailedEvent FSM event.
func TestDriverNotifyFailureReturnsFSMEvent(t *testing.T) {
	t.Parallel()

	_, arkPSBT, _ := buildTestSubmitPackage(t, nil)

	driver := NewDriver(DriverCfg{
		RecipientEvents: &failingRecipientEventStore{
			err: errors.New("recipient write failed"),
		},
	})

	follows, err := driver.Handle(
		t.Context(), SessionID{2},
		&NotifyRecipientsReq{ArkPSBT: arkPSBT},
	)
	require.NoError(t, err)
	require.Len(t, follows, 1)

	failed, ok := follows[0].(*NotifyRecipientsFailedEvent)
	require.True(t, ok)
	require.Equal(t, "recipient write failed", failed.Reason)
}

// TestDriverRequiresAtomicCoSignPath asserts that when both session store and
// VTXO store are configured, the session store must implement
// CoSignedAtomicStore.
func TestDriverRequiresAtomicCoSignPath(t *testing.T) {
	t.Parallel()

	dbh := db.NewTestDB(t)
	sessionStore := &nonAtomicSessionStore{
		SessionStore: NewDBSessionStore(
			dbh, clock.NewDefaultClock(), btclog.Disabled,
		),
	}

	driver := NewDriver(DriverCfg{
		SessionStore: sessionStore,
		Store:        vtxo.NewInMemoryStore(),
	})

	var sessionID SessionID
	sessionID[0] = 0x33

	follows, err := driver.Handle(
		t.Context(), sessionID, &CoSignReq{},
	)
	require.NoError(t, err)
	require.Len(t, follows, 1)

	failed, ok := follows[0].(*SignFailedEvent)
	require.True(t, ok)
	require.Contains(t, failed.Reason, "CoSignedAtomicStore")
}

// TestDriverRequiresAtomicFinalizePath asserts that when both session store
// and VTXO store are configured, the session store must implement
// FinalizeAtomicStore.
func TestDriverRequiresAtomicFinalizePath(t *testing.T) {
	t.Parallel()

	dbh := db.NewTestDB(t)
	sessionStore := &nonAtomicSessionStore{
		SessionStore: NewDBSessionStore(
			dbh, clock.NewDefaultClock(), btclog.Disabled,
		),
	}

	_, arkPSBT, checkpointPSBTs := buildTestSubmitPackage(t, nil)

	driver := NewDriver(DriverCfg{
		SessionStore: sessionStore,
		Store:        vtxo.NewInMemoryStore(),
	})

	follows, err := driver.Handle(
		t.Context(), SessionID{4}, &FinalizeReq{
			ArkPSBT:              arkPSBT,
			FinalCheckpointPSBTs: checkpointPSBTs,
		},
	)
	require.Nil(t, follows)
	require.ErrorContains(t, err, "FinalizeAtomicStore")
}
