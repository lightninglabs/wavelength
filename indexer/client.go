package indexer

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	btclog "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/internal/indexerlimits"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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
	// rpc is the generated mailbox-RPC client. It is a concrete type
	// rather than an interface because the generated variadic
	// RPCOptions parameter does not lend itself to a clean interface
	// boundary.
	rpc *arkrpc.IndexerServiceMailboxClient

	// signer produces BIP-340 Schnorr signatures for
	// proof-of-control messages. The implementation is
	// responsible for selecting the correct key based on the
	// pkScript passed to each signing call.
	signer SchnorrSigner

	serverID  string
	principal string

	// Log is an optional logger for this client. If None, the client
	// falls back to extracting a logger from context via
	// LoggerFromContext, or uses btclog.Disabled if no logger is
	// found.
	Log fn.Option[btclog.Logger]
}

// logger returns the configured logger, falling back to extracting a logger
// from context. If neither is available, returns btclog.Disabled which safely
// no-ops all log calls.
func (c *Client) logger(ctx context.Context) btclog.Logger {
	return c.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

const (
	// registrationMessageType is the canonical proof "type" string.
	registrationMessageType = "receive_script_registration"

	// registrationMessageVersion is the current message version.
	registrationMessageVersion = 0

	// registrationNonceBytes is the number of random bytes used for
	// nonces.
	registrationNonceBytes = 32

	// offlineReceiveProofTTL is the lifetime used for proof-gated
	// script queries (ListOORRecipientEventsByScript, ListVTXOs,
	// etc.) and for unregister proofs. This TTL is signed into the
	// proof's TLV expiresAt field and limits the window in which a
	// captured proof can be replayed.
	//
	// This is independent of the registration expiry
	// (expiresAt param in RegisterReceiveScriptTaproot), which
	// controls how long the server retains the script binding.
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

	// purposeGetOORSessionByTxid is the canonical purpose string expected
	// by the server when verifying proofs for GetOORSessionByTxid.
	purposeGetOORSessionByTxid = "get_oor_session_by_txid"

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

	// proofTLVTypeOwnerPubKey identifies the compressed owner pubkey used
	// to prove control over supported standardized receive scripts such as
	// VTXO tapscripts.
	proofTLVTypeOwnerPubKey tlv.Type = 10

	// proofTLVTypeSignerPubKey identifies the compressed participant pubkey
	// used to sign script-scope query proofs.
	proofTLVTypeSignerPubKey tlv.Type = 11
)

// New creates an Indexer client wrapper. The signer is used for all
// proof-of-control operations; its SignSchnorr method receives the
// pkScript so it can select the appropriate key. The optional log is
// used for constructor and runtime logging; if unset, the client falls
// back to context-based logging.
func New(rpc mailboxrpc.RPCClient, signer SchnorrSigner, serverID string,
	principal string, log fn.Option[btclog.Logger]) *Client {

	c := &Client{
		rpc:       arkrpc.NewIndexerServiceMailboxClient(rpc),
		signer:    signer,
		serverID:  serverID,
		principal: principal,
		Log:       log,
	}

	c.logger(context.Background()).InfoS(
		context.Background(),
		"Initializing indexer client",
		slog.String("server_id", serverID),
		slog.String("principal", principal),
	)

	return c
}

// WithSigner returns a shallow copy of the client that uses signer for
// proof-of-control operations. This allows callers to reuse the same mailbox
// RPC transport while switching to the wallet key that controls a specific
// receive script.
func (c *Client) WithSigner(signer SchnorrSigner) *Client {
	if c == nil {
		return nil
	}

	clone := *c
	clone.signer = signer

	return &clone
}

// firstOpt returns the first RPCOptions from opts, or the zero value
// if none were provided. At most one option should be passed; any
// additional options beyond the first are silently ignored.
func firstOpt(opts []mailboxrpc.RPCOptions) mailboxrpc.RPCOptions {
	if len(opts) > 0 {
		return opts[0]
	}

	return mailboxrpc.RPCOptions{}
}

// encodeProofTLV encodes a proof message to its canonical TLV byte
// representation. The msgType distinguishes registration proofs from
// scope proofs, and purpose binds the proof to a specific RPC method
// to prevent cross-purpose replay.
func encodeProofTLV(msgType, serverID, principal, purpose string, pkScript,
	nonce []byte, issuedAt, expiresAt uint64) ([]byte, error) {

	return encodeProofTLVWithOwner(
		msgType, serverID, principal, purpose, pkScript, nil, nonce,
		issuedAt, expiresAt,
	)
}

// encodeScriptScopeProofTLV encodes a script-scope query proof message that
// is bound to one explicit participant signer rather than a specific script.
//
// This uses a distinct TLV schema from encodeProofTLV/encodeProofTLVWithOwner:
// it carries signerPubKey instead of pk_script because the prover is
// authorizing queries for all scripts derived from their key, not one
// specific script. The server verifies this variant separately via the
// "script_scope" message type discriminator.
func encodeScriptScopeProofTLV(serverID, principal, purpose string,
	signerPubKey, nonce []byte, issuedAt,
	expiresAt uint64) ([]byte, error) {

	proofTypeBytes := []byte(scriptScopeMessageType)
	version := uint32(registrationMessageVersion)
	serverIDBytes := []byte(serverID)
	principalBytes := []byte(principal)
	purposeBytes := []byte(purpose)
	records := []tlv.Record{
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofTypeBytes,
			tlv.SizeVarBytes(&proofTypeBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce, tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePurpose, &purposeBytes,
			tlv.SizeVarBytes(&purposeBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeSignerPubKey, &signerPubKey,
			tlv.SizeVarBytes(&signerPubKey), tlv.EVarBytes,
			tlv.DVarBytes,
		),
	}

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("build TLV stream: %w", err)
	}

	var buf bytes.Buffer
	if err := tlvStream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode TLV proof: %w", err)
	}

	return buf.Bytes(), nil
}

