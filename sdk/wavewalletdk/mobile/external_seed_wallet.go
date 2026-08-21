//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
	"github.com/lightningnetwork/lnd/aezeed"
)

// externalSeedWalletStartRequest is the private mobile/WASM transport envelope
// used by typed host SDKs. Config is the final daemon configuration, including
// its already-selected data directory. SeedEntropy is standard base64 in JSON,
// matching encoding/json's []byte representation.
type externalSeedWalletStartRequest struct {
	Config json.RawMessage `json:"config"`

	SeedEntropy      []byte `json:"seed_entropy"`
	ExpectedIdentity string `json:"expected_identity_pubkey,omitempty"`
	RecoverState     bool   `json:"recover_state,omitempty"`
	RecoveryWindow   uint32 `json:"recovery_window,omitempty"`
}

// StartExternalSeedWallet is the private binding ABI for atomically booting an
// embedded daemon at a host-selected data directory and importing or unlocking
// its internal wallet from already-derived entropy. Typed host SDKs must route
// it through the same lifecycle queue and runtime lock as Start and Stop;
// application code should use that typed API instead of constructing this JSON
// request directly. The request accepts only a nested final config and exactly
// 16 bytes of seed_entropy encoded as standard base64. Wavelength assigns no
// BIP39, derivation-path, account-index, network, or storage-path semantics to
// the entropy.
//
// Only one mobile wallet may be active at a time; call Stop before selecting
// another wallet directory. A successful response means wallet-dependent
// actors and mailbox ingress are online, so the first wallet operation does not
// need a separate readiness poll. Later authenticated terms refreshes and
// wallet-ready hooks are outside this transport's success boundary.
//
// Existing Start callers are unaffected. After this path claims a stopped
// lifecycle, a startup, import, unlock, recovery, or identity-check failure
// resets it to stopped. A call rejected because another wallet is already
// active leaves that wallet running. RecoverState is a per-start restore
// request; after a successful recovery, ordinary starts should leave it false.
func StartExternalSeedWallet(reqJSON []byte) ([]byte, error) {
	var result *wavewalletdk.ExternalSeedWalletOpenResult
	err := startEmbedded(func(startCtx, operationCtx context.Context) (
		*wavewalletdk.Client, error) {

		cfg, req, err := parseExternalSeedWalletStartRequest(reqJSON)
		if err != nil {
			return nil, fmt.Errorf("parse external-seed wallet "+
				"request: %w", err)
		}
		defer clear(req.SeedEntropy)

		client, openResult, err :=
			wavewalletdk.StartExternalSeedWalletWithContexts(
				startCtx, operationCtx, cfg, req,
			)
		if err != nil {
			return nil, fmt.Errorf("start external-seed wallet: %w",
				err)
		}

		result = openResult

		return client, nil
	})
	if err != nil {
		return nil, err
	}

	return marshal(result)
}

// parseExternalSeedWalletStartRequest separates the final startup config from
// the secret wallet entropy. Only the nested config shape is accepted so the
// selected state directory cannot be confused with transport-level fields.
func parseExternalSeedWalletStartRequest(reqJSON []byte) (wavewalletdk.Config,
	wavewalletdk.ExternalSeedWalletRequest, error) {

	var envelope externalSeedWalletStartRequest
	parseFailed := true
	defer func() {
		if parseFailed {
			clear(envelope.SeedEntropy)
		}
	}()

	if err := json.Unmarshal(reqJSON, &envelope); err != nil {
		return wavewalletdk.Config{},
			wavewalletdk.ExternalSeedWalletRequest{}, fmt.Errorf(
				"decode "+
					"request: %w", err)
	}

	nested := bytes.TrimSpace(envelope.Config)
	if len(nested) == 0 || bytes.Equal(nested, []byte("null")) {
		return wavewalletdk.Config{},
			wavewalletdk.ExternalSeedWalletRequest{},
			errors.New("config must be a nested object")
	}

	var mobileCfg mobileConfig
	if err := json.Unmarshal(nested, &mobileCfg); err != nil {
		return wavewalletdk.Config{},
			wavewalletdk.ExternalSeedWalletRequest{}, fmt.Errorf(
				"decode nested "+
					"config: %w", err)
	}
	if mobileCfg.DataDir == "" {
		return wavewalletdk.Config{},
			wavewalletdk.ExternalSeedWalletRequest{}, errors.New(
				"config.data_dir must select the final " +
					"wallet directory")
	}

	cfg, err := parseConfig(string(nested))
	if err != nil {
		return wavewalletdk.Config{},
			wavewalletdk.ExternalSeedWalletRequest{}, err
	}
	if len(envelope.SeedEntropy) != aezeed.EntropySize {
		return wavewalletdk.Config{},
			wavewalletdk.ExternalSeedWalletRequest{}, fmt.Errorf(
				"seed_entropy must decode to exactly %d "+
					"bytes: got %d", aezeed.EntropySize,
				len(envelope.SeedEntropy))
	}

	req := wavewalletdk.ExternalSeedWalletRequest{
		SeedEntropy:            envelope.SeedEntropy,
		ExpectedIdentityPubKey: envelope.ExpectedIdentity,
		RecoverState:           envelope.RecoverState,
		RecoveryWindow:         envelope.RecoveryWindow,
	}
	parseFailed = false

	return cfg, req, nil
}
