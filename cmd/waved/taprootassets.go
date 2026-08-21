package main

import (
	"github.com/lightninglabs/wavelength/waved"
)

// configureTaprootAssets delegates production runtime registration to waved so
// command and embedded consumers install the same tapd lifecycle.
func configureTaprootAssets(cfg *waved.Config) {
	waved.ConfigureTaprootAssets(cfg)
}
