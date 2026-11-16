//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"encoding/json"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// DecodeInvoice validates a complete invoice locally, without requiring a
// running wallet. JSON fields are invoice and network; result is
// InvoiceDetails.
func DecodeInvoice(reqJSON []byte) ([]byte, error) {
	var req struct {
		Invoice string `json:"invoice"`
		Network string `json:"network"`
	}
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return nil, err
	}
	result, err := wavewalletdk.DecodeInvoice(req.Invoice, req.Network)
	if err != nil {
		return nil, err
	}

	return json.Marshal(result)
}
