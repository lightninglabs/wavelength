package waved

import (
	"bytes"
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/keychain"
)

// prepareArkChannelClaimRoot binds the exact armed receive-recovery job to a
// recovery-only VTXO descriptor before the channel OOR can consume it. This
// gives the channel archive the vHTLC's complete ancestry without admitting
// the value into wallet coin selection or triggering unilateral recovery.
func (s *Server) prepareArkChannelClaimRoot(ctx context.Context,
	terms arkchannel.Terms, source ArkChannelClaimSource,
	recoverySource arkchannel.ReceiveClaimRecoverySource,
	recovery arkchannel.RecoveryPackage) error {

	switch {
	case source.RecoveryID == "":
		return fmt.Errorf("incoming vHTLC recovery id is required")

	case s.vhtlcRecovery == nil:
		return fmt.Errorf("vHTLC recovery service is not initialized")

	case s.vhtlcRecoveryTarget == nil:
		return fmt.Errorf("vHTLC recovery materializer is not " +
			"initialized")
	}
	status, err := s.vhtlcRecovery.GetRecoveryStatus(
		ctx, source.RecoveryID,
	)
	if err != nil {
		return fmt.Errorf("load incoming vHTLC recovery: %w", err)
	}
	job := status.Job
	if err := validateArkChannelClaimRecovery(
		job, terms, source,
	); err != nil {
		return err
	}
	desc, err := s.loadIndexedArkChannelClaimRoot(
		ctx, job, terms, source,
	)
	if err != nil {
		return err
	}

	return s.vhtlcRecoveryTarget.InstallChannelRoot(
		ctx, job, desc, recoverySource, recovery,
	)
}

// loadIndexedArkChannelClaimRoot recovers the operator-created vHTLC descriptor
// from the proof-gated indexer. The finalized OOR package is transferred only
// by the authenticated channel peer and must match this independent view.
func (s *Server) loadIndexedArkChannelClaimRoot(ctx context.Context,
	job vhtlcrecovery.RecoveryJob, terms arkchannel.Terms,
	source ArkChannelClaimSource) (*vtxo.Descriptor, error) {

	if s.indexer == nil {
		return nil, fmt.Errorf("Ark indexer is not initialized")
	}
	resp, err := s.indexer.ListVTXOsByScriptsTaproot(
		ctx, []indexer.TaprootScriptScope{{
			PkScript: append([]byte(nil), source.PkScript...),
		}}, nil, 10, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("load indexed vHTLC channel root: %w",
			err)
	}
	var indexedRoot *arkrpc.VTXO
	for _, candidate := range vtxo.FlattenListVTXOsByScriptsResponse(resp) {
		outpoint, err := recoveryOutpoint(candidate.GetOutpoint())
		if err != nil {
			return nil, err
		}
		if outpoint == job.VTXOOutpoint {
			indexedRoot = candidate
			break
		}
	}
	if indexedRoot == nil {
		return nil, fmt.Errorf("indexed vHTLC channel root %s "+
			"not found", job.VTXOOutpoint)
	}
	desc, err := indexedArkChannelClaimDescriptor(
		indexedRoot, job, terms, source,
	)
	if err != nil {
		return nil, err
	}

	return desc, nil
}

