package oor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	lib_tree "github.com/lightninglabs/wavelength/lib/tree"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	libtypes "github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// testPackageStore records package persistence calls for handler assertions.
type testPackageStore struct {
	packageCalls int
	bindingCalls int

	lastDirection PackageDirection
	lastSessionID chainhash.Hash
	sessions      []chainhash.Hash

	packageErr error
}

// UpsertPackage records one package upsert call.
func (s *testPackageStore) UpsertPackage(_ context.Context,
	direction PackageDirection, sessionID chainhash.Hash, _ *psbt.Packet,
	_ []*psbt.Packet) error {

	s.packageCalls++
	s.lastDirection = direction
	s.lastSessionID = sessionID
	s.sessions = append(s.sessions, sessionID)

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
	records       map[wire.OutPoint]*vtxo.Descriptor
	getErr        error
	lastGetCtxHas bool
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
func (s *testVTXOStore) GetVTXO(ctx context.Context, outpoint wire.OutPoint) (
	*vtxo.Descriptor, error) {

	s.lastGetCtxHas = actor.HasTx(ctx)

	if s.getErr != nil {
		return nil, s.getErr
	}

	desc, ok := s.records[outpoint]
	if !ok {
		return nil, fmt.Errorf("get VTXO: %w", sql.ErrNoRows)
	}

	cpy := *desc

	return &cpy, nil
}

// ListLiveVTXOs returns all stored descriptors.
func (s *testVTXOStore) ListLiveVTXOs(_ context.Context) ([]*vtxo.Descriptor,
	error) {

	out := make([]*vtxo.Descriptor, 0, len(s.records))
	for _, desc := range s.records {
		cpy := *desc
		out = append(out, &cpy)
	}

	return out, nil
}

// ListRecoverableVTXOs mirrors ListLiveVTXOs: this fixture holds no expired
// records, so the recoverable and live sets coincide.
func (s *testVTXOStore) ListRecoverableVTXOs(ctx context.Context) (
	[]*vtxo.Descriptor, error) {

	return s.ListLiveVTXOs(ctx)
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

// ListSelectionCandidatesByStatus projects stored descriptors matching the
// given status down to the selection fields.
func (s *testVTXOStore) ListSelectionCandidatesByStatus(_ context.Context,
	status vtxo.VTXOStatus) ([]vtxo.SelectedVTXO, error) {

	var out []vtxo.SelectedVTXO
	for _, desc := range s.records {
		if desc.Status == status {
			out = append(out, vtxo.SelectedVTXO{
				Outpoint: desc.Outpoint,
				Amount:   desc.Amount,
				PkScript: desc.PkScript,
			})
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

func (s *testVTXOStore) UpdateVTXOStatusReleasingReservation(
	ctx context.Context, outpoint wire.OutPoint,
	status vtxo.VTXOStatus) error {

	return s.UpdateVTXOStatus(ctx, outpoint, status)
}

// MarkForfeiting is unused by these tests.
func (s *testVTXOStore) MarkForfeiting(_ context.Context, _ wire.OutPoint,
	_ string, _ *wire.MsgTx) error {

	return nil
}

// GetForfeitTx is unused by these tests.
func (s *testVTXOStore) GetForfeitTx(_ context.Context, _ wire.OutPoint) (
	*wire.MsgTx, error) {

	return nil, nil
}

// MarkForfeited is unused by these tests.
func (s *testVTXOStore) MarkForfeited(_ context.Context, _ wire.OutPoint,
	_ chainhash.Hash) error {

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
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet, finalCheckpoints []*psbt.Packet) (
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
				Ancestry: validTestIncomingAncestry(
					parentCommitment,
				),
				CreatedHeight: 700,
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
	require.EqualValues(t, 1, desc.MaxTreeDepth())
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

// TestLocalPersistenceOutboxHandlerRejectsInvalidAncestorPackage asserts
// untrusted incoming ancestor packages are validated before they can poison the
// package store used by recovery.
func TestLocalPersistenceOutboxHandlerRejectsInvalidAncestorPackage(
	t *testing.T) {

	t.Parallel()

	arkPSBT, finalCheckpoints, recipients, _, _, operatorKey :=
		buildTestIncomingMaterialization(t)

	packageStore := &testPackageStore{}
	handler := &LocalPersistenceOutboxHandler{
		Store:        newTestVTXOStore(),
		PackageStore: packageStore,
		OperatorKey:  operatorKey,
		ExitDelay:    10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			return nil
		},
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{}, nil
		},
	}

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	ancestorID := sessionID
	ancestorID[0] ^= 0x01

	req := &MaterializeIncomingVTXOsRequest{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpoints,
		Recipients:           recipients,
		AncestorPackages: []PackageArtifact{{
			SessionID:            ancestorID,
			ArkPSBT:              arkPSBT,
			FinalCheckpointPSBTs: finalCheckpoints,
		}},
	}

	events, err := handler.Handle(t.Context(), sessionID, req)
	require.Error(t, err)
	require.ErrorContains(
		t, err, "ancestor package 0 session id does not match ark txid",
	)
	require.Empty(t, events)
	require.Zero(t, packageStore.packageCalls)
}

// TestLocalPersistenceOutboxHandlerRejectsInvalidCheckpointWitness proves a
// restored or replayed materialization request reruns script validation before
// writing either a VTXO or package artifact. The check remains mandatory when
// the optional package store is disabled.
func TestLocalPersistenceOutboxHandlerRejectsInvalidCheckpointWitness(
	t *testing.T) {

	t.Parallel()

	arkPSBT, finalCheckpoints, recipients, _, _, operatorKey :=
		buildTestIncomingMaterialization(t)
	finalCheckpoints[0].Inputs[0].TaprootScriptSpendSig = nil
	finalCheckpoints[0].Inputs[0].TaprootKeySpendSig = make([]byte, 64)

	store := newTestVTXOStore()
	handler := &LocalPersistenceOutboxHandler{
		Store:       store,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			return nil
		},
		ResolveIncomingClientKey: func(_ context.Context,
			_ ArkRecipientOutput) (keychain.KeyDescriptor, error) {

			return keychain.KeyDescriptor{}, nil
		},
	}
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	req := &MaterializeIncomingVTXOsRequest{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpoints,
		Recipients:           recipients,
	}

	events, err := handler.Handle(t.Context(), sessionID, req)
	require.ErrorContains(t, err, "script validation failed")
	require.Empty(t, events)
	require.Empty(t, store.records)
}

// TestValidateIncomingPackageGraphRejectsUnconsumedAncestor asserts valid but
// unrelated ancestor packages must not be accepted as recovery ancestors.
func TestValidateIncomingPackageGraphRejectsUnconsumedAncestor(t *testing.T) {
	t.Parallel()

	arkPSBT, finalCheckpoints, _, _, _, _ :=
		buildTestIncomingMaterialization(t)
	ancestorArk, ancestorCheckpoints, _, _, _, _ :=
		buildTestIncomingMaterialization(t)

	root := packageArtifactForValidation(
		SessionID(
			arkPSBT.UnsignedTx.TxHash(),
		),
		arkPSBT,
		finalCheckpoints,
	)
	ancestor := packageArtifactForValidation(
		SessionID(
			ancestorArk.UnsignedTx.TxHash(),
		),
		ancestorArk,
		ancestorCheckpoints,
	)

	err := validateIncomingPackageGraph(root, []PackageArtifact{ancestor})
	require.Error(t, err)
	require.ErrorContains(
		t, err, "is not consumed by incoming package chain",
	)
}

// TestValidateIncomingPackageGraphAcceptsConnectedAncestor asserts a package
// whose checkpoint spends an ancestor Ark output can carry that ancestor as
// recovery material.
func TestValidateIncomingPackageGraphAcceptsConnectedAncestor(t *testing.T) {
	t.Parallel()

	root, ancestor := buildSignedTestIncomingPackageChain(t)

	err := validateIncomingPackageGraph(root, []PackageArtifact{ancestor})
	require.NoError(t, err)
}

// TestValidateIncomingPackageGraphRejectsAncestorWitnessUtxoMismatch proves a
// checkpoint cannot bind its valid signature to a fabricated witness UTXO
// while claiming to spend a different output from a supplied ancestor.
func TestValidateIncomingPackageGraphRejectsAncestorWitnessUtxoMismatch(
	t *testing.T) {

	t.Parallel()

	root, ancestor := buildSignedTestIncomingPackageChainWithChildAmount(
		t, 9_999,
	)

	err := validateIncomingPackageGraph(root, []PackageArtifact{ancestor})
	require.ErrorContains(
		t, err, "is not consumed by incoming package chain",
	)
}

// TestValidateIncomingPackageGraphRejectsInvalidAncestorWitness proves every
// supplied recovery ancestor executes its checkpoint witness before the graph
// can be accepted. A valid root cannot hide an unspendable earlier hop.
func TestValidateIncomingPackageGraphRejectsInvalidAncestorWitness(
	t *testing.T) {

	t.Parallel()

	root, ancestor := buildSignedTestIncomingPackageChain(t)
	input := &ancestor.FinalCheckpointPSBTs[0].Inputs[0]
	require.NotEmpty(t, input.TaprootScriptSpendSig)
	require.NotEmpty(t, input.TaprootScriptSpendSig[0].Signature)
	input.TaprootScriptSpendSig[0].Signature[0] ^= 0x01

	err := validateIncomingPackageGraph(root, []PackageArtifact{ancestor})
	require.ErrorContains(t, err, "script validation failed")
}

// TestValidateIncomingPackageGraphRejectsDuplicateAncestor asserts valid
// ancestors still cannot be supplied more than once.
func TestValidateIncomingPackageGraphRejectsDuplicateAncestor(t *testing.T) {
	t.Parallel()

	root, ancestor := buildSignedTestIncomingPackageChain(t)

	err := validateIncomingPackageGraph(
		root, []PackageArtifact{ancestor, ancestor},
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "duplicate ancestor package")
}

// TestLocalPersistenceOutboxHandlerUsesMetadataOperatorKey asserts incoming
// materialization prefers the per-VTXO operator key returned by the indexer
// over the handler's compatibility fallback key.
func TestLocalPersistenceOutboxHandlerUsesMetadataOperatorKey(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkPSBT, finalCheckpoints, recipients, parentCommitment, recipientKey,
		operatorKey :=
		buildTestIncomingMaterialization(t)

	staleOperatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()
	handler := &LocalPersistenceOutboxHandler{
		Store:       store,
		OperatorKey: staleOperatorKey.PubKey(),
		ExitDelay:   10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			return nil
		},
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
	}

	req := &MaterializeIncomingVTXOsRequest{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpoints,
		Recipients:           recipients,
		MetadataMatches: []IncomingMetadataMatch{{
			OutputIndex: recipients[0].OutputIndex,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "round-incoming",
				CommitmentTxID: parentCommitment,
				BatchExpiry:    1000,
				OperatorKey:    operatorKey,
				Ancestry: validTestIncomingAncestry(
					parentCommitment,
				),
				CreatedHeight: 700,
			},
		}},
	}

	events, err := handler.Handle(ctx, sessionID, req)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &IncomingHandledEvent{}, events[0])

	desc, err := store.GetVTXO(ctx, wire.OutPoint{
		Hash:  arkPSBT.UnsignedTx.TxHash(),
		Index: recipients[0].OutputIndex,
	})
	require.NoError(t, err)
	require.True(t, desc.OperatorKey.IsEqual(operatorKey))
	require.False(t, desc.OperatorKey.IsEqual(staleOperatorKey.PubKey()))
}

