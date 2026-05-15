package indexer

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/stretchr/testify/require"
)

// retryRecipientEventStore simulates a retryable read transaction by invoking
// the transaction body twice before reporting success.
type retryRecipientEventStore struct {
	Store

	attempts    int
	rows        []OORRecipientEventWithSession
	checkpoints map[string][]OORSessionCheckpoint
}

// ExecReadTx invokes fn twice to model a successful retry after the first
// transaction attempt already built an in-memory response.
func (s *retryRecipientEventStore) ExecReadTx(ctx context.Context,
	fn func(Store) error) error {

	s.attempts++
	if err := fn(s); err != nil {
		return err
	}

	s.attempts++

	return fn(s)
}

// ListOORRecipientEventsAfterWithSession returns the fixed test event page.
func (s *retryRecipientEventStore) ListOORRecipientEventsAfterWithSession(
	context.Context, []byte, int64, int32) ([]OORRecipientEventWithSession,
	error) {

	return s.rows, nil
}

// GetOORSessionCheckpoints returns finalized checkpoint PSBTs for a session.
func (s *retryRecipientEventStore) GetOORSessionCheckpoints(_ context.Context,
	sessionID []byte) ([]OORSessionCheckpoint, error) {

	return s.checkpoints[string(sessionID)], nil
}

// buildRecipientEventsScopeProof builds the legacy script-bound scope proof
// used by ListOORRecipientEventsByScript.
func buildRecipientEventsScopeProof(t *testing.T, serverID string,
	principal string) ([]byte, *arkrpc.TaprootSchnorrProof, time.Time) {

	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(priv.PubKey())
	require.NoError(t, err)

	nonce := make([]byte, 16)
	_, err = rand.Read(nonce)
	require.NoError(t, err)

	issuedAt := time.Unix(1_700_001_000, 0)
	msgBytes, err := encodeScriptScopeProofTLV(&scriptScopeProofMessage{
		Type:        proofTypeScriptScope,
		Version:     0,
		ServerID:    serverID,
		Principal:   principal,
		Purpose:     purposeOORRecipientEvents,
		PkScript:    pkScript,
		OwnerPubKey: priv.PubKey().SerializeCompressed(),
		IssuedAt:    uint64(issuedAt.Unix()),
		ExpiresAt:   uint64(issuedAt.Add(10 * time.Minute).Unix()),
		Nonce:       nonce,
	})
	require.NoError(t, err)

	msgHash := chainhash.TaggedHash(ProofTagHash, msgBytes)
	sig, err := schnorr.Sign(priv, msgHash[:])
	require.NoError(t, err)

	return pkScript, &arkrpc.TaprootSchnorrProof{
		Message: msgBytes,
		Sig64:   sig.Serialize(),
	}, issuedAt.Add(time.Minute)
}

// TestListOORRecipientEventsByScriptRetrySafe verifies that retried read
// transactions replace, rather than append to, response state from earlier
// attempts.
func TestListOORRecipientEventsByScriptRetrySafe(t *testing.T) {
	t.Parallel()

	const (
		serverID  = "server-retry"
		principal = "client-retry"
	)

	pkScript, proof, now := buildRecipientEventsScopeProof(
		t, serverID, principal,
	)

	sessionID := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}
	store := &retryRecipientEventStore{
		rows: []OORRecipientEventWithSession{
			{
				RecipientPkScript: pkScript,
				EventID:           7,
				SessionID:         sessionID,
				OutputIndex:       1,
				Value:             12_345,
				ArkPsbt: []byte{
					0xaa,
					0xbb,
				},
			},
		},
		checkpoints: make(map[string][]OORSessionCheckpoint),
	}

	svc := NewService(serverID, store)
	svc.now = func() time.Time {
		return now
	}

	ctx := ContextWithPrincipal(t.Context(), Principal{
		MailboxID: principal,
	})
	proofReq := &arkrpc.
		ListOORRecipientEventsByScriptRequest_TaprootSchnorr{
		TaprootSchnorr: proof,
	}
	resp, err := svc.ListOORRecipientEventsByScript(
		ctx, &arkrpc.ListOORRecipientEventsByScriptRequest{
			PkScript: pkScript,
			Proof:    proofReq,
			Limit:    10,
		},
	)
	require.NoError(t, err)
	require.Equal(t, 2, store.attempts)
	require.Len(t, resp.Events, 1)
	require.EqualValues(t, 7, resp.NextCursor)
	require.EqualValues(t, 7, resp.Events[0].EventId)
	require.Equal(t, sessionID, resp.Events[0].SessionId)
}
