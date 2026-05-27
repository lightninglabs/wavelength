package walletdk

import "errors"

// ErrWalletRPCUnavailable reports that an embedded walletdk runtime was built
// without the wallet RPC subserver needed by wallet payment methods.
var ErrWalletRPCUnavailable = errors.New("walletdk wallet rpc unavailable; " +
	"rebuild with -tags walletdkrpc,swapruntime")

// ErrSwapRuntimeUnavailable is retained as a compatibility sentinel for code
// that still checks the old swapruntime-only error.
var ErrSwapRuntimeUnavailable = ErrWalletRPCUnavailable