// TestLocalPersistenceOutboxHandlerMaterializeIncomingSkipsNotOwned asserts
// non-owned recipients are skipped while owned recipients are materialized.
func TestLocalPersistenceOutboxHandlerMaterializeIncomingSkipsNotOwned(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, finalCheckpoints, recipients, parentCommitment, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

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
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

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
			ark *psbt.Packet, finalCheckpoints []*psbt.Packet) (
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
				Ancestry: validTestIncomingAncestry(
					parentCommitment,
				),
				CreatedHeight: 700,
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

	arkPSBT, finalCheckpoints, recipients, _, _, operatorKey :=
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
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{},
				ErrIncomingRecipientNotOwned
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet, finalCheckpoints []*psbt.Packet) (
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
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpoints,
		Recipients:           recipients,
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
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet, finalCheckpoints []*psbt.Packet) (
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
				Ancestry: validTestIncomingAncestry(
					parentCommitment,
				),
				CreatedHeight: 700,
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
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet, finalCheckpoints []*psbt.Packet) (
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
				Ancestry: validTestIncomingAncestry(
					parentCommitment,
				),
				CreatedHeight: 700,
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
//
//nolint:ll
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
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet, finalCheckpoints []*psbt.Packet) (
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
//
//nolint:ll
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
		packageErr: fmt.Errorf(
			"%w: existing=outgoing requested=incoming",
			libtypes.ErrOORPackageDirectionConflict,
		),
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
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet, finalCheckpoints []*psbt.Packet) (
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
				Ancestry: validTestIncomingAncestry(
					parentCommitment,
				),
				CreatedHeight: 700,
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

	inputOwnerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputValue := btcutil.Amount(10000)
	spentOutpoint := wire.OutPoint{
		Hash: [32]byte{
			0x11,
		},
		Index: 0,
	}

	arkPSBT, checkpoints, recipients := buildSignedTestIncomingPackage(
		t, operatorKey, inputOwnerKey, spentOutpoint, inputValue,
		recipientKey,
	)

	return arkPSBT, checkpoints, recipients, spentOutpoint.Hash,
		recipientKey, operatorKey.PubKey()
}

