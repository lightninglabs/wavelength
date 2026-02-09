package indexer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func randomNonceHex(n int) (string, error) {
	if n <= 0 {
		return "", nil
	}

	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random nonce bytes: %w", err)
	}

	return hex.EncodeToString(b), nil
}
