//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// SetNotificationFeedGrant authorizes a subscriber at a configured source.
// JSON follows wavewalletdk.NotificationFeedGrant. The wallet lifecycle owns
// the ten-second deadline and final-access join. This never unlocks a wallet.
func SetNotificationFeedGrant(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	var request wavewalletdk.NotificationFeedGrant
	if err := decode(reqJSON, &request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	response, err := client.SetNotificationFeedGrant(ctx, request)
	if err != nil {
		return nil, err
	}

	return marshal(response)
}
