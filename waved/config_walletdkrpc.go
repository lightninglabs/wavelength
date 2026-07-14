//go:build walletdkrpc

package waved

// defaultEagerRoundJoin returns the default Config.EagerRoundJoin value for
// the walletdkrpc-tagged build. Wallet-shaped hosts expect freshly confirmed
// deposits and cooperative-leave intents to flow into a round join without a
// follow-up Board / LeaveVTXOs RPC, so the default flips to true. Operators
// that need the batched semantics can still pass --eagerroundjoin=false (or
// the config / env equivalent); viper precedence wins over this default.
func defaultEagerRoundJoin() bool {
	return true
}
