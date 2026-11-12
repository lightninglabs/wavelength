//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"fmt"
	"time"
)

// ProcessNext returns JSON for one bounded native intake attempt. Pass a fresh
// invocation ID and cancel early OS expiry with CancelReceiptWindow(id). The
// result describes recoverable intake, not completed work or safe suspension.
func ProcessNext(id string, timeoutSeconds int64) ([]byte, error) {
	if timeoutSeconds < 1 || timeoutSeconds > 20 {
		return nil, fmt.Errorf("ingress step budget must be 1..20 " +
			"seconds")
	}
	client, parent, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(
		parent, time.Duration(timeoutSeconds)*time.Second,
	)
	defer cancel()
	result, err := client.ProcessNext(ctx, id)
	if err != nil {
		return nil, err
	}

	return marshal(result)
}
