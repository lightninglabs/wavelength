package waved

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/google/uuid"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// assetVTXOClaimJournalPrefix keys VTXO claims in the claim journal.
	assetVTXOClaimJournalPrefix = "vtxo/"

	// assetVTXOClaimChainTimeout bounds the lookup of the blocks that
	// confirmed an asset VTXO's tree path. The unroll actor retries a
	// timed-out claim at the next block.
	assetVTXOClaimChainTimeout = time.Minute
)

// assetVTXOClaimer claims the asset VTXOs whose exit leaf has matured for the
// unroll subsystem. It proves the asset from the VTXO's sealed package, with
// the blocks that confirmed its tree path.
type assetVTXOClaimer struct {
	claimer     assetClaimer
	chainSource chainSourceRef
}

// ClaimAssetVTXO spends an asset VTXO through its owner's exit leaf into the
// tapd wallet. The claim journal returns the same claim for every call.
func (c *assetVTXOClaimer) ClaimAssetVTXO(ctx context.Context,
	desc *vtxo.Descriptor, heightHint uint32) (*wire.MsgTx, error) {

	request, err := c.claimRequest(ctx, desc, heightHint)
	if err != nil {
		return nil, err
	}
	result, err := c.claimer.Claim(ctx, request)
	if err != nil {
		return nil, err
	}

	return result.Tx, nil
}

// claimRequest builds the claim of an asset VTXO: the leaf spent through its
// exit leaf, composed with the asset root, and the proof of each issuance it
// holds confirmed by the blocks of the VTXO's tree path.
func (c *assetVTXOClaimer) claimRequest(ctx context.Context,
	desc *vtxo.Descriptor, heightHint uint32) (*tapassets.ClaimRequest,
	error) {

	switch {
	case desc == nil:
		return nil, fmt.Errorf("asset VTXO descriptor is required")

	case desc.TaprootAssetRoot == nil || desc.TaprootAssetRef == "" ||
		desc.TaprootAssetAmount == 0 ||
		len(desc.TaprootAssetSealedPackage) == 0:
		return nil, fmt.Errorf("VTXO %v carries no asset state",
			desc.Outpoint)

	case desc.ClientKey.PubKey == nil || desc.OperatorKey == nil:
		return nil, fmt.Errorf("VTXO %v has no exit keys",
			desc.Outpoint)
	}

	sources, err := tapassets.DecodeAssetVTXOClaimInputs(
		desc.TaprootAssetSealedPackage, desc.Outpoint,
	)
	if err != nil {
		return nil, err
	}
	anchors, err := claimAnchorTransactions(sources)
	if err != nil {
		return nil, err
	}
	confirmations, err := c.anchorConfirmations(ctx, anchors, heightHint)
	if err != nil {
		return nil, err
	}

	var total uint64
	inputs := make([]tapassets.ClaimInput, len(sources))
	for idx, source := range sources {
		proofFile, err := source.ProofPath.ConfirmProofFile(
			confirmations,
		)
		if err != nil {
			return nil, fmt.Errorf("confirm asset VTXO proof "+
				"%d: %w", idx, err)
		}
		inputs[idx] = tapassets.ClaimInput{
			Amount:        source.Amount,
			ProofFile:     proofFile,
			OPTrueWitness: source.OPTrueWitness,
		}
		if source.Amount > math.MaxUint64-total {
			return nil, fmt.Errorf("VTXO %v asset amount overflow",
				desc.Outpoint)
		}
		total += source.Amount
	}
	if total != desc.TaprootAssetAmount {
		return nil, fmt.Errorf("VTXO %v package holds %d units, "+
			"descriptor %d", desc.Outpoint, total,
			desc.TaprootAssetAmount)
	}

	exitSpend, err := arkscript.ComposedBoardingExitSpend(
		desc.ClientKey.PubKey, desc.OperatorKey, desc.RelativeExpiry,
		*desc.TaprootAssetRoot,
	)
	if err != nil {
		return nil, err
	}

	return &tapassets.ClaimRequest{
		RequestID: assetVTXOClaimJournalPrefix +
			desc.Outpoint.String(),
		Outpoint:  desc.Outpoint,
		Output:    wire.NewTxOut(int64(desc.Amount), desc.PkScript),
		ExitSpend: exitSpend,
		OwnerKey:  desc.ClientKey,
		AssetRef:  tapsdk.AssetRef(desc.TaprootAssetRef),
		Inputs:    inputs,
	}, nil
}