// encodeProofTLVWithOwner encodes a proof message to its canonical TLV byte
// representation and optionally commits to the script owner pubkey.
func encodeProofTLVWithOwner(msgType, serverID, principal, purpose string,
	pkScript, ownerPubKey, nonce []byte, issuedAt,
	expiresAt uint64) ([]byte, error) {

	proofTypeBytes := []byte(msgType)
	version := uint32(registrationMessageVersion)
	serverIDBytes := []byte(serverID)
	principalBytes := []byte(principal)
	purposeBytes := []byte(purpose)
	records := []tlv.Record{
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofTypeBytes,
			tlv.SizeVarBytes(&proofTypeBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript,
			tlv.SizeVarBytes(&pkScript), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce, tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePurpose, &purposeBytes,
			tlv.SizeVarBytes(&purposeBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
	}
	if len(ownerPubKey) > 0 {
		records = append(
			records,
			tlv.MakeDynamicRecord(
				proofTLVTypeOwnerPubKey, &ownerPubKey,
				tlv.SizeVarBytes(&ownerPubKey), tlv.EVarBytes,
				tlv.DVarBytes,
			),
		)
	}

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("build TLV stream: %w", err)
	}

	var buf bytes.Buffer
	if err := tlvStream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode TLV proof: %w", err)
	}

	return buf.Bytes(), nil
}

// SchnorrSigner produces 64-byte BIP-340 Schnorr signatures. This
// interface abstracts signing so that callers need not hold raw
// private keys; implementations may delegate to hardware wallets,
// remote signers, or test stubs. The pkScript parameter identifies
// which key to use when the signer manages multiple keys.
type SchnorrSigner interface {
	// SignSchnorr signs the 32-byte hash for the key that
	// controls pkScript and returns a 64-byte BIP-340 Schnorr
	// signature.
	SignSchnorr(pkScript []byte, hash [32]byte) ([]byte, error)
}

