//go:build mobile && wavewalletrpc && swapruntime

package mobile

import "github.com/lightninglabs/wavelength/sdk/wavewalletdk"

// ExitBatch submits exactly one cooperative selection. The JSON request is
// wavewalletdk.ExitBatchRequest (Outpoints, Destination); the response is
// ExitBatchResult (Results, Destination). Inspect every input's Outcome, even
// on nil error. Unknown outcomes and transport errors require reconciliation,
// never an automatic retry. This call does not support unilateral exit.
func ExitBatch(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.ExitBatchRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	result, err := client.ExitBatch(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(result)
}
