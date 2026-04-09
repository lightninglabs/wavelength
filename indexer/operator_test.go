package indexer_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/indexer"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	testServerID        = "srv-test"
	testSenderMailboxID = "svc:indexer"
	testClientMailboxID = "client-1"

	testProtocolVersion uint32 = 1

	testIndexerServiceName = "arkrpc.IndexerService"
	testP2TRPush32Opcode   = 0x20

	testEventValue uint64 = 1234
)

// recordingEdge captures envelopes sent through the mailbox edge for test
// verification.
type recordingEdge struct {
	mu   sync.Mutex
	sent []*mailboxpb.SendRequest
}

// Send records the request and returns a successful status.
func (r *recordingEdge) Send(_ context.Context,
	in *mailboxpb.SendRequest,
	_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	r.mu.Lock()
	r.sent = append(r.sent, in)
	r.mu.Unlock()

	return &mailboxpb.SendResponse{
		Status: &mailboxpb.Status{Ok: true},
	}, nil
}

// Pull is a no-op for the recording edge.
func (r *recordingEdge) Pull(_ context.Context,
	_ *mailboxpb.PullRequest,
	_ ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	return &mailboxpb.PullResponse{
		Status: &mailboxpb.Status{Ok: true},
	}, nil
}

// AckUpTo is a no-op for the recording edge.
func (r *recordingEdge) AckUpTo(_ context.Context,
	_ *mailboxpb.AckUpToRequest,
	_ ...grpc.CallOption) (*mailboxpb.AckUpToResponse, error) {

	return &mailboxpb.AckUpToResponse{
		Status: &mailboxpb.Status{Ok: true},
	}, nil
}

// sentEnvelopes returns a copy of the recorded send requests.
func (r *recordingEdge) sentEnvelopes() []*mailboxpb.SendRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*mailboxpb.SendRequest, len(r.sent))
	copy(out, r.sent)

	return out
}

// Compile-time interface check.
var _ mailboxpb.MailboxServiceClient = (*recordingEdge)(nil)

// newTestStore creates a test DB and returns a *db.Store for the indexer.
func newTestStore(t *testing.T) *db.Store {
	t.Helper()

	sqlDB := db.NewTestDB(t)

	return db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
}

// newTestP2TRScript generates a random P2TR pk_script from a fresh key.
func newTestP2TRScript(
	t *testing.T) ([]byte, *btcec.PrivateKey) {

	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	xOnly := schnorr.SerializePubKey(priv.PubKey())
	pkScript := []byte{txscript.OP_1, testP2TRPush32Opcode}
	pkScript = append(pkScript, xOnly...)

	return pkScript, priv
}

// buildTestRegistrationProof constructs a valid TaprootSchnorrProof for a
// receive-script registration request using TLV-encoded proof messages.
func buildTestRegistrationProof(t *testing.T, priv *btcec.PrivateKey,
	pkScript []byte, serverID string,
	principal string) *arkrpc.TaprootSchnorrProof {

	t.Helper()

	nonce := make([]byte, 16)
	_, err := rand.Read(nonce)
	require.NoError(t, err)

	now := time.Now()
	msgBytes, err := indexer.BuildReceiveScriptProofMessage(
		serverID, principal, pkScript, nonce,
		now, now.Add(10*time.Minute),
	)
	require.NoError(t, err)

	msgHash := chainhash.TaggedHash(
		indexer.ProofTagHash, msgBytes,
	)
	sig, err := schnorr.Sign(priv, msgHash[:])
	require.NoError(t, err)

	return &arkrpc.TaprootSchnorrProof{
		Message: msgBytes,
		Sig64:   sig.Serialize(),
	}
}

