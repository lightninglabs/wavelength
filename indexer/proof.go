package indexer

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// proofTypeReceiveScriptRegistration is the proof type string
	// used in the canonical TLV message.
	proofTypeReceiveScriptRegistration = "receive_script_registration"

	// proofTypeScriptScope is the proof type string used for proof-gated
	// script-scoped indexer queries (option B).
	proofTypeScriptScope = "script_scope"

	// proofSkewAllowance is a small allowance for clock skew between
	// a client and operator.
	proofSkewAllowance = 2 * time.Minute

	// maxProofLifetime bounds proof replay windows for the taproot schnorr
	// scheme. This is only a default for the in-memory draft
	// implementation.
	maxProofLifetime = 24 * time.Hour

	// p2trPkScriptLen is the length of a pay-to-taproot output pkScript:
	// OP_1 OP_DATA_32 <x-only pubkey>.
	p2trPkScriptLen = 34

	// p2trWitnessProgramLenByte is the script opcode byte for
	// pushing 32 bytes.
	p2trWitnessProgramLenByte = 0x20

	// schnorrSignatureLen is the length of a BIP-340 schnorr signature.
	schnorrSignatureLen = 64
)

// ProofTagHash is the BIP-340 tagged hash domain separator for indexer
// proof signatures. Using a tagged hash ensures that signatures produced
// for the indexer cannot be replayed in other protocols that also use
// BIP-340 Schnorr signatures (e.g., taproot key-path spends).
var ProofTagHash = []byte("darepo/indexer/v1")

// TLV type constants for proof message fields. Types are allocated
// sequentially from a private range. The canonical TLV stream must be
// encoded with records sorted by type in ascending order.
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
)

var (
	// ErrMissingProof indicates an RPC did not include a required proof.
	ErrMissingProof = errors.New("missing proof")

	// ErrBIP322Unimplemented indicates a request attempted to use BIP-322
	// proofs which are not implemented in the current draft.
	ErrBIP322Unimplemented = errors.New("bip322 proofs not implemented")
)

// proofMessage is the decoded TLV proof message used for both
// receive-script registration and script-scope queries. The Type
// field distinguishes the two variants.
type proofMessage struct {
	Type        string
	Version     uint32
	ServerID    string
	Principal   string
	Purpose     string
	PkScript    []byte
	OwnerPubKey []byte
	IssuedAt    uint64
	ExpiresAt   uint64
	Nonce       []byte
}

// receiveScriptProofMessage is an alias preserved for call-site
// clarity and backward compatibility with test helpers.
type receiveScriptProofMessage = proofMessage

// scriptScopeProofMessage is an alias preserved for call-site
// clarity and backward compatibility with test helpers.
type scriptScopeProofMessage = proofMessage

// taprootProofVerificationConfig carries optional server-side context for
// validating owner-key proofs over standardized receive scripts.
type taprootProofVerificationConfig struct {
	vtxoOperatorKey *btcec.PublicKey
	vtxoExitDelay   uint32
}

// parseReceiveScriptProofMessage decodes messageBytes from the canonical TLV
// encoding into a typed proof message.
func parseReceiveScriptProofMessage(
	messageBytes []byte) (*receiveScriptProofMessage, error) {

	var (
		proofType   []byte
		version     uint32
		serverID    []byte
		principal   []byte
		purpose     []byte
		pkScript    []byte
		ownerPubKey []byte
		issuedAt    uint64
		expiresAt   uint64
		nonce       []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofType, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(proofTLVTypeVersion, &version),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverID, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principal, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(proofTLVTypeIssuedAt, &issuedAt),
		tlv.MakePrimitiveRecord(proofTLVTypeExpiresAt, &expiresAt),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePurpose, &purpose, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeOwnerPubKey, &ownerPubKey, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build TLV stream: %w", err)
	}

	if err := tlvStream.Decode(bytes.NewReader(messageBytes)); err != nil {
		return nil, fmt.Errorf("decode TLV proof: %w", err)
	}

	return &receiveScriptProofMessage{
		Type:        string(proofType),
		Version:     version,
		ServerID:    string(serverID),
		Principal:   string(principal),
		Purpose:     string(purpose),
		PkScript:    pkScript,
		OwnerPubKey: ownerPubKey,
		IssuedAt:    issuedAt,
		ExpiresAt:   expiresAt,
		Nonce:       nonce,
	}, nil
}

