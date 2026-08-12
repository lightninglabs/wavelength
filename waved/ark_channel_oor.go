package waved

import (
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
)

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
