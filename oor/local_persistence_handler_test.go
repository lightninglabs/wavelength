package oor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	libtypes "github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// testPackageStore records package persistence calls for handler assertions.
type testPackageStore struct {
	packageCalls int
	bindingCalls int

	lastDirection PackageDirection
	lastSessionID chainhash.Hash

	packageErr error
}

// UpsertPackage records one package upsert call.
func (s *testPackageStore) UpsertPackage(_ context.Context,
	direction PackageDirection, sessionID chainhash.Hash, _ *psbt.Packet,
	_ []*psbt.Packet) error {

	s.packageCalls++
	s.lastDirection = direction
	s.lastSessionID = sessionID

	return s.packageErr
}

// UpsertBinding records one package binding upsert call.
func (s *testPackageStore) UpsertBinding(_ context.Context, _ wire.OutPoint,
	_ chainhash.Hash, _ uint32, _ PackageLinkKind) error {

	s.bindingCalls++
	return nil
}

// testVTXOStore is a minimal in-memory vtxo.VTXOStore used by handler tests.
type testVTXOStore struct {
	records map[wire.OutPoint]*vtxo.Descriptor
}

// newTestVTXOStore creates a new testVTXOStore.
func newTestVTXOStore() *testVTXOStore {
	return &testVTXOStore{
		records: make(map[wire.OutPoint]*vtxo.Descriptor),
	}
}

// SaveVTXO persists a descriptor unless it already exists.
func (s *testVTXOStore) SaveVTXO(_ context.Context,
	desc *vtxo.Descriptor) error {

	if desc == nil {
		return fmt.Errorf("descriptor must be provided")
	}

	if _, ok := s.records[desc.Outpoint]; ok {
		return fmt.Errorf("duplicate vtxo")
	}

	cpy := *desc
	s.records[desc.Outpoint] = &cpy

	return nil
}

// GetVTXO returns a descriptor by outpoint.
func (s *testVTXOStore) GetVTXO(_ context.Context,
	outpoint wire.OutPoint) (*vtxo.Descriptor, error) {

	desc, ok := s.records[outpoint]
	if !ok {
		return nil, fmt.Errorf("not found")
	}

	cpy := *desc

	return &cpy, nil
}

// ListLiveVTXOs returns all stored descriptors.
func (s *testVTXOStore) ListLiveVTXOs(_ context.Context) (
	[]*vtxo.Descriptor, error) {

	out := make([]*vtxo.Descriptor, 0, len(s.records))
	for _, desc := range s.records {
		cpy := *desc
		out = append(out, &cpy)
	}

	return out, nil
}

// ListVTXOsByStatus returns descriptors matching the given status.
func (s *testVTXOStore) ListVTXOsByStatus(_ context.Context,
	status vtxo.VTXOStatus) ([]*vtxo.Descriptor, error) {

	var out []*vtxo.Descriptor
	for _, desc := range s.records {
		if desc.Status == status {
			cpy := *desc
			out = append(out, &cpy)
		}
	}

	return out, nil
}

// UpdateVTXOStatus updates status for the given outpoint.
func (s *testVTXOStore) UpdateVTXOStatus(_ context.Context,
	outpoint wire.OutPoint, status vtxo.VTXOStatus) error {

	desc, ok := s.records[outpoint]
	if !ok {
		return fmt.Errorf("not found")
	}

	desc.Status = status

	return nil
}

// MarkForfeiting is unused by these tests.
func (s *testVTXOStore) MarkForfeiting(_ context.Context,
	_ wire.OutPoint, _ string, _ *wire.MsgTx) error {

	return nil
}

// GetForfeitTx is unused by these tests.
func (s *testVTXOStore) GetForfeitTx(_ context.Context,
	_ wire.OutPoint) (*wire.MsgTx, error) {

	return nil, nil
}

// MarkForfeited is unused by these tests.
func (s *testVTXOStore) MarkForfeited(_ context.Context,
	_ wire.OutPoint, _ chainhash.Hash) error {

	return nil
}

// DeleteVTXO removes an outpoint from the test store.
func (s *testVTXOStore) DeleteVTXO(_ context.Context,
	outpoint wire.OutPoint) error {

	delete(s.records, outpoint)
	return nil
}