func buildTestVTXORegistrationProof(t *testing.T, ownerPriv *btcec.PrivateKey,
	operatorKey *btcec.PublicKey, exitDelay uint32, serverID,
	principal string) ([]byte, *arkrpc.TaprootSchnorrProof) {

	t.Helper()

	tapKey, err := scripts.VTXOTapKey(
		ownerPriv.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	nonce := make([]byte, 16)
	_, err = rand.Read(nonce)
	require.NoError(t, err)

	now := time.Now()
	msgBytes, err := indexer.BuildReceiveScriptProofMessageWithOwner(
		serverID, principal, "", pkScript,
		ownerPriv.PubKey().SerializeCompressed(), nonce, now,
		now.Add(10*time.Minute),
	)
	require.NoError(t, err)

	msgHash := chainhash.TaggedHash(
		indexer.ProofTagHash, msgBytes,
	)
	sig, err := schnorr.Sign(ownerPriv, msgHash[:])
	require.NoError(t, err)

	return pkScript, &arkrpc.TaprootSchnorrProof{
		Message: msgBytes,
		Sig64:   sig.Serialize(),
	}
}

// buildRequestEnvelope constructs a KIND_REQUEST envelope for dispatching.
func buildRequestEnvelope(t *testing.T, method string,
	req proto.Message) *mailboxpb.Envelope {

	t.Helper()

	bodyAny, err := anypb.New(req)
	require.NoError(t, err)

	return &mailboxpb.Envelope{
		ProtocolVersion: testProtocolVersion,
		MsgId:           "test-msg-1",
		Sender:          testClientMailboxID,
		Recipient:       testSenderMailboxID,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Body:            bodyAny,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
			Service:       testIndexerServiceName,
			Method:        method,
			CorrelationId: "corr-1",
		},
	}
}

// TestOperatorDispatchers verifies that the operator returns the expected
// set of dispatchers for all 7 IndexerService RPC methods.
func TestOperatorDispatchers(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	edge := &recordingEdge{}
	bridge := clientconn.NewClientsConnBridge()
	defer bridge.Stop()

	sqlcStore := indexer.NewSQLCStore(store.Queries)
	svc := indexer.NewService(testServerID, sqlcStore)
	op, err := indexer.NewOperator(indexer.OperatorConfig{
		Edge:            edge,
		SenderMailboxID: testSenderMailboxID,
		Bridge:          bridge,
	}, svc)
	require.NoError(t, err)

	dm := op.Dispatchers()
	require.Len(t, dm, 7)

	expectedMethods := []string{
		"RegisterReceiveScript",
		"ListMyReceiveScripts",
		"UnregisterReceiveScript",
		"ListOORRecipientEventsByScript",
		"ListVTXOsByScripts",
		"GetSubtreeByScripts",
		"ListVTXOEventsByScripts",
	}

	for _, method := range expectedMethods {
		key := mailboxrpc.ServiceMethod{
			Service: testIndexerServiceName,
			Method:  method,
		}

		_, ok := dm[key]
		require.True(t, ok, "missing dispatcher for %s", method)
	}
}

