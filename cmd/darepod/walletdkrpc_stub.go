//go:build !walletdkrpc || !swapruntime

package main

import "github.com/lightninglabs/darepo-client/darepod"

// configureWalletRPC is a no-op in builds that do not include both the
// walletdkrpc and swapruntime build tags. The simplified wallet subserver
// composes the daemon-owned swap subsystem, so it cannot exist without
// swap support.
func configureWalletRPC(_ *darepod.Config) {
}