// TestLocalPersistenceOutboxHandlerMaterializeIncoming asserts incoming
// materialization persists recipient VTXOs and emits IncomingHandledEvent.
func TestLocalPersistenceOutboxHandlerMaterializeIncoming(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkPSBT, finalCheckpoints, recipients, parentCommitment, recipientKey,
		operatorKey :=
		buildTestIncomingMaterialization(t)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()
	packageStore := &testPackageStore{}

	notifyCalls := 0
	handler := &LocalPersistenceOutboxHandler{
		Store:        store,
		PackageStore: packageStore,
		OperatorKey:  operatorKey,
		ExitDelay:    10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			notifyCalls++
			return nil
		},
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (
			IncomingVTXOMetadata, error) {

			_ = ctx
			_ = sessionID
			_ = recipient
			_ = ark
			_ = finalCheckpoints

			return IncomingVTXOMetadata{
				RoundID:        "round-incoming",
				CommitmentTxID: parentCommitment,
				BatchExpiry:    1000,
				TreeDepth:      1,
				CreatedHeight:  700,
			}, nil
		},
	}

	req := &MaterializeIncomingVTXOsRequest{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpoints,
		Recipients:           recipients,
	}

	events, err := handler.Handle(ctx, sessionID, req)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &IncomingHandledEvent{}, events[0])
	require.Equal(t, 1, notifyCalls)

	handledEvt, ok := events[0].(*IncomingHandledEvent)
	require.True(t, ok)
	require.Len(t, handledEvt.MaterializedVTXOs, 1)

	desc, err := store.GetVTXO(ctx, wire.OutPoint{
		Hash:  arkPSBT.UnsignedTx.TxHash(),
		Index: recipients[0].OutputIndex,
	})
	require.NoError(t, err)
	require.Equal(t, "round-incoming", desc.RoundID)
	require.Equal(t, parentCommitment, desc.CommitmentTxID)
	require.EqualValues(t, 1000, desc.BatchExpiry)
	require.EqualValues(t, 1, desc.TreeDepth)
	require.EqualValues(t, 700, desc.CreatedHeight)

	// Re-materialization should be idempotent.
	events, err = handler.Handle(ctx, sessionID, req)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &IncomingHandledEvent{}, events[0])
	require.Equal(t, 2, packageStore.packageCalls)
	require.Equal(t, 2, packageStore.bindingCalls)
	require.Equal(t, PackageDirectionIncoming, packageStore.lastDirection)
	require.Equal(t, chainhash.Hash(sessionID), packageStore.lastSessionID)
}

// TestLocalPersistenceOutboxHandlerMaterializeIncomingSkipsNotOwned asserts
// non-owned recipients are skipped while owned recipients are materialized.
func TestLocalPersistenceOutboxHandlerMaterializeIncomingSkipsNotOwned(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, _, recipients, parentCommitment, recipientKey, operatorKey :=
		buildTestIncomingMaterialization(t)

	anchorIndex := uint32(len(arkPSBT.UnsignedTx.TxOut) - 1)
	recipients = append(recipients, ArkRecipientOutput{
		OutputIndex: anchorIndex,
		Value:       0,
		PkScript:    arkPSBT.UnsignedTx.TxOut[anchorIndex].PkScript,
	})

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()

	metadataCalls := 0
	handler := &LocalPersistenceOutboxHandler{
		Store:       store,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			return nil
		},
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			_ = ctx

			if recipient.OutputIndex == anchorIndex {
				return keychain.KeyDescriptor{},
					ErrIncomingRecipientNotOwned
			}

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (
			IncomingVTXOMetadata, error) {

			_ = ctx
			_ = sessionID
			_ = recipient
			_ = ark
			_ = finalCheckpoints

			metadataCalls++

			return IncomingVTXOMetadata{
				RoundID:        "round-incoming",
				CommitmentTxID: parentCommitment,
				BatchExpiry:    1000,
				TreeDepth:      1,
				CreatedHeight:  700,
			}, nil
		},
	}

	req := &MaterializeIncomingVTXOsRequest{
		SessionID:  sessionID,
		ArkPSBT:    arkPSBT,
		Recipients: recipients,
	}
	events, err := handler.Handle(ctx, sessionID, req)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &IncomingHandledEvent{}, events[0])
	require.Equal(t, 1, metadataCalls)

	live, err := store.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.EqualValues(
		t, recipients[0].OutputIndex, live[0].Outpoint.Index,
	)
}

