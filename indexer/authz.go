package indexer

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
)

// ScriptAuthorizationRequest describes a script-scoped access check.
type ScriptAuthorizationRequest struct {
	// PrincipalMailboxID is the mailbox principal making the request.
	PrincipalMailboxID string

	// Purpose is the canonical purpose string for the requested operation.
	Purpose string

	// PkScripts are the scripts the principal requests access to.
	PkScripts [][]byte

	// Now is the time used for expiry checks.
	Now time.Time
}

// ScriptAuthorizer gates script-scoped indexer access per request principal.
type ScriptAuthorizer interface {
	// AuthorizeScripts validates req and returns nil when access
	// is granted.
	AuthorizeScripts(ctx context.Context,
		req ScriptAuthorizationRequest) error
}

// AllowAllScriptAuthorizer grants all requests.
type AllowAllScriptAuthorizer struct{}

// NewAllowAllScriptAuthorizer creates an authorizer that always allows.
func NewAllowAllScriptAuthorizer() ScriptAuthorizer {
	return &AllowAllScriptAuthorizer{}
}

// AuthorizeScripts always grants access.
func (a *AllowAllScriptAuthorizer) AuthorizeScripts(_ context.Context,
	_ ScriptAuthorizationRequest) error {

	return nil
}

// RegistrationScriptAuthorizer enforces that principals can only access scripts
// they currently registered through the indexer registration API.
type RegistrationScriptAuthorizer struct {
	store *db.Store
}

// NewRegistrationScriptAuthorizer creates a registration-backed authorizer.
func NewRegistrationScriptAuthorizer(
	store *db.Store) *RegistrationScriptAuthorizer {

	return &RegistrationScriptAuthorizer{
		store: store,
	}
}

// AuthorizeScripts ensures all requested scripts are currently registered for
// req.PrincipalMailboxID.
func (a *RegistrationScriptAuthorizer) AuthorizeScripts(ctx context.Context,
	req ScriptAuthorizationRequest) error {

	if a == nil || a.store == nil {
		return fmt.Errorf("registration authorizer missing db store")
	}
	if req.PrincipalMailboxID == "" {
		return fmt.Errorf("missing principal mailbox id")
	}
	if len(req.PkScripts) == 0 {
		return nil
	}

	rows, err := a.store.Queries.ListActiveIndexerReceiveScriptsByPrincipal(
		ctx,
		sqlc.ListActiveIndexerReceiveScriptsByPrincipalParams{
			PrincipalMailboxID: req.PrincipalMailboxID,
			ExpiresAtUnixS:     req.Now.Unix(),
		},
	)
	if err != nil {
		return fmt.Errorf("list active scripts: %w", err)
	}

	authorized := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		scriptHex := hex.EncodeToString(row.PkScript)
		authorized[scriptHex] = struct{}{}
	}

	for _, script := range req.PkScripts {
		scriptHex := hex.EncodeToString(script)
		if _, ok := authorized[scriptHex]; ok {
			continue
		}

		return fmt.Errorf("script not registered for principal")
	}

	return nil
}

var _ ScriptAuthorizer = (*AllowAllScriptAuthorizer)(nil)
var _ ScriptAuthorizer = (*RegistrationScriptAuthorizer)(nil)
