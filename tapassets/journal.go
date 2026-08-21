package tapassets

import (
	"context"
	"errors"
)

// ErrStoreNotFound reports that a journal key has no durable value.
var ErrStoreNotFound = errors.New("asset transition state not found")

// Store persists opaque transition state. Implementations must atomically
// replace each value. Transition owners serialize the full commit workflow for
// a key.
type Store interface {
	Load(context.Context, string) ([]byte, error)

	Store(context.Context, string, []byte) error
}

// ErrReconciliationRequired reports a tapd commit that may have succeeded.
// Retrying without reconciling the recorded attempt could spend the same
// asset state twice.
var ErrReconciliationRequired = errors.New("asset transition reconciliation " +
	"required")
