package systest

import (
	"context"
	"fmt"

	clientindexer "github.com/lightninglabs/darepo-client/indexer"
	"google.golang.org/protobuf/proto"
)

// testDurableUnaryBuilder adapts the shared client indexer proof builder to
// the generic serverconn durable-unary builder interface used in systest.
//
//nolint:unused
type testDurableUnaryBuilder struct {
	indexerClient **clientindexer.Client
}

// BuildListOORRecipientEventsByScriptRequest builds the proof-gated
// request body for one taproot recipient-event query.
//
//nolint:unused
func (b *testDurableUnaryBuilder) BuildListOORRecipientEventsByScriptRequest(
	ctx context.Context, pkScript []byte, afterEventID uint64, limit uint32,
) (proto.Message, error) {

	if b == nil || b.indexerClient == nil || *b.indexerClient == nil {
		return nil, fmt.Errorf("indexer client not initialized")
	}

	return (*b.indexerClient).
		BuildListOORRecipientEventsByScriptTaprootRequest(
			ctx, pkScript, afterEventID, limit,
		)
}

// BuildListVTXOsByScriptsRequest builds the proof-gated request body
// for one or more taproot VTXO scope queries.
//
//nolint:unused
func (b *testDurableUnaryBuilder) BuildListVTXOsByScriptsRequest(
	ctx context.Context, pkScripts [][]byte, afterCursor []byte,
	limit uint32,
) (proto.Message, error) {

	if b == nil || b.indexerClient == nil || *b.indexerClient == nil {
		return nil, fmt.Errorf("indexer client not initialized")
	}

	scopes := make([]clientindexer.TaprootScriptScope, 0, len(pkScripts))
	for i := range pkScripts {
		scopes = append(scopes, clientindexer.TaprootScriptScope{
			PkScript: append([]byte(nil), pkScripts[i]...),
		})
	}

	return (*b.indexerClient).BuildListVTXOsByScriptsTaprootRequest(
		ctx, scopes, afterCursor, limit, nil,
	)
}
