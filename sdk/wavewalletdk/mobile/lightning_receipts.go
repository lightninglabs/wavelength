//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// ReadLightningReceipts checks a finite set of locally enrolled invoice
// references. The returned array contains only eligible known sources; only
// state complete permits amount presentation. It does not run a host window.
func ReadLightningReceipts(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	var request wavewalletdk.LightningReceiptRequest
	if err := decode(reqJSON, &request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	receipts, err := client.ReadLightningReceipts(ctx, request)
	if err != nil {
		return nil, err
	}

	return marshal(receipts)
}
