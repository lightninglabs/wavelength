package indexer

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/rounds"
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

// retryVTXOStore simulates retried VTXO list read transactions.
type retryVTXOStore struct {
	Store

	attempts      int
	inReadTx      bool
	rows          []VTXORow
	rowsByAttempt [][]VTXORow
	round         RoundRow
	tree          *tree.Tree
}

// ExecReadTx invokes fn twice to model a successful retry after the first
// transaction attempt already built an in-memory response.
func (s *retryVTXOStore) ExecReadTx(ctx context.Context,
	fn func(Store) error) error {

	runAttempt := func() error {
		s.attempts++
		s.inReadTx = true
		defer func() {
			s.inReadTx = false
		}()

		return fn(s)
	}

	if err := runAttempt(); err != nil {
		return err
	}

	return runAttempt()
}

// ListVTXOsByPkScripts returns no authorization rows so the service uses the
// default allow-all registration policy for this focused retry test.
func (s *retryVTXOStore) ListVTXOsByPkScripts(context.Context, [][]byte) (
	[]VTXORow, error) {

	if s.inReadTx {
		return s.currentRows(), nil
	}

	return nil, nil
}

// ListVTXOsByPkScriptsAfter returns the fixed test VTXO page.
func (s *retryVTXOStore) ListVTXOsByPkScriptsAfter(context.Context, [][]byte,
	[]string, *wire.OutPoint, int32) ([]VTXORow, error) {

	return s.currentRows(), nil
}

// ListRoundsByIDs returns the round metadata needed for lineage resolution.
func (s *retryVTXOStore) ListRoundsByIDs(_ context.Context,
	_ []rounds.RoundID) ([]RoundRow, error) {

	return []RoundRow{s.round}, nil
}

// LoadVTXOTree returns the fixed test tree needed for lineage resolution.
func (s *retryVTXOStore) LoadVTXOTree(context.Context, rounds.RoundID, int) (
	*tree.Tree, error) {

	return s.tree, nil
}

// GetOORSpendingSessionTxidByInput reports no OOR spender in this test.
func (s *retryVTXOStore) GetOORSpendingSessionTxidByInput(context.Context,
	wire.OutPoint) ([]byte, error) {

	return nil, ErrNotFound
}

// currentRows returns the row snapshot for the current retry attempt.
func (s *retryVTXOStore) currentRows() []VTXORow {
	if len(s.rowsByAttempt) == 0 {
		return s.rows
	}

	attemptIndex := s.attempts - 1
	if attemptIndex < 0 {
		attemptIndex = 0
	}
	if attemptIndex >= len(s.rowsByAttempt) {
		attemptIndex = len(s.rowsByAttempt) - 1
	}

	return s.rowsByAttempt[attemptIndex]
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

// buildRetryVTXOTree builds a small round-backed tree and matching VTXO rows
// for ListVTXOsByScripts retry tests.
func buildRetryVTXOTree(t *testing.T, pkScript []byte) ([]VTXORow, RoundRow,
	*tree.Tree) {

	t.Helper()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	leaves := make([]tree.LeafDescriptor, 2)
	for i := range leaves {
		cosignerPriv, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		leaves[i] = tree.LeafDescriptor{
			PkScript:    append([]byte(nil), pkScript...),
			Amount:      btcutil.Amount(1000 * (i + 1)),
			CoSignerKey: cosignerPriv.PubKey(),
		}
	}

	rootOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("retry-root")),
		Index: 0,
	}
	rootOutput := &wire.TxOut{Value: 3000}
	vtxoTree, err := tree.NewTree(
		rootOutpoint, rootOutput, leaves, operatorPriv.PubKey(),
		make([]byte, 32), 2,
	)
	require.NoError(t, err)

	var leafOutpoints []wire.OutPoint
	for leaf := range vtxoTree.Root.LeavesIter() {
		outpoint, err := leaf.GetNonAnchorOutpoint()
		require.NoError(t, err)

		leafOutpoints = append(leafOutpoints, *outpoint)
	}
	require.Len(t, leafOutpoints, 2)

	var roundID rounds.RoundID
	roundID[0] = 0x41

	batchIndex := int32(0)
	confirmationHeight := int32(144)
	round := RoundRow{
		RoundID:            roundID,
		CommitmentTxid:     chainhash.HashH([]byte("retry-commit")),
		ConfirmationHeight: &confirmationHeight,
		CsvDelay:           12,
		OperatorPubKey: operatorPriv.PubKey().
			SerializeCompressed(),
	}

	rows := []VTXORow{
		{
			Outpoint:         leafOutpoints[0],
			BatchOutputIndex: &batchIndex,
			Amount:           int64(leaves[0].Amount),
			PkScript:         append([]byte(nil), pkScript...),
			Status:           storeVTXOStatusLive,
			RoundID:          &roundID,
		},
		{
			Outpoint:         leafOutpoints[1],
			BatchOutputIndex: &batchIndex,
			Amount:           int64(leaves[1].Amount),
			PkScript:         append([]byte(nil), pkScript...),
			Status:           storeVTXOStatusLive,
			RoundID:          &roundID,
		},
	}

	return rows, round, vtxoTree
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