// claimAnchorTransactions lists the anchor transactions of every piece's
// proof path once each.
func claimAnchorTransactions(sources []tapassets.AssetVTXOClaimInput) (
	[]*wire.MsgTx, error) {

	var anchors []*wire.MsgTx
	seen := make(map[chainhash.Hash]struct{})
	for idx, source := range sources {
		pathAnchors, err := source.ProofPath.AnchorTransactions()
		if err != nil {
			return nil, fmt.Errorf("asset VTXO proof path %d: %w",
				idx, err)
		}
		for _, anchor := range pathAnchors {
			txid := anchor.TxHash()
			if _, ok := seen[txid]; ok {
				continue
			}
			seen[txid] = struct{}{}
			anchors = append(anchors, anchor)
		}
	}

	return anchors, nil
}

// anchorConfirmations reads the block that confirmed each anchor transaction.
func (c *assetVTXOClaimer) anchorConfirmations(ctx context.Context,
	anchors []*wire.MsgTx, heightHint uint32) (
	map[tapsdk.Hash]customanchor.AnchorConfirmation, error) {

	if c.chainSource == nil {
		return nil, fmt.Errorf("chain source is required")
	}

	ctx, cancel := context.WithTimeout(ctx, assetVTXOClaimChainTimeout)
	defer cancel()

	confirmations := make(
		map[tapsdk.Hash]customanchor.AnchorConfirmation, len(anchors),
	)
	for _, anchor := range anchors {
		if len(anchor.TxOut) == 0 {
			return nil, fmt.Errorf("anchor transaction %v has "+
				"no outputs", anchor.TxHash())
		}
		txid := anchor.TxHash()
		confirmation, err := c.confirmedBlock(
			ctx, txid, anchor.TxOut[0].PkScript, heightHint,
		)
		if err != nil {
			return nil, fmt.Errorf("read the block of anchor "+
				"transaction %v: %w", txid, err)
		}
		confirmations[tapsdk.Hash(txid)] = confirmation
	}

	return confirmations, nil
}

// confirmedBlock waits for one transaction's confirmation with its block.
func (c *assetVTXOClaimer) confirmedBlock(ctx context.Context,
	txid chainhash.Hash, pkScript []byte, heightHint uint32) (
	customanchor.AnchorConfirmation, error) {

	var empty customanchor.AnchorConfirmation

	callerID := "asset-claim-" + uuid.NewString()
	defer unregisterExpiryConfirmation(
		ctx, c.chainSource, callerID, txid, pkScript,
	)

	response, err := c.chainSource.Ask(
		ctx, &chainsource.RegisterConfRequest{
			CallerID:     callerID,
			Txid:         &txid,
			PkScript:     pkScript,
			TargetConfs:  1,
			HeightHint:   heightHint,
			IncludeBlock: true,
			NotifyActor:  fn.None[confRef](),
		},
	).Await(ctx).Unpack()
	if err != nil {
		return empty, err
	}
	registered, ok := response.(*confirmationRegistration)
	if !ok || registered.Future == nil {
		return empty, fmt.Errorf("unexpected response %T", response)
	}

	confirmation, err := registered.Future.Await(ctx).Unpack()
	if err != nil {
		return empty, err
	}
	if confirmation.Block == nil || confirmation.BlockHeight <= 0 {
		return empty, fmt.Errorf("confirmation carries no block")
	}

	var block bytes.Buffer
	if err := confirmation.Block.Serialize(&block); err != nil {
		return empty, err
	}

	return customanchor.AnchorConfirmation{
		BlockHeight: uint32(confirmation.BlockHeight),
		Block:       block.Bytes(),
	}, nil
}

// unrollAssetClaimer returns the claimer the unroll subsystem hands matured
// asset VTXOs to, or nil when assets are disabled.
func (s *Server) unrollAssetClaimer(
	chainSource chainSourceRef) unroll.AssetClaimer {

	if s.assetClaimer == nil {
		return nil
	}

	return &assetVTXOClaimer{
		claimer:     s.assetClaimer,
		chainSource: chainSource,
	}
}
