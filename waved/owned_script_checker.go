package waved

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// ownedScriptCheckerAdapter implements round.OwnedScriptChecker by
// looking up pkScripts in the owned_receive_scripts persistence store.
type ownedScriptCheckerAdapter struct {
	store *db.OORArtifactPersistenceStore
}

var _ round.OwnedScriptChecker = (*ownedScriptCheckerAdapter)(nil)

// IsOwnedScript returns whether the pkScript is registered as an owned
// receive script in the OOR artifact store. Returns an error for real
// store failures; a not-found result returns false with no error.
func (a *ownedScriptCheckerAdapter) IsOwnedScript(ctx context.Context,
	pkScript []byte) fn.Result[bool] {

	if a.store == nil {
		return fn.Ok(false)
	}

	// Use a context that survives cancellation so the DB lookup
	// completes even if the caller's context is being torn down
	// (e.g., during round confirmation in a shutting-down FSM).
	lookupCtx := context.WithoutCancel(ctx)

	_, err := a.store.LookupOwnedReceiveScript(lookupCtx, pkScript)
	if err != nil {
		// Not-found means the script isn't ours.
		if errors.Is(err, sql.ErrNoRows) {
			return fn.Ok(false)
		}

		return fn.Err[bool](
			fmt.Errorf("lookup owned receive script: %w", err),
		)
	}

	return fn.Ok(true)
}

// ownedScriptRegistrarAdapter implements round.OwnedScriptRegistrar by
// persisting pkScripts in the owned_receive_scripts table.
type ownedScriptRegistrarAdapter struct {
	store       *db.OORArtifactPersistenceStore
	operatorKey *btcec.PublicKey
	exitDelay   uint32
}

var _ round.OwnedScriptRegistrar = (*ownedScriptRegistrarAdapter)(nil)

// RegisterOwnedScript persists the pkScript as a locally owned receive
// script in the OOR artifact store.
func (a *ownedScriptRegistrarAdapter) RegisterOwnedScript(ctx context.Context,
	pkScript []byte, ownerKey keychain.KeyDescriptor) error {

	if a.store == nil {
		return fmt.Errorf("store is nil")
	}

	return a.store.UpsertOwnedReceiveScript(
		ctx, db.OwnedReceiveScriptRecord{
			PkScript:       pkScript,
			ClientKey:      ownerKey,
			OperatorPubKey: a.operatorKey,
			ExitDelay:      int64(a.exitDelay),
			Source:         db.OwnedReceiveScriptSourceWallet,
			CreatedAt:      time.Now(),
			LastUsedAt:     fn.None[time.Time](),
		},
	)
}
