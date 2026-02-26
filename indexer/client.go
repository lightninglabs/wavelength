package indexer

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightningnetwork/lnd/tlv"
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

	// registrationNonceBytes is the number of random bytes used for
	// nonces.
	registrationNonceBytes = 32

	// offlineReceiveProofTTL is the lifetime used for proof-gated script
	// queries. It should be short to limit replay windows.
	offlineReceiveProofTTL = 10 * time.Minute

	// scriptScopeMessageType is the canonical proof "type" string used
	// for script-scoped queries.
	scriptScopeMessageType = "script_scope"

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

	// purposeOORRecipientEvents is the canonical purpose string
	// expected by the server when verifying script-scope proofs
	// for ListOORRecipientEventsByScript.
	purposeOORRecipientEvents = "list_oor_recipient_events_by_script"

	// purposeRegisterReceiveScript is the canonical purpose string
	// expected by the server when verifying proofs for
	// RegisterReceiveScript.
	purposeRegisterReceiveScript = "register_receive_script"

	// purposeUnregisterReceiveScript is the canonical purpose
	// string expected by the server when verifying proofs for
	// UnregisterReceiveScript.
	purposeUnregisterReceiveScript = "unregister_receive_script"
)

// TLV type constants for proof message fields. These MUST match the
// server-side constants defined in the indexer verifier. Types are
// allocated sequentially and the canonical TLV stream must be encoded
// with records sorted by type in ascending order.
const (
	// proofTLVTypeType identifies the proof type string
	// (e.g. "receive_script_registration" or "script_scope").
	proofTLVTypeType tlv.Type = 1

	// proofTLVTypeVersion identifies the proof schema version.
	proofTLVTypeVersion tlv.Type = 2

	// proofTLVTypeServerID identifies the operator's server identifier.
	proofTLVTypeServerID tlv.Type = 3

	// proofTLVTypePrincipal identifies the mailbox principal
	// (client ID).
	proofTLVTypePrincipal tlv.Type = 4

	// proofTLVTypePkScript identifies the raw pkScript bytes.
	proofTLVTypePkScript tlv.Type = 5

	// proofTLVTypeIssuedAt identifies the proof issuance unix
	// timestamp.
	proofTLVTypeIssuedAt tlv.Type = 6

	// proofTLVTypeExpiresAt identifies the proof expiration unix
	// timestamp.
	proofTLVTypeExpiresAt tlv.Type = 7

	// proofTLVTypeNonce identifies the unique nonce bytes.
	proofTLVTypeNonce tlv.Type = 8

	// proofTLVTypePurpose identifies the purpose string for
	// script-scope proofs.
	proofTLVTypePurpose tlv.Type = 9
)

// New creates an Indexer client wrapper.
func New(rpc mailboxrpc.RPCClient, serverID string,
	principal string) *Client {

	return &Client{
		rpc:       arkrpc.NewIndexerServiceMailboxClient(rpc),
		serverID:  serverID,
		principal: principal,
	}
}

// encodeProofTLV encodes a proof message to its canonical TLV byte
// representation. The msgType distinguishes registration proofs from
// scope proofs, and purpose binds the proof to a specific RPC method
// to prevent cross-purpose replay.
func encodeProofTLV(msgType, serverID, principal, purpose string,
	pkScript, nonce []byte,
	issuedAt, expiresAt uint64) ([]byte, error) {

	proofTypeBytes := []byte(msgType)
	version := uint32(registrationMessageVersion)
	serverIDBytes := []byte(serverID)
	principalBytes := []byte(principal)
	purposeBytes := []byte(purpose)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofTypeBytes,
			tlv.SizeVarBytes(&proofTypeBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript,
			tlv.SizeVarBytes(&pkScript),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce,
			tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePurpose, &purposeBytes,
			tlv.SizeVarBytes(&purposeBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build TLV stream: %w", err)
	}

	var buf bytes.Buffer
	if err := tlvStream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode TLV proof: %w", err)
	}

	return buf.Bytes(), nil
}

// proofTag returns the BIP-340 tagged hash domain separator for indexer
// proof signatures. A fresh slice is returned each call to prevent
// accidental mutation. This must match the server-side ProofTagHash
// constant in the indexer package.
func proofTag() []byte {
	return []byte("darepo/indexer/v1")
}

// schnorrSigOverMessage returns a 64-byte schnorr signature over a
// BIP-340 tagged hash of the message. The tag provides domain separation
// so indexer proof signatures cannot be replayed in other protocols.
func schnorrSigOverMessage(message []byte,
	priv *btcec.PrivateKey) ([]byte, error) {

	msgHash := chainhash.TaggedHash(proofTag(), message)
	sig, err := schnorr.Sign(priv, msgHash[:])
	if err != nil {
		return nil, err
	}

	return sig.Serialize(), nil
}