// buildSignedTestIncomingPackage constructs an incoming package whose
// checkpoint spends one real VTXO collaborative leaf. The helper keeps
// materialization tests on the same cryptographic path as receiver admission.
func buildSignedTestIncomingPackage(t *testing.T,
	operatorKey, inputOwnerKey *btcec.PrivateKey,
	spentOutpoint wire.OutPoint, amount btcutil.Amount,
	recipientKey *btcec.PrivateKey) (*psbt.Packet, []*psbt.Packet,
	[]ArkRecipientOutput) {

	t.Helper()

	checkpoint, tapTree := buildSignedTestIncomingCheckpoint(
		t, operatorKey, inputOwnerKey, spentOutpoint, amount,
	)

	vtxoTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), operatorKey.PubKey(), 10,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT([]oortx.CheckpointOutput{{
		Txid:           checkpoint.UnsignedTx.TxHash(),
		Output:         checkpoint.UnsignedTx.TxOut[0],
		TapTreeEncoded: tapTree,
	}}, []oortx.RecipientOutput{{
		PkScript: recipientPkScript,
		Value:    amount,
	}})
	require.NoError(t, err)

	recipients, err := ExtractArkRecipients(arkPSBT)
	require.NoError(t, err)

	return arkPSBT, []*psbt.Packet{checkpoint}, recipients
}

