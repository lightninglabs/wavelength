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

	packageErr        error
	lastAssetTransfer *oortx.TaprootAssetTransfer
}

// UpsertPackageWithAssets records one asset-aware package upsert call.
func (s *testPackageStore) UpsertPackageWithAssets(ctx context.Context,
	direction PackageDirection, sessionID chainhash.Hash, ark *psbt.Packet,
	checkpoints []*psbt.Packet,
	assetTransfer *oortx.TaprootAssetTransfer) error {

	s.lastAssetTransfer = assetTransfer.Clone()

	return s.UpsertPackage(
		ctx, direction, sessionID, ark, checkpoints,
	)
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

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		recipientKey.PubKey(), operatorKey, 10,
	)
	require.NoError(t, err)
	assetRoot := chainhash.Hash{0x81, 0x82, 0x83}
	assetDesc := &vtxo.Descriptor{
		PolicyTemplate:     policyTemplate,
		TaprootAssetRoot:   &assetRoot,
		TaprootAssetRef:    "asset-id:010203",
		TaprootAssetAmount: 21,
	}
	assetPkScript, err := assetDesc.EffectivePkScript()
	require.NoError(t, err)
	arkPSBT.UnsignedTx.TxOut[recipients[0].OutputIndex].PkScript =
		assetPkScript
	recipients[0].PkScript = assetPkScript
	recipients[0].VTXOPolicyTemplate = policyTemplate
	recipients[0].TaprootAssetRoot = &assetRoot
	recipients[0].TaprootAssetRef = "asset-id:010203"
	recipients[0].TaprootAssetAmount = 21
	assetTransfer := &oortx.TaprootAssetTransfer{
		Version: oortx.TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			{
				0x84,
			},
		},
		ArkPackage: []byte{
			0x85,
		},
	}

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
		TaprootAssetTransfer: assetTransfer,
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
	require.Equal(t, &assetRoot, desc.TaprootAssetRoot)
	require.Equal(t, "asset-id:010203", desc.TaprootAssetRef)
	require.Equal(t, uint64(21), desc.TaprootAssetAmount)
	require.Equal(t, assetTransfer, packageStore.lastAssetTransfer)

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

	ancestorArk, ancestorCheckpoints, _, _, _, _ :=
		buildTestIncomingMaterialization(t)
	rootArk, rootCheckpoints, _, _, _, _ :=
		buildTestIncomingMaterialization(t)
	rootArk, rootCheckpoints = reparentTestIncomingPackage(
		t, rootArk, rootCheckpoints, ancestorArk,
	)

	root := packageArtifactForValidation(
		SessionID(
			rootArk.UnsignedTx.TxHash(),
		),
		rootArk,
		rootCheckpoints,
	)
	ancestor := packageArtifactForValidation(
		SessionID(
			ancestorArk.UnsignedTx.TxHash(),
		),
		ancestorArk,
		ancestorCheckpoints,
	)

	err := validateIncomingPackageGraph(root, []PackageArtifact{ancestor})
	require.NoError(t, err)
}

// TestValidateIncomingPackageGraphRejectsDuplicateAncestor asserts valid
// ancestors still cannot be supplied more than once.
func TestValidateIncomingPackageGraphRejectsDuplicateAncestor(t *testing.T) {
	t.Parallel()

	ancestorArk, ancestorCheckpoints, _, _, _, _ :=
		buildTestIncomingMaterialization(t)
	rootArk, rootCheckpoints, _, _, _, _ :=
		buildTestIncomingMaterialization(t)
	rootArk, rootCheckpoints = reparentTestIncomingPackage(
		t, rootArk, rootCheckpoints, ancestorArk,
	)

	root := packageArtifactForValidation(
		SessionID(
			rootArk.UnsignedTx.TxHash(),
		),
		rootArk,
		rootCheckpoints,
	)
	ancestor := packageArtifactForValidation(
		SessionID(
			ancestorArk.UnsignedTx.TxHash(),
		),
		ancestorArk,
		ancestorCheckpoints,
	)

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
					Hash: [32]byte{
						0x11,
					},
					Index: 0,
				},
				Output: &wire.TxOut{
					Value: int64(inputValue),
					PkScript: newTestTaprootPkScript(
						t, operatorKey.PubKey(),
					),
				},
			},
			OwnerLeafScript: []byte{
				0x51,
			},
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
	cp.PSBT.Inputs[0].FinalScriptWitness = []byte{0x01, 0x51}

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

