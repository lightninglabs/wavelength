package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/serverconn"
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

// BuildListVTXOsByScopedScriptsRequest builds the proof-gated indexer request
// body for one or more taproot VTXO scopes whose local proof signing key is
// resolved through an optional uncomposed signerScript.
func (b *serverDurableUnaryBuilder) BuildListVTXOsByScopedScriptsRequest(
	ctx context.Context, pkScripts, signerScripts [][]byte,
	afterCursor []byte, limit uint32) (proto.Message, error) {

	if b == nil || b.server == nil || b.server.indexer == nil {
		return nil, fmt.Errorf("indexer client not initialized")
	}
	if len(signerScripts) > 0 && len(signerScripts) != len(pkScripts) {
		return nil, fmt.Errorf("signer scripts count %d does not "+
			"match pk scripts count %d", len(signerScripts),
			len(pkScripts))
	}

	scopes := make([]indexer.TaprootScriptScope, 0, len(pkScripts))
	for i := range pkScripts {
		scope := indexer.TaprootScriptScope{
			PkScript: append([]byte(nil), pkScripts[i]...),
		}
		if i < len(signerScripts) && len(signerScripts[i]) > 0 {
			scope.SignerScript = append(
				[]byte(nil), signerScripts[i]...,
			)
		}
		scopes = append(scopes, scope)
	}

	return b.server.indexer.BuildListVTXOsByScriptsTaprootRequest(
		ctx, scopes, afterCursor, limit, nil,
	)
}

var _ serverconn.ScopedVTXOQueryBuilder = (*serverDurableUnaryBuilder)(nil)
