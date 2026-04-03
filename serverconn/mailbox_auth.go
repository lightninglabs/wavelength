package serverconn

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// AuthHeaderKey is the envelope header key that carries the Schnorr
// signature proving the sender holds the private key corresponding
// to their claimed pubkey-derived mailbox ID.
const AuthHeaderKey = "x-mailbox-auth-sig"

// MailboxAuthTagStr is the BIP-340 tagged hash domain separator used
// when constructing the message digest for mailbox authentication
// signatures. This prevents cross-protocol signature reuse.
const MailboxAuthTagStr = "mailbox-auth"

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
func VerifyMailboxAuth(senderPubKey *btcec.PublicKey,
	recipientMailboxID string, sigHex string) error {

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
		return fmt.Errorf("mailbox auth signature verification "+
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
