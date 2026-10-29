//go:build js && wasm

package db

import (
	"io"
	"testing"

	"github.com/lightninglabs/wavelength/internal/testutils"
)

// TestWASMSQLiteOpenRetry checks reload-time contention and the failure budget
// through the same persistent opener used by daemon stores.
func TestWASMSQLiteOpenRetry(t *testing.T) {
	testutils.CheckWASMSQLiteOpen(t, func() (io.Closer, error) {
		result, err := OpenSQLiteDatabase(SQLiteOpenConfig{
			DatabaseFileName: "retry-test.db",
		})
		if err != nil {
			return nil, err
		}

		return result.DB, nil
	})
}
