//go:build js && wasm

package darepod

import (
	"encoding/base64"
	"fmt"
	"syscall/js"
)

const wasmSeedStoragePrefix = "darepod:encrypted-seed:"

// SaveEncryptedSeed stores the encrypted seed in browser localStorage. The
// payload is already password-encrypted by EncryptSeed before this boundary.
func SaveEncryptedSeed(path string, ciphertext []byte) error {
	storage := js.Global().Get("localStorage")
	if storage.IsUndefined() || storage.IsNull() {
		return fmt.Errorf("browser localStorage is unavailable")
	}

	storage.Call(
		"setItem", wasmSeedStoragePrefix+path,
		base64.StdEncoding.EncodeToString(ciphertext),
	)

	return nil
}

// LoadEncryptedSeed reads the encrypted seed ciphertext from browser
// localStorage.
func LoadEncryptedSeed(path string) ([]byte, error) {
	storage := js.Global().Get("localStorage")
	if storage.IsUndefined() || storage.IsNull() {
		return nil, fmt.Errorf("browser localStorage is unavailable")
	}

	value := storage.Call("getItem", wasmSeedStoragePrefix+path)
	if value.IsNull() || value.IsUndefined() {
		return nil, fmt.Errorf("reading seed file %q: not found", path)
	}

	data, err := base64.StdEncoding.DecodeString(value.String())
	if err != nil {
		return nil, fmt.Errorf("decode seed file %q: %w", path, err)
	}

	return data, nil
}

// SeedFileExists returns true if an encrypted seed exists in browser
// localStorage for the network data directory.
func SeedFileExists(networkDir string) bool {
	path := SeedFilePath(networkDir)
	storage := js.Global().Get("localStorage")
	if storage.IsUndefined() || storage.IsNull() {
		return false
	}

	value := storage.Call("getItem", wasmSeedStoragePrefix+path)

	return !value.IsNull() && !value.IsUndefined()
}
