package tapassets

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// ClaimRequest describes one confirmed asset VTXO being claimed into the
// owner's tapd wallet after a unilateral exit put its anchor on chain.
type ClaimRequest struct {
	// Outpoint is the claimed VTXO's anchor outpoint, confirmed on
	// chain by the exit.
	Outpoint wire.OutPoint

	// CarrierValueSat is the anchor output's Bitcoin value.
	CarrierValueSat int64

	// AssetRef and AssetAmount identify the claimed units.
	AssetRef    string
	AssetAmount uint64

	// TaprootAssetRoot is the anchor's asset commitment root.
	TaprootAssetRoot chainhash.Hash

	// SealedPackage is the sealed transition package behind the VTXO,
	// the leaf's only lineage.
	SealedPackage []byte

	// Confirmations carries the raw block and height for every anchor
	// transaction in the leaf's unconfirmed lineage, keyed by txid.
	Confirmations map[chainhash.Hash]tapsdk.AnchorConfirmation

	// ExitDelay is the CSV delay of the exit leaf being spent; it
	// becomes the claim input's sequence.
	ExitDelay uint32

	// FeeSat is the miner fee, paid out of the carrier value.
	FeeSat int64

	// SignExit signs the claim transaction's single input, the composed
	// leaf's exit path, and returns the finalized PSBT.
	SignExit tapsdk.AnchorSigner
}

// ClaimResult reports the published claim.
type ClaimResult struct {
	// Txid is the claim transaction ID.
	Txid chainhash.Hash

	// AnchorOutpoint is the new tapd-owned anchor holding the claimed
	// units.
	AnchorOutpoint wire.OutPoint

	// OutputValueSat is the new anchor's Bitcoin value.
	OutputValueSat int64
}