// schnorrMessageSigner signs the canonical proof preimage with the requested
// tag applied inside the signer.
type schnorrMessageSigner interface {
	SignSchnorrMessage(ctx context.Context, pkScript []byte, message []byte,
		tag []byte) ([]byte, error)
}

// schnorrProofPubKeySource returns the owner pubkey that should be committed
// into proofs for the given script.
type schnorrProofPubKeySource interface {
	ProofPubKey(pkScript []byte) (*btcec.PublicKey, error)
}

// PrivKeySchnorrSigner wraps a single btcec.PrivateKey to satisfy
// SchnorrSigner. It ignores pkScript since it always signs with the
// same key. Suitable for tests and single-key setups.
type PrivKeySchnorrSigner struct {
	Key *btcec.PrivateKey
}

// SignSchnorr signs the 32-byte hash and returns a 64-byte BIP-340
// Schnorr signature. The pkScript parameter is ignored since this
// signer wraps a single key.
func (s *PrivKeySchnorrSigner) SignSchnorr(_ []byte, hash [32]byte) ([]byte,
	error) {

	sig, err := schnorr.Sign(s.Key, hash[:])
	if err != nil {
		return nil, err
	}

	return sig.Serialize(), nil
}

// ProofPubKey returns the public key corresponding to the wrapped private key.
func (s *PrivKeySchnorrSigner) ProofPubKey(_ []byte) (*btcec.PublicKey, error) {
	if s.Key == nil {
		return nil, fmt.Errorf("private key not configured")
	}

	return s.Key.PubKey(), nil
}

// proofTag returns the BIP-340 tagged hash domain separator for indexer
// proof signatures. A fresh slice is returned each call to prevent
// accidental mutation. This must match the server-side ProofTagHash
// constant in the indexer package.
func proofTag() []byte {
	return []byte("darepo/indexer/v1")
}

// proofOwnerPubKey returns the compressed owner pubkey to include in the
// signed TLV proof when the signer can identify it.
func proofOwnerPubKey(pkScript []byte, signer SchnorrSigner) ([]byte, error) {
	pubKeySource, ok := signer.(schnorrProofPubKeySource)
	if !ok {
		return nil, nil
	}

	pubKey, err := pubKeySource.ProofPubKey(pkScript)
	if err != nil {
		return nil, err
	}
	if pubKey == nil {
		return nil, nil
	}

	return pubKey.SerializeCompressed(), nil
}

// proofSignerPubKey returns the explicit participant pubkey committed into a
// script-scope query proof.
func proofSignerPubKey(pkScript []byte, signer SchnorrSigner) ([]byte, error) {
	return proofOwnerPubKey(pkScript, signer)
}

// schnorrSigOverMessage returns a 64-byte schnorr signature over a
// BIP-340 tagged hash of the message. The tag provides domain separation so
// indexer proof signatures cannot be replayed in other protocols. The
// pkScript identifies which key the signer should use.
func schnorrSigOverMessage(ctx context.Context, message []byte, pkScript []byte,
	signer SchnorrSigner) ([]byte, error) {

	if signer == nil {
		return nil, fmt.Errorf("schnorr signer not configured")
	}

	if messageSigner, ok := signer.(schnorrMessageSigner); ok {
		return messageSigner.SignSchnorrMessage(
			ctx, pkScript, message, proofTag(),
		)
	}

	msgHash := chainhash.TaggedHash(proofTag(), message)

	return signer.SignSchnorr(pkScript, *msgHash)
}

// validateTaprootPkScript returns an error if pkScript is not a valid
// pay-to-taproot output script. This catches obvious misuse before
// signing a proof that the server would reject anyway.
func validateTaprootPkScript(pkScript []byte) error {
	if len(pkScript) == 0 {
		return fmt.Errorf("empty pkScript")
	}

	if !txscript.IsPayToTaproot(pkScript) {
		return fmt.Errorf("pkScript is not P2TR (len=%d, version=%d)",
			len(pkScript), pkScript[0])
	}

	return nil
}

