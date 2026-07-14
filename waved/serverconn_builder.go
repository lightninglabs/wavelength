package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/indexer"
	"google.golang.org/protobuf/proto"
)

// serverDurableUnaryBuilder adapts the daemon's indexer proof builder to the
// generic serverconn durable-unary builder interface.
type serverDurableUnaryBuilder struct {
	server *Server
}

// BuildListOORRecipientEventsByScriptRequest builds the proof-gated indexer
// request body for one taproot recipient-event query.
func (b *serverDurableUnaryBuilder) BuildListOORRecipientEventsByScriptRequest(
	ctx context.Context, pkScript []byte, afterEventID uint64,
	limit uint32) (proto.Message, error) {

	if b == nil || b.server == nil || b.server.indexer == nil {
		return nil, fmt.Errorf("indexer client not initialized")
	}

	return b.server.indexer.
		BuildListOORRecipientEventsByScriptTaprootRequest(
			ctx, pkScript, afterEventID, limit,
		)
}

// BuildListVTXOsByScriptsRequest builds the proof-gated indexer request body
// for one or more taproot VTXO scope queries.
func (b *serverDurableUnaryBuilder) BuildListVTXOsByScriptsRequest(
	ctx context.Context, pkScripts [][]byte, afterCursor []byte,
	limit uint32) (proto.Message, error) {

	if b == nil || b.server == nil || b.server.indexer == nil {
		return nil, fmt.Errorf("indexer client not initialized")
	}

	scopes := make([]indexer.TaprootScriptScope, 0, len(pkScripts))
	for i := range pkScripts {
		scopes = append(scopes, indexer.TaprootScriptScope{
			PkScript: append([]byte(nil), pkScripts[i]...),
		})
	}

	return b.server.indexer.BuildListVTXOsByScriptsTaprootRequest(
		ctx, scopes, afterCursor, limit, nil,
	)
}