// ClaimAssetVTXO moves an exited asset VTXO's units into the owner's tapd
// wallet. The VTXO's compact lineage is completed into a confirmed proof
// file, and one custom-anchor transition spends the composed leaf output
// through its exit path into a fresh wallet-owned anchor: the script key
// and internal key both come from tapd, so the units become ordinary,
// spendable wallet balance once the claim confirms. The workflow is
// stateless; a failed attempt is retried from scratch.
func ClaimAssetVTXO(ctx context.Context, wallet *tapsdk.Wallet,
	req *ClaimRequest) (*ClaimResult, error) {

	if wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}
	if req == nil || req.SignExit == nil {
		return nil, fmt.Errorf("claim request and exit signer are " +
			"required")
	}
	if req.ExitDelay == 0 {
		return nil, fmt.Errorf("exit delay is required")
	}
	outputValue := req.CarrierValueSat - req.FeeSat
	if req.FeeSat <= 0 || outputValue < int64(onboardingDustFloorSat) {
		return nil, fmt.Errorf("claim fee %d leaves output %d below "+
			"the Taproot dust floor", req.FeeSat, outputValue)
	}

	// The sealed package is the leaf's lineage: resolve its compact
	// proof path and OP_TRUE witness, then complete the path into a
	// confirmed proof file now that the exit put every hop on chain.
	source, err := ResolveCreatedAssetProofSource(
		req.SealedPackage, req.Outpoint, req.CarrierValueSat,
		req.AssetRef, req.AssetAmount, req.TaprootAssetRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve claim lineage: %w", err)
	}

	var path tapsdk.AssetProofPath
	if err := path.UnmarshalBinary(source.CompactProofPath); err != nil {
		return nil, fmt.Errorf("decode claim proof path: %w", err)
	}
	confirmedFile, err := path.ConfirmProofFile(
		claimConfirmations(req.Confirmations),
	)
	if err != nil {
		return nil, fmt.Errorf("confirm claim proof file: %w", err)
	}

	// The publish step verifies the transition against tapd's own proof
	// archive, so the leaf's lineage must be imported first. Importing
	// needs the leaf's OP_TRUE script key declared; its spec is fully
	// recoverable from the witness stack's script and control block.
	if err := importClaimLineage(
		ctx, wallet, source, confirmedFile,
	); err != nil {
		return nil, err
	}

	assetRef, err := tapsdk.ParseAssetRef(req.AssetRef)
	if err != nil {
		return nil, fmt.Errorf("parse claim asset ref: %w", err)
	}
	internalKey, err := wallet.Client().DeriveInternalKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("derive claim anchor internal key: %w",
			err)
	}
	internalPub, err := btcec.ParsePubKey(internalKey.PubKey[:])
	if err != nil {
		return nil, fmt.Errorf("parse claim anchor internal key: %w",
			err)
	}

	buildRequest := func(anchorPSBT []byte) *tapsdk.CustomAnchorRequest {
		return &tapsdk.CustomAnchorRequest{
			Inputs: []tapsdk.CustomAssetInput{{
				ID:        "wavelength-claim-input-0",
				AssetRef:  assetRef,
				Amount:    req.AssetAmount,
				ProofFile: confirmedFile,
				Witness: tapsdk.CustomAssetWitnessPlan{
					Mode: witnessCallerProvided,
					Stack: cloneByteSlices(
						source.OPTrueWitness,
					),
				},
			}},
			Outputs: []tapsdk.CustomAssetOutput{{
				ID:                "wavelength-claim-output-0",
				AssetRef:          assetRef,
				Amount:            req.AssetAmount,
				AnchorOutputIndex: 0,
				AnchorValueSat:    uint64(outputValue),
				Script: tapsdk.CustomAssetScriptPlan{
					Mode: tapsdk.CustomAssetScriptWallet,
					Wallet: &tapsdk.
						CustomAssetWalletScriptPlan{},
				},
				Anchor: anchorPlan(internalPub, nil),
			}},
			AnchorPSBT: anchorPSBT,
			Funding: tapsdk.CustomAnchorFundingPlan{
				Mode: tapsdk.
					CustomAnchorFundingCallerFundedExact,
				CallerFundedExact: &tapsdk.
					CustomAnchorCallerFundedExact{},
			},
			PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
				Policy: tapsdk.CustomAnchorPassiveReject,
			},
			LossPolicy: tapsdk.CustomAnchorLossPolicy{
				Mode: tapsdk.CustomAnchorLossReject,
			},
			SigningPlans: []tapsdk.CustomAnchorInputSigningPlan{{
				InputIndex: 0,
				CallerSigned: &tapsdk.
					CustomAnchorCallerSignedPlan{},
			}},
		}
	}

	// The wallet script plan derives a fresh asset script key at commit
	// time, so the anchor output script is only knowable once tapd
	// commits: the template ships a placeholder and the committed
	// transaction carries the authoritative script.
	driver := &sdkDriver{wallet: wallet}
	anchorPSBT, err := claimAnchorPSBT(req, nil)
	if err != nil {
		return nil, err
	}
	committed, err := driver.CommitClaim(
		ctx, buildRequest(anchorPSBT), nil,
	)
	if err != nil {
		return nil, fmt.Errorf("commit claim transition: %w", err)
	}
	if len(committed.outputs) != 1 ||
		committed.outputs[0].anchorOutputIndex != 0 ||
		committed.outputs[0].anchorValueSat != outputValue ||
		committed.outputs[0].scriptMode !=
			tapsdk.CustomAssetScriptWallet ||
		committed.outputs[0].amount != req.AssetAmount {
		return nil, fmt.Errorf("committed claim diverges from the " +
			"request")
	}
	committedPacket, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, fmt.Errorf("parse committed claim PSBT: %w", err)
	}
	if len(committedPacket.UnsignedTx.TxOut) != 1 ||
		len(committedPacket.UnsignedTx.TxIn) != 1 ||
		committedPacket.UnsignedTx.TxIn[0].Sequence != req.ExitDelay {
		return nil, fmt.Errorf("committed claim transaction shape " +
			"diverges from the template")
	}
	claimScript := committedPacket.UnsignedTx.TxOut[0].PkScript

	finalPSBT, err := req.SignExit(ctx, committed.anchorPSBT)
	if err != nil {
		return nil, fmt.Errorf("sign claim exit spend: %w", err)
	}
	if err := driver.VerifyFinalOnboarding(
		committed.packageBytes, finalPSBT,
	); err != nil {
		return nil, fmt.Errorf("verify final claim PSBT: %w", err)
	}
	if err := driver.PublishOnboarding(
		ctx, committed.packageBytes, finalPSBT,
	); err != nil {
		return nil, fmt.Errorf("publish claim transfer: %w", err)
	}

	packet, err := psbtutil.Parse(finalPSBT)
	if err != nil {
		return nil, fmt.Errorf("parse final claim PSBT: %w", err)
	}
	txid := packet.UnsignedTx.TxHash()
	if len(packet.UnsignedTx.TxOut) == 0 || !bytes.Equal(
		packet.UnsignedTx.TxOut[0].PkScript, claimScript,
	) {
		return nil, fmt.Errorf("final claim output script diverges " +
			"from its derivation")
	}

	return &ClaimResult{
		Txid: txid,
		AnchorOutpoint: wire.OutPoint{
			Hash:  txid,
			Index: 0,
		},
		OutputValueSat: outputValue,
	}, nil
}

