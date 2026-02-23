package indexer

import (
	"crypto/rand"
	"fmt"
)

// randomNonce returns n random bytes suitable for use as a proof nonce.
func randomNonce(n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}

	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read random nonce bytes: %w", err)
	}

	return b, nil
}