// TaprootScriptScope identifies a P2TR output script to query. The
// client's SchnorrSigner (provided at construction) signs the
// proof-of-control for each scope using the pkScript to select the
// appropriate key.
type TaprootScriptScope struct {
	// PkScript is the raw P2TR output script to query. Must be
	// a valid pay-to-taproot script (OP_1 <32-byte key>).
	PkScript []byte
}

// newTaprootScope builds a ScriptScope proto with a TLV-encoded
// script-scope proof signed via the client's signer.
func (c *Client) newTaprootScope(ctx context.Context, pkScript []byte,
	purpose string) (*arkrpc.ScriptScope, error) {

	if err := validateTaprootPkScript(pkScript); err != nil {
		return nil, err
	}
	if purpose == "" {
		return nil, fmt.Errorf("missing purpose")
	}

	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)
	signerPubKey, err := proofSignerPubKey(pkScript, c.signer)
	if err != nil {
		return nil, err
	}
	if len(signerPubKey) == 0 {
		return nil, fmt.Errorf("signer pubkey not configured")
	}

	nonce, err := randomNonce(registrationNonceBytes)
	if err != nil {
		return nil, err
	}

	msgBytes, err := encodeScriptScopeProofTLV(
		c.serverID, c.principal, purpose, signerPubKey, nonce,
		uint64(
			now.Unix(),
		),
		uint64(
			expiresAt.Unix(),
		),
	)
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(
		ctx, msgBytes, pkScript, c.signer,
	)
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

// buildTaprootScopes converts a slice of TaprootScriptScope into
// proto ScriptScope messages, constructing a signed proof for each
// entry under the given purpose.
func (c *Client) buildTaprootScopes(ctx context.Context,
	scopes []TaprootScriptScope, purpose string) ([]*arkrpc.ScriptScope,
	error) {

	out := make([]*arkrpc.ScriptScope, 0, len(scopes))
	for _, scope := range scopes {
		ss, err := c.newTaprootScope(
			ctx, scope.PkScript, purpose,
		)
		if err != nil {
			return nil, err
		}

		out = append(out, ss)
	}

	return out, nil
}

// BuildListVTXOsByScriptsTaprootRequest builds a proof-gated VTXO query for
// one or more pkScripts.
func (c *Client) BuildListVTXOsByScriptsTaprootRequest(ctx context.Context,
	scopes []TaprootScriptScope, afterCursor []byte, limit uint32,
	statusFilter []arkrpc.VTXOStatus) (*arkrpc.ListVTXOsByScriptsRequest,
	error) {

	c.logger(ctx).TraceS(ctx, "Building ListVTXOsByScripts request",
		slog.Int("scope_count", len(scopes)),
		slog.Int("after_cursor_len", len(afterCursor)),
		slog.Int("limit", int(limit)),
		slog.Int("status_filter_count", len(statusFilter)))

	if err := indexerlimits.ValidateVTXOsByScriptsCursor(
		afterCursor,
	); err != nil {
		return nil, fmt.Errorf("after cursor: %w", err)
	}

	scriptScopes, err := c.buildTaprootScopes(
		ctx, scopes, purposeListVTXOsByScripts,
	)
	if err != nil {
		return nil, err
	}

	return &arkrpc.ListVTXOsByScriptsRequest{
		Scripts:      scriptScopes,
		StatusFilter: statusFilter,
		Cursor:       append([]byte(nil), afterCursor...),
		Limit:        limit,
	}, nil
}

