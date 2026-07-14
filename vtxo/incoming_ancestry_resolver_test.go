package vtxo

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/internal/indexerlimits"
	lib_tree "github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// TestResolveIncomingAncestryMatchesPaginatedOutpoint verifies the
// resolver scans across pages and only returns ancestry from the exact
// target outpoint.
func TestResolveIncomingAncestryMatchesPaginatedOutpoint(t *testing.T) {
	t.Parallel()

	target := wire.OutPoint{
		Hash:  testAncestryTxID(1),
		Index: 7,
	}
	query := newScriptedAncestryQuery(
		testIncomingAncestryResponse(
			[]byte("next"), &arkrpc.VTXO{
				Outpoint: &arkrpc.OutPoint{
					Txid: testAncestryTxIDBytes(2),
					Vout: target.Index,
				},
			},
		),
		testIncomingAncestryResponse(
			nil, testIncomingAncestryVTXO(target, 42),
		),
	)

	extras, err := ResolveIncomingAncestry(
		t.Context(), query.Query, target, testAncestryPkScript, 128, 2,
	)
	require.NoError(t, err)
	require.Equal(t, int32(42), extras.CreatedHeight)
	require.Len(t, extras.Ancestry, 1)
	require.Equal(t, 2, query.Calls())
}

// TestResolveIncomingAncestryRejectsOversizedNextCursor verifies the
// cursor guard stays with the extracted resolver.
func TestResolveIncomingAncestryRejectsOversizedNextCursor(t *testing.T) {
	t.Parallel()

	target := wire.OutPoint{
		Hash:  testAncestryTxID(1),
		Index: 7,
	}
	query := newScriptedAncestryQuery(
		testIncomingAncestryResponse(
			make(
				[]byte,
				indexerlimits.MaxVTXOsByScriptsCursorBytes+1,
			),
			&arkrpc.VTXO{
				Outpoint: &arkrpc.OutPoint{
					Txid: testAncestryTxIDBytes(2),
					Vout: target.Index,
				},
			},
		),
	)

	_, err := ResolveIncomingAncestry(
		t.Context(), query.Query, target, testAncestryPkScript, 128, 2,
	)
	require.ErrorContains(t, err, "indexer next cursor: vtxo cursor length")
	require.Equal(t, 1, query.Calls())
}

// TestResolveIncomingAncestryCapsScannedVTXOs verifies a matching VTXO
// beyond the caller's scan budget is not accepted.
func TestResolveIncomingAncestryCapsScannedVTXOs(t *testing.T) {
	t.Parallel()

	target := wire.OutPoint{
		Hash:  testAncestryTxID(1),
		Index: 7,
	}
	query := newScriptedAncestryQuery(
		testIncomingAncestryResponse(
			nil, &arkrpc.VTXO{
				Outpoint: &arkrpc.OutPoint{
					Txid: testAncestryTxIDBytes(2),
					Vout: target.Index,
				},
			},
			testIncomingAncestryVTXO(target, 42),
		),
	)

	_, err := ResolveIncomingAncestry(
		t.Context(), query.Query, target, testAncestryPkScript, 128, 1,
	)
	require.ErrorContains(t, err, "ancestry index scan exceeded limit 1")
	require.Equal(t, 1, query.Calls())
}

type scriptedQuery struct {
	responses []*arkrpc.ListVTXOsByScriptsResponse
	calls     int
}

// newScriptedAncestryQuery returns a query that replays responses.
func newScriptedAncestryQuery(
	responses ...*arkrpc.ListVTXOsByScriptsResponse) *scriptedQuery {

	return &scriptedQuery{
		responses: responses,
	}
}

// Query returns the next scripted response.
func (q *scriptedQuery) Query(_ context.Context, _ []byte, _ []byte, _ uint32) (
	*arkrpc.ListVTXOsByScriptsResponse, error) {

	if q.calls >= len(q.responses) {
		return &arkrpc.ListVTXOsByScriptsResponse{}, nil
	}

	resp := q.responses[q.calls]
	q.calls++

	return resp, nil
}

// Calls returns the number of query invocations.
func (q *scriptedQuery) Calls() int {
	return q.calls
}

var testAncestryPkScript = []byte{0x51, 0x20}

// testIncomingAncestryResponse returns an indexer response carrying the
// flat VTXO slice the resolver iterates over.
func testIncomingAncestryResponse(nextCursor []byte,
	vtxos ...*arkrpc.VTXO) *arkrpc.ListVTXOsByScriptsResponse {

	return &arkrpc.ListVTXOsByScriptsResponse{
		Vtxos:      vtxos,
		NextCursor: nextCursor,
	}
}

// testIncomingAncestryVTXO returns a valid indexer VTXO fixture. The
// fixture stamps PkScript on the row so the response mirrors what the
// real indexer returns; the resolver does not filter by pkScript itself
// (the indexer query is already script-scoped) but populating it keeps
// the fixture honest if a future caller does want to filter.
func testIncomingAncestryVTXO(outpoint wire.OutPoint,
	createdHeight int32) *arkrpc.VTXO {

	return &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: outpoint.Hash[:],
			Vout: outpoint.Index,
		},
		PkScript:      testAncestryPkScript,
		CreatedHeight: createdHeight,
		AncestryPaths: []*arkrpc.AncestryPath{
			testIncomingAncestryPath(testAncestryTxID(3)),
		},
	}
}

// testIncomingAncestryPath returns a minimally valid ancestry path.
func testIncomingAncestryPath(
	commitmentTxID chainhash.Hash) *arkrpc.AncestryPath {

	tree := &lib_tree.Tree{
		Root: &lib_tree.Node{},
		BatchOutpoint: wire.OutPoint{
			Hash: commitmentTxID,
		},
	}

	path, err := arkrpc.AncestryPathFromTree(
		tree, commitmentTxID, []uint32{0},
	)
	if err != nil {
		panic(fmt.Sprintf("build test ancestry path: %v", err))
	}

	return path
}

// testAncestryTxID returns a deterministic 32-byte txid.
func testAncestryTxID(prefix byte) chainhash.Hash {
	var txid chainhash.Hash
	txid[0] = prefix

	return txid
}

// testAncestryTxIDBytes returns a deterministic txid byte slice.
func testAncestryTxIDBytes(prefix byte) []byte {
	txid := testAncestryTxID(prefix)

	return txid[:]
}