// validateTaprootPkScript returns an error if pkScript is not a valid
// pay-to-taproot output script. This catches obvious misuse before
// signing a proof that the server would reject anyway.
func validateTaprootPkScript(pkScript []byte) error {
	if len(pkScript) == 0 {
		return fmt.Errorf("empty pkScript")
	}

	if !txscript.IsPayToTaproot(pkScript) {
		return fmt.Errorf(
			"pkScript is not P2TR (len=%d, version=%d)",
			len(pkScript), pkScript[0],
		)
	}

	return nil
}

// TaprootScriptScope selects a pkScript and its corresponding signing
// key.
//
// The signing key must be the P2TR output key for pkScript.
type TaprootScriptScope struct {
	PkScript  []byte
	SigningKey *btcec.PrivateKey
}

// newTaprootScope builds a ScriptScope proto with a TLV-encoded
// script-scope proof signed under signingKey.
func (c *Client) newTaprootScope(
	pkScript []byte,
	signingKey *btcec.PrivateKey,
	purpose string,
) (*arkrpc.ScriptScope, error) {

	if signingKey == nil {
		return nil, fmt.Errorf("missing signing key")
	}
	if err := validateTaprootPkScript(pkScript); err != nil {
		return nil, err
	}
	if purpose == "" {
		return nil, fmt.Errorf("missing purpose")
	}

	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)

	nonce, err := randomNonce(registrationNonceBytes)
	if err != nil {
		return nil, err
	}

	msgBytes, err := encodeProofTLV(
		scriptScopeMessageType,
		c.serverID, c.principal, purpose, pkScript, nonce,
		uint64(now.Unix()), uint64(expiresAt.Unix()),
	)
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(msgBytes, signingKey)
	if err != nil {
		return nil, err
	}

	return &arkrpc.ScriptScope{
		PkScript: pkScript,
		Proof: &arkrpc.ScriptScope_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: msgBytes,
				Sig64:   sig64,
			},
		},
	}, nil
}

// ListVTXOsByScriptsTaproot performs a proof-gated VTXO query for one
// or more pkScripts.
func (c *Client) ListVTXOsByScriptsTaproot(ctx context.Context,
	scopes []TaprootScriptScope, afterCursor uint64, limit uint32,
	statusFilter []arkrpc.VTXOStatus,
	opts ...mailboxrpc.RPCOptions) (
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

// GetSubtreeByScriptsTaproot performs a proof-gated subtree query for
// one or more pkScripts.
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

// ListVTXOEventsByScriptsTaproot performs a proof-gated, monotonic VTXO
// event feed query for one or more pkScripts.
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

// RegisterReceiveScriptTaproot registers a single P2TR receive script
// using a schnorr signature proof under the output key.
func (c *Client) RegisterReceiveScriptTaproot(ctx context.Context,
	pkScript []byte, signingKey *btcec.PrivateKey,
	expiresAt time.Time, label string,
	opts ...mailboxrpc.RPCOptions) (
	*arkrpc.RegisterReceiveScriptResponse, error) {

	if signingKey == nil {
		return nil, fmt.Errorf("missing signing key")
	}
	if err := validateTaprootPkScript(pkScript); err != nil {
		return nil, err
	}

	now := time.Now()

	nonce, err := randomNonce(registrationNonceBytes)
	if err != nil {
		return nil, err
	}

	msgBytes, err := encodeProofTLV(
		registrationMessageType,
		c.serverID, c.principal,
		purposeRegisterReceiveScript, pkScript, nonce,
		uint64(now.Unix()), uint64(expiresAt.Unix()),
	)
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(msgBytes, signingKey)
	if err != nil {
		return nil, err
	}

	req := &arkrpc.RegisterReceiveScriptRequest{
		PkScript:       pkScript,
		ExpiresAtUnixS: uint64(expiresAt.Unix()),
		Label:          label,
		Proof: &arkrpc.RegisterReceiveScriptRequest_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: msgBytes,
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

// ListOORRecipientEventsByScriptTaproot performs a proof-gated
// script-keyed recipient event query. This enables "offline receive
// without registration" while preventing third-party enumeration
// (proof-of-control required).
func (c *Client) ListOORRecipientEventsByScriptTaproot(
	ctx context.Context, pkScript []byte,
	signingKey *btcec.PrivateKey, afterEventID uint64,
	limit uint32,
	opts ...mailboxrpc.RPCOptions) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	if signingKey == nil {
		return nil, fmt.Errorf("missing signing key")
	}
	if err := validateTaprootPkScript(pkScript); err != nil {
		return nil, err
	}

	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)

	nonce, err := randomNonce(registrationNonceBytes)
	if err != nil {
		return nil, err
	}

	msgBytes, err := encodeProofTLV(
		scriptScopeMessageType,
		c.serverID, c.principal,
		purposeOORRecipientEvents, pkScript,
		nonce, uint64(now.Unix()),
		uint64(expiresAt.Unix()),
	)
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(msgBytes, signingKey)
	if err != nil {
		return nil, err
	}

	proof := &arkrpc.TaprootSchnorrProof{
		Message: msgBytes,
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