// TestOperatorDispatcherRegisterAndList tests the register and list
// receive scripts flow through the dispatcher envelope interface.
func TestOperatorDispatcherRegisterAndList(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	edge := &recordingEdge{}
	bridge := clientconn.NewClientsConnBridge()
	defer bridge.Stop()

	sqlcStore := indexer.NewSQLCStore(store.Queries)
	svc := indexer.NewService(testServerID, sqlcStore)
	svc.SetScriptAuthorizer(
		indexer.NewRegistrationScriptAuthorizer(sqlcStore),
	)

	op, err := indexer.NewOperator(indexer.OperatorConfig{
		Edge:            edge,
		SenderMailboxID: testSenderMailboxID,
		Bridge:          bridge,
	}, svc)
	require.NoError(t, err)

	dm := op.Dispatchers()
	ctx := t.Context()

	pkScript, priv := newTestP2TRScript(t)

	// Build a registration proof. The RegisterReceiveScript handler
	// expects a taproot schnorr proof that the caller controls the
	// script's internal key. The proof message is a canonical JSON
	// string over which the schnorr signature is computed.
	proof := buildTestRegistrationProof(
		t, priv, pkScript, testServerID, testClientMailboxID,
	)

	regReq := &arkrpc.RegisterReceiveScriptRequest{
		PkScript: pkScript,
		Proof: &arkrpc.RegisterReceiveScriptRequest_TaprootSchnorr{
			TaprootSchnorr: proof,
		},
	}

	// Dispatch the register request.
	regKey := mailboxrpc.ServiceMethod{
		Service: testIndexerServiceName,
		Method:  "RegisterReceiveScript",
	}
	regEnv := buildRequestEnvelope(t, "RegisterReceiveScript", regReq)
	err = dm[regKey](ctx, regEnv)
	require.NoError(t, err)

	// Verify a response was sent.
	responses := edge.sentEnvelopes()
	require.Len(t, responses, 1)
	respEnv := responses[0].Envelope
	require.Equal(t, testSenderMailboxID, respEnv.Sender)
	require.Equal(t, testClientMailboxID, respEnv.Recipient)
	require.Equal(t,
		mailboxpb.RpcMeta_KIND_RESPONSE,
		respEnv.Rpc.Kind,
	)
	require.Equal(t, "corr-1", respEnv.Rpc.CorrelationId)
	require.NotNil(t, respEnv.Body)

	// Unmarshal and verify the registration response.
	var regResp arkrpc.RegisterReceiveScriptResponse
	err = respEnv.Body.UnmarshalTo(&regResp)
	require.NoError(t, err)

	// Now dispatch a list request to verify the script was persisted.
	listReq := &arkrpc.ListMyReceiveScriptsRequest{}
	listKey := mailboxrpc.ServiceMethod{
		Service: testIndexerServiceName,
		Method:  "ListMyReceiveScripts",
	}
	listEnv := buildRequestEnvelope(t, "ListMyReceiveScripts", listReq)
	listEnv.MsgId = "test-msg-2"
	listEnv.Rpc.CorrelationId = "corr-2"

	err = dm[listKey](ctx, listEnv)
	require.NoError(t, err)

	// Verify the list response.
	responses = edge.sentEnvelopes()
	require.Len(t, responses, 2)
	listRespEnv := responses[1].Envelope
	require.NotNil(t, listRespEnv.Body)

	var listResp arkrpc.ListMyReceiveScriptsResponse
	err = listRespEnv.Body.UnmarshalTo(&listResp)
	require.NoError(t, err)
	require.Len(t, listResp.Scripts, 1)
	require.Equal(t, pkScript, listResp.Scripts[0].PkScript)
}

// TestOperatorDispatcherRegisterVTXOReceiveScriptPersistsMetadata verifies
// standardized Ark VTXO receive-script registrations persist the collaborative
// descriptor metadata needed for later OOR materialization.
func TestOperatorDispatcherRegisterVTXOReceiveScriptPersistsMetadata(
	t *testing.T) {

	t.Parallel()

	store := newTestStore(t)
	edge := &recordingEdge{}
	bridge := clientconn.NewClientsConnBridge()
	defer bridge.Stop()

	sqlcStore := indexer.NewSQLCStore(store.Queries)
	svc := indexer.NewService(testServerID, sqlcStore)
	svc.SetScriptAuthorizer(
		indexer.NewRegistrationScriptAuthorizer(sqlcStore),
	)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay = uint32(144)
	svc.SetVTXOProofPolicy(operatorPriv.PubKey(), exitDelay)

	op, err := indexer.NewOperator(indexer.OperatorConfig{
		Edge:            edge,
		SenderMailboxID: testSenderMailboxID,
		Bridge:          bridge,
	}, svc)
	require.NoError(t, err)

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript, proof := buildTestVTXORegistrationProof(
		t, ownerPriv, operatorPriv.PubKey(), exitDelay,
		testServerID, testClientMailboxID,
	)

	regReq := &arkrpc.RegisterReceiveScriptRequest{
		PkScript: pkScript,
		Proof: &arkrpc.RegisterReceiveScriptRequest_TaprootSchnorr{
			TaprootSchnorr: proof,
		},
	}

	regKey := mailboxrpc.ServiceMethod{
		Service: testIndexerServiceName,
		Method:  "RegisterReceiveScript",
	}
	regEnv := buildRequestEnvelope(t, "RegisterReceiveScript", regReq)
	err = op.Dispatchers()[regKey](t.Context(), regEnv)
	require.NoError(t, err)

	scriptsByPrincipal, err :=
		sqlcStore.ListActiveReceiveScriptsByPrincipal(
			t.Context(), testClientMailboxID,
			time.Now().Add(time.Minute),
		)
	require.NoError(t, err)
	require.Len(t, scriptsByPrincipal, 1)
	require.Equal(t, pkScript, scriptsByPrincipal[0].PkScript)
	require.Equal(
		t,
		ownerPriv.PubKey().SerializeCompressed(),
		scriptsByPrincipal[0].OwnerPubKey,
	)
	require.Equal(
		t,
		operatorPriv.PubKey().SerializeCompressed(),
		scriptsByPrincipal[0].OperatorPubKey,
	)
	require.Equal(t, exitDelay, scriptsByPrincipal[0].ExitDelay)
}

