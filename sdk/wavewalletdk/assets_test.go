//go:build !js

package wavewalletdk

import (
	"encoding/json"
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestAssetsEnabledOverride verifies embedded hosts can turn on Taproot
// Assets, and that unset convenience fields keep a caller-owned daemon
// config.
func TestAssetsEnabledOverride(t *testing.T) {
	t.Parallel()

	t.Run("default disables assets", func(t *testing.T) {
		t.Parallel()

		daemonCfg, err := daemonConfig(DefaultConfig())
		require.NoError(t, err)
		require.False(t, daemonCfg.Assets.Enabled)
	})

	t.Run("convenience override", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		cfg.AssetsEnabled = true
		cfg.AssetGroupKeys = []string{"02aa"}
		daemonCfg, err := daemonConfig(cfg)
		require.NoError(t, err)
		require.True(t, daemonCfg.Assets.Enabled)
		require.Equal(t, []string{"02aa"}, daemonCfg.Assets.GroupKeys)
	})

	t.Run("unset preserves daemon config", func(t *testing.T) {
		t.Parallel()

		base := waved.DefaultConfig()
		base.Assets.Enabled = true
		base.Assets.GroupKeys = []string{"03bb"}
		daemonCfg, err := daemonConfig(Config{DaemonConfig: base})
		require.NoError(t, err)
		require.True(t, daemonCfg.Assets.Enabled)
		require.Equal(t, []string{"03bb"}, daemonCfg.Assets.GroupKeys)

		// The daemon config is a copy, not the caller's slice.
		daemonCfg.Assets.GroupKeys[0] = "changed"
		require.Equal(t, []string{"03bb"}, base.Assets.GroupKeys)
	})
}

// TestAssetCourierOverride verifies embedded hosts can name the proof
// courier of the wallet's Taproot Asset addresses, and that the daemon names
// none unless told to.
func TestAssetCourierOverride(t *testing.T) {
	t.Parallel()

	const (
		courier  = "authmailbox+universerpc://tapd:10029"
		endpoint = "tapd:10029=127.0.0.1:12029"
	)

	daemonCfg, err := daemonConfig(DefaultConfig())
	require.NoError(t, err)
	require.Empty(t, daemonCfg.Assets.Courier.Addr)
	require.Empty(t, daemonCfg.Assets.Courier.Endpoints)

	cfg := DefaultConfig()
	cfg.AssetCourierAddr = courier
	cfg.AssetCourierEndpoints = []string{endpoint}
	daemonCfg, err = daemonConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, courier, daemonCfg.Assets.Courier.Addr)
	require.Equal(
		t, []string{endpoint}, daemonCfg.Assets.Courier.Endpoints,
	)
	require.False(t, daemonCfg.Assets.Enabled)

	// Unset fields keep a caller-owned daemon config, as a copy.
	base := waved.DefaultConfig()
	base.Assets.Courier.Addr = courier
	base.Assets.Courier.Endpoints = []string{endpoint}
	daemonCfg, err = daemonConfig(Config{DaemonConfig: base})
	require.NoError(t, err)
	require.Equal(t, courier, daemonCfg.Assets.Courier.Addr)
	daemonCfg.Assets.Courier.Endpoints[0] = "changed"
	require.Equal(t, []string{endpoint}, base.Assets.Courier.Endpoints)
}

// TestAssetReceiveFromProto maps an address and a receive with its
// lifecycle stage to the wrapper types.
func TestAssetReceiveFromProto(t *testing.T) {
	t.Parallel()

	addr := assetAddressFromProto(&waverpc.AssetAddress{
		Address:          "taprt1addr",
		AssetRef:         "tapr1ref",
		ProofCourierAddr: "authmailbox+universerpc://tapd:10029",
	})
	require.Equal(t, &AssetAddress{
		Address:          "taprt1addr",
		AssetRef:         "tapr1ref",
		ProofCourierAddr: "authmailbox+universerpc://tapd:10029",
	}, addr)

	confirmed := waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_CONFIRMED
	receive := assetReceiveFromProto(&waverpc.AssetReceive{
		Outpoint:           "aa:1",
		Address:            "taprt1addr",
		Status:             confirmed,
		AssetRef:           "tapr1ref",
		AssetId:            "aabb",
		Amount:             1<<53 + 1,
		ConfirmationHeight: 120,
	})
	require.Equal(t, &AssetReceive{
		Outpoint:           "aa:1",
		Address:            "taprt1addr",
		Status:             "confirmed",
		AssetRef:           "tapr1ref",
		AssetID:            "aabb",
		Amount:             1<<53 + 1,
		ConfirmationHeight: 120,
	}, receive)

	var encoded any = *receive
	receiveJSON, err := json.Marshal(encoded)
	require.NoError(t, err)
	require.Contains(
		t, string(receiveJSON), `"Amount":"9007199254740993"`,
	)

	detected := waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_DETECTED
	failed := waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_FAILED
	for rpcStatus, status := range map[waverpc.AssetReceiveStatus]string{
		detected: "detected",
		failed:   "failed",
		0:        "unspecified",
	} {
		require.Equal(
			t, status, assetReceiveFromProto(
				&waverpc.AssetReceive{Status: rpcStatus},
			).Status,
		)
	}
}

