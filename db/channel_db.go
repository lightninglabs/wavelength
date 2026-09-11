package db

import (
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
)

// OpenChannelDB opens application-owned LND state on the platform SQL backend.
// The caller must close the returned database after stopping its runtime.
func OpenChannelDB(dataDir string) (*channeldb.DB, error) {
	backend, err := openChannelBackend(dataDir)
	if err != nil {
		return nil, fmt.Errorf("open channel SQL backend: %w", err)
	}
	store, err := channeldb.CreateWithBackend(backend)
	if err != nil {
		_ = backend.Close()

		return nil, fmt.Errorf("initialize channel database: %w", err)
	}

	return store, nil
}
