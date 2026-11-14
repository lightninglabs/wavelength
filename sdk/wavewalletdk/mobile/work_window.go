//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// RunWorkWindow runs trusted local receive references and restricted mailbox
// work within one host lease. OS expiry uses CancelReceiptWindow with the same
// unique ID. The returned JSON never contains receipt proof or an amount.
func RunWorkWindow(reqJSON []byte, timeoutSeconds int64) ([]byte, error) {
	if timeoutSeconds < 1 || timeoutSeconds > 30 {
		return nil, fmt.Errorf("work window budget must be 1..30 " +
			"seconds")
	}
	client, parent, err := activeClient()
	if err != nil {
		return nil, err
	}
	var request wavewalletdk.WorkWindowRequest
	if err := decode(reqJSON, &request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(
		parent, time.Duration(timeoutSeconds)*time.Second,
	)
	defer cancel()
	result, err := client.RunWorkWindow(ctx, request)
	if err != nil {
		return nil, err
	}

	return marshal(result)
}
