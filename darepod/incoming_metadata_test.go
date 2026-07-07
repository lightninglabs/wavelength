package darepod

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	btclog "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/internal/indexerlimits"
	lib_tree "github.com/lightninglabs/darepo-client/lib/tree"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestResolveIncomingMetadataFromIndexerRejectsOversizedNextCursor verifies a
// remote indexer cannot force the metadata resolver to copy and replay an
// attacker-sized opaque cursor.
func TestResolveIncomingMetadataFromIndexerRejectsOversizedNextCursor(
	t *testing.T) {

	t.Parallel()

	idx, rpcClient, recipient, sessionID := newTestIncomingMetadataIndexer(
		t,
		testIncomingMetadataResponse(
			make(
				[]byte,
				indexerlimits.MaxVTXOsByScriptsCursorBytes+1,
			),
			&arkrpc.VTXO{
				Outpoint: &arkrpc.OutPoint{
					Txid: testTxIDBytes(2),
					Vout: 0,
				},
			},
		),
	)

	_, err := ResolveIncomingMetadataFromIndexerWithLimits(
		t.Context(), idx, sessionID, recipient, oor.ReceiveLimits{
			MaxVTXOMatches: 2,
		},
	)
	require.ErrorContains(t, err, "indexer next cursor: vtxo cursor length")
	require.Equal(t, 1, rpcClient.sendCount())
}

// TestResolveIncomingMetadataFromIndexerCapsScannedVTXOs verifies the direct
// metadata resolver bounds pagination work even when a remote indexer keeps
// returning non-matching VTXOs.
func TestResolveIncomingMetadataFromIndexerCapsScannedVTXOs(t *testing.T) {
	t.Parallel()

	idx, rpcClient, recipient, sessionID := newTestIncomingMetadataIndexer(
		t,
		testIncomingMetadataResponse(
			nil, &arkrpc.VTXO{
				Outpoint: &arkrpc.OutPoint{
					Txid: testTxIDBytes(2),
					Vout: 0,
				},
			}, &arkrpc.VTXO{
				Outpoint: &arkrpc.OutPoint{
					Txid: testTxIDBytes(3),
					Vout: 0,
				},
			},
		),
	)

	_, err := ResolveIncomingMetadataFromIndexerWithLimits(
		t.Context(), idx, sessionID, recipient, oor.ReceiveLimits{
			MaxVTXOMatches: 1,
		},
	)
	require.ErrorContains(
		t, err, "incoming metadata index scan exceeds limit 1",
	)
	require.Equal(t, 1, rpcClient.sendCount())
}

// TestResolveIncomingMetadataFromIndexerRejectsZeroTreeDepth verifies a
// malicious indexer cannot strand an OOR-received VTXO by returning a
// matching VTXO whose AncestryPath claims tree_depth = 0. Without
// validation at this trust boundary the descriptor would persist as
// "valid" and only fail later during unilateral exit or under-report
// the worst-case CSV window for expiry monitoring — both fund-availability
// issues. This is the regression test for darepo-client#370.
func TestResolveIncomingMetadataFromIndexerRejectsZeroTreeDepth(t *testing.T) {
	t.Parallel()

	sessionID := oor.SessionID(testTxID(1))
	candidate := testIncomingVTXO(sessionID, recipientIndex)

	// Override the otherwise-valid ancestry with a zero tree_depth
	// claim. The reconstructed tree still has depth 1, so this models
	// an indexer that returns a usable tree path but under-reports the
	// scalar that drives expiry/refresh decisions.
	candidate.AncestryPaths[0].TreeDepth = 0

	idx, _, recipient, _ := newTestIncomingMetadataIndexer(
		t, testIncomingMetadataResponse(nil, candidate),
	)

	_, err := ResolveIncomingMetadataFromIndexerWithLimits(
		t.Context(), idx, sessionID, recipient, oor.ReceiveLimits{
			MaxVTXOMatches: 1,
		},
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "tree_depth")
}

// TestResolveIncomingMetadataFromIndexerRejectsDepthMismatch verifies the
// receive boundary rejects an AncestryPath whose claimed tree_depth
// disagrees with the depth of the supplied tree_path. A low-but-non-zero
// claim is the more dangerous variant of darepo-client#370 because it
// passes the obvious "zero" check downstream but still under-reports
// MaxTreeDepth for expiry monitoring.
func TestResolveIncomingMetadataFromIndexerRejectsDepthMismatch(t *testing.T) {
	t.Parallel()

	sessionID := oor.SessionID(testTxID(1))
	candidate := testIncomingVTXO(sessionID, recipientIndex)

	// Lie about the depth: reconstructed tree depth is 1; claim 7.
	candidate.AncestryPaths[0].TreeDepth = 7

	idx, _, recipient, _ := newTestIncomingMetadataIndexer(
		t, testIncomingMetadataResponse(nil, candidate),
	)

	_, err := ResolveIncomingMetadataFromIndexerWithLimits(
		t.Context(), idx, sessionID, recipient, oor.ReceiveLimits{
			MaxVTXOMatches: 1,
		},
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "does not match reconstructed")
}

