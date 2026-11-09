//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// ReadActivityBatch returns one finite canonical activity page. Its JSON
// contracts are wavewalletdk.ActivityBatchRequest and ActivityBatch. The host
// persists next_cursor even when it suppresses entries during presentation.
func ReadActivityBatch(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	var request wavewalletdk.ActivityBatchRequest
	if err := decode(reqJSON, &request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	response, err := client.ReadActivityBatch(ctx, request)
	if err != nil {
		return nil, err
	}

	return marshal(response)
}

// ActivityCursor returns {"cursor":number} after the host's own reconciliation
// pass. Capturing a cursor does not initialize or certify that pass.
func ActivityCursor() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	cursor, err := client.ActivityCursor(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(struct {
		Cursor int64 `json:"cursor"`
	}{Cursor: cursor})
}
