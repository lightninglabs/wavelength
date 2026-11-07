//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// SetNotificationSubscription authorizes one app-issued relay route through
// the active wallet's mailbox signer. The JSON contract is
// wavewalletdk.NotificationSubscription. The ten-second deadline derives from
// the wallet lifecycle, so Stop cancels it. A timed-out mutation can have
// committed; retry the same logical subscription with a fresh invocation.
// Never log the request, which contains private bearer capabilities.
func SetNotificationSubscription(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	var request wavewalletdk.NotificationSubscription
	if err := decode(reqJSON, &request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	response, err := client.SetNotificationSubscription(ctx, request)
	if err != nil {
		return nil, err
	}

	return marshal(response)
}