// TestOperatorPublishOORRecipientEvent verifies that publishing an OOR
// recipient event persists the event in the DB and gracefully handles
// unregistered bridge clients.
func TestOperatorPublishOORRecipientEvent(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	edge := &recordingEdge{}
	bridge := clientconn.NewClientsConnBridge()
	defer bridge.Stop()

	sqlcStore := indexer.NewSQLCStore(store.Queries)
	svc := indexer.NewService(testServerID, sqlcStore)
	svc.SetScriptAuthorizer(
		indexer.NewRegistrationScriptAuthorizer(sqlcStore),
	)

	op, err := indexer.NewOperator(indexer.OperatorConfig{
		Edge:            edge,
		SenderMailboxID: testSenderMailboxID,
		Bridge:          bridge,
	}, svc)
	require.NoError(t, err)

	ctx := t.Context()
	pkScript, _ := newTestP2TRScript(t)

	// Register a receive script so the event has a matching principal.
	err = store.Queries.UpsertIndexerReceiveScript(
		ctx, sqlc.UpsertIndexerReceiveScriptParams{
			PkScript:           pkScript,
			PrincipalMailboxID: testClientMailboxID,
			Label:              "test",
			UpdatedAt:          time.Now().Unix(),
			ExpiresAtUnixS:     time.Now().Add(time.Hour).Unix(),
		},
	)
	require.NoError(t, err)

	// Insert a matching OOR session in the DB so the event insert
	// succeeds (FK constraint on session_db_id).
	nowUnix := time.Now().UnixNano()
	sessionID := []byte{0x01, 0x02}
	_, err = store.Queries.UpsertOORSession(
		ctx,
		sqlc.UpsertOORSessionParams{
			SessionID: sessionID,
			State:     "finalized",
			ArkPsbt:   []byte{0x01},
			CreatedAt: nowUnix,
			UpdatedAt: nowUnix,
			ExpiresAt: nowUnix + int64(time.Hour),
			FinalizedAt: sql.NullInt64{
				Int64: nowUnix,
				Valid: true,
			},
		},
	)
	require.NoError(t, err)

	oorEvent := &arkrpc.OORRecipientEvent{
		RecipientPkScript: pkScript,
		SessionId:         sessionID,
		OutputIndex:       0,
		Value:             testEventValue,
	}

	// Publishing should succeed even though the client is not
	// registered with the bridge (events accumulate in the DB).
	err = op.PublishOORRecipientEvent(ctx, oorEvent)
	require.NoError(t, err)

	// Verify the event was persisted by querying via sqlc directly.
	// Using the service List method would require a script-scope proof,
	// so direct DB verification is more practical for unit tests.
	events, err := store.Queries.ListOORRecipientEventsAfter(
		ctx, sqlc.ListOORRecipientEventsAfterParams{
			RecipientPkScript: pkScript,
			EventID:           0,
			Limit:             10,
		},
	)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, int64(testEventValue), events[0].Value)
}

