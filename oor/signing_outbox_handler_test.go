package oor

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestSigningOutboxHandlerReusesStoredArkSignature(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sessionID := SessionID(chainhash.Hash{1, 2, 3})
	storedArk := testSigningHandlerPSBT(t, 9)
	store := &testArkSignatureStore{
		ark: storedArk,
		ok:  true,
	}

	handler := &SigningOutboxHandler{
		SigningArtifactStore: store,
	}

	events, err := handler.Handle(ctx, sessionID, &RequestArkSignatures{
		ArkPSBT: testSigningHandlerPSBT(t, 1),
	})
	require.NoError(t, err)
	require.Len(t, events, 1)

	signed, ok := events[0].(*ArkSignedEvent)
	require.True(t, ok)
	require.Same(t, storedArk, signed.ArkPSBT)
	require.Equal(t, sessionID, store.loadedSessionID)
	require.Equal(t, 0, store.saved)
}

func TestSigningOutboxHandlerPropagatesArkSignatureLoadError(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("load signed ark")
	handler := &SigningOutboxHandler{
		SigningArtifactStore: &testArkSignatureStore{
			loadErr: expectedErr,
		},
	}

	_, err := handler.Handle(
		t.Context(),
		SessionID(
			chainhash.Hash{3},
		),
		&RequestArkSignatures{},
	)
	require.ErrorIs(t, err, expectedErr)
}

func TestSigningOutboxHandlerReusesStoredCheckpointSignatures(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sessionID := SessionID(chainhash.Hash{4, 5, 6})
	storedCheckpoints := []*psbt.Packet{
		testSigningHandlerPSBT(t, 7),
		testSigningHandlerPSBT(t, 8),
	}
	store := &testArkSignatureStore{
		checkpoints:   storedCheckpoints,
		checkpointsOK: true,
	}

	handler := &SigningOutboxHandler{
		SigningArtifactStore: store,
	}

	events, err := handler.Handle(ctx, sessionID,
		&RequestCheckpointSignatures{
			CoSignedCheckpointPSBTs: []*psbt.Packet{
				testSigningHandlerPSBT(t, 1),
				testSigningHandlerPSBT(t, 2),
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, events, 1)

	signed, ok := events[0].(*CheckpointsSignedEvent)
	require.True(t, ok)
	require.Equal(t, storedCheckpoints, signed.FinalCheckpointPSBTs)
	require.Equal(t, sessionID, store.loadedCheckpointSessionID)
	require.Equal(t, 2, store.loadedCheckpointExpectedCount)
	require.Equal(t, 0, store.savedCheckpoints)
}

type testArkSignatureStore struct {
	ark *psbt.Packet
	ok  bool

	checkpoints   []*psbt.Packet
	checkpointsOK bool

	loadErr error
	saveErr error

	loadedSessionID               SessionID
	loadedCheckpointSessionID     SessionID
	loadedCheckpointExpectedCount int
	savedSessionID                SessionID
	savedCheckpointSessionID      SessionID
	savedArk                      *psbt.Packet
	savedCheckpointArtifacts      []*psbt.Packet
	saved                         int
	savedCheckpoints              int
}

func (s *testArkSignatureStore) LoadArkSignedArtifact(_ context.Context,
	sessionID SessionID) (*psbt.Packet, bool, error) {

	s.loadedSessionID = sessionID
	if s.loadErr != nil {
		return nil, false, s.loadErr
	}

	return s.ark, s.ok, nil
}

func (s *testArkSignatureStore) SaveArkSignedArtifact(_ context.Context,
	sessionID SessionID, ark *psbt.Packet) error {

	s.saved++
	s.savedSessionID = sessionID
	s.savedArk = ark

	return s.saveErr
}

func (s *testArkSignatureStore) LoadFinalCheckpointArtifacts(_ context.Context,
	sessionID SessionID, expectedCount int) ([]*psbt.Packet, bool, error) {

	s.loadedCheckpointSessionID = sessionID
	s.loadedCheckpointExpectedCount = expectedCount

	return s.checkpoints, s.checkpointsOK, nil
}

func (s *testArkSignatureStore) SaveFinalCheckpointArtifacts(_ context.Context,
	sessionID SessionID, checkpoints []*psbt.Packet) error {

	s.savedCheckpoints++
	s.savedCheckpointSessionID = sessionID
	s.savedCheckpointArtifacts = checkpoints

	return nil
}

func testSigningHandlerPSBT(t *testing.T, seed byte) *psbt.Packet {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{seed},
			Index: uint32(seed),
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(seed) + 1,
		PkScript: []byte{0x51},
	})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	return pkt
}
