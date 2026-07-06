package bip322

import "github.com/btcsuite/btcd/chainhash/v2"

// bip322TagStr is the BIP-340 tag used for BIP-322 message commitments.
const bip322TagStr = "BIP0322-signed-message"

// bip322MessageTag is the BIP-340 tag used for BIP-322 message commitments.
var bip322MessageTag = []byte(bip322TagStr)

// MessageHash returns the BIP-322 tagged hash of the raw message bytes.
func MessageHash(message []byte) [32]byte {
	taggedHash := chainhash.TaggedHash(bip322MessageTag, message)

	var messageHash [32]byte
	copy(messageHash[:], taggedHash[:])

	return messageHash
}