// TestLocalPersistenceOutboxHandlerMaterializeIncomingRequiresOwned asserts the
// handler fails fast if no incoming recipient belongs to this wallet.
func TestLocalPersistenceOutboxHandlerMaterializeIncomingRequiresOwned(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, _, recipients, _, _, operatorKey :=
		buildTestIncomingMaterialization(t)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()

	handler := &LocalPersistenceOutboxHandler{
		Store:       store,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			return nil
		},
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{},
				ErrIncomingRecipientNotOwned
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (
			IncomingVTXOMetadata, error) {

			_ = ctx
			_ = sessionID
			_ = recipient
			_ = ark
			_ = finalCheckpoints

			return IncomingVTXOMetadata{},
				fmt.Errorf("metadata should not be resolved")
		},
	}

	req := &MaterializeIncomingVTXOsRequest{
		SessionID:  sessionID,
		ArkPSBT:    arkPSBT,
		Recipients: recipients,
	}
	events, err := handler.Handle(ctx, sessionID, req)
	require.Error(t, err)
	require.ErrorContains(t, err, "no wallet-owned recipients")
	require.Empty(t, events)
}

// TestLocalPersistenceOutboxHandlerMaterializeIncomingNotifierFailure asserts
// notifier failures abort incoming materialization completion.
func TestLocalPersistenceOutboxHandlerMaterializeIncomingNotifierFailure(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, finalCheckpoints, recipients, parentCommitment, recipientKey,
		operatorKey :=
		buildTestIncomingMaterialization(t)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()

	handler := &LocalPersistenceOutboxHandler{
		Store:       store,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			return fmt.Errorf("notify failed")
		},
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (
			IncomingVTXOMetadata, error) {

			_ = ctx
			_ = sessionID
			_ = recipient
			_ = ark
			_ = finalCheckpoints

			return IncomingVTXOMetadata{
				RoundID:        "round-incoming",
				CommitmentTxID: parentCommitment,
				BatchExpiry:    1000,
				TreeDepth:      1,
				CreatedHeight:  700,
			}, nil
		},
	}

	req := &MaterializeIncomingVTXOsRequest{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpoints,
		Recipients:           recipients,
	}
	events, err := handler.Handle(ctx, sessionID, req)
	require.Error(t, err)
	require.ErrorContains(t, err, "notify failed")
	require.Empty(t, events)
}

// TestLocalPersistenceOutboxHandlerMaterializeIncomingRequiresNotifier asserts
// notifier wiring is mandatory for incoming materialization.
func TestLocalPersistenceOutboxHandlerMaterializeIncomingRequiresNotifier(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, finalCheckpoints, recipients, parentCommitment, recipientKey,
		operatorKey :=
		buildTestIncomingMaterialization(t)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()

	handler := &LocalPersistenceOutboxHandler{
		Store:       store,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (
			IncomingVTXOMetadata, error) {

			_ = ctx
			_ = sessionID
			_ = recipient
			_ = ark
			_ = finalCheckpoints

			return IncomingVTXOMetadata{
				RoundID:        "round-incoming",
				CommitmentTxID: parentCommitment,
				BatchExpiry:    1000,
				TreeDepth:      1,
				CreatedHeight:  700,
			}, nil
		},
	}

	req := &MaterializeIncomingVTXOsRequest{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpoints,
		Recipients:           recipients,
	}
	events, err := handler.Handle(ctx, sessionID, req)
	require.Error(t, err)
	require.ErrorContains(t, err, "incoming VTXO notifier")
	require.Empty(t, events)
}

// TestLocalPersistenceOutboxHandlerMaterializeIncomingMissingMetadataRetryable
// verifies that actor-path metadata gaps are surfaced as retryable outbox
// errors instead of terminal failures.
func TestLocalPersistenceOutboxHandlerMaterializeIncomingMissingMetadataRetryable(
	t *testing.T) {

	t.Parallel()

	ctx := actor.WithTx(t.Context(), (*sql.Tx)(nil))

	arkPSBT, finalCheckpoints, recipients, _, recipientKey, operatorKey :=
		buildTestIncomingMaterialization(t)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()

	handler := &LocalPersistenceOutboxHandler{
		Store:       store,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (
			IncomingVTXOMetadata, error) {

			_ = ctx
			_ = sessionID
			_ = recipient
			_ = ark
			_ = finalCheckpoints

			return IncomingVTXOMetadata{},
				fmt.Errorf("resolver should not be called")
		},
	}

	req := &MaterializeIncomingVTXOsRequest{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpoints,
		Recipients:           recipients,
	}
	events, err := handler.Handle(ctx, sessionID, req)
	require.Error(t, err)
	require.Empty(t, events)
	require.ErrorContains(t, err, "incoming metadata missing")

	var retryErr *RetryableOutboxError
	require.True(t, errors.As(err, &retryErr))
	require.Equal(t, defaultRetryDelay, retryErr.RetryAfter)
}