// TestAssetDepositFromProto maps a deposit and its lifecycle stage to the
// wrapper type.
func TestAssetDepositFromProto(t *testing.T) {
	t.Parallel()

	confirmed := waverpc.AssetDepositStatus_ASSET_DEPOSIT_STATUS_CONFIRMED
	deposit := assetDepositFromProto(&waverpc.AssetDeposit{
		RequestId:       "alice-1",
		Status:          confirmed,
		AssetRef:        "tapr1ref",
		AssetAmount:     600,
		Outpoint:        "aa:0",
		ValueSat:        4_000,
		FeeSat:          300,
		ConfirmedHeight: 120,
		RoundId:         "round-1",
		ClaimTxid:       "bb",
		Claimed:         true,
	})
	require.Equal(t, &AssetDeposit{
		RequestID:       "alice-1",
		Status:          "confirmed",
		AssetRef:        "tapr1ref",
		AssetAmount:     600,
		Outpoint:        "aa:0",
		ValueSat:        4_000,
		FeeSat:          300,
		ConfirmedHeight: 120,
		RoundID:         "round-1",
		ClaimTxid:       "bb",
		Claimed:         true,
	}, deposit)

	require.Equal(
		t, "unspecified",
		assetDepositFromProto(&waverpc.AssetDeposit{}).Status,
	)

	owned := ownedAssetFromProto(&waverpc.OwnedAsset{
		AssetRef:       "tapr1ref",
		AssetId:        "aabb",
		Amount:         400,
		Outpoint:       "cc:1",
		ScriptKey:      []byte{0x02, 0xab},
		AnchorValueSat: 1_000,
	})
	require.Equal(t, &OwnedAsset{
		AssetRef:       "tapr1ref",
		AssetID:        "aabb",
		Amount:         400,
		Outpoint:       "cc:1",
		ScriptKey:      "02ab",
		AnchorValueSat: 1_000,
	}, owned)
}

// TestAssetUnitsJSON checks that an asset amount above the largest integer
// JavaScript reads exactly from a JSON number survives the JSON encoding of
// the asset types, as a decimal string.
func TestAssetUnitsJSON(t *testing.T) {
	t.Parallel()

	const (
		amount  = AssetUnits(1<<53 + 1)
		encoded = `"9007199254740993"`
	)

	var owned any = OwnedAsset{Amount: amount}
	ownedJSON, err := json.Marshal(owned)
	require.NoError(t, err)
	require.Contains(t, string(ownedJSON), `"Amount":`+encoded)

	var deposit any = AssetDeposit{AssetAmount: amount}
	depositJSON, err := json.Marshal(deposit)
	require.NoError(t, err)
	require.Contains(t, string(depositJSON), `"AssetAmount":`+encoded)

	var req any = &DepositAssetRequest{}
	err = json.Unmarshal(
		[]byte(`{"AssetAmount":`+encoded+`,"MaxFeeSat":500}`), req,
	)
	require.NoError(t, err)
	require.Equal(t, &DepositAssetRequest{
		AssetAmount: amount,
		MaxFeeSat:   500,
	}, req)

	// A JSON number may already have lost precision, so it is refused,
	// and so is a string that is not a decimal uint64.
	var units AssetUnits
	require.Error(t, json.Unmarshal([]byte(`9007199254740993`), &units))
	require.Error(t, json.Unmarshal([]byte(`"-1"`), &units))
	require.Error(t, json.Unmarshal([]byte(`"1.5"`), &units))
	require.NoError(t, json.Unmarshal(
		[]byte(`"18446744073709551615"`), &units,
	))
	require.EqualValues(t, uint64(1<<64-1), units)
}
