package wavewalletdk

import (
	"context"
	"errors"

	"github.com/lightninglabs/wavelength/waved"
)

// VTXOAnchors returns public anchoring outputs for one embedded wallet VTXO.
// A cross-commitment OOR output can have several Bitcoin anchors. Unknown
// ancestry returns an empty list, never an invented transaction output index.
func (c *Client) VTXOAnchors(ctx context.Context, outpoint string) (
	[]waved.VTXOAnchor, error) {

	if c == nil || c.readVTXOAnchors == nil {
		return nil, errors.New("VTXO ancestry requires an embedded " +
			"wallet")
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	return c.readVTXOAnchors(callCtx, outpoint)
}
