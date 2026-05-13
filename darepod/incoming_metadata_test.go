package darepod

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	btclog "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/internal/indexerlimits"
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
		&arkrpc.ListVTXOsByScriptsResponse{
			Vtxos: []*arkrpc.VTXO{{
				Outpoint: &arkrpc.OutPoint{
					Txid: testTxIDBytes(2),
					Vout: 0,
				},
			}},
			NextCursor: make(
				[]byte,
				indexerlimits.MaxVTXOsByScriptsCursorBytes+1,
			),
		},
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
		&arkrpc.ListVTXOsByScriptsResponse{
			Vtxos: []*arkrpc.VTXO{
				{
					Outpoint: &arkrpc.OutPoint{
						Txid: testTxIDBytes(2),
						Vout: 0,
					},
				},
				{
					Outpoint: &arkrpc.OutPoint{
						Txid: testTxIDBytes(3),
						Vout: 0,
					},
				},
			},
		},
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

// TestResolveIncomingMetadataFromIndexerAllowsMatchAtScanLimit verifies the
// scan cap is enforced per candidate, so a valid match at the final permitted
// item is still accepted.
func TestResolveIncomingMetadataFromIndexerAllowsMatchAtScanLimit(
	t *testing.T) {

	t.Parallel()

	sessionID := oor.SessionID(testTxID(1))
	idx, rpcClient, recipient, _ := newTestIncomingMetadataIndexer(
		t,
		&arkrpc.ListVTXOsByScriptsResponse{
			Vtxos: []*arkrpc.VTXO{
				{
					Outpoint: &arkrpc.OutPoint{
						Txid: testTxIDBytes(2),
						Vout: 0,
					},
				},
				testIncomingVTXO(sessionID, recipientIndex),
			},
		},
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
	responses ...*arkrpc.ListVTXOsByScriptsResponse) (*indexer.Client,
	*scriptedIndexerRPC, oor.ArkRecipientOutput, oor.SessionID) {

	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := append(
		[]byte{0x51, 0x20},
		privKey.PubKey().SerializeCompressed()[1:]...,
	)

	rpcClient := &scriptedIndexerRPC{
		responses: responses,
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
		AncestryPaths: []*arkrpc.AncestryPath{{
			CommitmentTxid: testTxIDBytes(13),
			TreeDepth:      0,
		}},
	}
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