// ListVTXOsByScriptsTaproot performs a proof-gated VTXO query for one
// or more pkScripts.
func (c *Client) ListVTXOsByScriptsTaproot(ctx context.Context,
	scopes []TaprootScriptScope, afterCursor []byte, limit uint32,
	statusFilter []arkrpc.VTXOStatus, opts ...mailboxrpc.RPCOptions) (
	*arkrpc.ListVTXOsByScriptsResponse, error) {

	req, err := c.BuildListVTXOsByScriptsTaprootRequest(
		ctx, scopes, afterCursor, limit, statusFilter,
	)
	if err != nil {
		return nil, err
	}

	resp, err := c.rpc.ListVTXOsByScripts(ctx, req, firstOpt(opts))
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// BuildGetOORSessionByTxidTaprootRequest builds the proof-gated indexer
// request body for one script-scoped OOR session lookup.
func (c *Client) BuildGetOORSessionByTxidTaprootRequest(ctx context.Context,
	pkScript []byte, sessionTxid []byte) (
	*arkrpc.GetOORSessionByTxidRequest, error) {

	c.logger(ctx).TraceS(ctx, "Building GetOORSessionByTxid request",
		btclog.Hex("pk_script", pkScript),
		btclog.Hex("session_txid", sessionTxid))

	scope, err := c.newTaprootScope(
		ctx, pkScript, purposeGetOORSessionByTxid,
	)
	if err != nil {
		return nil, err
	}

	return &arkrpc.GetOORSessionByTxidRequest{
		Script:      scope,
		SessionTxid: append([]byte(nil), sessionTxid...),
	}, nil
}

// GetOORSessionByTxidTaproot performs a proof-gated OOR session lookup for
// one script and deterministic session txid.
func (c *Client) GetOORSessionByTxidTaproot(ctx context.Context,
	pkScript []byte, sessionTxid []byte, opts ...mailboxrpc.RPCOptions) (
	*arkrpc.GetOORSessionByTxidResponse, error) {

	req, err := c.BuildGetOORSessionByTxidTaprootRequest(
		ctx, pkScript, sessionTxid,
	)
	if err != nil {
		return nil, err
	}

	return c.rpc.GetOORSessionByTxid(ctx, req, firstOpt(opts))
}

// GetSubtreeByScriptsTaproot performs a proof-gated subtree query for
// one or more pkScripts.
func (c *Client) GetSubtreeByScriptsTaproot(ctx context.Context,
	scopes []TaprootScriptScope, includeInternalNodes bool,
	opts ...mailboxrpc.RPCOptions) (*arkrpc.GetSubtreeByScriptsResponse,
	error) {

	c.logger(ctx).TraceS(ctx, "Getting subtree by scripts",
		slog.Int("scope_count", len(scopes)),
		slog.Bool("include_internal_nodes", includeInternalNodes))

	scriptScopes, err := c.buildTaprootScopes(
		ctx, scopes, purposeGetSubtreeByScripts,
	)
	if err != nil {
		return nil, err
	}

	req := &arkrpc.GetSubtreeByScriptsRequest{
		Scripts:              scriptScopes,
		IncludeInternalNodes: includeInternalNodes,
	}

	return c.rpc.GetSubtreeByScripts(ctx, req, firstOpt(opts))
}

// ListVTXOEventsByScriptsTaproot performs a proof-gated, monotonic VTXO
// event feed query for one or more pkScripts.
func (c *Client) ListVTXOEventsByScriptsTaproot(ctx context.Context,
	scopes []TaprootScriptScope, afterEventID uint64, limit uint32,
	opts ...mailboxrpc.RPCOptions) (*arkrpc.ListVTXOEventsByScriptsResponse,
	error) {

	c.logger(ctx).TraceS(ctx, "Listing VTXO events by scripts",
		slog.Int("scope_count", len(scopes)),
		slog.Uint64("after_event_id", afterEventID),
		slog.Int("limit", int(limit)))

	scriptScopes, err := c.buildTaprootScopes(
		ctx, scopes, purposeListVTXOEventsByScripts,
	)
	if err != nil {
		return nil, err
	}

	req := &arkrpc.ListVTXOEventsByScriptsRequest{
		Scripts:      scriptScopes,
		AfterEventId: afterEventID,
		Limit:        limit,
	}

	return c.rpc.ListVTXOEventsByScripts(ctx, req, firstOpt(opts))
}

// RegisterReceiveScriptTaproot registers a single P2TR receive script
// using a schnorr signature proof under the output key.
//
// expiresAt controls server-side retention of the script registration
// (proto ExpiresAtUnixS). The server may garbage-collect the binding
// after this time. This is distinct from the proof TTL
// (offlineReceiveProofTTL), which limits the replay window of the
// signed TLV proof itself. A long registration expiry with a short
// proof TTL is the expected configuration: the binding persists, but
// each proof is only valid for minutes.
func (c *Client) RegisterReceiveScriptTaproot(ctx context.Context,
	pkScript []byte, expiresAt time.Time, label string,
	opts ...mailboxrpc.RPCOptions) (*arkrpc.RegisterReceiveScriptResponse,
	error) {

	if err := validateTaprootPkScript(pkScript); err != nil {
		return nil, err
	}

	now := time.Now()
	if !expiresAt.After(now) {
		return nil, fmt.Errorf("expiresAt must be in the future (got "+
			"%v, now %v)", expiresAt, now)
	}

	proofExpiresAt := now.Add(offlineReceiveProofTTL)
	ownerPubKey, err := proofOwnerPubKey(pkScript, c.signer)
	if err != nil {
		return nil, err
	}

	// A zero time means "use server default", so we leave the
	// field at 0 rather than casting time.Time{}.Unix() which
	// would wrap to a huge uint64. We derive the safe value
	// before encoding the TLV proof so both the signed message
	// and the proto request field use the same expiry.
	var expiresAtUnixS uint64
	if !expiresAt.IsZero() {
		expiresAtUnixS = uint64(expiresAt.Unix())
	}

	nonce, err := randomNonce(registrationNonceBytes)
	if err != nil {
		return nil, err
	}

	msgBytes, err := encodeProofTLVWithOwner(
		registrationMessageType, c.serverID, c.principal,
		purposeRegisterReceiveScript, pkScript, ownerPubKey, nonce,
		uint64(
			now.Unix(),
		),
		uint64(
			proofExpiresAt.Unix(),
		),
	)
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(
		ctx, msgBytes, pkScript, c.signer,
	)
	if err != nil {
		return nil, err
	}

	c.logger(ctx).TraceS(ctx, "Registering receive script",
		btclog.Hex("pk_script", pkScript),
		slog.String("label", label),
		slog.Time("expires_at", expiresAt))

	req := &arkrpc.RegisterReceiveScriptRequest{
		PkScript:       pkScript,
		ExpiresAtUnixS: expiresAtUnixS,
		Label:          label,
		Proof: &arkrpc.RegisterReceiveScriptRequest_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: msgBytes,
				Sig64:   sig64,
			},
		},
	}

	return c.rpc.RegisterReceiveScript(ctx, req, firstOpt(opts))
}