// indexedArkChannelClaimDescriptor converts one authenticated indexer result
// into a recovery-only descriptor while preserving the vHTLC's actual policy.
func indexedArkChannelClaimDescriptor(indexedRoot *arkrpc.VTXO,
	job vhtlcrecovery.RecoveryJob, terms arkchannel.Terms,
	source ArkChannelClaimSource) (*vtxo.Descriptor, error) {

	if indexedRoot == nil {
		return nil, fmt.Errorf("indexed vHTLC channel root is nil")
	}
	switch indexedRoot.GetStatus() {
	case arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED,
		arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
		arkrpc.VTXOStatus_VTXO_STATUS_SPENT:

	default:
		return nil, fmt.Errorf("indexed vHTLC channel root has "+
			"status %s", indexedRoot.GetStatus())
	}
	if indexedRoot.GetValueSat() > math.MaxInt64 ||
		btcutil.Amount(indexedRoot.GetValueSat()) != source.Amount ||
		!bytes.Equal(indexedRoot.GetPkScript(), source.PkScript) {
		return nil, fmt.Errorf("indexed vHTLC channel root does not " +
			"match claim source")
	}
	template, err := arkscript.DecodePolicyTemplate(source.PolicyTemplate)
	if err != nil {
		return nil, fmt.Errorf("decode vHTLC channel policy: %w", err)
	}
	if !template.MatchesPkScript(source.PkScript) {
		return nil, fmt.Errorf("vHTLC channel policy does not match " +
			"script")
	}
	constructionVersion := arkrpc.ConstructionVersion(
		indexedRoot.GetConstructionVersion(),
	)
	if err := arkrpc.ValidateConstructionVersion(
		constructionVersion,
	); err != nil {
		return nil, err
	}
	operatorKey := terms.VTXO.ArkOperatorKey[:]
	if len(indexedRoot.GetOperatorPubkey()) != 0 {
		operatorKey = indexedRoot.GetOperatorPubkey()
	}
	operator, err := btcec.ParsePubKey(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("parse indexed vHTLC operator key: %w",
			err)
	}
	client, err := recoverySigningKey(job)
	if err != nil {
		return nil, err
	}
	ancestry, err := vtxo.AncestryFromRPC(indexedRoot.GetAncestryPaths())
	if err != nil {
		return nil, fmt.Errorf("convert indexed vHTLC ancestry: %w",
			err)
	}
	if len(ancestry) == 0 {
		return nil, fmt.Errorf("indexed vHTLC ancestry is empty")
	}
	commitmentTxID, err := chainhash.NewHash(
		indexedRoot.GetCommitmentTxid(),
	)
	if err != nil {
		return nil, fmt.Errorf("parse indexed vHTLC commitment "+
			"txid: %w", err)
	}
	csvDelay, err := recoveryCSVDelay(job)
	if err != nil {
		return nil, err
	}
	if indexedRoot.GetRoundId() == "" ||
		indexedRoot.GetBatchExpiryHeight() <= 0 ||
		indexedRoot.GetCreatedHeight() <= 0 ||
		indexedRoot.GetChainDepth() > math.MaxInt32 {
		return nil, fmt.Errorf("indexed vHTLC recovery metadata is " +
			"incomplete")
	}
	desc := &vtxo.Descriptor{
		Outpoint: job.VTXOOutpoint, Amount: source.Amount,
		PolicyTemplate: append([]byte(nil), source.PolicyTemplate...),
		PkScript:       append([]byte(nil), source.PkScript...),
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(job.SignerKeyFamily),
				Index:  uint32(job.SignerKeyIndex),
			},
			PubKey: client,
		},
		OperatorKey:    operator,
		Ancestry:       ancestry,
		RoundID:        indexedRoot.GetRoundId(),
		CommitmentTxID: *commitmentTxID,
		BatchExpiry:    indexedRoot.GetBatchExpiryHeight(),
		RelativeExpiry: csvDelay, ChainDepth: int(
			indexedRoot.GetChainDepth(),
		),
		CreatedHeight:       indexedRoot.GetCreatedHeight(),
		Status:              vtxo.VTXOStatusRecoveryOnly,
		ConstructionVersion: constructionVersion,
	}
	if _, err := channelOperatorKey(
		terms.VTXO.ArkOperatorKey, []*vtxo.Descriptor{desc},
	); err != nil {
		return nil, err
	}

	return desc, nil
}

// validateArkChannelClaimRecovery proves the swap recovery row and channel
// request identify one dormant preimage claim before sharing its ancestry.
func validateArkChannelClaimRecovery(job vhtlcrecovery.RecoveryJob,
	terms arkchannel.Terms, source ArkChannelClaimSource) error {

	outpoint, err := parseOutpointString(source.Outpoint)
	if err != nil {
		return fmt.Errorf("parse incoming vHTLC outpoint: %w", err)
	}
	switch {
	case job.ID != source.RecoveryID:
		return fmt.Errorf("incoming vHTLC recovery id does not match")

	case job.Direction != vhtlcrecovery.DirectionReceive:
		return fmt.Errorf("incoming vHTLC recovery direction is %q",
			job.Direction)

	case job.Action != vhtlcrecovery.ActionClaim:
		return fmt.Errorf("incoming vHTLC recovery action is %q",
			job.Action)

	case job.State != vhtlcrecovery.StateArmed:
		return fmt.Errorf("incoming vHTLC recovery state is %q",
			job.State)

	case job.VTXOOutpoint != outpoint:
		return fmt.Errorf("incoming vHTLC recovery outpoint does not " +
			"match")

	case job.VTXOAmountSat != int64(source.Amount):
		return fmt.Errorf("incoming vHTLC recovery amount does not " +
			"match")

	case !bytes.Equal(job.SwapID, terms.PaymentHash[:]):
		return fmt.Errorf("incoming vHTLC recovery swap id does not " +
			"match")

	case !bytes.Equal(job.PreimageHash, terms.PaymentHash[:]):
		return fmt.Errorf("incoming vHTLC recovery payment hash does " +
			"not match")
	}

	return nil
}

