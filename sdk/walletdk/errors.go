package walletdk

import "errors"

// ErrSwapRuntimeUnavailable reports that walletdk was built without the
// daemon-owned swap executor needed by Send and Receive.
var ErrSwapRuntimeUnavailable = errors.New("walletdk swap runtime " +
	"unavailable; rebuild with -tags swapruntime")