// buildSignedTestIncomingCheckpoint constructs and fully signs one checkpoint
// against the exact VTXO prevout supplied to receiver validation.
func buildSignedTestIncomingCheckpoint(t *testing.T,
	operatorKey, inputOwnerKey *btcec.PrivateKey,
	spentOutpoint wire.OutPoint, amount btcutil.Amount) (*psbt.Packet,
	[]byte) {

	t.Helper()

	transferInput := newTestTransferInput(
		t, inputOwnerKey, operatorKey.PubKey(), spentOutpoint, amount,
	)
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    transferInput.VTXO.RelativeExpiry,
	}
	checkpoint, err := oortx.BuildCheckpointPSBT(
		policy, oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: transferInput.VTXO.Outpoint,
				Output: &wire.TxOut{
					Value: int64(
						transferInput.VTXO.Amount,
					),
					PkScript: transferInput.VTXO.PkScript,
				},
			},
			OwnerLeafScript: transferInput.OwnerLeafScript,
		},
	)
	require.NoError(t, err)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)
	ownerSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{inputOwnerKey}, nil,
	)
	checkpoints := []*psbt.Packet{checkpoint.PSBT}
	inputs := []TransferInput{transferInput}

	err = coSignCheckpointPSBTsForTest(
		operatorSigner, inputs, checkpoints,
	)
	require.NoError(t, err)
	err = SignCheckpointPSBTs(ownerSigner, inputs, checkpoints)
	require.NoError(t, err)

	return checkpoint.PSBT, checkpoint.TapTreeEncoded
}

// buildSignedTestIncomingPackageChain constructs two finalized packages where
// the child checkpoint spends the parent's recipient VTXO. It exercises the
// same ancestor graph and script checks used for chained recovery material.
func buildSignedTestIncomingPackageChain(t *testing.T) (PackageArtifact,
	PackageArtifact) {

	t.Helper()

	return buildSignedTestIncomingPackageChainWithChildAmount(t, 10_000)
}

// buildSignedTestIncomingPackageChainWithChildAmount constructs a signed
// parent and child while allowing the child to self-declare a different input
// value from the parent's real output.
func buildSignedTestIncomingPackageChainWithChildAmount(t *testing.T,
	childAmount btcutil.Amount) (PackageArtifact, PackageArtifact) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	parentInputOwner, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	childInputOwner, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	childRecipient, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const parentAmount = btcutil.Amount(10_000)
	parentArk, parentCheckpoints, parentRecipients :=
		buildSignedTestIncomingPackage(
			t, operatorKey, parentInputOwner, wire.OutPoint{
				Hash:  chainhash.Hash{0x31},
				Index: 0,
			}, parentAmount, childInputOwner,
		)

	childArk, childCheckpoints, _ := buildSignedTestIncomingPackage(
		t, operatorKey, childInputOwner, wire.OutPoint{
			Hash:  parentArk.UnsignedTx.TxHash(),
			Index: parentRecipients[0].OutputIndex,
		}, childAmount, childRecipient,
	)

	parent := packageArtifactForValidation(
		SessionID(
			parentArk.UnsignedTx.TxHash(),
		),
		parentArk,
		parentCheckpoints,
	)
	child := packageArtifactForValidation(
		SessionID(
			childArk.UnsignedTx.TxHash(),
		),
		childArk,
		childCheckpoints,
	)

	return child, parent
}