// reparentTestIncomingPackage rewrites the test package's checkpoint input to
// spend output zero of parentArk, then rebuilds the Ark transaction so its
// session ID follows the new checkpoint txid.
func reparentTestIncomingPackage(t *testing.T, arkPSBT *psbt.Packet,
	checkpoints []*psbt.Packet,
	parentArk *psbt.Packet) (*psbt.Packet, []*psbt.Packet) {

	t.Helper()

	require.Len(t, checkpoints, 1)
	require.NotNil(t, parentArk)
	require.NotNil(t, parentArk.UnsignedTx)
	require.NotEmpty(t, parentArk.UnsignedTx.TxOut)

	checkpoint := checkpoints[0]
	require.NotNil(t, checkpoint)
	require.NotNil(t, checkpoint.UnsignedTx)
	require.NotEmpty(t, checkpoint.UnsignedTx.TxIn)
	require.NotEmpty(t, checkpoint.UnsignedTx.TxOut)

	parentTxid := parentArk.UnsignedTx.TxHash()
	checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint = wire.OutPoint{
		Hash:  parentTxid,
		Index: 0,
	}
	checkpoint.Inputs[0].WitnessUtxo = parentArk.UnsignedTx.TxOut[0]

	recipients, err := ExtractArkRecipients(arkPSBT)
	require.NoError(t, err)

	outputs := make([]oortx.RecipientOutput, 0, len(recipients))
	for i := range recipients {
		outputs = append(outputs, oortx.RecipientOutput{
			PkScript: recipients[i].PkScript,
			Value:    recipients[i].Value,
		})
	}

	arkPSBT, err = oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{{
			Txid:   checkpoint.UnsignedTx.TxHash(),
			Output: checkpoint.UnsignedTx.TxOut[0],
		}},
		outputs,
	)
	require.NoError(t, err)

	return arkPSBT, checkpoints
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

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Two independent checkpoint inputs anchored to distinct
	// upstream Ark txids so the produced Ark tx has two inputs,
	// each contributable by a different ancestry fragment.
	inputAmt := btcutil.Amount(5_000)
	makeInput := func(seed byte) oortx.CheckpointInput {
		return oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: wire.OutPoint{
					Hash: [32]byte{
						seed,
					},
					Index: 0,
				},
				Output: &wire.TxOut{
					Value: int64(inputAmt),
					PkScript: newTestTaprootPkScript(
						t, operatorKey.PubKey(),
					),
				},
			},
			OwnerLeafScript: []byte{
				0x51,
			},
		}
	}
	inputs := []oortx.CheckpointInput{
		makeInput(0x11), makeInput(0x22),
	}

	cp0, err := oortx.BuildCheckpointPSBT(policy, inputs[0])
	require.NoError(t, err)

	cp1, err := oortx.BuildCheckpointPSBT(policy, inputs[1])
	require.NoError(t, err)

	vtxoTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, 10,
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
				Txid:           cp0.PSBT.UnsignedTx.TxHash(),
				Output:         cp0.PSBT.UnsignedTx.TxOut[0],
				TapTreeEncoded: cp0.TapTreeEncoded,
			},
			{
				Txid:           cp1.PSBT.UnsignedTx.TxHash(),
				Output:         cp1.PSBT.UnsignedTx.TxOut[0],
				TapTreeEncoded: cp1.TapTreeEncoded,
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
		cp0.PSBT.UnsignedTx.TxHash(),
		cp1.PSBT.UnsignedTx.TxHash(),
	}

	return arkPSBT, []*psbt.Packet{cp0.PSBT, cp1.PSBT}, recipients,
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
