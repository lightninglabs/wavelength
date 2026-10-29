//go:build js && wasm

package testutils

import (
	"io"
	"syscall/js"
	"testing"

	"github.com/lightninglabs/go-wasmsqlite"
	"github.com/stretchr/testify/require"
)

// CheckWASMSQLiteOpen exercises a persistent database opener through the real
// Go driver, injecting controlled failures at its JavaScript bridge.
// It changes the global bridge, so callers must not run it in parallel.
func CheckWASMSQLiteOpen(t *testing.T, open func() (io.Closer, error)) {
	t.Helper()

	tests := []struct {
		name     string
		message  string
		failures int
		attempts int
		wantErr  bool
	}{
		{
			name: "busy releases", message: "SQLITE_BUSY",
			failures: 2, attempts: 3,
		},
		{
			name:     "plain lock releases",
			message:  "database is locked by another process",
			failures: 2, attempts: 3,
		},
		{
			name: "cantopen releases", message: "SQLITE_CANTOPEN",
			failures: 2, attempts: 3,
		},
		{
			name:     "plain cantopen releases",
			message:  "unable to open database file",
			failures: 2, attempts: 3,
		},
		{
			name: "lock remains", message: "SQLITE_BUSY",
			failures: 100, attempts: 25, wantErr: true,
		},
		{
			name: "storage unavailable",
			message: "persistent storage required but " +
				"no durable VFS",
			failures: 100, attempts: 1, wantErr: true,
		},
		{
			name: "unrelated error", message: "SQLITE_CORRUPT",
			failures: 100, attempts: 1, wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Promise results follow the driver's bridge protocol;
			// the actual SQL engine is irrelevant to open retries.
			bridge := js.Global().Get("Function").New(
				"message", "failures", "version", `
const bridge = {
  attempts: 0,
  closes: 0,
  async init() {
    return {ok: true, bridgeProtocolVersion: version,
      workerProtocolVersion: version};
  },
  async open(options) {
    bridge.attempts++;
    if (!options.requirePersistent) {
      throw new Error("opener did not require persistence");
    }
    if (bridge.attempts <= failures) throw new Error(message);
    return {dbId: "test", vfsType: options.vfs};
  },
  async query() { return {columns: ["1"], rows: [[1]]}; },
  async exec() { return {rowsAffected: 0}; },
  async close() { bridge.closes++; }
};
return bridge;`,
			).Invoke(
				test.message, test.failures,
				wasmsqlite.ProtocolVersion,
			)

			previous := js.Global().Get("sqliteBridge")
			js.Global().Set("sqliteBridge", bridge)
			t.Cleanup(func() {
				js.Global().Set("sqliteBridge", previous)
			})

			db, err := open()
			if test.wantErr {
				require.ErrorContains(t, err, test.message)
				require.Nil(t, db)
			} else {
				require.NoError(t, err)
				require.NoError(t, db.Close())
				require.Equal(t, 1, bridge.Get("closes").Int())
			}
			require.Equal(
				t, test.attempts, bridge.Get("attempts").Int(),
			)
		})
	}
}
