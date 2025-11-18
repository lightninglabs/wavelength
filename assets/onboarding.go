package assets

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// OnboardingMode defines the type of onboarding anchor.
type OnboardingMode int

const (
	// OnboardingModeMuSig2 uses MuSig2 keyspend for cooperative path and
	// CSV scriptspend for timeout.
	//
	// Anchor structure:
	//   - InternalKey: MuSig2(userInternal, operatorInternal)
	//   - Tapscript: CSV timeout path
	OnboardingModeMuSig2 OnboardingMode = iota

	// OnboardingModeScriptOnly uses NUMS internal key (no keyspend) with
	// two scriptspend paths: cooperative multisig and CSV timeout.
	//
	// Anchor structure:
	//   - InternalKey: NUMS (forces scriptspend)
	//   - Tapscript: 1) CHECKSIGADD cooperative path
	//                2) CSV timeout path
	OnboardingModeScriptOnly
)

// OnboardingKit contains all information needed to create and operate an asset
// onboarding transaction. It supports two modes:
//
//  1. MuSig2 mode: Standard onboarding with MuSig2 keyspend + CSV timeout
//  2. Script-only mode: NUMS anchor with CHECKSIGADD cooperative + CSV timeout
//
// The mode is determined at creation time and affects the anchor structure.
type OnboardingKit struct {
	// Mode determines the anchor structure (MuSig2 or script-only).
	Mode OnboardingMode

	// UserKey is the user's public key used for both cooperative signing
	// and timeout sweep path.
	UserKey *btcec.PublicKey

	// OperatorKey is the operator's public key used for cooperative
	// signing.
	OperatorKey *btcec.PublicKey

	// KeyLocator is the locator of the user's key (for timeout sweep).
	KeyLocator keychain.KeyLocator

	// AssetID is the identifier of the asset being onboarded.
	AssetID asset.ID

	// Amount is the asset amount being onboarded.
	Amount uint64

	// CsvExpiry is the relative timelock (in blocks) for the timeout path.
	CsvExpiry uint32

	// MuSig2Key is the aggregate key of user + operator (for cooperative
	// spend in MuSig2 mode). Nil for script-only mode.
	MuSig2Key *musig2.AggregateKey

	// InternalKey is the anchor internal key. For MuSig2 mode, this is the
	// MuSig2 aggregate key (pre-BIP86-tweak). For script-only mode, this
	// is NUMS.
	InternalKey *btcec.PublicKey

	// chainParams is the chain parameters.
	chainParams *address.ChainParams
}

// NewOnboardingKit creates a new onboarding kit in MuSig2 mode.
//
// The MuSig2 aggregate key is computed from the user and operator keys with a
// BIP86 taproot tweak, following the standard MuSig2 protocol.
func NewOnboardingKit(userKey, operatorKey *btcec.PublicKey,
	keyLocator keychain.KeyLocator, assetID asset.ID, amount uint64,
	csvExpiry uint32, chainParams *address.ChainParams) (
	*OnboardingKit, error) {

	// Combine the user and operator keys using MuSig2.
	sortKeys := true
	muSig2Key, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{userKey, operatorKey},
		sortKeys,
		&input.MuSig2Tweaks{
			TaprootBIP0086Tweak: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("combine musig2 keys: %w", err)
	}

	return &OnboardingKit{
		UserKey:     userKey,
		OperatorKey: operatorKey,
		KeyLocator:  keyLocator,
		AssetID:     assetID,
		Amount:      amount,
		CsvExpiry:   csvExpiry,
		MuSig2Key:   muSig2Key,
		Mode:        OnboardingModeMuSig2,
		InternalKey: muSig2Key.PreTweakedKey,
		chainParams: chainParams,
	}, nil
}

// NewScriptOnlyOnboardingKit creates a new onboarding kit in script-only mode.
//
// This mode uses NUMS as the internal key (forcing scriptspend) with two
// spending paths:
//  1. Cooperative: CHECKSIGADD requiring both user and operator signatures
//  2. Timeout: CSV allowing user to recover after expiry
func NewScriptOnlyOnboardingKit(userKey, operatorKey *btcec.PublicKey,
	keyLocator keychain.KeyLocator, assetID asset.ID, amount uint64,
	csvExpiry uint32, chainParams *address.ChainParams) (*OnboardingKit,
	error) {

	return &OnboardingKit{
		UserKey:     userKey,
		OperatorKey: operatorKey,
		KeyLocator:  keyLocator,
		AssetID:     assetID,
		Amount:      amount,
		CsvExpiry:   csvExpiry,
		MuSig2Key:   nil,
		Mode:        OnboardingModeScriptOnly,
		InternalKey: asset.NUMSPubKey,
		chainParams: chainParams,
	}, nil
}

