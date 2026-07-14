package vtxo

import (
	"github.com/lightninglabs/wavelength/arkrpc"
)

// FlattenListVTXOsByScriptsResponse returns every VTXO carried in the
// indexer response. The response is a flat slice (each VTXO carries its
// own pkScript and outpoint) so consumers that want a per-script or
// per-outpoint index build it locally; the wire format stays neutral
// and we avoid baking one access pattern into the helper API.
func FlattenListVTXOsByScriptsResponse(
	resp *arkrpc.ListVTXOsByScriptsResponse) []*arkrpc.VTXO {

	if resp == nil {
		return nil
	}

	return resp.GetVtxos()
}
