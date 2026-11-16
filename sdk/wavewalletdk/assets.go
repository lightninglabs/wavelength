package wavewalletdk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/lightninglabs/wavelength/waverpc"
)

// AssetUnits is a number of asset units. It is a decimal string in JSON,
// because an asset amount can exceed the largest integer JavaScript reads
// exactly from a JSON number.
type AssetUnits uint64

// MarshalJSON encodes the units as a decimal string.
func (u AssetUnits) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(u), 10))
}

// UnmarshalJSON decodes the units from a decimal string. A JSON number is
// refused, since it may have lost precision on the way.
func (u *AssetUnits) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("asset units must be a decimal string: %w",
			err)
	}

	units, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return fmt.Errorf("asset units %q: %w", text, err)
	}
	*u = AssetUnits(units)

	return nil
}

// AssetReceiveKeys are the keys a sender transfers a Taproot Asset to on
// chain. All keys are hex compressed public keys.
type AssetReceiveKeys struct {
	// ScriptKey is the Taproot output key the asset is sent to.
	ScriptKey string

	// RawScriptKey is the key ScriptKey is the BIP-86 output key of.
	RawScriptKey string

	// AnchorInternalKey is the internal key of the anchor output that
	// carries the asset.
	AnchorInternalKey string
}

// AssetFundingAddress is a backing-wallet address. Bitcoin sent to it pays
// for asset deposits.
type AssetFundingAddress struct {
	Address string
}

// ImportAssetProofRequest adds a received asset to the asset wallet.
type ImportAssetProofRequest struct {
	// ProofFile is the proof file of the asset output.
	ProofFile []byte

	// AssetRef is the asset the caller expects: the asset ID, or the
	// group key of a grouped asset.
	AssetRef string
}

// OwnedAsset is one asset output the asset wallet holds on chain.
type OwnedAsset struct {
	// AssetRef is the group key of a grouped asset, else the asset ID.
	AssetRef string

	// AssetID is the ID of the issuance the units belong to.
	AssetID string

	// Amount is the number of asset units in the output.
	Amount AssetUnits

	// Outpoint is the anchor output that carries the asset.
	Outpoint string

	// ScriptKey is the hex compressed key the asset is locked to.
	ScriptKey string

	// AnchorValueSat is the Bitcoin value of the anchor output.
	AnchorValueSat int64
}

// DepositAssetRequest funds a boarding output holding a Taproot Asset.
type DepositAssetRequest struct {
	// RequestID identifies the deposit; a retry with the same ID resumes
	// it.
	RequestID string

	// AssetRef is the asset ID, or the group key of a grouped asset.
	AssetRef string

	// AssetAmount is the number of asset units to deposit.
	AssetAmount AssetUnits

	// SatPerVByte is the funding fee rate. Zero uses TargetConf.
	SatPerVByte uint64

	// TargetConf is the confirmation target used when SatPerVByte is
	// zero.
	TargetConf uint32

	// MaxFeeSat bounds the funding fee and is required.
	MaxFeeSat uint64
}

// BoardAssetDepositRequest joins the next round with a confirmed deposit.
type BoardAssetDepositRequest struct {
	// RequestID names a confirmed deposit.
	RequestID string
}

// AssetDeposit is one asset deposit and its lifecycle stage.
type AssetDeposit struct {
	RequestID string

	// Status is "funding", "published" or "confirmed".
	Status      string
	AssetRef    string
	AssetAmount AssetUnits

	// Outpoint is the deposit output, empty while funding.
	Outpoint string
	ValueSat int64
	FeeSat   uint64

	// ConfirmedHeight is the height of the confirmed proof, zero before.
	ConfirmedHeight uint32

	// Error is the latest failure, empty when none.
	Error string

	// RoundID is the round that spends the deposit, empty until one does.
	RoundID string

	// ClaimTxid is the transaction that claims the deposit back to the
	// asset backend's wallet, empty unless a claim was signed. A deposit
	// with a claim does not board.
	ClaimTxid string

	// Claimed is set once the asset backend records the claim as final.
	Claimed bool
}

// NewAssetReceiveKeys derives the keys a sender transfers an asset to.
func (c *Client) NewAssetReceiveKeys(ctx context.Context) (*AssetReceiveKeys,
	error) {

	resp, err := c.daemon.NewAssetReceiveKeys(
		ctx, &waverpc.NewAssetReceiveKeysRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("derive asset receive keys: %w", err)
	}

	return &AssetReceiveKeys{
		ScriptKey:    hex.EncodeToString(resp.GetScriptKey()),
		RawScriptKey: hex.EncodeToString(resp.GetRawScriptKey()),
		AnchorInternalKey: hex.EncodeToString(
			resp.GetAnchorInternalKey(),
		),
	}, nil
}

