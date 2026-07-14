package waved

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// newTestMacaroonService creates a DB-backed macaroon service for tests.
func newTestMacaroonService(t *testing.T, macaroonPath, location string,
	permissions map[string][]bakery.Op) *lndclient.MacaroonService {

	t.Helper()

	sqlDB := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)

	require.NoError(t, os.MkdirAll(filepath.Dir(macaroonPath), 0o700))
	authService, err := lndclient.NewMacaroonService(
		&lndclient.MacaroonServiceConfig{
			RootKeyStore:     dbStore.NewMacaroonRootKeyStore(),
			MacaroonLocation: location,
			MacaroonPath:     macaroonPath,
			Checkers: []macaroons.Checker{
				macaroons.IPLockChecker,
				macaroons.IPRangeLockChecker,
			},
			RequiredPerms: permissions,
		},
	)
	require.NoError(t, err)
	require.NoError(t, authService.Start())
	require.NoError(t, os.Chmod(macaroonPath, 0o600))
	t.Cleanup(func() {
		require.NoError(t, authService.Stop())
	})

	return authService
}
