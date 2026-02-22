package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/arkrpc"
)

const (
	// proofTypeReceiveScriptRegistration is the proof type string
	// used in the canonical JSON message.
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

var (
	// ErrMissingProof indicates an RPC did not include a required proof.
	ErrMissingProof = errors.New("missing proof")

	// ErrBIP322Unimplemented indicates a request attempted to use BIP-322
	// proofs which are not implemented in the current draft.
	ErrBIP322Unimplemented = errors.New("bip322 proofs not implemented")
)

// receiveScriptProofMessage is the canonical JSON message that a wallet signs
// to bind a receive script to a mailbox principal.
type receiveScriptProofMessage struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	ServerID    string `json:"server_id"`
	Principal   string `json:"principal"`
	PkScriptHex string `json:"pk_script_hex"`
	IssuedAt    int64  `json:"issued_at"`
	ExpiresAt   int64  `json:"expires_at"`
	Nonce       string `json:"nonce"`
}

// scriptScopeProofMessage is the canonical JSON message signed to prove control
// of a receive script for a specific purpose (RPC method / feed).
type scriptScopeProofMessage struct {
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

// parseReceiveScriptProofMessage parses messageJSON into a typed proof
// message.
func parseReceiveScriptProofMessage(messageJSON string) (
	*receiveScriptProofMessage, error) {

	var msg receiveScriptProofMessage
	if err := json.Unmarshal([]byte(messageJSON), &msg); err != nil {
		return nil, err
	}

	return &msg, nil
}

// validateReceiveScriptProofMessage validates msg against the expected
// serverID, principal, and pkScript.
func validateReceiveScriptProofMessage(now time.Time,
	msg *receiveScriptProofMessage, serverID string,
	principal string, pkScript []byte) error {

	if msg == nil {
		return fmt.Errorf("missing proof message")
	}

	if msg.Type != proofTypeReceiveScriptRegistration {
		return fmt.Errorf("unexpected proof type: %s", msg.Type)
	}

	if msg.Version != 0 {
		return fmt.Errorf("unsupported proof version: %d", msg.Version)
	}

	if msg.ServerID != serverID {
		return fmt.Errorf("unexpected server id: %s", msg.ServerID)
	}

	if msg.Principal != principal {
		return fmt.Errorf("unexpected principal: %s", msg.Principal)
	}

	wantScriptHex := hex.EncodeToString(pkScript)
	if msg.PkScriptHex != wantScriptHex {
		return fmt.Errorf("pk_script_hex mismatch")
	}

	if msg.ExpiresAt <= 0 || msg.IssuedAt <= 0 {
		return fmt.Errorf("missing issued_at/expires_at")
	}

	issuedAt := time.Unix(msg.IssuedAt, 0)
	expiresAt := time.Unix(msg.ExpiresAt, 0)

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

	if msg.Nonce == "" {
		return fmt.Errorf("missing nonce")
	}

	return nil
}

func parseScriptScopeProofMessage(messageJSON string) (
	*scriptScopeProofMessage, error) {

	var msg scriptScopeProofMessage
	if err := json.Unmarshal([]byte(messageJSON), &msg); err != nil {
		return nil, err
	}

	return &msg, nil
}

func validateScriptScopeProofMessage(now time.Time,
	msg *scriptScopeProofMessage, serverID string,
	principal string, purpose string, pkScript []byte) error {

	if msg == nil {
		return fmt.Errorf("missing proof message")
	}

	if msg.Type != proofTypeScriptScope {
		return fmt.Errorf("unexpected proof type: %s", msg.Type)
	}

	if msg.Version != 0 {
		return fmt.Errorf("unsupported proof version: %d", msg.Version)
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

	wantScriptHex := hex.EncodeToString(pkScript)
	if msg.PkScriptHex != wantScriptHex {
		return fmt.Errorf("pk_script_hex mismatch")
	}

	if msg.ExpiresAt <= 0 || msg.IssuedAt <= 0 {
		return fmt.Errorf("missing issued_at/expires_at")
	}

	issuedAt := time.Unix(msg.IssuedAt, 0)
	expiresAt := time.Unix(msg.ExpiresAt, 0)

	if expiresAt.Before(issuedAt) {
		return fmt.Errorf("expires_at before issued_at")
	}

	if expiresAt.Sub(issuedAt) > maxProofLifetime {
		return fmt.Errorf("proof lifetime too long")
	}

	if issuedAt.After(now.Add(proofSkewAllowance)) {
		return fmt.Errorf("issued_at too far in the future")
	}

	if now.After(expiresAt.Add(proofSkewAllowance)) {
		return fmt.Errorf("proof expired")
	}

	if msg.Nonce == "" {
		return fmt.Errorf("missing nonce")
	}

	return nil
}

// taprootOutputKeyFromPkScript extracts the taproot output key from a P2TR
// pkScript.
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

// verifyTaprootSchnorrProof verifies proof against pkScript and binds
// it to the expected principal and server ID.
func verifyTaprootSchnorrProof(now time.Time, pkScript []byte,
	proof *arkrpc.TaprootSchnorrProof, serverID string,
	principal string) error {

	if proof == nil {
		return fmt.Errorf("missing taproot schnorr proof")
	}

	pubKey, err := taprootOutputKeyFromPkScript(pkScript)
	if err != nil {
		return err
	}

	msg, err := parseReceiveScriptProofMessage(proof.Message)
	if err != nil {
		return err
	}

	if err := validateReceiveScriptProofMessage(
		now, msg, serverID, principal, pkScript,
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

	msgHash := sha256.Sum256([]byte(proof.Message))
	if !sig.Verify(msgHash[:], pubKey) {
		return fmt.Errorf("invalid schnorr signature")
	}

	return nil
}

func verifyScriptScopeProof(now time.Time, pkScript []byte,
	proof any, serverID string, principal string, purpose string) error {

	switch p := proof.(type) {
	case *arkrpc.ScriptScope_TaprootSchnorr:
		return verifyTaprootSchnorrScopeProof(
			now, pkScript, p.TaprootSchnorr, serverID,
			principal, purpose,
		)

	case *arkrpc.ScriptScope_Bip322:
		return ErrBIP322Unimplemented

	default:
		return ErrMissingProof
	}
}

func verifyTaprootSchnorrScopeProof(now time.Time, pkScript []byte,
	proof *arkrpc.TaprootSchnorrProof, serverID string,
	principal string, purpose string) error {

	if proof == nil {
		return fmt.Errorf("missing taproot schnorr proof")
	}

	pubKey, err := taprootOutputKeyFromPkScript(pkScript)
	if err != nil {
		return err
	}

	msg, err := parseScriptScopeProofMessage(proof.Message)
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

	msgHash := sha256.Sum256([]byte(proof.Message))
	if !sig.Verify(msgHash[:], pubKey) {
		return fmt.Errorf("invalid schnorr signature")
	}

	return nil
}
