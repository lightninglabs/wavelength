//go:build js && wasm

package lwwallet

// testWalletDBBackends lists the wallet database backends the platform
// supports. Browser builds have exactly one: BoltDB cannot be used
// under js/wasm, so Config.DBBackend is ignored there and the store is
// always the OPFS-backed SQLite database.
var testWalletDBBackends = []string{""}
