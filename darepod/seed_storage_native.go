//go:build !js || !wasm

package darepod

import (
	"fmt"
	"os"
	"path/filepath"
)

// SaveEncryptedSeed writes the encrypted seed ciphertext to disk at the given
// path. The file is created with restrictive permissions to prevent
// unauthorized access.
func SaveEncryptedSeed(path string, ciphertext []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating directory %q: %w", dir, err)
	}

	if err := os.WriteFile(path, ciphertext, 0600); err != nil {
		return fmt.Errorf("writing seed file %q: %w", path, err)
	}

	return nil
}

// LoadEncryptedSeed reads the encrypted seed ciphertext from disk.
func LoadEncryptedSeed(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304
	if err != nil {
		return nil, fmt.Errorf("reading seed file %q: %w", path, err)
	}

	return data, nil
}

// SeedFileExists returns true if an encrypted seed file exists at the expected
// path within the network data directory.
func SeedFileExists(networkDir string) bool {
	path := SeedFilePath(networkDir)

	_, err := os.Stat(path)

	return err == nil
}
