package swaprpc

import (
	"encoding/binary"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// OutSwapHTLCAckTag is the BIP-340 domain separator for receiver
	// acknowledgements of accepted out-swap vHTLC terms. The committed
	// vHTLC P2TR script binds the swap sender and Ark operator keys through
	// the vHTLC policy, preventing reuse across servers with different
	// keys.
	OutSwapHTLCAckTag = "out-swap-htlc-ack-v1"
)

// OutSwapHTLCAckMessage returns the canonical message committed to by an
// out-swap vHTLC acknowledgement signature.
func OutSwapHTLCAckMessage(receiverKey *btcec.PublicKey,
	paymentHash lntypes.Hash, amountSat uint64,
	vhtlcPkScript []byte) []byte {

	keyBytes := receiverKey.SerializeCompressed()
	msg := make(
		[]byte, 0, len(keyBytes)+len(paymentHash)+8+len(vhtlcPkScript),
	)
	msg = append(msg, keyBytes...)
	msg = append(msg, paymentHash[:]...)
	msg = binary.BigEndian.AppendUint64(msg, amountSat)
	msg = append(msg, vhtlcPkScript...)

	return msg
}

// OutSwapHTLCAckDigest returns the tagged digest for an out-swap vHTLC
// acknowledgement signature.
func OutSwapHTLCAckDigest(receiverKey *btcec.PublicKey,
	paymentHash lntypes.Hash, amountSat uint64,
	vhtlcPkScript []byte) [32]byte {

	msg := OutSwapHTLCAckMessage(
		receiverKey, paymentHash, amountSat, vhtlcPkScript,
	)
	hash := chainhash.TaggedHash([]byte(OutSwapHTLCAckTag), msg)

	return *hash
}
