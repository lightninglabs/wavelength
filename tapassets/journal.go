package tapassets

import (
	"context"
	"errors"
)

// ErrStoreNotFound reports that a journal key has no durable value.
var ErrStoreNotFound = errors.New("asset transition state not found")

// Store persists completed transition packages. Implementations must replace
// each value atomically.
type Store interface {
	Load(context.Context, string) ([]byte, error)

	Store(context.Context, string, []byte) error
}
