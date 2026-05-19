package vtxo

import (
	"encoding/hex"
	"sort"

	"github.com/lightninglabs/darepo-client/arkrpc"
)

// ListVTXOsForScript returns the VTXO bucket matching pkScript.
func ListVTXOsForScript(resp *arkrpc.ListVTXOsByScriptsResponse,
	pkScript []byte) []*arkrpc.VTXO {

	if resp == nil {
		return nil
	}

	bucket := resp.GetVtxosByScript()[hex.EncodeToString(pkScript)]

	return bucket.GetVtxos()
}

// FlattenListVTXOsByScriptsResponse returns all VTXOs in deterministic script
// order for callers that need to inspect every returned script bucket.
func FlattenListVTXOsByScriptsResponse(
	resp *arkrpc.ListVTXOsByScriptsResponse) []*arkrpc.VTXO {

	if resp == nil {
		return nil
	}

	buckets := resp.GetVtxosByScript()
	keys := make([]string, 0, len(buckets))
	total := 0
	for key, bucket := range buckets {
		keys = append(keys, key)
		total += len(bucket.GetVtxos())
	}
	sort.Strings(keys)

	vtxos := make([]*arkrpc.VTXO, 0, total)
	for _, key := range keys {
		vtxos = append(vtxos, buckets[key].GetVtxos()...)
	}

	return vtxos
}
