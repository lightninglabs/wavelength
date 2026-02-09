package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
)

// Client is a small convenience wrapper around the generated
// arkrpc.IndexerServiceMailboxClient that helps construct canonical receive
// script registration proofs.
//
// This package intentionally does not dictate how the mailbox principal is
// minted or stored; callers provide the canonical principal identifier string
// used in the signed message (typically the mailbox ID, e.g. "client:<id>").
type Client struct {
	rpc       *arkrpc.IndexerServiceMailboxClient
	serverID  string
	principal string
}

const (
	// registrationMessageType is the canonical proof "type" string.
	registrationMessageType = "receive_script_registration"

	// registrationMessageVersion is the current message version.
	registrationMessageVersion = 0

	// registrationNonceBytes is the number of random bytes used for nonces.
	registrationNonceBytes = 32

	// offlineReceiveProofTTL is the lifetime used for proof-gated script
	// queries. It should be short to limit replay windows.
	offlineReceiveProofTTL = 10 * time.Minute

	// scriptScopeMessageType is the canonical proof "type" string used for
	// script-scoped queries (option B).
	scriptScopeMessageType = "script_scope"

	// scriptScopeMessageVersion is the current script-scope message
	// version.
	scriptScopeMessageVersion = 0

	// purposeListVTXOsByScripts is the canonical purpose string expected
	// by the server when verifying script-scope proofs for
	// ListVTXOsByScripts.
	purposeListVTXOsByScripts = "list_vtxos_by_scripts"

	// purposeGetSubtreeByScripts is the canonical purpose string expected
	// by the server when verifying script-scope proofs for
	// GetSubtreeByScripts.
	purposeGetSubtreeByScripts = "get_subtree_by_scripts"

	// purposeListVTXOEventsByScripts is the canonical purpose string
	// expected by the server when verifying script-scope proofs for
	// ListVTXOEventsByScripts.
	purposeListVTXOEventsByScripts = "list_vtxo_events_by_scripts"

	// purposeListOORRecipientEventsByScript is the canonical purpose
	// string expected by the server when verifying script-scope proofs
	// for ListOORRecipientEventsByScript.
	purposeListOORRecipientEventsByScript = "list_oor_recipient_events_by_script"
)

// New creates an Indexer client wrapper.
func New(rpc mailboxrpc.RPCClient, serverID string, principal string) *Client {
	return &Client{
		rpc:       arkrpc.NewIndexerServiceMailboxClient(rpc),
		serverID:  serverID,
		principal: principal,
	}
}

// ReceiveScriptRegistrationMessage is the canonical JSON message signed to
// bind a receive script to a mailbox principal.
type ReceiveScriptRegistrationMessage struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	ServerID    string `json:"server_id"`
	Principal   string `json:"principal"`
	PkScriptHex string `json:"pk_script_hex"`
	IssuedAt    int64  `json:"issued_at"`
	ExpiresAt   int64  `json:"expires_at"`
	Nonce       string `json:"nonce"`
}

// ScriptScopeMessage is the canonical JSON message signed to prove control of
// a script for a specific purpose (RPC method / feed).
type ScriptScopeMessage struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	ServerID    string `json:"server_id"`
	Principal   string `json:"principal"`
	Purpose     string `json:"purpose"`
	PkScriptHex string `json:"pk_script_hex"`
	IssuedAt    int64  `json:"issued_at"`
	ExpiresAt   int64  `json:"expires_at"`
	Nonce       string `json:"nonce"`
}

// MarshalCanonical returns the canonical JSON encoding for m.
func (m *ReceiveScriptRegistrationMessage) MarshalCanonical() (string, error) {
	if m.Type == "" {
		m.Type = registrationMessageType
	}

	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}

	return string(b), nil
}

// MarshalCanonical returns the canonical JSON encoding for m.
func (m *ScriptScopeMessage) MarshalCanonical() (string, error) {
	if m.Type == "" {
		m.Type = scriptScopeMessageType
	}

	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}

	return string(b), nil
}

// schnorrSigOverMessage returns a 64-byte schnorr signature over
// sha256(message).
func schnorrSigOverMessage(message string,
	priv *btcec.PrivateKey) ([]byte, error) {

	msgHash := sha256.Sum256([]byte(message))
	sig, err := schnorr.Sign(priv, msgHash[:])
	if err != nil {
		return nil, err
	}

	return sig.Serialize(), nil
}

