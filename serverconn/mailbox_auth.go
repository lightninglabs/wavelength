package serverconn

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
)

// AuthHeaderKey is the envelope header key that carries the Schnorr
// signature proving the sender holds the private key corresponding
// to their claimed pubkey-derived mailbox ID.
const AuthHeaderKey = "x-mailbox-auth-sig"

// TLSBindHeaderKey is the envelope header key that carries the
// Schnorr signature binding the sender's secp256k1 mailbox identity
// to the SubjectPublicKeyInfo of the P-256 TLS leaf certificate
// presented on the same connection. The server verifies this
// signature against the leaf pubkey it actually observed during the
// TLS handshake before recording the cert fingerprint binding for
// the mailbox identity (issue #448). Without it, an attacker who
// replays a captured Send envelope across a fresh TLS connection
// of their own would have their leaf fingerprint bound to the
// victim's mailbox ID.
const TLSBindHeaderKey = "x-mailbox-tls-bind-sig"

// MailboxAuthTagStr is the BIP-340 tagged hash domain separator used
// when constructing the message digest for mailbox authentication
// signatures. This prevents cross-protocol signature reuse.
const MailboxAuthTagStr = "mailbox-auth"

// MailboxTLSBindTagStr is the BIP-340 tagged hash domain separator
// used when constructing the message digest that binds a
// secp256k1 mailbox identity to the TLS leaf SubjectPublicKeyInfo.
// Using a dedicated tag prevents a mailbox-auth signature from
// being reinterpreted as a TLS-binding signature and vice versa.
const MailboxTLSBindTagStr = "mailbox-tls-bind"

// MailboxAuthMessage returns the raw message bytes that are fed into
// the BIP-340 tagged hash for mailbox authentication:
//
//	senderCompressedPubKey || recipientMailboxID
//
// Including the recipient mailbox ID (the server's pubkey-derived ID)
// prevents cross-server replay of the signature.
func MailboxAuthMessage(senderPubKey *btcec.PublicKey,
	recipientMailboxID string) []byte {

	pubBytes := senderPubKey.SerializeCompressed()
	msg := make([]byte, 0, len(pubBytes)+len(recipientMailboxID))
	msg = append(msg, pubBytes...)
	msg = append(msg, []byte(recipientMailboxID)...)

	return msg
}

// MailboxAuthDigest constructs the BIP-340 tagged hash digest that a
// client signs to prove ownership of its identity key. The digest is:
//
//	TaggedHash("mailbox-auth", senderPubKey || recipientID)
//
// This follows the BIP-340 tagged hashing convention used throughout
// the codebase, ensuring domain separation and compatibility with
// LND's tagged Schnorr signing RPC.
func MailboxAuthDigest(senderPubKey *btcec.PublicKey,
	recipientMailboxID string) [32]byte {

	msg := MailboxAuthMessage(senderPubKey, recipientMailboxID)
	hash := chainhash.TaggedHash(
		[]byte(MailboxAuthTagStr), msg,
	)

	return *hash
}

// SignMailboxAuth produces a Schnorr signature over the mailbox auth
// digest, proving the caller holds the private key for senderPubKey.
// The returned signature is suitable for setting on
// ConnectorConfig.AuthSignature.
func SignMailboxAuth(privKey *btcec.PrivateKey,
	recipientMailboxID string) (*schnorr.Signature, error) {

	digest := MailboxAuthDigest(
		privKey.PubKey(), recipientMailboxID,
	)

	sig, err := schnorr.Sign(privKey, digest[:])
	if err != nil {
		return nil, fmt.Errorf("schnorr sign: %w", err)
	}

	return sig, nil
}

// VerifyMailboxAuth verifies that sigHex is a valid Schnorr signature
// over the mailbox auth digest for the given sender pubkey and
// recipient mailbox ID. Returns nil on success.
func VerifyMailboxAuth(senderPubKey *btcec.PublicKey, recipientMailboxID string,
	sigHex string) error {

	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return fmt.Errorf("decode auth sig hex: %w", err)
	}

	sig, err := schnorr.ParseSignature(sigBytes)
	if err != nil {
		return fmt.Errorf("parse auth sig: %w", err)
	}

	digest := MailboxAuthDigest(senderPubKey, recipientMailboxID)

	if !sig.Verify(digest[:], senderPubKey) {
		return fmt.Errorf("mailbox auth signature verification failed "+
			"for sender %x", senderPubKey.SerializeCompressed())
	}

	return nil
}