// NewAssetFundingAddress returns a new backing-wallet address. Bitcoin sent
// to it pays for asset deposits: the deposit output's value and the miner
// fee.
func (c *Client) NewAssetFundingAddress(ctx context.Context) (
	*AssetFundingAddress, error) {

	resp, err := c.daemon.NewAssetFundingAddress(
		ctx, &waverpc.NewAssetFundingAddressRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("derive asset funding address: %w", err)
	}

	return &AssetFundingAddress{Address: resp.GetAddress()}, nil
}

// ImportAssetProof verifies a received asset's proof file and adds the asset
// to the asset wallet.
func (c *Client) ImportAssetProof(ctx context.Context,
	req ImportAssetProofRequest) (*OwnedAsset, error) {

	resp, err := c.daemon.ImportAssetProof(
		ctx, &waverpc.ImportAssetProofRequest{
			ProofFile: req.ProofFile,
			AssetRef:  req.AssetRef,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("import asset proof: %w", err)
	}

	return ownedAssetFromProto(resp), nil
}

// ListOwnedAssets returns the assets the asset wallet holds on chain.
func (c *Client) ListOwnedAssets(ctx context.Context) ([]OwnedAsset, error) {
	resp, err := c.daemon.ListOwnedAssets(
		ctx, &waverpc.ListOwnedAssetsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("list owned assets: %w", err)
	}

	assets := make([]OwnedAsset, 0, len(resp.GetAssets()))
	for _, held := range resp.GetAssets() {
		assets = append(assets, *ownedAssetFromProto(held))
	}

	return assets, nil
}

// DepositAsset funds a boarding output holding a Taproot Asset, or resumes
// the deposit with the same request ID.
func (c *Client) DepositAsset(ctx context.Context, req DepositAssetRequest) (
	*AssetDeposit, error) {

	resp, err := c.daemon.DepositAsset(ctx, &waverpc.DepositAssetRequest{
		RequestId:   req.RequestID,
		AssetRef:    req.AssetRef,
		AssetAmount: uint64(req.AssetAmount),
		SatPerVbyte: req.SatPerVByte,
		TargetConf:  req.TargetConf,
		MaxFeeSat:   req.MaxFeeSat,
	})
	if err != nil {
		return nil, fmt.Errorf("deposit asset: %w", err)
	}

	return assetDepositFromProto(resp), nil
}

// ListAssetDeposits returns the asset deposits of the wallet.
func (c *Client) ListAssetDeposits(ctx context.Context) ([]AssetDeposit,
	error) {

	resp, err := c.daemon.ListAssetDeposits(
		ctx, &waverpc.ListAssetDepositsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("list asset deposits: %w", err)
	}

	deposits := make([]AssetDeposit, 0, len(resp.GetDeposits()))
	for _, deposit := range resp.GetDeposits() {
		deposits = append(deposits, *assetDepositFromProto(deposit))
	}

	return deposits, nil
}

// BoardAssetDeposit joins the next round with a confirmed asset deposit.
func (c *Client) BoardAssetDeposit(ctx context.Context,
	req BoardAssetDepositRequest) (*AssetDeposit, error) {

	resp, err := c.daemon.BoardAssetDeposit(
		ctx, &waverpc.BoardAssetDepositRequest{
			RequestId: req.RequestID,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("board asset deposit: %w", err)
	}

	return assetDepositFromProto(resp), nil
}

// ownedAssetFromProto converts an owned asset to its wrapper type.
func ownedAssetFromProto(held *waverpc.OwnedAsset) *OwnedAsset {
	return &OwnedAsset{
		AssetRef:       held.GetAssetRef(),
		AssetID:        held.GetAssetId(),
		Amount:         AssetUnits(held.GetAmount()),
		Outpoint:       held.GetOutpoint(),
		ScriptKey:      hex.EncodeToString(held.GetScriptKey()),
		AnchorValueSat: held.GetAnchorValueSat(),
	}
}

// assetDepositFromProto converts an asset deposit to its wrapper type.
func assetDepositFromProto(deposit *waverpc.AssetDeposit) *AssetDeposit {
	var status string
	switch deposit.GetStatus() {
	case waverpc.AssetDepositStatus_ASSET_DEPOSIT_STATUS_FUNDING:
		status = "funding"

	case waverpc.AssetDepositStatus_ASSET_DEPOSIT_STATUS_PUBLISHED:
		status = "published"

	case waverpc.AssetDepositStatus_ASSET_DEPOSIT_STATUS_CONFIRMED:
		status = "confirmed"

	default:
		status = "unspecified"
	}

	return &AssetDeposit{
		RequestID:       deposit.GetRequestId(),
		Status:          status,
		AssetRef:        deposit.GetAssetRef(),
		AssetAmount:     AssetUnits(deposit.GetAssetAmount()),
		Outpoint:        deposit.GetOutpoint(),
		ValueSat:        deposit.GetValueSat(),
		FeeSat:          deposit.GetFeeSat(),
		ConfirmedHeight: deposit.GetConfirmedHeight(),
		Error:           deposit.GetError(),
		RoundID:         deposit.GetRoundId(),
		ClaimTxid:       deposit.GetClaimTxid(),
		Claimed:         deposit.GetClaimed(),
	}
}