// TestResolveIncomingMetadataFromIndexerRejectsOverCapTreeDepth verifies
// the receive boundary rejects an AncestryPath whose claimed tree_depth
// exceeds the receive-path walk cap (arkrpc.MaxAncestryTreeWalkDepth).
// Such a claim cannot be honoured by the same client that persisted it,
// so accepting it would silently strand the VTXO.
func TestResolveIncomingMetadataFromIndexerRejectsOverCapTreeDepth(
	t *testing.T) {

	t.Parallel()

	sessionID := oor.SessionID(testTxID(1))
	candidate := testIncomingVTXO(sessionID, recipientIndex)

	candidate.AncestryPaths[0].TreeDepth = arkrpc.MaxAncestryTreeWalkDepth +
		1

	idx, _, recipient, _ := newTestIncomingMetadataIndexer(
		t, testIncomingMetadataResponse(nil, candidate),
	)

	_, err := ResolveIncomingMetadataFromIndexerWithLimits(
		t.Context(), idx, sessionID, recipient, oor.ReceiveLimits{
			MaxVTXOMatches: 1,
		},
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "exceeds max")
}

// TestResolveIncomingMetadataFromIndexerAllowsMatchAtScanLimit verifies the
// scan cap is enforced per candidate, so a valid match at the final permitted
// item is still accepted.
func TestResolveIncomingMetadataFromIndexerAllowsMatchAtScanLimit(
	t *testing.T) {

	t.Parallel()

	sessionID := oor.SessionID(testTxID(1))
	idx, rpcClient, recipient, _ := newTestIncomingMetadataIndexer(
		t,
		testIncomingMetadataResponse(
			nil, &arkrpc.VTXO{
				Outpoint: &arkrpc.OutPoint{
					Txid: testTxIDBytes(2),
					Vout: 0,
				},
			},
			testIncomingVTXO(sessionID, recipientIndex),
		),
	)

	metadata, err := ResolveIncomingMetadataFromIndexerWithLimits(
		t.Context(), idx, sessionID, recipient, oor.ReceiveLimits{
			MaxVTXOMatches: 2,
		},
	)
	require.NoError(t, err)
	require.Equal(t, testTxID(10).String(), metadata.RoundID)
	require.Equal(t, 1, rpcClient.sendCount())
}

// scriptedIndexerRPC returns scripted ListVTXOsByScripts responses.
type scriptedIndexerRPC struct {
	mu        sync.Mutex
	responses []*arkrpc.ListVTXOsByScriptsResponse
	sent      []*arkrpc.ListVTXOsByScriptsRequest
	awaits    int
}

type scriptedMetadataResponse struct {
	nextCursor []byte
	vtxos      []*arkrpc.VTXO
}

// testIncomingMetadataResponse returns a script-keyed response fixture.
func testIncomingMetadataResponse(nextCursor []byte,
	vtxos ...*arkrpc.VTXO) scriptedMetadataResponse {

	return scriptedMetadataResponse{
		nextCursor: nextCursor,
		vtxos:      vtxos,
	}
}

// listVTXOsByScriptResponse returns the proto response for a generated
// script. The pkScript argument is unused: the indexer scopes responses
// to the queried scripts, so callers iterate the flat slice and match by
// outpoint / session metadata. The signature is retained so per-script
// fixtures stay readable at call sites.
func listVTXOsByScriptResponse(_ []byte,
	resp scriptedMetadataResponse) *arkrpc.ListVTXOsByScriptsResponse {

	return &arkrpc.ListVTXOsByScriptsResponse{
		Vtxos:      resp.vtxos,
		NextCursor: resp.nextCursor,
	}
}