// GenTimeoutPathScript generates the timeout path script for the onboarding.
//
// Script structure:
//
//	<UserKey> OP_CHECKSIGVERIFY <CsvExpiry> OP_CHECKSEQUENCEVERIFY
//
// This allows the user to sweep back their asset after the CSV expiry if the
// operator is unresponsive.
func (k *OnboardingKit) GenTimeoutPathScript() ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	// Add user's key for signature verification.
	builder.AddData(schnorr.SerializePubKey(k.UserKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)

	// Add CSV timelock.
	builder.AddInt64(int64(k.CsvExpiry))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	return builder.Script()
}

// GenCooperativePathScript generates the cooperative multisig script for
// script-only mode.
//
// Script structure (CHECKSIGADD 2-of-2):
//
//	<UserKey> OP_CHECKSIG <OperatorKey> OP_CHECKSIGADD <2> OP_EQUAL
//
// This requires both user and operator signatures to spend cooperatively.
// Only valid in script-only mode.
func (k *OnboardingKit) GenCooperativePathScript() ([]byte, error) {
	if k.Mode != OnboardingModeScriptOnly {
		return nil, fmt.Errorf("cooperative script only available in " +
			"script-only mode")
	}

	builder := txscript.NewScriptBuilder()

	// Add user key + CHECKSIG.
	builder.AddData(schnorr.SerializePubKey(k.UserKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	// Add operator key + CHECKSIGADD.
	builder.AddData(schnorr.SerializePubKey(k.OperatorKey))
	builder.AddOp(txscript.OP_CHECKSIGADD)

	// Require exactly 2 valid signatures.
	builder.AddInt64(2)
	builder.AddOp(txscript.OP_EQUAL)

	return builder.Script()
}

// genSiblingPreimage generates the tapscript sibling preimage for the
// onboarding address.
//
// For MuSig2 mode: Single CSV timeout leaf.
// For script-only mode: Two leaves (cooperative + CSV timeout) in a branch.
func (k *OnboardingKit) genSiblingPreimage() ([]byte, error) {
	// Generate timeout leaf (common to both modes).
	timeoutScript, err := k.GenTimeoutPathScript()
	if err != nil {
		return nil, fmt.Errorf("generate timeout script: %w", err)
	}

	timeoutLeaf := txscript.TapLeaf{
		LeafVersion: txscript.BaseLeafVersion,
		Script:      timeoutScript,
	}

	// For MuSig2 mode, only timeout leaf is needed.
	if k.Mode == OnboardingModeMuSig2 {
		siblingPreimage, err := commitment.NewPreimageFromLeaf(
			timeoutLeaf,
		)
		if err != nil {
			return nil, fmt.Errorf("create sibling preimage: %w",
				err)
		}

		preimageBytes, _, err :=
			commitment.MaybeEncodeTapscriptPreimage(
				siblingPreimage,
			)
		if err != nil {
			return nil, fmt.Errorf("encode sibling preimage: %w",
				err)
		}

		return preimageBytes, nil
	}

	// For script-only mode, create branch with cooperative + timeout
	// leaves.
	coopScript, err := k.GenCooperativePathScript()
	if err != nil {
		return nil, fmt.Errorf("generate cooperative script: %w", err)
	}

	coopLeaf := txscript.TapLeaf{
		LeafVersion: txscript.BaseLeafVersion,
		Script:      coopScript,
	}

	// Create taproot branch with both leaves.
	branch := txscript.NewTapBranch(coopLeaf, timeoutLeaf)
	preimage := commitment.NewPreimageFromBranch(branch)

	preimageBytes, _, err := commitment.MaybeEncodeTapscriptPreimage(
		&preimage,
	)
	if err != nil {
		return nil, fmt.Errorf("encode branch preimage: %w", err)
	}

	return preimageBytes, nil
}

// NewOnboardingAddr creates a new onboarding address for the user to send funds
// to.
//
// Address structure (depends on mode):
//
// MuSig2 mode:
//   - Internal Key: MuSig2 aggregate (keyspend available)
//   - Script Key: OP_TRUE tweaked with MuSig2 pre-tweaked key
//   - Tapscript Sibling: Single CSV timeout leaf
//
// Script-only mode:
//   - Internal Key: NUMS (keyspend impossible)
//   - Script Key: OP_TRUE tweaked with NUMS
//   - Tapscript Sibling: Branch with cooperative multisig + CSV timeout
func (k *OnboardingKit) NewOnboardingAddr(ctx context.Context,
	tapdClient taprpc.TaprootAssetsClient) (*taprpc.Addr, error) {

	// Generate the tapscript sibling preimage
	siblingPreimageBytes, err := k.genSiblingPreimage()
	if err != nil {
		return nil, fmt.Errorf("generate sibling preimage: %w", err)
	}

	// Create OP_TRUE artifacts for the asset script key
	artifacts, err := BuildOpTrueArtifacts()
	if err != nil {
		return nil, fmt.Errorf("build OP_TRUE artifacts: %w", err)
	}

	// Build the asset script key structure
	// Tweak is the anchor internal key (MuSig2 pre-tweaked or NUMS)
	merkleRoot := k.InternalKey.SerializeCompressed()
	tapScriptKey := asset.ScriptKey{
		PubKey: artifacts.OutputKey,
		TweakedScriptKey: &asset.TweakedScriptKey{
			RawKey: keychain.KeyDescriptor{
				PubKey: asset.NUMSPubKey,
			},
			Tweak: merkleRoot,
		},
	}

	// Create the onboarding address via tapd
	addr, err := tapdClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:   k.AssetID[:],
		Amt:       k.Amount,
		ScriptKey: rpcutils.MarshalScriptKey(tapScriptKey),
		InternalKey: &taprpc.KeyDescriptor{
			RawKeyBytes: k.InternalKey.SerializeCompressed(),
		},
		TapscriptSibling: siblingPreimageBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("create tapd address: %w", err)
	}

	return addr, nil
}

// FindMatchingTransfer finds a transfer output that matches this OnboardingKit.
// It compares script key, internal key, tapscript sibling, and amount.
func (k *OnboardingKit) FindMatchingTransfer(
	transfers []*taprpc.AssetTransfer) (*taprpc.AssetTransfer, int, error) {

	// Build expected script key
	artifacts, err := BuildOpTrueArtifacts()
	if err != nil {
		return nil, 0, fmt.Errorf("build OP_TRUE artifacts: %w", err)
	}

	merkleRoot := k.InternalKey.SerializeCompressed()
	tapScriptKey := asset.ScriptKey{
		PubKey: artifacts.OutputKey,
		TweakedScriptKey: &asset.TweakedScriptKey{
			RawKey: keychain.KeyDescriptor{
				PubKey: asset.NUMSPubKey,
			},
			Tweak: merkleRoot,
		},
	}

	expectedScriptKey := tapScriptKey.PubKey.SerializeCompressed()
	// Normalize to even prefix.
	expectedScriptKey[0] = secp256k1.PubKeyFormatCompressedEven

	expectedInternalKey := k.InternalKey.SerializeCompressed()
	expectedAmount := k.Amount

	// Get the tapscript sibling bytes.
	expectedSiblingBytes, err := k.genSiblingPreimage()
	if err != nil {
		return nil, 0, fmt.Errorf("get tapscript sibling: %w", err)
	}

	// Match against transfers.
	for _, transfer := range transfers {
		for outIndex, out := range transfer.Outputs {
			// Check script key matches.
			if !bytes.Equal(out.ScriptKey, expectedScriptKey) {
				continue
			}

			// Check internal key matches.
			if !bytes.Equal(
				out.Anchor.InternalKey, expectedInternalKey,
			) {

				continue
			}

			// Check tapscript sibling matches.
			if !bytes.Equal(
				out.Anchor.TapscriptSibling,
				expectedSiblingBytes,
			) {

				continue
			}

			// Check amount matches.
			if out.Amount == expectedAmount {
				return transfer, outIndex, nil
			}
		}
	}

	return nil, 0, fmt.Errorf("no matching transfer found")
}