// prepareArkChannelOOR reserves ordinary wallet VTXOs and prepares the exact
// transfer that creates one channel-policy VTXO. The channel FSM commits or
// aborts the prepared OOR after native lnd funding reaches its durable gate.
func (s *Server) prepareArkChannelOOR(ctx context.Context,
	terms arkchannel.Terms, backingFee btcutil.Amount) (
	arkchannel.VTXOBinding, error) {

	if !s.walletRef.IsSome() {
		return arkchannel.VTXOBinding{}, fmt.Errorf("Ark channel " +
			"wallet actor is not initialized")
	}
	if s.vtxoStore == nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("Ark channel " +
			"VTXO store is not initialized")
	}
	if backingFee <= 0 || terms.Capacity >
		btcutil.Amount(math.MaxInt64)-backingFee {
		return arkchannel.VTXOBinding{}, fmt.Errorf("invalid Ark " +
			"channel backing amount")
	}
	operatorTerms, err := s.fetchOperatorTerms(ctx)
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	target := terms.Capacity + backingFee
	walletRef := s.walletRef.UnsafeFromSome()
	result := walletRef.Ask(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount:    target,
		MinChangeAmount: operatorTerms.MinVTXOAmountFloor(),
	}).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("select Ark "+
			"channel VTXOs: %w", err)
	}
	locked, ok := response.(*wallet.SelectAndLockVTXOsResponse)
	if !ok {
		return arkchannel.VTXOBinding{}, fmt.Errorf("unexpected VTXO "+
			"selection response %T", response)
	}
	rpcServer := &RPCServer{server: s}
	fail := func(err error) (arkchannel.VTXOBinding, error) {
		rpcServer.unlockSelectedVTXOsBestEffort(ctx, locked)

		return arkchannel.VTXOBinding{}, err
	}

	outpoints := make([]wire.OutPoint, 0, len(locked.SelectedVTXOs))
	for _, selected := range locked.SelectedVTXOs {
		outpoints = append(outpoints, selected.Outpoint)
	}
	inputs, err := BuildTransferInputs(ctx, s.vtxoStore, outpoints)
	if err != nil {
		return fail(fmt.Errorf("build Ark channel inputs: %w", err))
	}
	inputTotal, err := sumOORInputAmounts(inputs)
	if err != nil {
		return fail(fmt.Errorf("sum Ark channel inputs: %w", err))
	}
	if inputTotal < target {
		return fail(
			fmt.Errorf("selected Ark channel inputs total "+
				"%d, need %d", inputTotal, target),
		)
	}

	var changeOutput *oortx.RecipientOutput
	change := inputTotal - target
	if change > 0 {
		if change < operatorTerms.MinVTXOAmountFloor() {
			return fail(
				fmt.Errorf("Ark channel change %d is below "+
					"the VTXO floor", change),
			)
		}
		output, err := rpcServer.buildOORChangeRecipient(
			ctx, operatorTerms.PubKey, operatorTerms.VTXOExitDelay,
			change,
		)
		if err != nil {
			return fail(err)
		}
		changeOutput = &output
	}

	controller, err := oorbridge.New(s.actorSystem)
	if err != nil {
		return fail(err)
	}
	binding, err := controller.PrepareChannel(
		ctx, oorbridge.PrepareRequest{
			Terms: terms,
			CheckpointPolicy: arkscript.CheckpointPolicy{
				OperatorKey: operatorTerms.PubKey,
				CSVDelay:    operatorTerms.VTXOExitDelay,
			},
			Inputs:       inputs,
			BackingFee:   backingFee,
			ChangeOutput: changeOutput,
		},
	)
	if err != nil {
		return fail(err)
	}

	return binding, nil
}

// prepareArkChannelClaimOOR prepares an exact preimage-path vHTLC transfer
// into the channel-policy output. PrepareOnly keeps the preimage-bearing spend
// behind the channel FSM's fully-signed-backing gate.
func (s *Server) prepareArkChannelClaimOOR(ctx context.Context,
	terms arkchannel.Terms, backingFee btcutil.Amount,
	source ArkChannelClaimSource) (arkchannel.VTXOBinding, error) {

	if s.vtxoStore == nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("Ark channel " +
			"VTXO store is not initialized")
	}
	if backingFee <= 0 || source.Amount != terms.Capacity+backingFee {
		return arkchannel.VTXOBinding{}, fmt.Errorf("incoming vHTLC " +
			"does not match channel amount")
	}
	operatorTerms, err := s.fetchOperatorTerms(ctx)
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	inputs, err := BuildCustomTransferInputs(
		ctx, s.vtxoStore, []*waverpc.CustomOORInput{{
			Outpoint: source.Outpoint,
			VtxoPolicyTemplate: append(
				[]byte(nil), source.PolicyTemplate...,
			),
			SpendPath: append([]byte(nil), source.SpendPath...),
			AmountSat: int64(source.Amount),
			PkScript:  append([]byte(nil), source.PkScript...),
		}}, s.clientKeyDesc, operatorTerms.PubKey,
		operatorTerms.VTXOExitDelay,
	)
	if err != nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("build incoming "+
			"vHTLC channel input: %w", err)
	}
	controller, err := oorbridge.New(s.actorSystem)
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}

	return controller.PrepareChannel(ctx, oorbridge.PrepareRequest{
		Terms: terms,
		CheckpointPolicy: arkscript.CheckpointPolicy{
			OperatorKey: operatorTerms.PubKey,
			CSVDelay:    operatorTerms.VTXOExitDelay,
		},
		Inputs: inputs, BackingFee: backingFee,
	})
}