// UnregisterReceiveScript removes a receive script registration. The
// caller must provide the signing key that controls the P2TR output so
// that the server can verify ownership before removing the binding.
func (c *Client) UnregisterReceiveScript(ctx context.Context, pkScript []byte,
	opts ...mailboxrpc.RPCOptions) (*arkrpc.UnregisterReceiveScriptResponse,
	error) {

	if err := validateTaprootPkScript(pkScript); err != nil {
		return nil, err
	}

	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)
	ownerPubKey, err := proofOwnerPubKey(pkScript, c.signer)
	if err != nil {
		return nil, err
	}

	nonce, err := randomNonce(registrationNonceBytes)
	if err != nil {
		return nil, err
	}

	msgBytes, err := encodeProofTLVWithOwner(
		registrationMessageType, c.serverID, c.principal,
		purposeUnregisterReceiveScript, pkScript, ownerPubKey, nonce,
		uint64(
			now.Unix(),
		),
		uint64(
			expiresAt.Unix(),
		),
	)
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(
		ctx, msgBytes, pkScript, c.signer,
	)
	if err != nil {
		return nil, err
	}

	c.logger(ctx).TraceS(ctx, "Unregistering receive script",
		btclog.Hex("pk_script", pkScript))

	req := &arkrpc.UnregisterReceiveScriptRequest{
		PkScript: pkScript,
		Proof: &arkrpc.UnregisterReceiveScriptRequest_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: msgBytes,
				Sig64:   sig64,
			},
		},
	}

	return c.rpc.UnregisterReceiveScript(ctx, req, firstOpt(opts))
}