// buildTestIncomingMaterializationMultiInput is the two-checkpoint
// variant of buildTestIncomingMaterialization. It returns an Ark PSBT
// spending two distinct checkpoint inputs (so len(arkPSBT.UnsignedTx.TxIn)
// == 2). Cross-round multi-input OOR receive coverage exercises
// validateIncomingAncestry's partition checks, which require the union
// of all fragments' InputIndices to cover every Ark input — a property
// that cannot be exercised against the single-input helper.
//
// The two commitment txids returned correspond to inputs[0] and
// inputs[1] respectively; callers stitch them into two-fragment
// IncomingVTXOMetadata.Ancestry slices.
func buildTestIncomingMaterializationMultiInput(t *testing.T) (*psbt.Packet,
	[]*psbt.Packet, []ArkRecipientOutput, [2]chainhash.Hash,
	*btcec.PrivateKey, *btcec.PublicKey) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	inputOwnerA, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	inputOwnerB, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Two independent checkpoint inputs anchored to distinct
	// upstream Ark txids so the produced Ark tx has two inputs,
	// each contributable by a different ancestry fragment.
	inputAmt := btcutil.Amount(5_000)
	cp0, tapTree0 := buildSignedTestIncomingCheckpoint(
		t, operatorKey, inputOwnerA, wire.OutPoint{
			Hash:  chainhash.Hash{0x11},
			Index: 0,
		}, inputAmt,
	)
	cp1, tapTree1 := buildSignedTestIncomingCheckpoint(
		t, operatorKey, inputOwnerB, wire.OutPoint{
			Hash:  chainhash.Hash{0x22},
			Index: 0,
		}, inputAmt,
	)

	vtxoTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), operatorKey.PubKey(), 10,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	outputs := []oortx.RecipientOutput{
		{
			PkScript: recipientPkScript,
			Value:    inputAmt * 2,
		},
	}

	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{
			{
				Txid:           cp0.UnsignedTx.TxHash(),
				Output:         cp0.UnsignedTx.TxOut[0],
				TapTreeEncoded: tapTree0,
			},
			{
				Txid:           cp1.UnsignedTx.TxHash(),
				Output:         cp1.UnsignedTx.TxOut[0],
				TapTreeEncoded: tapTree1,
			},
		},
		outputs,
	)
	require.NoError(t, err)

	recipients, err := ExtractArkRecipients(arkPSBT)
	require.NoError(t, err)

	// Use the checkpoint tx ids as the per-fragment "commitment"
	// txids so that callers can name a real Ark-tx input prevout
	// for each fragment. (Ark inputs reference checkpoint tx ids,
	// not the upstream SpentVTXO outpoint hashes.) The validator
	// only requires that BatchOutpoint.Hash matches CommitmentTxID
	// across the per-fragment cross-check; it does not interpret
	// the commitment txid itself.
	commits := [2]chainhash.Hash{
		cp0.UnsignedTx.TxHash(),
		cp1.UnsignedTx.TxHash(),
	}

	return arkPSBT, []*psbt.Packet{cp0, cp1}, recipients,
		commits, recipientKey, operatorKey.PubKey()
}

// validTestIncomingAncestry returns a minimal Ancestry slice that passes
// BuildIncomingVTXODescriptor's structural cross-check, anchored at the
// supplied commitment txid. The test ark PSBT built by
// buildTestIncomingMaterialization has a single input, so input index 0
// is always within range.
//
// BatchOutpoint.Hash mirrors the commitment txid so the fragment-to-
// commitment binding check (validateIncomingAncestry) passes.
func validTestIncomingAncestry(commit chainhash.Hash) []vtxo.Ancestry {
	return []vtxo.Ancestry{{
		TreePath: &lib_tree.Tree{
			Root: &lib_tree.Node{},
			BatchOutpoint: wire.OutPoint{
				Hash: commit,
			},
		},
		CommitmentTxID: commit,
		InputIndices: []uint32{
			0,
		},
		TreeDepth: 1,
	}}
}
