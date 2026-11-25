//go:build js && wasm

package waved

import (
	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/wavelength/db"
)

// assetReceiveConfig returns nil: a browser opens no gRPC connection to a
// proof courier, so the asset engine creates no address there.
func (s *Server) assetReceiveConfig(*db.AssetEngineStore, *assetKeyRing) (
	*embedded.ReceiveConfig, error) {

	return nil, nil
}
