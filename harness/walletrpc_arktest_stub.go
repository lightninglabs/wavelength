//go:build !walletrpc || !swapruntime

package harness

import (
	clientdarepod "github.com/lightninglabs/darepo-client/darepod"
)

// configureClientWalletRPC is the default-build no-op for the optional
// walletrpc subserver wiring. Without both the `walletrpc` and
// `swapruntime` tags set the `swapclientserver` and `swapwallet`
// packages are not even compiled, so the call site in
// `launchClientDaemon` falls through to this no-op and the daemon
// starts without the extra subservers.
func configureClientWalletRPC(_ *clientdarepod.Config) {
}
