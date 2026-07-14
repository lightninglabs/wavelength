package waved

import (
	"context"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/vtxo"
)

// ownedScriptLookupAdapter wraps db.OORArtifactPersistenceStore to
// satisfy the vtxo.OwnedScriptLookup interface. It converts the
// db-specific record type to the vtxo-level OwnedReceiveScript.
type ownedScriptLookupAdapter struct {
	store *db.OORArtifactPersistenceStore
}

// LookupOwnedReceiveScript delegates to the underlying store and
// converts the result to a vtxo.OwnedReceiveScript.
func (a *ownedScriptLookupAdapter) LookupOwnedReceiveScript(ctx context.Context,
	pkScript []byte) (*vtxo.OwnedReceiveScript, error) {

	rec, err := a.store.LookupOwnedReceiveScript(ctx, pkScript)
	if err != nil {
		return nil, err
	}

	return &vtxo.OwnedReceiveScript{
		ClientKey:      rec.ClientKey,
		OperatorPubKey: rec.OperatorPubKey,
		ExitDelay:      rec.ExitDelay,
	}, nil
}

var _ vtxo.OwnedScriptLookup = (*ownedScriptLookupAdapter)(nil)