// TestOperatorPublishVTXOEvent verifies that publishing a VTXO event
// persists the event in the DB.
func TestOperatorPublishVTXOEvent(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	edge := &recordingEdge{}
	bridge := clientconn.NewClientsConnBridge()
	defer bridge.Stop()

	sqlcStore := indexer.NewSQLCStore(store.Queries)
	svc := indexer.NewService(testServerID, sqlcStore)
	svc.SetScriptAuthorizer(
		indexer.NewRegistrationScriptAuthorizer(sqlcStore),
	)

	op, err := indexer.NewOperator(indexer.OperatorConfig{
		Edge:            edge,
		SenderMailboxID: testSenderMailboxID,
		Bridge:          bridge,
	}, svc)
	require.NoError(t, err)

	ctx := t.Context()
	pkScript, _ := newTestP2TRScript(t)

	// Register a receive script so the event has a matching principal.
	err = store.Queries.UpsertIndexerReceiveScript(
		ctx, sqlc.UpsertIndexerReceiveScriptParams{
			PkScript:           pkScript,
			PrincipalMailboxID: testClientMailboxID,
			Label:              "test",
			UpdatedAt:          time.Now().Unix(),
			ExpiresAtUnixS:     time.Now().Add(time.Hour).Unix(),
		},
	)
	require.NoError(t, err)

	outpoint := &arkrpc.OutPoint{
		Txid: []byte{
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
			0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
			0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
		},
		Vout: 0,
	}

	// Publishing should succeed even though the client is not
	// registered with the bridge.
	err = op.PublishVTXOEvent(
		ctx,
		pkScript,
		arkrpc.VTXOEventType_VTXO_EVENT_TYPE_CREATED,
		outpoint,
		arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
		0, "", 0, 0,
		arkrpc.VTXOOrigin_VTXO_ORIGIN_UNSPECIFIED, nil,
	)
	require.NoError(t, err)

	// Verify the event was persisted by querying via the SQLCStore
	// adapter. This exercises the Backend() dispatch and works with
	// both SQLite and PostgreSQL build tags.
	events, err := sqlcStore.ListVTXOEventsAfterByScripts(
		ctx, 0, [][]byte{pkScript}, 10,
	)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "created", events[0].EventType)
	require.Equal(t, "live", events[0].Status)
}

// TestNewOperatorValidation verifies that NewOperator rejects invalid
// configurations.
func TestNewOperatorValidation(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	sqlcStore := indexer.NewSQLCStore(store.Queries)
	svc := indexer.NewService(testServerID, sqlcStore)
	edge := &recordingEdge{}
	bridge := clientconn.NewClientsConnBridge()
	defer bridge.Stop()

	// Missing Edge.
	_, err := indexer.NewOperator(indexer.OperatorConfig{
		SenderMailboxID: testSenderMailboxID,
		Bridge:          bridge,
	}, svc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "edge is required")

	// Missing SenderMailboxID.
	_, err = indexer.NewOperator(indexer.OperatorConfig{
		Edge:   edge,
		Bridge: bridge,
	}, svc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sender mailbox id is required")

	// Missing Bridge.
	_, err = indexer.NewOperator(indexer.OperatorConfig{
		Edge:            edge,
		SenderMailboxID: testSenderMailboxID,
	}, svc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bridge is required")

	// Missing service.
	_, err = indexer.NewOperator(indexer.OperatorConfig{
		Edge:            edge,
		SenderMailboxID: testSenderMailboxID,
		Bridge:          bridge,
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "service is required")

	// Valid config.
	op, err := indexer.NewOperator(indexer.OperatorConfig{
		Edge:            edge,
		SenderMailboxID: testSenderMailboxID,
		Bridge:          bridge,
	}, svc)
	require.NoError(t, err)
	require.NotNil(t, op)
}

// TestOperatorDispatcherNilEnvelope verifies that dispatchers handle nil
// and malformed envelopes gracefully.
func TestOperatorDispatcherNilEnvelope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	edge := &recordingEdge{}
	bridge := clientconn.NewClientsConnBridge()
	defer bridge.Stop()

	sqlcStore := indexer.NewSQLCStore(store.Queries)
	svc := indexer.NewService(testServerID, sqlcStore)
	op, err := indexer.NewOperator(indexer.OperatorConfig{
		Edge:            edge,
		SenderMailboxID: testSenderMailboxID,
		Bridge:          bridge,
	}, svc)
	require.NoError(t, err)

	dm := op.Dispatchers()
	ctx := t.Context()

	regKey := mailboxrpc.ServiceMethod{
		Service: testIndexerServiceName,
		Method:  "RegisterReceiveScript",
	}
	dispatcher := dm[regKey]

	// Nil envelope is silently ignored.
	err = dispatcher(ctx, nil)
	require.NoError(t, err)

	// Envelope with nil Rpc is silently ignored.
	err = dispatcher(ctx, &mailboxpb.Envelope{})
	require.NoError(t, err)

	// Envelope with nil Body returns an error.
	err = dispatcher(ctx, &mailboxpb.Envelope{
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_REQUEST,
			Service: testIndexerServiceName,
			Method:  "RegisterReceiveScript",
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing request body")
}
