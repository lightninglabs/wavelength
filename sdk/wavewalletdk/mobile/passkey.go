//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"encoding/hex"
	"fmt"
)

// OpenWalletFromPasskey imports or unlocks the embedded wallet from a WebAuthn
// passkey PRF output. reqJSON decodes to a {prfOutput} object carrying the
// hex-encoded PRF bytes the host obtained from the passkey ceremony; the
// response is wavewalletdk.OpenWalletResult. The PRF→seed derivation stays in
// Go so the wasm and gomobile bindings share one source of truth and the
// browser never handles raw seed material.
func OpenWalletFromPasskey(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req struct {
		// PRFOutput is the hex-encoded WebAuthn PRF assertion output.
		// The JSON key matches the camelCase the browser bridge sends.
		PRFOutput string `json:"prfOutput"`
	}
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	prf, err := hex.DecodeString(req.PRFOutput)
	if err != nil {
		return nil, fmt.Errorf("decode passkey prf output: %w", err)
	}

	res, err := client.OpenWalletFromPasskey(ctx, prf)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}
