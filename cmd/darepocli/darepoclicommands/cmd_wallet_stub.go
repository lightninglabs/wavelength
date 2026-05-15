//go:build !walletrpc

package darepoclicommands

import "github.com/spf13/cobra"

// addWalletRPCSubcommands is a no-op in builds without the walletrpc tag.
// The walletrpc-tagged variant in cmd_wallet_rpc.go attaches the
// simplified high-level wallet subcommands (send, recv, list, deposit,
// status) to the same parent so users get one wallet vocabulary when the
// subserver is compiled in.
func addWalletRPCSubcommands(_ *cobra.Command) {}
