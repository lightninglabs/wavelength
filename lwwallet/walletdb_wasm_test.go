//go:build js && wasm

package lwwallet

import (
	"io"
	"testing"

	"github.com/lightninglabs/wavelength/internal/testutils"
)

// TestWASMWalletOpenRetry checks that walletdb initialization recovers from
// transient contention without hiding permanent storage failures.
func TestWASMWalletOpenRetry(t *testing.T) {
	testutils.CheckWASMSQLiteOpen(t, func() (io.Closer, error) {
		return openWASMWalletDB("retry-test-wallet")
	})
}
