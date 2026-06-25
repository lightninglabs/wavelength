//go:build !js || !wasm

// Command walletdk-wasm is only meaningful as a js/wasm target. This stub
// keeps the package buildable (and `go build ./...` green) on native
// toolchains by providing a main that explains how to build the real binary.
package main

import (
	"fmt"
	"os"
)

// main reports that walletdk-wasm must be built for js/wasm.
func main() {
	fmt.Fprintln(
		os.Stderr, "walletdk-wasm is only supported on js/wasm; "+
			"build with GOOS=js GOARCH=wasm -tags \"mobile "+
			"walletdkrpc swapruntime\"",
	)
	os.Exit(1)
}
