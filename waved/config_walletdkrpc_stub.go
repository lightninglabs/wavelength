//go:build !walletdkrpc

package waved

// defaultEagerRoundJoin returns the default Config.EagerRoundJoin value for
// the standalone non-walletdkrpc build. Operator-driven hosts (wavecli,
// server deployments) rely on the batched semantics, so the default stays
// off. Hosts that need eager round-joining can opt in via --eagerroundjoin
// (or the config / env equivalent).
func defaultEagerRoundJoin() bool {
	return false
}