// importClaimLineage declares the exited leaf's OP_TRUE script key and
// imports its confirmed proof file into the owner's tapd, which archives
// the lineage the publish step verifies against. Declaring is correct at
// claim time: the anchor is on chain under the owner's exclusive control,
// and the claim spends it in the same flow.
func importClaimLineage(ctx context.Context, wallet *tapsdk.Wallet,
	source *CreatedAssetProofSource, confirmedFile []byte) error {

	if len(source.OPTrueWitness) != 2 {
		return fmt.Errorf("claim OP_TRUE witness has %d items, "+
			"expected script and control block",
			len(source.OPTrueWitness))
	}
	leafScript := source.OPTrueWitness[0]
	controlBlock, err := txscript.ParseControlBlock(
		source.OPTrueWitness[1],
	)
	if err != nil {
		return fmt.Errorf("parse claim OP_TRUE control block: %w", err)
	}
	tapTweak := controlBlock.RootHash(leafScript)

	scriptKey, err := tapsdk.ParsePubKey(source.ScriptKey[:])
	if err != nil {
		return fmt.Errorf("parse claim script key: %w", err)
	}
	internalKey, err := tapsdk.ParsePubKey(
		controlBlock.InternalKey.SerializeCompressed(),
	)
	if err != nil {
		return fmt.Errorf("parse claim script internal key: %w", err)
	}

	_, err = wallet.Client().DeclareScriptKey(
		ctx, &tapsdk.DeclareScriptKeyRequest{
			ScriptKey: tapsdk.ScriptKey{
				PubKey: scriptKey,
				KeyDesc: tapsdk.KeyDescriptor{
					RawKeyBytes: internalKey,
				},
				TapTweak: append([]byte(nil), tapTweak...),
			},
		},
	)
	if err != nil {
		return fmt.Errorf("declare claim script key: %w", err)
	}

	if _, err := wallet.ImportProofFile(ctx, &tapsdk.ProofFile{
		RawProofFile: confirmedFile,
	}); err != nil {
		return fmt.Errorf("import claim proof file: %w", err)
	}

	return nil
}

// claimConfirmations converts the caller's chainhash-keyed block data into
// the SDK's hash-keyed shape.
func claimConfirmations(
	confirmations map[chainhash.Hash]tapsdk.AnchorConfirmation,
) map[tapsdk.Hash]tapsdk.AnchorConfirmation {

	converted := make(
		map[tapsdk.Hash]tapsdk.AnchorConfirmation, len(confirmations),
	)
	for txid, confirmation := range confirmations {
		converted[tapsdk.Hash(txid)] = confirmation
	}

	return converted
}

// claimAnchorPSBT builds the claim's caller-funded anchor template: one
// input spending the exited leaf with the exit leaf's CSV sequence, and
// one output paying the claimed carrier value minus fees. A nil script
// selects a placeholder for the preview pass.
func claimAnchorPSBT(req *ClaimRequest, script []byte) ([]byte, error) {
	if script == nil {
		placeholderKey := txscript.ComputeTaprootKeyNoScript(
			&arkscript.ARKNUMSKey,
		)
		placeholder, err := txscript.PayToTaprootScript(placeholderKey)
		if err != nil {
			return nil, err
		}
		script = placeholder
	}

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: req.Outpoint,
		Sequence:         req.ExitDelay,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    req.CarrierValueSat - req.FeeSat,
		PkScript: script,
	})
	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("build claim anchor PSBT: %w", err)
	}

	return psbtutil.Serialize(packet)
}
