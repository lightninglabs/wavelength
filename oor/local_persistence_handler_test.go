package oor

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

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
func (s *testVTXOStore) SaveVTXO(_ context.Context, desc *vtxo.Descriptor) error {
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
func (s *testVTXOStore) ListLiveVTXOs(_ context.Context) ([]*vtxo.Descriptor, error) {
	out := make([]*vtxo.Descriptor, 0, len(s.records))
	for _, desc := range s.records {
		cpy := *desc
		out = append(out, &cpy)
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

	arkPSBT, recipients, parentCommitment, recipientKey, operatorKey := buildTestIncomingMaterialization(
		t,
	)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()

	handler := &LocalPersistenceOutboxHandler{
		Store:       store,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (keychain.KeyDescriptor, error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (IncomingVTXOMetadata, error) {

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
		SessionID:  sessionID,
		ArkPSBT:    arkPSBT,
		Recipients: recipients,
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
}

// TestLocalPersistenceOutboxHandlerMaterializeIncomingSkipsNotOwned asserts
// non-owned recipients are skipped while owned recipients are materialized.
func TestLocalPersistenceOutboxHandlerMaterializeIncomingSkipsNotOwned(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, recipients, parentCommitment, recipientKey, operatorKey := buildTestIncomingMaterialization(
		t,
	)

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
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (keychain.KeyDescriptor, error) {

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
			finalCheckpoints []*psbt.Packet) (IncomingVTXOMetadata, error) {

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

	events, err := handler.Handle(ctx, sessionID, &MaterializeIncomingVTXOsRequest{
		SessionID:  sessionID,
		ArkPSBT:    arkPSBT,
		Recipients: recipients,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.IsType(t, &IncomingHandledEvent{}, events[0])
	require.Equal(t, 1, metadataCalls)

	live, err := store.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.EqualValues(t, recipients[0].OutputIndex, live[0].Outpoint.Index)
}

// TestLocalPersistenceOutboxHandlerMaterializeIncomingRequiresOwned asserts the
// handler fails fast if no incoming recipient belongs to this wallet.
func TestLocalPersistenceOutboxHandlerMaterializeIncomingRequiresOwned(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	arkPSBT, recipients, _, _, operatorKey := buildTestIncomingMaterialization(t)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	store := newTestVTXOStore()

	handler := &LocalPersistenceOutboxHandler{
		Store:       store,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient ArkRecipientOutput) (keychain.KeyDescriptor, error) {

			_ = ctx
			_ = recipient

			return keychain.KeyDescriptor{}, ErrIncomingRecipientNotOwned
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID SessionID, recipient ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (IncomingVTXOMetadata, error) {

			_ = ctx
			_ = sessionID
			_ = recipient
			_ = ark
			_ = finalCheckpoints

			return IncomingVTXOMetadata{},
				fmt.Errorf("metadata should not be resolved")
		},
	}

	events, err := handler.Handle(ctx, sessionID, &MaterializeIncomingVTXOsRequest{
		SessionID:  sessionID,
		ArkPSBT:    arkPSBT,
		Recipients: recipients,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "no wallet-owned recipients")
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
	[]ArkRecipientOutput, chainhash.Hash, *btcec.PrivateKey,
	*btcec.PublicKey) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
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

	vtxoTapKey, err := scripts.VTXOTapKey(
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

	return arkPSBT, recipients, inputs[0].SpentVTXO.Outpoint.Hash, recipientKey,
		operatorKey.PubKey()
}
