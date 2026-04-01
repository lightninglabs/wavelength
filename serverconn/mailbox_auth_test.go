package serverconn

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestPubKeyMailboxID verifies the canonical mailbox ID derivation
// from a public key matches the expected hex encoding.
func TestPubKeyMailboxID(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKey := privKey.PubKey()
	mailboxID := PubKeyMailboxID(pubKey)

	// The mailbox ID should be 66 hex chars (33 bytes compressed).
	require.Len(t, mailboxID, 66)

	// Round-trip through ParseMailboxPubKey.
	parsed, err := ParseMailboxPubKey(mailboxID)
	require.NoError(t, err)
	require.True(t, pubKey.IsEqual(parsed))
}

// TestSignVerifyMailboxAuth verifies that a valid signature passes
// verification and that a signature from a different key fails.
func TestSignVerifyMailboxAuth(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientID := PubKeyMailboxID(serverKey.PubKey())

	// Sign with the client's key.
	sig, err := SignMailboxAuth(clientKey, recipientID)
	require.NoError(t, err)
	require.NotNil(t, sig)

	sigHex := hex.EncodeToString(sig.Serialize())

	// Verify with the correct sender key succeeds.
	err = VerifyMailboxAuth(clientKey.PubKey(), recipientID, sigHex)
	require.NoError(t, err)

	// Verify with a different sender key fails.
	otherKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	err = VerifyMailboxAuth(otherKey.PubKey(), recipientID, sigHex)
	require.Error(t, err)
	require.Contains(t, err.Error(), "verification failed")
}

// TestVerifyMailboxAuthWrongRecipient verifies that a signature
// bound to one server is rejected when verified against a
// different server's mailbox ID.
func TestVerifyMailboxAuthWrongRecipient(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	server1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	server2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientID1 := PubKeyMailboxID(server1Key.PubKey())
	recipientID2 := PubKeyMailboxID(server2Key.PubKey())

	// Sign for server 1.
	sig, err := SignMailboxAuth(clientKey, recipientID1)
	require.NoError(t, err)

	sigHex := hex.EncodeToString(sig.Serialize())

	// Verify against server 2 should fail.
	err = VerifyMailboxAuth(clientKey.PubKey(), recipientID2, sigHex)
	require.Error(t, err)
}

// TestVerifyMailboxAuthBadHex verifies that malformed hex is
// rejected gracefully.
func TestVerifyMailboxAuthBadHex(t *testing.T) {
	t.Parallel()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	err = VerifyMailboxAuth(key.PubKey(), "recipient", "not-hex!!")
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode auth sig hex")
}

// TestParseMailboxPubKeyInvalid verifies that invalid mailbox IDs
// are rejected.
func TestParseMailboxPubKeyInvalid(t *testing.T) {
	t.Parallel()

	// Not hex.
	_, err := ParseMailboxPubKey("zzz")
	require.Error(t, err)

	// Valid hex but not a valid pubkey.
	_, err = ParseMailboxPubKey("deadbeef")
	require.Error(t, err)
}
