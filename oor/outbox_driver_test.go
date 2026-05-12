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
func (s *failingRecipientEventStore) AppendRecipientEvents(_ context.Context,
	_ SessionID, _ *psbt.Packet, _ []clientoor.ArkRecipientOutput) error {

	return s.err
}

// ListRecipientEvents is a no-op implementation.
func (s *failingRecipientEventStore) ListRecipientEvents(_ context.Context,
	_ []byte, _ int64, _ int32) ([]*RecipientEvent, error) {

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
		t.Context(), SessionID{2}, &NotifyRecipientsReq{
			ArkPSBT: arkPSBT,
		},
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
	require.Contains(
		t, failed.Reason,
		"missing owner signature for collaborative leaf",
	)
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

// stubLineageVBytesEstimator is a fake estimator for cap-rejection tests.
// Returns the configured (vbytes, err) tuple regardless of inputs and
// counts how many times it was invoked, so tests can assert the cap
// check ran (or did not run) under various configurations.
type stubLineageVBytesEstimator struct {
	vbytes uint32
	err    error

	calls int
}

// EstimateOORLineageVBytes records the call and returns the stub's
// pre-configured response. Tests use the inputs argument only
// indirectly by inspecting `calls`.
func (s *stubLineageVBytesEstimator) EstimateOORLineageVBytes(_ context.Context,
	_ []wire.OutPoint, _ *psbt.Packet, _ []*psbt.Packet) (uint32, error) {

	s.calls++

	return s.vbytes, s.err
}

// TestDriverValidateSubmitCapRejection asserts the cap-rejection
// wiring in handleValidateSubmit produces a typed reject event with
// `Code = RejectCodeLineageTooLarge` whenever the configured estimator
// reports vbytes above the operator's cap. The reason string surfaces
// both the offending count and the cap so operators can diagnose
// which submit tripped the threshold.
func TestDriverValidateSubmitCapRejection(t *testing.T) {
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

	// Configure a tight 1 vB cap with a stub that returns a value
	// well above it. The submit package must pass rebuild + owner
	// proof validation cleanly so the cap check is the only thing
	// rejecting it.
	estimator := &stubLineageVBytesEstimator{vbytes: 5_000}
	driver := NewDriver(DriverCfg{
		Store:                  store,
		MaxOORLineageVBytes:    1,
		LineageVBytesEstimator: estimator,
	})

	follows, err := driver.Handle(
		ctx, SessionID{20},
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
	require.True(t, ok, "cap exceed must produce SubmitFailedEvent")
	require.Equal(
		t, RejectCodeLineageTooLarge, failed.Code,
		"typed cap-too-large code must be set",
	)
	require.Contains(
		t, failed.Reason, "5000 vB",
		"reason must surface offending vbyte count",
	)
	require.Contains(
		t, failed.Reason, "cap 1 vB",
		"reason must surface configured cap",
	)
	require.Contains(
		t, failed.Reason, ErrLineageWeightExceeded.Error(),
		"reason must wrap the typed sentinel error so callers can "+
			"detect cap rejections in logs",
	)
	require.Equal(
		t, 1, estimator.calls,
		"estimator must be invoked exactly once per submit",
	)
}

// TestDriverValidateSubmitWithinCap asserts the cap check accepts
// submits whose lineage vbytes fit within the configured cap and the
// FSM proceeds to SubmitValidatedEvent. The estimator is invoked
// exactly once per submit even on the success path.
func TestDriverValidateSubmitWithinCap(t *testing.T) {
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

	estimator := &stubLineageVBytesEstimator{vbytes: 100}
	driver := NewDriver(DriverCfg{
		Store:                  store,
		MaxOORLineageVBytes:    25_000,
		LineageVBytesEstimator: estimator,
	})

	follows, err := driver.Handle(
		ctx, SessionID{21},
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
	require.Equal(t, 1, estimator.calls)
}

// TestDriverValidateSubmitCapDisabled asserts that when MaxOORLineageVBytes
// is zero (operator opted out), the cap check is skipped entirely —
// even when an estimator is wired. This isolates the cap-disabled
// branch from the typed-reject branch.
func TestDriverValidateSubmitCapDisabled(t *testing.T) {
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

	// MaxOORLineageVBytes=0 disables the cap. Stub vbytes is huge to
	// confirm it is not consulted.
	estimator := &stubLineageVBytesEstimator{vbytes: ^uint32(0)}
	driver := NewDriver(DriverCfg{
		Store:                  store,
		MaxOORLineageVBytes:    0,
		LineageVBytesEstimator: estimator,
	})

	follows, err := driver.Handle(
		ctx, SessionID{22},
		&ValidateSubmitReq{
			ArkPSBT:                arkPSBT,
			CheckpointPSBTs:        checkpointPSBTs,
			VTXOSigningDescriptors: []VTXOSigningDescriptor{desc},
			CheckpointPolicy:       policy,
		},
	)
	require.NoError(t, err)
	require.Len(t, follows, 1)
	require.IsType(
		t, &SubmitValidatedEvent{}, follows[0],
		"cap=0 must skip the cap check entirely",
	)
	require.Equal(
		t, 0, estimator.calls,
		"estimator must not be called when cap is disabled",
	)
}

// TestDriverValidateSubmitEstimatorError asserts that an estimator
// internal failure (e.g., DB lookup error) surfaces as
// SubmitFailedEvent with Code=RejectCodeUnspecified, never as a typed
// cap rejection. Clients that route on the typed code must not
// mistake an infrastructure error for an over-cap submit.
func TestDriverValidateSubmitEstimatorError(t *testing.T) {
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

	estimator := &stubLineageVBytesEstimator{
		err: errors.New("simulated db lookup failure"),
	}
	driver := NewDriver(DriverCfg{
		Store:                  store,
		MaxOORLineageVBytes:    25_000,
		LineageVBytesEstimator: estimator,
	})

	follows, err := driver.Handle(
		ctx, SessionID{23},
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
	require.Equal(
		t, RejectCodeUnspecified, failed.Code, "infrastructure "+
			"failures must use unspecified code, not the typed "+
			"cap rejection",
	)
	require.Contains(
		t, failed.Reason, "oor lineage weight calculation failed",
	)
	require.Contains(t, failed.Reason, "simulated db lookup failure")
}

// TestDriverValidateSubmitMissingEstimator asserts that operator
// misconfiguration (cap configured but no estimator wired) fails
// closed with a typed unspecified reject rather than silently
// proceeding past an unenforceable cap.
func TestDriverValidateSubmitMissingEstimator(t *testing.T) {
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

	// Cap > 0 but no estimator wired: fail-closed contract.
	driver := NewDriver(DriverCfg{
		Store:               store,
		MaxOORLineageVBytes: 25_000,
	})

	follows, err := driver.Handle(
		ctx, SessionID{24},
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
	require.Equal(t, RejectCodeUnspecified, failed.Code)
	require.Contains(t, failed.Reason, "estimator not configured")
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
	require.Contains(
		t, failed.Reason,
		"missing owner signature for collaborative leaf",
	)
}
