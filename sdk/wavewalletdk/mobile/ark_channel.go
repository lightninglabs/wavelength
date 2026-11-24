//go:build mobile && wavewalletrpc && swapruntime

package mobile

import "github.com/lightninglabs/wavelength/sdk/wavewalletdk"

// PromoteVTXO promotes wallet liquidity using a caller-owned idempotency key.
// The JSON types are wavewalletdk.PromoteVTXORequest and PromoteVTXOResult.
func PromoteVTXO(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}
	var req wavewalletdk.PromoteVTXORequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}
	result, err := client.PromoteVTXO(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(result)
}

// PayOnActiveChannel pays an invoice directly using outbound channel balance.
// JSON is wavewalletdk.PayOnActiveChannelRequest and PayOnActiveChannelResult.
func PayOnActiveChannel(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}
	var req wavewalletdk.PayOnActiveChannelRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}
	result, err := client.PayOnActiveChannel(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(result)
}
