package conn

import (
	"crypto/sha256"
	"encoding/hex"
)

// StableEventMsgID derives a deterministic mailbox message ID from payload.
func StableEventMsgID(payload []byte) string {
	return "evt-" + shortPayloadHash(payload)
}

// StableEventIdempotencyKey derives a deterministic idempotency key from
// payload.
func StableEventIdempotencyKey(payload []byte) string {
	return "idem-" + shortPayloadHash(payload)
}

// shortPayloadHash returns a compact hex-encoded hash suffix for payload.
func shortPayloadHash(payload []byte) string {
	sum := sha256.Sum256(payload)

	// 16 bytes (32 hex chars) is enough for internal dedupe IDs.
	return hex.EncodeToString(sum[:16])
}