// SendRPC records the request and returns a deterministic correlation id.
func (r *scriptedIndexerRPC) SendRPC(_ context.Context,
	_ mailboxrpc.ServiceMethod, req proto.Message,
	_ mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	r.mu.Lock()
	defer r.mu.Unlock()

	listReq, ok := req.(*arkrpc.ListVTXOsByScriptsRequest)
	if !ok {
		return mailboxrpc.SendResult{}, fmt.Errorf("unexpected "+
			"request type %T", req)
	}

	cloned := proto.Clone(listReq)
	clonedReq, ok := cloned.(*arkrpc.ListVTXOsByScriptsRequest)
	if !ok {
		return mailboxrpc.SendResult{}, fmt.Errorf("unexpected cloned "+
			"request type %T", listReq)
	}
	r.sent = append(r.sent, clonedReq)

	return mailboxrpc.SendResult{
		CorrelationID: "corr-1",
	}, nil
}

// AwaitRPC copies the next scripted response into resp.
func (r *scriptedIndexerRPC) AwaitRPC(_ context.Context, _ string,
	resp proto.Message) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.awaits >= len(r.responses) {
		return nil
	}

	dst, ok := resp.(*arkrpc.ListVTXOsByScriptsResponse)
	if !ok {
		return fmt.Errorf("unexpected response type %T", resp)
	}

	proto.Merge(dst, r.responses[r.awaits])
	r.awaits++

	return nil
}

// sendCount returns the number of recorded SendRPC calls.
func (r *scriptedIndexerRPC) sendCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.sent)
}

// newTestIncomingMetadataIndexer returns a proof-capable indexer client and a
// recipient using a valid taproot script.
func newTestIncomingMetadataIndexer(t *testing.T,
	responses ...scriptedMetadataResponse) (*indexer.Client,
	*scriptedIndexerRPC, oor.ArkRecipientOutput, oor.SessionID) {

	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := append(
		[]byte{0x51, 0x20},
		privKey.PubKey().SerializeCompressed()[1:]...,
	)

	protoResponses := make(
		[]*arkrpc.ListVTXOsByScriptsResponse, 0, len(responses),
	)
	for _, resp := range responses {
		protoResponses = append(
			protoResponses,
			listVTXOsByScriptResponse(pkScript, resp),
		)
	}

	rpcClient := &scriptedIndexerRPC{
		responses: protoResponses,
	}
	idx := indexer.New(
		rpcClient, &indexer.PrivKeySchnorrSigner{
			Key: privKey,
		}, "test-server", "client:test",
		fn.None[btclog.Logger](),
	)

	return idx, rpcClient, oor.ArkRecipientOutput{
		OutputIndex: recipientIndex,
		PkScript:    pkScript,
	}, oor.SessionID(testTxID(1))
}

const recipientIndex uint32 = 1

// testIncomingVTXO returns a minimally valid matching incoming VTXO.
func testIncomingVTXO(sessionID oor.SessionID,
	outputIndex uint32) *arkrpc.VTXO {

	txid := chainhash.Hash(sessionID)

	return &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: txid[:],
			Vout: outputIndex,
		},
		RoundId:           testTxID(10).String(),
		CommitmentTxid:    testTxIDBytes(11),
		BatchOutputIndex:  0,
		BatchExpiryHeight: 1000,
		OperatorPubkey:    testPubKeyBytes(3),
		ChainDepth:        1,
		AncestryPaths: []*arkrpc.AncestryPath{
			testAncestryPath(testTxID(13)),
		},
	}
}

// testAncestryPath returns a minimally valid AncestryPath whose
// reconstructed tree depth matches the wire-format tree_depth. Receive-time
// validation (arkrpc.ValidateAncestryPathDepth) rejects zero or
// inconsistent depths, so test fixtures must keep these in sync.
func testAncestryPath(commitmentTxID chainhash.Hash) *arkrpc.AncestryPath {
	t := &lib_tree.Tree{
		Root: &lib_tree.Node{},
		BatchOutpoint: wire.OutPoint{
			Hash: commitmentTxID,
		},
	}

	p, err := arkrpc.AncestryPathFromTree(t, commitmentTxID, []uint32{0})
	if err != nil {
		panic(fmt.Sprintf("build test ancestry path: %v", err))
	}

	return p
}

// testPubKeyBytes returns a deterministic compressed public key.
func testPubKeyBytes(prefix byte) []byte {
	privKey, _ := btcec.PrivKeyFromBytes(testTxIDBytes(prefix))

	return privKey.PubKey().SerializeCompressed()
}

// testTxID returns a deterministic 32-byte txid.
func testTxID(prefix byte) chainhash.Hash {
	var txid chainhash.Hash
	txid[0] = prefix

	return txid
}

// testTxIDBytes returns a deterministic txid byte slice.
func testTxIDBytes(prefix byte) []byte {
	txid := testTxID(prefix)

	return txid[:]
}
