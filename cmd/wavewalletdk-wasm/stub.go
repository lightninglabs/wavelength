//go:build !js || !wasm

// Command wavewalletdk-wasm is only meaningful as a js/wasm target. This stub
// keeps the package buildable (and `go build ./...` green) on native
// toolchains by providing a main that explains how to build the real binary.
package main

import (
	"fmt"
	"os"
)

// main reports that wavewalletdk-wasm must be built for js/wasm.
func main() {
	fmt.Fprintln(
		os.Stderr, "wavewalletdk-wasm is only supported on "+
			"js/wasm; build with GOOS=js GOARCH=wasm -tags "+
			"\"mobile wavewalletrpc swapruntime\"",
	)
	os.Exit(1)
}
