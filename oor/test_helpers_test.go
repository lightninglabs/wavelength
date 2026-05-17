package oor

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newTestTransferInput creates a minimally valid transfer input for unit
// tests.
func newTestTransferInput(t *testing.T, ownerKey *btcec.PrivateKey,
	operatorKey *btcec.PublicKey, outpoint wire.OutPoint,
	amount btcutil.Amount) TransferInput {

	t.Helper()

	exitDelay := uint32(10)

	tapscript, err := arkscript.VTXOTapScript(
		ownerKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	tapKey, err := arkscript.VTXOTapKey(
		ownerKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	return TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: outpoint,
			Amount:   amount,
			PkScript: pkScript,
			ClientKey: keychain.KeyDescriptor{
				PubKey: ownerKey.PubKey(),
			},
			OperatorKey:    operatorKey,
			TapScript:      tapscript,
			RelativeExpiry: exitDelay,
		},
		OwnerLeafScript: newTestCollabLeaf(
			t, ownerKey.PubKey(), operatorKey,
		),
	}
}

// newTestCollabLeaf builds the 2-of-2 collaborative multisig leaf script
// for checkpoint outputs in tests. This matches the protocol spec where
// the checkpoint collab path requires both owner and operator signatures.
func newTestCollabLeaf(t *testing.T,
	ownerKey, operatorKey *btcec.PublicKey) []byte {

	t.Helper()

	leaf, err := arkscript.MultiSigCollabTapLeaf(ownerKey, operatorKey)
	require.NoError(t, err)

	return leaf.Script
}

// newTestTaprootPkScript returns a valid P2TR pkScript for tests.
func newTestTaprootPkScript(t *testing.T, key *btcec.PublicKey) []byte {
	t.Helper()

	pkScript, err := txscript.PayToTaprootScript(key)
	require.NoError(t, err)

	return pkScript
}

// buildIncomingResolveResponse creates an indexer response carrying the full
// Ark/checkpoint package for a lightweight incoming-transfer hint.
func buildIncomingResolveResponse(t *testing.T) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, SessionID, []byte,
	uint64) {

	t.Helper()

	arkPSBT, finalCheckpoints, recipients, _, _, _ :=
		buildTestIncomingMaterialization(t)

	arkRaw, err := psbtutil.Serialize(arkPSBT)
	require.NoError(t, err)

	checkpointRaws := make([][]byte, 0, len(finalCheckpoints))
	for _, checkpoint := range finalCheckpoints {
		checkpointRaw, checkpointErr := psbtutil.Serialize(
			checkpoint,
		)
		require.NoError(t, checkpointErr)

		checkpointRaws = append(checkpointRaws, checkpointRaw)
	}

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	recipient := recipients[0]

	return &arkrpc.ListOORRecipientEventsByScriptResponse{
		Events: []*arkrpc.OORRecipientEvent{
			{
				RecipientPkScript: recipient.PkScript,
				EventId:           7,
				SessionId:         sessionID[:],
				OutputIndex:       recipient.OutputIndex,
				Value:             uint64(recipient.Value),
				ArkPsbt:           arkRaw,
				CheckpointPsbts:   checkpointRaws,
			},
		},
		NextCursor: 8,
	}, sessionID, recipient.PkScript, 7
}

func newTestSessionStore() *memoryOORClientSessionStore {
	return &memoryOORClientSessionStore{
		outgoing: make(map[SessionID]*OutgoingSnapshot),
		incoming: make(map[SessionID]*IncomingSnapshot),
		pending:  make(map[SessionID]*ResolveIncomingTransferRequest),
	}
}

type memoryOORClientSessionStore struct {
	outgoing map[SessionID]*OutgoingSnapshot
	incoming map[SessionID]*IncomingSnapshot
	pending  map[SessionID]*ResolveIncomingTransferRequest
}

func (s *memoryOORClientSessionStore) LoadActiveSessions(context.Context) (
	[]StoredClientSession, error) {

	sessions := make(
		[]StoredClientSession, 0,
		len(s.outgoing)+len(s.incoming)+len(s.pending),
	)
	for _, snapshot := range s.outgoing {
		sessions = append(sessions, StoredClientSession{
			Direction: SessionDirectionOutgoing,
			Outgoing:  snapshot,
		})
	}
	for _, snapshot := range s.incoming {
		sessions = append(sessions, StoredClientSession{
			Direction: SessionDirectionIncoming,
			Incoming:  snapshot,
		})
	}
	for _, req := range s.pending {
		sessions = append(sessions, StoredClientSession{
			Direction: SessionDirectionIncoming,
			Incoming: &IncomingSnapshot{
				Version:   1,
				SessionID: req.SessionID,
				Phase:     IncomingPhaseResolvePending,
				RecipientPkScript: append(
					[]byte(nil), req.RecipientPkScript...,
				),
				RecipientEventID: req.RecipientEventID,
			},
		})
	}

	return sessions, nil
}

func (s *memoryOORClientSessionStore) FindOutgoingByIdempotencyKey(
	_ context.Context, idempotencyKey string) (SessionID, bool, error) {

	for sessionID, snapshot := range s.outgoing {
		if snapshot.IdempotencyKey == idempotencyKey {
			return sessionID, true, nil
		}
	}

	return SessionID{}, false, nil
}

func (s *memoryOORClientSessionStore) SaveOutgoingSession(_ context.Context,
	snapshot *OutgoingSnapshot) error {

	s.outgoing[snapshot.SessionID] = snapshot

	return nil
}

func (s *memoryOORClientSessionStore) SaveIncomingSession(_ context.Context,
	snapshot *IncomingSnapshot) error {

	s.incoming[snapshot.SessionID] = snapshot

	return nil
}

func (s *memoryOORClientSessionStore) SavePendingIncomingHint(_ context.Context,
	req *ResolveIncomingTransferRequest) error {

	s.pending[req.SessionID] = req

	return nil
}