// TestLocalPersistenceOutboxHandlerMaterializeIncomingSelfTransferPackageReuse
// asserts that incoming self-transfer materialization tolerates an existing
// outgoing package row for the same session.
func TestLocalPersistenceOutboxHandlerMaterializeIncomingSelfTransferPackageReuse(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, finalCheckpoints, recipients, parentCommitment, recipientKey,
		operatorKey :=
		buildTestIncomingMaterialization(t)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()
	packageStore := &testPackageStore{
		packageErr: fmt.Errorf("%w: existing=outgoing requested=incoming",
			libtypes.ErrOORPackageDirectionConflict),
	}

	notifyCalls := 0
	handler := &LocalPersistenceOutboxHandler{
		Store:        store,
		PackageStore: packageStore,
		OperatorKey:  operatorKey,
		ExitDelay:    10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			notifyCalls++
			return nil
		},
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (
			IncomingVTXOMetadata, error) {

			_ = ctx
			_ = sessionID
			_ = recipient
			_ = ark
			_ = finalCheckpoints

			return IncomingVTXOMetadata{
				RoundID:        "round-incoming",
				CommitmentTxID: parentCommitment,
				BatchExpiry:    1000,
				TreeDepth:      1,
				CreatedHeight:  700,
			}, nil
		},
	}

	req := &MaterializeIncomingVTXOsRequest{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpoints,
		Recipients:           recipients,
	}
	events, err := handler.Handle(ctx, sessionID, req)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &IncomingHandledEvent{}, events[0])
	require.Equal(t, 1, packageStore.packageCalls)
	require.Equal(t, 1, packageStore.bindingCalls)
	require.Equal(t, 1, notifyCalls)

	desc, err := store.GetVTXO(ctx, wire.OutPoint{
		Hash:  arkPSBT.UnsignedTx.TxHash(),
		Index: recipients[0].OutputIndex,
	})
	require.NoError(t, err)
	require.Equal(t, "round-incoming", desc.RoundID)
	require.Equal(t, parentCommitment, desc.CommitmentTxID)
}

// TestLocalPersistenceHandlerMarkInputsSpentViaCompleter asserts that when
// CompleteSpend is configured, the handler routes spend completion through the
// callback instead of writing to the store directly.
func TestLocalPersistenceHandlerMarkInputsSpentViaCompleter(t *testing.T) {
	t.Parallel()

	outpoints := []wire.OutPoint{
		{Hash: [32]byte{0x01}, Index: 0},
		{Hash: [32]byte{0x02}, Index: 1},
	}

	var completedOutpoints []wire.OutPoint
	handler := &LocalPersistenceOutboxHandler{
		CompleteSpend: func(_ context.Context,
			ops []wire.OutPoint) error {

			completedOutpoints = ops
			return nil
		},
	}

	events, err := handler.Handle(
		t.Context(), SessionID{},
		&MarkInputsSpentRequest{Outpoints: outpoints},
	)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &InputsMarkedSpentEvent{}, events[0])
	require.Equal(t, outpoints, completedOutpoints)
}

