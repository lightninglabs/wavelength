package wasmwallet

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeGo creates the module cache only during compilation, reproducing a fresh
// runner where resolving the asset directory before the build returns nothing.
const fakeGo = `#!/bin/sh
set -eu
case "$1" in
  build)
    mkdir -p 'module cache/assets' 'module cache/bridge' goroot/lib/wasm
    for f in sqlite3.js sqlite3.wasm sqlite3-opfs-async-proxy.js; do
      printf '%s\n' "$f" > "module cache/assets/$f"
    done
    for f in sqlite-bridge.js sqlite-worker.js sqlite-node-vfs.js; do
      printf '%s\n' "$f" > "module cache/bridge/$f"
    done
    echo runtime > goroot/lib/wasm/wasm_exec.js
    echo wasm > bin/wasm/wavewalletdk.wasm
    ;;
  env) printf '%s/goroot\n' "$PWD" ;;
  list)
    case "$LOOKUP" in
      failure) exit 7 ;;
      empty) exit 0 ;;
    esac
    test -d 'module cache'
    printf '%s/module cache\n' "$PWD"
    ;;
  *) exit 1 ;;
esac
`

// TestWasmAssetStaging exercises the real Makefile recipe without downloading
// or compiling dependencies, including paths with spaces and failed module
// lookups.
func TestWasmAssetStaging(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}

	// Keep the fixture tied to the production recipe rather than
	// duplicating the ordering whose cold-cache regression we need to
	// catch.
	_, recipe, ok := strings.Cut(string(makefile), "WASM_WALLET_OUT :=")
	if !ok {
		t.Fatal("WASM output definition not found")
	}
	recipe, _, ok = strings.Cut(recipe, "\nwasm-publish:")
	if !ok {
		t.Fatal("WASM recipe boundary not found")
	}
	recipe = "GOCC := ./fake-go\nGOBUILD := ./fake-go build\n" +
		"RM := rm -f\nWASM_WALLET_OUT :=" + recipe

	for _, lookup := range []string{"success", "failure", "empty"} {
		t.Run(lookup, func(t *testing.T) {
			root := t.TempDir()
			err := os.WriteFile(
				filepath.Join(root, "Makefile"), []byte(recipe),
				0600,
			)
			if err != nil {
				t.Fatal(err)
			}
			err = os.WriteFile(
				filepath.Join(root, "fake-go"), []byte(fakeGo),
				0700,
			)
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(
				t.Context(),
				"make", "--silent", "wasm-wallet",
			)
			cmd.Dir = root
			t.Setenv("LOOKUP", lookup)
			output, err := cmd.CombinedOutput()
			if lookup != "success" {
				if err == nil {
					t.Fatalf("expected lookup failure: %s",
						output)
				}

				// Quiet Make suppresses recipe echoes, so this
				// detects an attempted copy, not the recipe
				// text.
				if bytes.Contains(output, []byte("/assets/")) {
					t.Fatalf("lookup reached asset "+
						"copy: %s", output)
				}

				return
			}
			if err != nil {
				t.Fatalf("asset staging failed: %v\n%s", err,
					output)
			}

			assertWasmAssets(t, filepath.Join(root, "bin", "wasm"))
		})
	}
}

// assertWasmAssets checks the complete bundle and the source asset contents so
// a successful command alone cannot hide missing or incorrectly copied files.
func assertWasmAssets(t *testing.T, bundle string) {
	t.Helper()

	sqliteAssets := []string{
		"sqlite3.js", "sqlite3.wasm", "sqlite3-opfs-async-proxy.js",
		"sqlite-bridge.js", "sqlite-worker.js", "sqlite-node-vfs.js",
	}
	expected := append([]string{
		"wavewalletdk.wasm", "wavewalletdk.wasm.gz", "wasm_exec.js",
	}, sqliteAssets...)
	slices.Sort(expected)

	entries, err := os.ReadDir(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, entry := range entries {
		actual = append(actual, entry.Name())
	}
	if !slices.Equal(actual, expected) {
		t.Fatalf("bundle files: got %v, want %v", actual, expected)
	}

	for _, name := range sqliteAssets {
		content, err := os.ReadFile(filepath.Join(bundle, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != name+"\n" {
			t.Fatalf("incorrect asset contents for %s: %q", name,
				content)
		}
	}
}