// parseScriptScopeProofMessage decodes messageBytes from the canonical
// TLV encoding into a typed scope-proof message. The TLV schema is
// shared with receive-script proofs; the Type field distinguishes them.
func parseScriptScopeProofMessage(
	messageBytes []byte) (*scriptScopeProofMessage, error) {

	return parseReceiveScriptProofMessage(messageBytes)
}

// encodeReceiveScriptProofTLV encodes a proof message to its canonical
// TLV byte representation. Used by both receive-script and
// script-scope proofs since they share the same TLV schema.
func encodeReceiveScriptProofTLV(
	msg *receiveScriptProofMessage) ([]byte, error) {

	proofType := []byte(msg.Type)
	serverID := []byte(msg.ServerID)
	principal := []byte(msg.Principal)
	purpose := []byte(msg.Purpose)
	records := []tlv.Record{
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofType,
			tlv.SizeVarBytes(&proofType),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &msg.Version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverID,
			tlv.SizeVarBytes(&serverID),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principal,
			tlv.SizeVarBytes(&principal),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &msg.PkScript,
			tlv.SizeVarBytes(&msg.PkScript),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &msg.IssuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &msg.ExpiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &msg.Nonce,
			tlv.SizeVarBytes(&msg.Nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePurpose, &purpose,
			tlv.SizeVarBytes(&purpose),
			tlv.EVarBytes, tlv.DVarBytes,
		),
	}
	if len(msg.OwnerPubKey) > 0 {
		records = append(records, tlv.MakeDynamicRecord(
			proofTLVTypeOwnerPubKey, &msg.OwnerPubKey,
			tlv.SizeVarBytes(&msg.OwnerPubKey),
			tlv.EVarBytes, tlv.DVarBytes,
		))
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

// encodeScriptScopeProofTLV encodes a script-scope proof message to
// its canonical TLV byte representation. The TLV schema is shared
// with receive-script proofs; the Type field distinguishes them.
func encodeScriptScopeProofTLV(
	msg *scriptScopeProofMessage) ([]byte, error) {

	return encodeReceiveScriptProofTLV(msg)
}

// validateProofMessage validates a proof message against the expected
// type, serverID, principal, purpose, and pkScript. This is the
// shared core for both receive-script and script-scope proof
// validation.
func validateProofMessage(now time.Time, msg *proofMessage,
	expectedType string, serverID string, principal string,
	purpose string, pkScript []byte) error {

	if msg == nil {
		return fmt.Errorf("missing proof message")
	}

	if msg.Type != expectedType {
		return fmt.Errorf("unexpected proof type: %s", msg.Type)
	}

	if msg.Version != 0 {
		return fmt.Errorf("unsupported proof version: %d",
			msg.Version)
	}

	if msg.ServerID != serverID {
		return fmt.Errorf("unexpected server id: %s", msg.ServerID)
	}

	if msg.Principal != principal {
		return fmt.Errorf("unexpected principal: %s", msg.Principal)
	}

	if msg.Purpose != purpose {
		return fmt.Errorf("unexpected purpose: %s", msg.Purpose)
	}

	if !bytes.Equal(msg.PkScript, pkScript) {
		return fmt.Errorf("pk_script mismatch")
	}

	if msg.ExpiresAt == 0 || msg.IssuedAt == 0 {
		return fmt.Errorf("missing issued_at/expires_at")
	}

	issuedAt := time.Unix(int64(msg.IssuedAt), 0)
	expiresAt := time.Unix(int64(msg.ExpiresAt), 0)

	if expiresAt.Before(issuedAt) {
		return fmt.Errorf("expires_at before issued_at")
	}

	// Enforce a bounded lifetime to reduce replay windows.
	if expiresAt.Sub(issuedAt) > maxProofLifetime {
		return fmt.Errorf("proof lifetime too long")
	}

	// Allow a small future skew to avoid surprising failures.
	if issuedAt.After(now.Add(proofSkewAllowance)) {
		return fmt.Errorf("issued_at too far in the future")
	}

	// Reject stale proofs.
	if now.After(expiresAt.Add(proofSkewAllowance)) {
		return fmt.Errorf("proof expired")
	}

	// NOTE: The nonce is checked for presence but not deduplicated
	// server-side. A valid proof can be replayed within its
	// lifetime window (maxProofLifetime + proofSkewAllowance).
	// Cross-purpose replay is prevented by the Purpose field
	// binding. A server-side nonce registry with TTL-based
	// eviction would eliminate within-lifetime replay at the cost
	// of per-server state; this is a deliberate trade-off for the
	// current design.
	if len(msg.Nonce) == 0 {
		return fmt.Errorf("missing nonce")
	}

	return nil
}

// validateReceiveScriptProofMessage validates msg for receive-script
// registration proofs.
func validateReceiveScriptProofMessage(now time.Time,
	msg *receiveScriptProofMessage, serverID string,
	principal string, purpose string, pkScript []byte) error {

	return validateProofMessage(
		now, msg, proofTypeReceiveScriptRegistration,
		serverID, principal, purpose, pkScript,
	)
}

// validateScriptScopeProofMessage validates msg for script-scope
// query proofs.
func validateScriptScopeProofMessage(now time.Time,
	msg *scriptScopeProofMessage, serverID string,
	principal string, purpose string, pkScript []byte) error {

	return validateProofMessage(
		now, msg, proofTypeScriptScope,
		serverID, principal, purpose, pkScript,
	)
}

// taprootOutputKeyFromPkScript extracts the taproot output key Q from a
// P2TR pkScript. Proof verification signs against Q directly, which means
// it only works for key-path spends where the wallet holds the private key
// corresponding to Q (the tweaked output key). For outputs with non-trivial
// script trees, Q = P + H(P||script_tree) and the wallet would need to
// sign with the tweaked key, not the internal key P.
func taprootOutputKeyFromPkScript(pkScript []byte) (*btcec.PublicKey, error) {
	// P2TR script is: OP_1 OP_DATA_32 <x-only pubkey>
	if len(pkScript) != p2trPkScriptLen {
		scriptLen := len(pkScript)
		return nil, fmt.Errorf("unexpected pkScript length: %d",
			scriptLen)
	}
	if pkScript[0] != txscript.OP_1 ||
		pkScript[1] != p2trWitnessProgramLenByte {

		return nil, fmt.Errorf("pkScript is not P2TR")
	}

	return schnorr.ParsePubKey(pkScript[2:])
}

// proofSigningKey resolves the pubkey that should verify a proof for pkScript.
// When the message carries an owner pubkey, we accept either a direct P2TR
// key-path script for that pubkey or the standardized VTXO tapscript derived
// from the server's operator policy. Otherwise we fall back to the taproot
// output key embedded in pkScript.
func proofSigningKey(pkScript []byte, ownerPubKeyBytes []byte,
	cfg taprootProofVerificationConfig) (*btcec.PublicKey, error) {

	if len(ownerPubKeyBytes) == 0 {
		return taprootOutputKeyFromPkScript(pkScript)
	}

	ownerPubKey, err := btcec.ParsePubKey(ownerPubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse owner pubkey: %w", err)
	}

	expectedOwnerScript, err := txscript.PayToTaprootScript(ownerPubKey)
	if err != nil {
		return nil, fmt.Errorf("derive owner taproot script: %w", err)
	}
	if bytes.Equal(expectedOwnerScript, pkScript) {
		return ownerPubKey, nil
	}

	if cfg.vtxoOperatorKey != nil && cfg.vtxoExitDelay > 0 {
		vtxoTapKey, err := scripts.VTXOTapKey(
			ownerPubKey, cfg.vtxoOperatorKey, cfg.vtxoExitDelay,
		)
		if err != nil {
			return nil, fmt.Errorf("derive vtxo tap key: %w", err)
		}

		expectedVTXOScript, err := txscript.PayToTaprootScript(
			vtxoTapKey,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"derive vtxo taproot script: %w", err,
			)
		}
		if bytes.Equal(expectedVTXOScript, pkScript) {
			return ownerPubKey, nil
		}
	}

	return nil, fmt.Errorf("owner pubkey does not match supported script")
}

// verifyTaprootSchnorrProof verifies proof against pkScript and binds
// it to the expected principal and server ID. The proof message is a
// canonical TLV-encoded byte stream carried in the proto Message field.
func verifyTaprootSchnorrProof(now time.Time, pkScript []byte,
	proof *arkrpc.TaprootSchnorrProof, serverID string,
	principal string, purpose string,
	cfg taprootProofVerificationConfig) error {

	if proof == nil {
		return fmt.Errorf("missing taproot schnorr proof")
	}

	msg, err := parseReceiveScriptProofMessage(proof.Message)
	if err != nil {
		return err
	}

	pubKey, err := proofSigningKey(
		pkScript, msg.OwnerPubKey, cfg,
	)
	if err != nil {
		return err
	}

	if err := validateReceiveScriptProofMessage(
		now, msg, serverID, principal, purpose, pkScript,
	); err != nil {
		return err
	}

	if len(proof.Sig64) != schnorrSignatureLen {
		sigLen := len(proof.Sig64)
		return fmt.Errorf("unexpected sig64 length: %d", sigLen)
	}

	sig, err := schnorr.ParseSignature(proof.Sig64)
	if err != nil {
		return err
	}

	msgHash := chainhash.TaggedHash(ProofTagHash, proof.Message)
	if !sig.Verify(msgHash[:], pubKey) {
		return fmt.Errorf("invalid schnorr signature")
	}

	return nil
}

// verifyScriptScopeProof dispatches proof verification for script-scope
// queries based on the oneof proof variant.
func verifyScriptScopeProof(now time.Time, pkScript []byte,
	proof any, serverID string, principal string, purpose string,
	cfg taprootProofVerificationConfig) error {

	switch p := proof.(type) {
	case *arkrpc.ScriptScope_TaprootSchnorr:
		return verifyTaprootSchnorrScopeProof(
			now, pkScript, p.TaprootSchnorr, serverID,
			principal, purpose, cfg,
		)

	case *arkrpc.ScriptScope_Bip322:
		return ErrBIP322Unimplemented

	default:
		return ErrMissingProof
	}
}

// verifyTaprootSchnorrScopeProof verifies a script-scope taproot schnorr
// proof including purpose binding.
func verifyTaprootSchnorrScopeProof(now time.Time, pkScript []byte,
	proof *arkrpc.TaprootSchnorrProof, serverID string,
	principal string, purpose string,
	cfg taprootProofVerificationConfig) error {

	if proof == nil {
		return fmt.Errorf("missing taproot schnorr proof")
	}

	msg, err := parseScriptScopeProofMessage(proof.Message)
	if err != nil {
		return err
	}

	pubKey, err := proofSigningKey(
		pkScript, msg.OwnerPubKey, cfg,
	)
	if err != nil {
		return err
	}

	if err := validateScriptScopeProofMessage(
		now, msg, serverID, principal, purpose, pkScript,
	); err != nil {
		return err
	}

	if len(proof.Sig64) != schnorrSignatureLen {
		sigLen := len(proof.Sig64)
		return fmt.Errorf("unexpected sig64 length: %d", sigLen)
	}

	sig, err := schnorr.ParseSignature(proof.Sig64)
	if err != nil {
		return err
	}

	msgHash := chainhash.TaggedHash(ProofTagHash, proof.Message)
	if !sig.Verify(msgHash[:], pubKey) {
		return fmt.Errorf("invalid schnorr signature")
	}

	return nil
}

// BuildReceiveScriptProofMessage constructs and TLV-encodes a
// receive-script proof message from the given parameters.
//
// The returned bytes are the canonical message that should be hashed
// with chainhash.TaggedHash(ProofTagHash, msg) and signed with either
// the direct P2TR output key or the owner key committed through
// BuildReceiveScriptProofMessageWithOwner.
func BuildReceiveScriptProofMessage(serverID, principal string,
	pkScript, nonce []byte,
	issuedAt, expiresAt time.Time) ([]byte, error) {

	return BuildReceiveScriptProofMessageWithOwner(
		serverID, principal, purposeRegisterReceiveScript, pkScript,
		nil, nonce, issuedAt, expiresAt,
	)
}

// BuildReceiveScriptProofMessageWithOwner constructs and TLV-encodes a
// receive-script proof message from the given parameters.
//
// The returned bytes are the canonical message that should be hashed
// with chainhash.TaggedHash(ProofTagHash, msg) and signed with either
// the direct P2TR output key or, for supported standardized receive
// scripts, the owner key committed in ownerPubKey.
func BuildReceiveScriptProofMessageWithOwner(serverID, principal,
	purpose string, pkScript, ownerPubKey, nonce []byte,
	issuedAt, expiresAt time.Time) ([]byte, error) {

	if purpose == "" {
		purpose = purposeRegisterReceiveScript
	}

	return encodeReceiveScriptProofTLV(&receiveScriptProofMessage{
		Type:        proofTypeReceiveScriptRegistration,
		Version:     0,
		ServerID:    serverID,
		Principal:   principal,
		Purpose:     purpose,
		PkScript:    pkScript,
		OwnerPubKey: ownerPubKey,
		IssuedAt:    uint64(issuedAt.Unix()),
		ExpiresAt:   uint64(expiresAt.Unix()),
		Nonce:       nonce,
	})
}

// BuildScriptScopeProofMessage constructs and TLV-encodes a script-scope
// proof message from the given parameters.
//
// The returned bytes are the canonical message that should be hashed
// with chainhash.TaggedHash(ProofTagHash, msg) and signed with either
// the direct P2TR output key or the owner key committed through
// BuildScriptScopeProofMessageWithOwner.
func BuildScriptScopeProofMessage(serverID, principal, purpose string,
	pkScript, nonce []byte,
	issuedAt, expiresAt time.Time) ([]byte, error) {

	return BuildScriptScopeProofMessageWithOwner(
		serverID, principal, purpose, pkScript, nil, nonce,
		issuedAt, expiresAt,
	)
}

// BuildScriptScopeProofMessageWithOwner constructs and TLV-encodes a
// script-scope proof message from the given parameters.
//
// The returned bytes are the canonical message that should be hashed
// with chainhash.TaggedHash(ProofTagHash, msg) and signed with either
// the direct P2TR output key or, for supported standardized receive
// scripts, the owner key committed in ownerPubKey.
func BuildScriptScopeProofMessageWithOwner(serverID, principal,
	purpose string, pkScript, ownerPubKey, nonce []byte,
	issuedAt, expiresAt time.Time) ([]byte, error) {

	return encodeScriptScopeProofTLV(&scriptScopeProofMessage{
		Type:        proofTypeScriptScope,
		Version:     0,
		ServerID:    serverID,
		Principal:   principal,
		Purpose:     purpose,
		PkScript:    pkScript,
		OwnerPubKey: ownerPubKey,
		IssuedAt:    uint64(issuedAt.Unix()),
		ExpiresAt:   uint64(expiresAt.Unix()),
		Nonce:       nonce,
	})
}
