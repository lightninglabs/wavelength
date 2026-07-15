package tapassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrStoreNotFound reports that no durable preparation state exists for a
// request ID.
var ErrStoreNotFound = errors.New("taproot asset preparation not found")

// Store persists opaque preparer state by OOR idempotency key. Implementations
// must replace each value atomically.
type Store interface {
	Load(context.Context, string) ([]byte, error)

	Store(context.Context, string, []byte) error
}

// FileStore is the PoC durable preparation journal. Each request is stored in
// a separate, atomically replaced file whose name is the request ID hash.
type FileStore struct {
	dir string
	mu  sync.Mutex
}

// NewFileStore constructs a file-backed preparation journal.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("taproot asset store directory is " +
			"required")
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create taproot asset store: %w", err)
	}

	return &FileStore{dir: dir}, nil
}

// Load reads one durable preparation value.
func (s *FileStore) Load(ctx context.Context, requestID string) ([]byte,
	error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("taproot asset store is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	value, err := os.ReadFile(s.path(requestID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrStoreNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read taproot asset preparation: %w",
			err)
	}

	return value, nil
}

// Store atomically replaces one durable preparation value.
func (s *FileStore) Store(ctx context.Context, requestID string,
	value []byte) error {

	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("taproot asset store is required")
	}
	if len(value) == 0 {
		return fmt.Errorf("taproot asset preparation value is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	temporary, err := os.CreateTemp(s.dir, ".preparation-*")
	if err != nil {
		return fmt.Errorf("create taproot asset preparation: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryName)
	}()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()

		return fmt.Errorf("secure taproot asset preparation: %w", err)
	}
	if _, err := temporary.Write(value); err != nil {
		_ = temporary.Close()

		return fmt.Errorf("write taproot asset preparation: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()

		return fmt.Errorf("sync taproot asset preparation: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close taproot asset preparation: %w", err)
	}
	// The destination name is the hex encoding of SHA-256(requestID), so
	// requestID cannot select a path despite the conservative taint rule.
	//nolint:gosec
	if err := os.Rename(temporaryName, s.path(requestID)); err != nil {
		return fmt.Errorf("replace taproot asset preparation: %w", err)
	}

	return nil
}

// path maps an untrusted request ID to a fixed-length local filename.
func (s *FileStore) path(requestID string) string {
	digest := sha256.Sum256([]byte(requestID))
	name := hex.EncodeToString(digest[:]) + ".tapoor"

	return filepath.Join(s.dir, name)
}