// MailboxTLSBindMessage returns the raw message bytes that are fed
// into the BIP-340 tagged hash for binding a mailbox identity to a
// TLS leaf certificate:
//
//	senderCompressedPubKey || tlsLeafSPKIDER
//
// The leaf is identified by its SubjectPublicKeyInfo DER bytes (i.e.
// the x509.Certificate.RawSubjectPublicKeyInfo field). SPKI is the
// stable, self-describing serialization of the public key plus
// algorithm parameters, and is what TLS uses internally to commit
// to the cert's key in the CertificateVerify proof of possession.
// Hashing the SPKI rather than just the raw key bytes also captures
// the curve/algorithm identifier, so a P-256 leaf cannot be
// confused with a leaf using a different curve carrying the same
// raw coordinates.
func MailboxTLSBindMessage(senderPubKey *btcec.PublicKey,
	tlsLeafSPKI []byte) []byte {

	pubBytes := senderPubKey.SerializeCompressed()
	msg := make([]byte, 0, len(pubBytes)+len(tlsLeafSPKI))
	msg = append(msg, pubBytes...)
	msg = append(msg, tlsLeafSPKI...)

	return msg
}

// MailboxTLSBindDigest constructs the BIP-340 tagged hash digest the
// client signs to bind its secp256k1 identity to the TLS leaf
// SubjectPublicKeyInfo:
//
//	TaggedHash("mailbox-tls-bind", senderPubKey || tlsLeafSPKI)
//
// Using a dedicated tag keeps this digest disjoint from the regular
// MailboxAuthDigest so neither signature can be replayed across the
// two purposes.
func MailboxTLSBindDigest(senderPubKey *btcec.PublicKey,
	tlsLeafSPKI []byte) [32]byte {

	msg := MailboxTLSBindMessage(senderPubKey, tlsLeafSPKI)
	hash := chainhash.TaggedHash(
		[]byte(MailboxTLSBindTagStr), msg,
	)

	return *hash
}

// SignMailboxTLSBind produces a Schnorr signature over the
// mailbox-to-TLS-leaf binding digest, proving the caller holds the
// secp256k1 private key for senderPubKey AND has chosen tlsLeafSPKI
// as the leaf the server should expect to observe on this
// connection.
func SignMailboxTLSBind(privKey *btcec.PrivateKey,
	tlsLeafSPKI []byte) (*schnorr.Signature, error) {

	if len(tlsLeafSPKI) == 0 {
		return nil, fmt.Errorf("tls leaf SPKI must not be empty")
	}

	digest := MailboxTLSBindDigest(privKey.PubKey(), tlsLeafSPKI)

	sig, err := schnorr.Sign(privKey, digest[:])
	if err != nil {
		return nil, fmt.Errorf("schnorr sign tls bind: %w", err)
	}

	return sig, nil
}

// VerifyMailboxTLSBind verifies that sigHex is a valid Schnorr
// signature binding senderPubKey to tlsLeafSPKI. Returns nil on
// success. The caller is responsible for sourcing tlsLeafSPKI from
// the observed TLS connection, not from the envelope or any other
// client-supplied field.
func VerifyMailboxTLSBind(senderPubKey *btcec.PublicKey, tlsLeafSPKI []byte,
	sigHex string) error {

	if len(tlsLeafSPKI) == 0 {
		return fmt.Errorf("tls leaf SPKI must not be empty")
	}

	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return fmt.Errorf("decode tls bind sig hex: %w", err)
	}

	sig, err := schnorr.ParseSignature(sigBytes)
	if err != nil {
		return fmt.Errorf("parse tls bind sig: %w", err)
	}

	digest := MailboxTLSBindDigest(senderPubKey, tlsLeafSPKI)

	if !sig.Verify(digest[:], senderPubKey) {
		return fmt.Errorf("mailbox tls-bind signature verification "+
			"failed for sender %x",
			senderPubKey.SerializeCompressed())
	}

	return nil
}

// ParseMailboxPubKey extracts the public key from a pubkey-derived
// mailbox ID string (hex-encoded compressed SEC pubkey).
func ParseMailboxPubKey(mailboxID string) (*btcec.PublicKey, error) {
	keyBytes, err := hex.DecodeString(mailboxID)
	if err != nil {
		return nil, fmt.Errorf("decode mailbox ID hex: %w", err)
	}

	pubKey, err := btcec.ParsePubKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse mailbox pubkey: %w", err)
	}

	return pubKey, nil
}