// TaprootScriptScope selects a pkScript and its corresponding signing key.
//
// The signing key must be the P2TR output key for pkScript.
type TaprootScriptScope struct {
	PkScript   []byte
	SigningKey *btcec.PrivateKey
}

func (c *Client) newTaprootScope(
	pkScript []byte,
	signingKey *btcec.PrivateKey,
	purpose string,
) (*arkrpc.ScriptScope, error) {

	if signingKey == nil {
		return nil, fmt.Errorf("missing signing key")
	}
	if len(pkScript) == 0 {
		return nil, fmt.Errorf("missing pkScript")
	}
	if purpose == "" {
		return nil, fmt.Errorf("missing purpose")
	}

	expiresAt := time.Now().Add(offlineReceiveProofTTL)

	msg := &ScriptScopeMessage{
		Type:        scriptScopeMessageType,
		Version:     scriptScopeMessageVersion,
		ServerID:    c.serverID,
		Principal:   c.principal,
		Purpose:     purpose,
		PkScriptHex: hex.EncodeToString(pkScript),
		IssuedAt:    time.Now().Unix(),
		ExpiresAt:   expiresAt.Unix(),
	}
	nonce, err := randomNonceHex(registrationNonceBytes)
	if err != nil {
		return nil, err
	}
	msg.Nonce = nonce

	msgStr, err := msg.MarshalCanonical()
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(msgStr, signingKey)
	if err != nil {
		return nil, err
	}

	return &arkrpc.ScriptScope{
		PkScript: pkScript,
		Proof: &arkrpc.ScriptScope_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: msgStr,
				Sig64:   sig64,
			},
		},
	}, nil
}

// ListVTXOsByScriptsTaproot performs a proof-gated VTXO query for one or more
// pkScripts.
func (c *Client) ListVTXOsByScriptsTaproot(ctx context.Context,
	scopes []TaprootScriptScope, afterCursor uint64, limit uint32,
	statusFilter []arkrpc.VTXOStatus, opts ...mailboxrpc.RPCOptions) (
	*arkrpc.ListVTXOsByScriptsResponse, error) {

	var scriptScopes []*arkrpc.ScriptScope
	for _, scope := range scopes {
		ss, err := c.newTaprootScope(
			scope.PkScript, scope.SigningKey,
			purposeListVTXOsByScripts,
		)
		if err != nil {
			return nil, err
		}

		scriptScopes = append(scriptScopes, ss)
	}

	req := &arkrpc.ListVTXOsByScriptsRequest{
		Scripts:      scriptScopes,
		StatusFilter: statusFilter,
		Cursor:       afterCursor,
		Limit:        limit,
	}

	var opt mailboxrpc.RPCOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	return c.rpc.ListVTXOsByScripts(ctx, req, opt)
}

// GetSubtreeByScriptsTaproot performs a proof-gated subtree query for one or
// more pkScripts.
func (c *Client) GetSubtreeByScriptsTaproot(ctx context.Context,
	scopes []TaprootScriptScope, includeInternalNodes bool,
	opts ...mailboxrpc.RPCOptions) (
	*arkrpc.GetSubtreeByScriptsResponse, error) {

	var scriptScopes []*arkrpc.ScriptScope
	for _, scope := range scopes {
		ss, err := c.newTaprootScope(
			scope.PkScript, scope.SigningKey,
			purposeGetSubtreeByScripts,
		)
		if err != nil {
			return nil, err
		}

		scriptScopes = append(scriptScopes, ss)
	}

	req := &arkrpc.GetSubtreeByScriptsRequest{
		Scripts:              scriptScopes,
		IncludeInternalNodes: includeInternalNodes,
	}

	var opt mailboxrpc.RPCOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	return c.rpc.GetSubtreeByScripts(ctx, req, opt)
}

// ListVTXOEventsByScriptsTaproot performs a proof-gated, monotonic VTXO event
// feed query for one or more pkScripts.
func (c *Client) ListVTXOEventsByScriptsTaproot(ctx context.Context,
	scopes []TaprootScriptScope, afterEventID uint64, limit uint32,
	opts ...mailboxrpc.RPCOptions) (
	*arkrpc.ListVTXOEventsByScriptsResponse, error) {

	var scriptScopes []*arkrpc.ScriptScope
	for _, scope := range scopes {
		ss, err := c.newTaprootScope(
			scope.PkScript, scope.SigningKey,
			purposeListVTXOEventsByScripts,
		)
		if err != nil {
			return nil, err
		}

		scriptScopes = append(scriptScopes, ss)
	}

	req := &arkrpc.ListVTXOEventsByScriptsRequest{
		Scripts:      scriptScopes,
		AfterEventId: afterEventID,
		Limit:        limit,
	}

	var opt mailboxrpc.RPCOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	return c.rpc.ListVTXOEventsByScripts(ctx, req, opt)
}