// TestLocalPersistenceHandlerMarkInputsSpentSkipsNonLocal asserts that custom
// external inputs do not block outgoing completion when they are not present in
// the local VTXO store.
func TestLocalPersistenceHandlerMarkInputsSpentSkipsNonLocal(t *testing.T) {
	t.Parallel()

	store := newTestVTXOStore()
	localOutpoint := wire.OutPoint{Hash: [32]byte{0x01}, Index: 0}
	externalOutpoint := wire.OutPoint{Hash: [32]byte{0x02}, Index: 1}
	store.records[localOutpoint] = &vtxo.Descriptor{
		Outpoint: localOutpoint,
		Status:   vtxo.VTXOStatusLive,
	}

	var completedOutpoints []wire.OutPoint
	handler := &LocalPersistenceOutboxHandler{
		Store: store,
		CompleteSpend: func(_ context.Context,
			ops []wire.OutPoint) error {

			completedOutpoints = ops
			return nil
		},
	}

	events, err := handler.Handle(
		t.Context(), SessionID{},
		&MarkInputsSpentRequest{
			Outpoints: []wire.OutPoint{
				localOutpoint,
				externalOutpoint,
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &InputsMarkedSpentEvent{}, events[0])
	require.Equal(t, []wire.OutPoint{localOutpoint},
		completedOutpoints)
}

// TestLocalPersistenceHandlerMarkInputsSpentCompleterError asserts that
// CompleteSpend errors are surfaced to the caller.
func TestLocalPersistenceHandlerMarkInputsSpentCompleterError(t *testing.T) {
	t.Parallel()

	handler := &LocalPersistenceOutboxHandler{
		CompleteSpend: func(_ context.Context,
			_ []wire.OutPoint) error {

			return fmt.Errorf("actor unavailable")
		},
	}

	events, err := handler.Handle(
		t.Context(), SessionID{},
		&MarkInputsSpentRequest{
			Outpoints: []wire.OutPoint{
				{Hash: [32]byte{0x01}, Index: 0},
			},
		},
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "actor unavailable")
	require.Empty(t, events)
}

// TestLocalPersistenceHandlerMarkInputsSpentFallback asserts that the handler
// falls back to direct store writes when CompleteSpend is nil.
func TestLocalPersistenceHandlerMarkInputsSpentFallback(t *testing.T) {
	t.Parallel()

	store := newTestVTXOStore()
	op := wire.OutPoint{Hash: [32]byte{0x01}, Index: 0}
	store.records[op] = &vtxo.Descriptor{
		Outpoint: op,
		Status:   vtxo.VTXOStatusLive,
	}

	handler := &LocalPersistenceOutboxHandler{
		Store: store,
	}

	events, err := handler.Handle(
		t.Context(), SessionID{},
		&MarkInputsSpentRequest{Outpoints: []wire.OutPoint{op}},
	)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &InputsMarkedSpentEvent{}, events[0])

	desc, err := store.GetVTXO(t.Context(), op)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusSpent, desc.Status)
}

// TestLocalPersistenceHandlerMarkInputsSpentEmptyOutpoints asserts that
// empty outpoints are rejected regardless of the completion path.
func TestLocalPersistenceHandlerMarkInputsSpentEmptyOutpoints(t *testing.T) {
	t.Parallel()

	handler := &LocalPersistenceOutboxHandler{
		Store: newTestVTXOStore(),
	}

	events, err := handler.Handle(
		t.Context(), SessionID{},
		&MarkInputsSpentRequest{},
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "outpoints must be provided")
	require.Empty(t, events)
}

// TestLocalPersistenceOutboxHandlerIncomingAck asserts incoming ack requests
// emit IncomingAckSentEvent.
func TestLocalPersistenceOutboxHandlerIncomingAck(t *testing.T) {
	t.Parallel()

	handler := &LocalPersistenceOutboxHandler{}
	events, err := handler.Handle(
		t.Context(), SessionID{}, &SendIncomingAckRequest{},
	)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &IncomingAckSentEvent{}, events[0])
}

// buildTestIncomingMaterialization returns a canonical Ark PSBT and its
// recipient list for incoming materialization tests.
func buildTestIncomingMaterialization(t *testing.T) (*psbt.Packet,
	[]*psbt.Packet, []ArkRecipientOutput, chainhash.Hash, *btcec.PrivateKey,
	*btcec.PublicKey) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputValue := btcutil.Amount(10000)
	inputs := []oortx.CheckpointInput{
		{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: wire.OutPoint{
					Hash:  [32]byte{0x11},
					Index: 0,
				},
				Output: &wire.TxOut{
					Value: int64(inputValue),
					PkScript: newTestTaprootPkScript(
						t, operatorKey.PubKey(),
					),
				},
			},
			OwnerLeafScript: []byte{0x51},
		},
	}

	vtxoTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, 10,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	outputs := []oortx.RecipientOutput{
		{
			PkScript: recipientPkScript,
			Value:    inputValue,
		},
	}

	cp, err := oortx.BuildCheckpointPSBT(policy, inputs[0])
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{
			{
				Txid:           cp.PSBT.UnsignedTx.TxHash(),
				Output:         cp.PSBT.UnsignedTx.TxOut[0],
				TapTreeEncoded: cp.TapTreeEncoded,
			},
		},
		outputs,
	)
	require.NoError(t, err)

	recipients, err := ExtractArkRecipients(arkPSBT)
	require.NoError(t, err)

	return arkPSBT, []*psbt.Packet{cp.PSBT}, recipients,
		inputs[0].SpentVTXO.Outpoint.Hash, recipientKey,
		operatorKey.PubKey()
}