// ListMyReceiveScripts lists the receive scripts currently registered
// to the caller's mailbox principal. No proof is required because the
// request is implicitly scoped to the authenticated principal.
func (c *Client) ListMyReceiveScripts(ctx context.Context,
	opts ...mailboxrpc.RPCOptions) (*arkrpc.ListMyReceiveScriptsResponse,
	error) {

	c.logger(ctx).TraceS(ctx, "Listing registered receive scripts")

	req := &arkrpc.ListMyReceiveScriptsRequest{}

	return c.rpc.ListMyReceiveScripts(ctx, req, firstOpt(opts))
}

// BuildListOORRecipientEventsByScriptTaprootRequest builds a proof-gated
// script-keyed recipient event query. This enables "offline receive without
// registration" while preventing third-party enumeration
// (proof-of-control required).
func (c *Client) BuildListOORRecipientEventsByScriptTaprootRequest(
	ctx context.Context, pkScript []byte, afterEventID uint64,
	limit uint32) (*arkrpc.ListOORRecipientEventsByScriptRequest, error) {

	if err := validateTaprootPkScript(pkScript); err != nil {
		return nil, err
	}

	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)
	ownerPubKey, err := proofOwnerPubKey(pkScript, c.signer)
	if err != nil {
		return nil, err
	}

	nonce, err := randomNonce(registrationNonceBytes)
	if err != nil {
		return nil, err
	}

	msgBytes, err := encodeProofTLVWithOwner(
		scriptScopeMessageType, c.serverID, c.principal,
		purposeOORRecipientEvents, pkScript, ownerPubKey, nonce,
		uint64(
			now.Unix(),
		),
		uint64(
			expiresAt.Unix(),
		),
	)
	if err != nil {
		return nil, err
	}

	sig64, err := schnorrSigOverMessage(
		ctx, msgBytes, pkScript, c.signer,
	)
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

	c.logger(ctx).TraceS(ctx, "Listing OOR recipient events",
		btclog.Hex("pk_script", pkScript),
		slog.Uint64("after_event_id", afterEventID),
		slog.Int("limit", int(limit)))

	req := &arkrpc.ListOORRecipientEventsByScriptRequest{
		PkScript:     pkScript,
		AfterEventId: afterEventID,
		Limit:        limit,
		Proof:        proofOneof,
	}

	return req, nil
}

// ListOORRecipientEventsByScriptTaproot performs a proof-gated
// script-keyed recipient event query. This enables "offline receive
// without registration" while preventing third-party enumeration
// (proof-of-control required).
func (c *Client) ListOORRecipientEventsByScriptTaproot(ctx context.Context,
	pkScript []byte, afterEventID uint64, limit uint32,
	opts ...mailboxrpc.RPCOptions) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	req, err := c.BuildListOORRecipientEventsByScriptTaprootRequest(
		ctx, pkScript, afterEventID, limit,
	)
	if err != nil {
		return nil, err
	}

	return c.rpc.ListOORRecipientEventsByScript(ctx, req, firstOpt(opts))
}