// RegisterReceiveScriptTaproot registers a single P2TR receive script using a
// schnorr signature proof under the output key.
func (c *Client) RegisterReceiveScriptTaproot(ctx context.Context,
	pkScript []byte, signingKey *btcec.PrivateKey,
	expiresAt time.Time, label string,
	opts ...mailboxrpc.RPCOptions) (
	*arkrpc.RegisterReceiveScriptResponse, error) {

	if signingKey == nil {
		return nil, fmt.Errorf("missing signing key")
	}

	msg := &ReceiveScriptRegistrationMessage{
		Type:        registrationMessageType,
		Version:     registrationMessageVersion,
		ServerID:    c.serverID,
		Principal:   c.principal,
		PkScriptHex: hex.EncodeToString(pkScript),
		IssuedAt:    time.Now().Unix(),
		ExpiresAt:   expiresAt.Unix(),
	}
	nonce, err := randomNonceHex(registrationNonceBytes)
	if err != nil {
		return nil, err
	}
	msg.Nonce = nonce

	msgStr, err := msg.MarshalCanonical()
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(msgStr, signingKey)
	if err != nil {
		return nil, err
	}

	req := &arkrpc.RegisterReceiveScriptRequest{
		PkScript:       pkScript,
		ExpiresAtUnixS: uint64(expiresAt.Unix()),
		Label:          label,
		Proof: &arkrpc.RegisterReceiveScriptRequest_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: msgStr,
				Sig64:   sig64,
			},
		},
	}

	var opt mailboxrpc.RPCOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	return c.rpc.RegisterReceiveScript(ctx, req, opt)
}

// UnregisterReceiveScript removes a receive script registration.
func (c *Client) UnregisterReceiveScript(ctx context.Context,
	pkScript []byte, opts ...mailboxrpc.RPCOptions) (
	*arkrpc.UnregisterReceiveScriptResponse, error) {

	req := &arkrpc.UnregisterReceiveScriptRequest{
		PkScript: pkScript,
	}

	var opt mailboxrpc.RPCOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	return c.rpc.UnregisterReceiveScript(ctx, req, opt)
}

// ListOORRecipientEventsByScriptTaproot performs a proof-gated script-keyed
// recipient event query. This enables "offline receive without registration"
// while preventing third-party enumeration (proof-of-control required).
func (c *Client) ListOORRecipientEventsByScriptTaproot(ctx context.Context,
	pkScript []byte, signingKey *btcec.PrivateKey,
	afterEventID uint64, limit uint32,
	opts ...mailboxrpc.RPCOptions) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	if signingKey == nil {
		return nil, fmt.Errorf("missing signing key")
	}

	expiresAt := time.Now().Add(offlineReceiveProofTTL)

	msg := &ScriptScopeMessage{
		Type:        scriptScopeMessageType,
		Version:     scriptScopeMessageVersion,
		ServerID:    c.serverID,
		Principal:   c.principal,
		Purpose:     purposeListOORRecipientEventsByScript,
		PkScriptHex: hex.EncodeToString(pkScript),
		IssuedAt:    time.Now().Unix(),
		ExpiresAt:   expiresAt.Unix(),
	}
	nonce, err := randomNonceHex(registrationNonceBytes)
	if err != nil {
		return nil, err
	}
	msg.Nonce = nonce

	msgStr, err := msg.MarshalCanonical()
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(msgStr, signingKey)
	if err != nil {
		return nil, err
	}

	proof := &arkrpc.TaprootSchnorrProof{
		Message: msgStr,
		Sig64:   sig64,
	}

	proofOneof :=
		&arkrpc.ListOORRecipientEventsByScriptRequest_TaprootSchnorr{
			TaprootSchnorr: proof,
		}

	req := &arkrpc.ListOORRecipientEventsByScriptRequest{
		PkScript:     pkScript,
		AfterEventId: afterEventID,
		Limit:        limit,
		Proof:        proofOneof,
	}

	var opt mailboxrpc.RPCOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	return c.rpc.ListOORRecipientEventsByScript(ctx, req, opt)
}
