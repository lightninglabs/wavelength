package indexer

import (
	"crypto/rand"
	"encoding/hex"
)

func randomNonceHex(n int) string {
	if n <= 0 {
		return ""
	}

	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
