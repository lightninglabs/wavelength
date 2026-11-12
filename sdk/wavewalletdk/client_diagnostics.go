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

// OORAmount returns a persisted requested outgoing or received incoming value.
// Nil means the source cannot prove an amount, never a zero-value transfer.
func (c *Client) OORAmount(ctx context.Context, sessionID string,
	outgoing bool) (*int64, error) {

	if c == nil || c.readOORAmount == nil {
		return nil, errors.New("OOR amounts require an embedded wallet")
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	return c.readOORAmount(callCtx, sessionID, outgoing)
}
