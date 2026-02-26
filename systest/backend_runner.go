//go:build systest

package systest

import "flag"

// backendFlag selects which wallet backend to test. Accepted values
// are "lnd" (default) or "lwwallet". This allows CI to run each
// backend as a separate parallel job via:
//
//	go test -systest.backend=lnd ./systest/...
//	go test -systest.backend=lwwallet ./systest/...
var backendFlag = flag.String(
	"systest.backend", "lnd",
	"wallet backend to test: lnd or lwwallet",
)

// NewBackend creates a new wallet backend based on the
// -systest.backend flag. This is the single entry point for backend
// selection; tests call NewTestClient(h, NewBackend(h)) and the
// flag determines which implementation is used.
func NewBackend(h *E2EHarness) ClientBackend {
	switch *backendFlag {
	case "lwwallet":
		return NewLWBackend(h)

	default:
		return NewLNDBackend(h)
	}
}
