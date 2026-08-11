package waved

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/tapassets"
)

// ClaimAssetVTXO claims an exited asset VTXO's units into the daemon's tapd
// wallet. The exit already put the composed leaf output on chain; once its
// CSV delay matures, the claim spends it through the exit leaf into a fresh
// tapd-owned anchor and publishes the transition, making the units ordinary
// wallet balance. The caller supplies the raw block and height for every
// anchor transaction in the leaf's lineage, keyed by txid.
func (s *Server) ClaimAssetVTXO(ctx context.Context, outpoint wire.OutPoint,
	feeSat int64,
	confirmations map[chainhash.Hash]tapsdk.AnchorConfirmation) (
	*tapassets.ClaimResult, error) {

	if s.taprootAssetWallet == nil {
		return nil, fmt.Errorf("taproot asset integration is not " +
			"configured")
	}
	if s.vtxoStore == nil {
		return nil, fmt.Errorf("VTXO store is not initialized")
	}

	desc, err := s.vtxoStore.GetVTXO(ctx, outpoint)
	if err != nil {
		return nil, fmt.Errorf("load claim VTXO %s: %w", outpoint, err)
	}
	if desc.TaprootAssetRoot == nil || desc.TaprootAssetRef == "" {
		return nil, fmt.Errorf("VTXO %s carries no Taproot Asset",
			outpoint)
	}
	if len(desc.TaprootAssetSealedPackage) == 0 {
		return nil, fmt.Errorf("VTXO %s has no sealed package to "+
			"claim from", outpoint)
	}

	params, err := desc.DecodeStandardPolicyTemplate()
	if err != nil {
		return nil, fmt.Errorf("decode claim VTXO policy: %w", err)
	}

	// The exit leaf's spend info carries the composed control block: the
	// policy tree branched with the asset commitment root.
	spendInfo, err := desc.EffectiveStandardSpendInfo(1)
	if err != nil {
		return nil, fmt.Errorf("derive claim exit spend: %w", err)
	}

	signer, err := s.newSweepWallet()
	if err != nil {
		return nil, fmt.Errorf("claim exit signer: %w", err)
	}

	prevOut := &wire.TxOut{
		Value:    int64(desc.Amount),
		PkScript: bytes.Clone(desc.PkScript),
	}
	signExit := func(_ context.Context, anchorPSBT []byte) ([]byte, error) {
		packet, err := psbtutil.Parse(anchorPSBT)
		if err != nil {
			return nil, fmt.Errorf("parse claim anchor PSBT: %w",
				err)
		}
		if len(packet.Inputs) != 1 ||
			packet.UnsignedTx.TxIn[0].PreviousOutPoint !=
				outpoint {
			return nil, fmt.Errorf("claim anchor PSBT does not " +
				"spend the exited leaf")
		}

		fetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, prevOut.Value,
		)
		sigHashes := txscript.NewTxSigHashes(
			packet.UnsignedTx, fetcher,
		)
		signDesc := spendInfo.BuildSignDescriptor(
			desc.ClientKey, prevOut, sigHashes, fetcher, 0,
		)
		sig, err := signer.SignOutputRaw(packet.UnsignedTx, signDesc)
		if err != nil {
			return nil, fmt.Errorf("sign claim exit spend: %w", err)
		}
		witness, err := spendInfo.TimeoutWitness(sig)
		if err != nil {
			return nil, fmt.Errorf("assemble claim exit "+
				"witness: %w", err)
		}

		packet.Inputs[0].WitnessUtxo = prevOut
		packet.Inputs[0].FinalScriptWitness, err =
			serializeClaimWitness(witness)
		if err != nil {
			return nil, err
		}

		return psbtutil.Serialize(packet)
	}

	return tapassets.ClaimAssetVTXO(
		ctx, s.taprootAssetWallet, &tapassets.ClaimRequest{
			Outpoint:         outpoint,
			CarrierValueSat:  int64(desc.Amount),
			AssetRef:         desc.TaprootAssetRef,
			AssetAmount:      desc.TaprootAssetAmount,
			TaprootAssetRoot: *desc.TaprootAssetRoot,
			SealedPackage: bytes.Clone(
				desc.TaprootAssetSealedPackage,
			),
			Confirmations: confirmations,
			ExitDelay:     params.ExitDelay,
			FeeSat:        feeSat,
			SignExit:      signExit,
		},
	)
}

// serializeClaimWitness encodes a witness stack into BIP-174
// FinalScriptWitness format: a CompactSize item count followed by
// length-prefixed items.
func serializeClaimWitness(witness wire.TxWitness) ([]byte, error) {
	var buf bytes.Buffer
	if err := wire.WriteVarInt(
		&buf, 0, uint64(len(witness)),
	); err != nil {
		return nil, fmt.Errorf("serialize claim witness: %w", err)
	}
	for _, item := range witness {
		if err := wire.WriteVarBytes(&buf, 0, item); err != nil {
			return nil, fmt.Errorf("serialize claim witness: %w",
				err)
		}
	}

	return buf.Bytes(), nil
}
