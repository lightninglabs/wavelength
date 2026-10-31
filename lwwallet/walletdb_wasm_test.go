//go:build js && wasm

package lwwallet

import (
	"io"
	"testing"

	"github.com/lightninglabs/wavelength/internal/testutils"
)

// testWalletDBBackends lists the wallet database backends the platform
// supports. Browser builds have exactly one: BoltDB cannot be used
// under js/wasm, so Config.DBBackend is ignored there and the store is
// always the OPFS-backed SQLite database.
var testWalletDBBackends = []string{""}

// TestWASMWalletOpenRetry checks that walletdb initialization recovers from
// transient contention without hiding permanent storage failures.
func TestWASMWalletOpenRetry(t *testing.T) {
	testutils.CheckWASMSQLiteOpen(t, func() (io.Closer, error) {
		return openWASMWalletDB("retry-test-wallet")
	})
}
