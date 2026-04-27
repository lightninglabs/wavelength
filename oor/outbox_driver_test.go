package oor

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
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

// TestDriverValidateSubmitAcceptsCollaborativeOwnerProof asserts submit
// validation succeeds when the package carries a collaborative owner-leaf
// signature that matches the authoritative descriptor binding.
func TestDriverValidateSubmitAcceptsCollaborativeOwnerProof(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPSBT, checkpointPSBTs, desc, _, _ :=
		buildTestSubmitPackageWithDescriptor(t, nil)

	store := vtxo.NewInMemoryStore()
	err := store.Create(ctx, &vtxo.Record{
		Outpoint: desc.Outpoint,
		Value:    checkpointPSBTs[0].Inputs[0].WitnessUtxo.Value,
		PkScript: checkpointPSBTs[0].Inputs[0].WitnessUtxo.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	driver := NewDriver(DriverCfg{
		Store: store,
	})

	follows, err := driver.Handle(
		ctx, SessionID{7},
		&ValidateSubmitReq{
			ArkPSBT:                arkPSBT,
			CheckpointPSBTs:        checkpointPSBTs,
			VTXOSigningDescriptors: []VTXOSigningDescriptor{desc},
			CheckpointPolicy:       policy,
		},
	)
	require.NoError(t, err)
	require.Len(t, follows, 1)
	require.IsType(t, &SubmitValidatedEvent{}, follows[0])
}

// TestDriverValidateSubmitAcceptsMultiInputCollaborativeOwnerProof asserts
// owner-proof validation uses the full Ark prevout context, which is required
// for Taproot signatures on multi-input packages.
func TestDriverValidateSubmitAcceptsMultiInputCollaborativeOwnerProof(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	policy, arkPSBT, checkpointPSBTs, descs :=
		buildTestMultiInputSubmitPackageWithDescriptors(t)

	store := vtxo.NewInMemoryStore()
	for i, desc := range descs {
		witnessUtxo := checkpointPSBTs[i].Inputs[0].WitnessUtxo

		err := store.Create(ctx, &vtxo.Record{
			Outpoint: desc.Outpoint,
			Value:    witnessUtxo.Value,
			PkScript: witnessUtxo.PkScript,
			Status:   vtxo.StatusLive,
		})
		require.NoError(t, err)
	}

	driver := NewDriver(DriverCfg{
		Store: store,
	})

	follows, err := driver.Handle(
		ctx, SessionID{11},
		&ValidateSubmitReq{
			ArkPSBT:                arkPSBT,
			CheckpointPSBTs:        checkpointPSBTs,
			VTXOSigningDescriptors: descs,
			CheckpointPolicy:       policy,
		},
	)
	require.NoError(t, err)
	require.Len(t, follows, 1)
	require.IsType(t, &SubmitValidatedEvent{}, follows[0])
}

// TestDriverValidateSubmitRejectsMissingOwnerProof asserts submit validation
// rejects packages that do not prove possession of the claimed owner key.
func TestDriverValidateSubmitRejectsMissingOwnerProof(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPSBT, checkpointPSBTs, desc, _, _ :=
		buildTestSubmitPackageWithDescriptor(t, nil)
	arkPSBT.Inputs[0].TaprootScriptSpendSig = nil

	store := vtxo.NewInMemoryStore()
	err := store.Create(ctx, &vtxo.Record{
		Outpoint: desc.Outpoint,
		Value:    checkpointPSBTs[0].Inputs[0].WitnessUtxo.Value,
		PkScript: checkpointPSBTs[0].Inputs[0].WitnessUtxo.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	driver := NewDriver(DriverCfg{
		Store: store,
	})

	follows, err := driver.Handle(
		ctx, SessionID{8},
		&ValidateSubmitReq{
			ArkPSBT:                arkPSBT,
			CheckpointPSBTs:        checkpointPSBTs,
			VTXOSigningDescriptors: []VTXOSigningDescriptor{desc},
			CheckpointPolicy:       policy,
		},
	)
	require.NoError(t, err)
	require.Len(t, follows, 1)

	failed, ok := follows[0].(*SubmitFailedEvent)
	require.True(t, ok)
	require.Contains(t, failed.Reason,
		"missing owner signature for collaborative leaf")
}

// TestDriverValidateSubmitRejectsWrongDescriptorBinding asserts submit
// validation rejects packages whose descriptor metadata no longer matches the
// authoritative stored VTXO binding.
func TestDriverValidateSubmitRejectsWrongDescriptorBinding(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPSBT, checkpointPSBTs, desc, _, _ :=
		buildTestSubmitPackageWithDescriptor(t, nil)

	otherOwnerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	desc.VTXOPolicyTemplate = testStandardVTXOPolicyTemplate(
		t, otherOwnerKey.PubKey(), policy.OperatorKey, policy.CSVDelay,
	)
	desc.SpendPath = testStandardCollabSpendPath(
		t, otherOwnerKey.PubKey(), policy.OperatorKey, policy.CSVDelay,
	)

	store := vtxo.NewInMemoryStore()
	err = store.Create(ctx, &vtxo.Record{
		Outpoint: desc.Outpoint,
		Value:    checkpointPSBTs[0].Inputs[0].WitnessUtxo.Value,
		PkScript: checkpointPSBTs[0].Inputs[0].WitnessUtxo.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	driver := NewDriver(DriverCfg{
		Store: store,
	})

	follows, err := driver.Handle(
		ctx, SessionID{9},
		&ValidateSubmitReq{
			ArkPSBT:                arkPSBT,
			CheckpointPSBTs:        checkpointPSBTs,
			VTXOSigningDescriptors: []VTXOSigningDescriptor{desc},
			CheckpointPolicy:       policy,
		},
	)
	require.NoError(t, err)
	require.Len(t, follows, 1)

	failed, ok := follows[0].(*SubmitFailedEvent)
	require.True(t, ok)
	require.Contains(t, failed.Reason, "vtxo pkscript mismatch")
}

// TestDriverValidateSubmitRejectsWrongOwnerProofWithoutStore asserts owner
// proof validation still rejects a mismatched owner signature even when the
// rebuild/store-dependent validation path is unavailable.
func TestDriverValidateSubmitRejectsWrongOwnerProofWithoutStore(t *testing.T) {
	t.Parallel()

	policy, arkPSBT, checkpointPSBTs, desc, _, _ :=
		buildTestSubmitPackageWithDescriptor(t, nil)

	otherOwnerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	arkPSBT.Inputs[0].TaprootScriptSpendSig[0].XOnlyPubKey =
		schnorr.SerializePubKey(otherOwnerKey.PubKey())

	driver := NewDriver(DriverCfg{})

	follows, err := driver.Handle(
		t.Context(), SessionID{10},
		&ValidateSubmitReq{
			ArkPSBT:                arkPSBT,
			CheckpointPSBTs:        checkpointPSBTs,
			VTXOSigningDescriptors: []VTXOSigningDescriptor{desc},
			CheckpointPolicy:       policy,
		},
	)
	require.NoError(t, err)
	require.Len(t, follows, 1)

	failed, ok := follows[0].(*SubmitFailedEvent)
	require.True(t, ok)
	require.Contains(t, failed.Reason,
		"missing owner signature for collaborative leaf")
}
