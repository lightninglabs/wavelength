package indexer

import (
	"crypto/rand"
	"fmt"
)

// randomNonce returns n random bytes suitable for use as a proof nonce.
// It returns an error if n is not positive, since a zero-length nonce
// provides no replay protection.
func randomNonce(n int) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf(
			"nonce length must be positive, got %d", n,
		)
	}

	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read random nonce bytes: %w", err)
	}

	return b, nil
}