// TestListVTXOsByScriptsRetrySafe verifies that retried read transactions do
// not append VTXOs or preserve cursors from earlier attempts.
func TestListVTXOsByScriptsRetrySafe(t *testing.T) {
	t.Parallel()

	pkScript, proof, _, now := buildOwnerKeyVTXOScopeProof(
		t, purposeListVTXOsByScripts,
	)
	proofOneof, ok := proof.(*arkrpc.ScriptScope_TaprootSchnorr)
	require.True(t, ok)

	rows, round, vtxoTree := buildRetryVTXOTree(t, pkScript)

	store := &retryVTXOStore{
		rows:  rows,
		round: round,
		tree:  vtxoTree,
	}
	svc := NewService(testProofServerID, store)
	svc.now = func() time.Time {
		return now
	}

	ctx := ContextWithPrincipal(t.Context(), Principal{
		MailboxID: testProofPrincipal,
	})
	resp, err := svc.ListVTXOsByScripts(
		ctx, &arkrpc.ListVTXOsByScriptsRequest{
			Scripts: []*arkrpc.ScriptScope{
				{
					PkScript: pkScript,
					Proof:    proofOneof,
				},
			},
			Limit: 1,
		},
	)
	require.NoError(t, err)
	require.Equal(t, 2, store.attempts)
	require.Len(t, resp.Vtxos, 1)
	require.Equal(t, encodeVTXOCursor(rows[0].Outpoint), resp.NextCursor)
	require.Equal(t, rows[0].Outpoint.Hash[:], resp.Vtxos[0].Outpoint.Txid)
	require.Equal(t, rows[0].Outpoint.Index, resp.Vtxos[0].Outpoint.Vout)
}

// TestGetSubtreeByScriptsRetrySafe verifies that retried read transactions do
// not leak tree nodes, edges, or leaves from earlier attempts.
func TestGetSubtreeByScriptsRetrySafe(t *testing.T) {
	t.Parallel()

	pkScript, proof, _, now := buildOwnerKeyVTXOScopeProof(
		t, purposeGetSubtreeByScripts,
	)
	proofOneof, ok := proof.(*arkrpc.ScriptScope_TaprootSchnorr)
	require.True(t, ok)

	rows, round, vtxoTree := buildRetryVTXOTree(t, pkScript)

	store := &retryVTXOStore{
		rowsByAttempt: [][]VTXORow{
			{
				rows[0],
				rows[1],
			},
			{
				rows[1],
			},
		},
		round: round,
		tree:  vtxoTree,
	}
	svc := NewService(testProofServerID, store)
	svc.now = func() time.Time {
		return now
	}

	ctx := ContextWithPrincipal(t.Context(), Principal{
		MailboxID: testProofPrincipal,
	})
	resp, err := svc.GetSubtreeByScripts(
		ctx, &arkrpc.GetSubtreeByScriptsRequest{
			Scripts: []*arkrpc.ScriptScope{
				{
					PkScript: pkScript,
					Proof:    proofOneof,
				},
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, 2, store.attempts)
	require.Len(t, resp.Vtxos, 1)
	require.Equal(t, rows[1].Outpoint.Hash[:], resp.Vtxos[0].Outpoint.Txid)
	require.Equal(t, rows[1].Outpoint.Index, resp.Vtxos[0].Outpoint.Vout)
	require.Len(t, resp.Edges, 1)
}
