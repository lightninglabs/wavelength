//go:build walletrpc && swapruntime

package harness

import (
	clientdarepod "github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/swapclientserver"
	"github.com/lightninglabs/darepo-client/swapwallet"
)

// configureClientWalletRPC attaches the optional swap and walletrpc
// subserver registrars to a client daemon config built by the harness.
//
// The behaviour mirrors `cmd/darepod`'s `configureSwapRuntime` +
// `configureWalletRPC` helpers: the harness-spawned darepod registers the
// same gRPC subservers as a tag-built `cmd/darepod` binary would, so the
// top-level walletrpc verbs (`balance`, `recv`, `send`, `list`, `create`,
// `unlock`, `mcp`) work against arktest just like they would against a
// production walletrpc-tagged daemon.
//
// Build-tag rationale: this file only compiles when both `walletrpc` and
// `swapruntime` are set, matching the gates on `swapclientserver`,
// `swapwallet`, and the corresponding `cmd/darepod` helpers. The
// matching stub file ([walletrpc_arktest_stub.go]) provides a no-op
// implementation for builds that omit either tag, so the call site in
// `launchClientDaemon` stays untouched.
//
// Ordering note: swapwallet.Register must run AFTER swapclientserver.Register
// because it reads `cfg.Swap.Backend`, which swapclientserver publishes
// on registration. We append in that order; the daemon walks
// `RPCServiceRegistrars` in slice order at startup.
//
// SuppressResume note: the wallet subserver owns the unified resume sweep
// across both walletrpc-pending and swap-pending sessions, so we set
// `cfg.Swap.SuppressResume = true` to tell swapclientserver to skip its
// own synchronous resume. Same handshake as cmd/darepod.
func configureClientWalletRPC(cfg *clientdarepod.Config) {
	if cfg.Swap == nil {
		cfg.Swap = &clientdarepod.SwapConfig{}
	}
	cfg.Swap.SuppressResume = true

	cfg.RPCServiceRegistrars = append(
		cfg.RPCServiceRegistrars, swapclientserver.Register,
		swapwallet.Register,
	)
	cfg.RPCGatewayRegistrars = append(
		cfg.RPCGatewayRegistrars, swapclientserver.RegisterGateway,
		swapwallet.RegisterGateway,
	)
}
