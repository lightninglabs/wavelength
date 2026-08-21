package waved

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/wallet"
)

// lookupArkChannelOOR reconciles one deterministic channel transfer without
// selecting or locking fresh VTXOs. Startup maintenance must use this path.
func (s *Server) lookupArkChannelOOR(ctx context.Context,
	terms arkchannel.Terms, backingFee btcutil.Amount) (
	oorbridge.PreparationLookup, error) {

	controller, err := oorbridge.New(s.actorSystem)
	if err != nil {
		return oorbridge.PreparationLookup{}, err
	}

	return controller.LookupChannelPreparation(ctx, terms, backingFee)
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
	controller, err := oorbridge.New(s.actorSystem)
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	recovered, found, err := controller.LookupPreparedChannel(
		ctx, terms, backingFee,
	)
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	if found {
		return recovered, nil
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

	prepared, err := controller.PrepareChannel(
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
		if errors.Is(err, arkchannel.ErrOORPreparationAmbiguous) {
			lookupCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx),
				submittedOORUnlockTimeout,
			)
			lookup, lookupErr :=
				controller.LookupChannelPreparation(
					lookupCtx, terms, backingFee,
				)
			cancel()
			if lookupErr != nil {
				s.reconcileArkChannelPreparation(
					ctx, controller, terms, backingFee,
					locked, outpoints,
				)

				return arkchannel.VTXOBinding{}, errors.Join(
					err, lookupErr,
				)
			}

			switch lookup.Status {
			case oorbridge.PreparationAbsent:
				return fail(err)

			case oorbridge.PreparationPrepared:
				if !sameChannelPreparationInputs(
					lookup.InputOutpoints, outpoints,
				) {

					rpcServer.unlockSelectedVTXOsBestEffort(
						context.WithoutCancel(ctx),
						locked,
					)
				}

				return lookup.Binding, nil

			case oorbridge.PreparationPending:
				s.reconcileArkChannelPreparation(
					ctx, controller, terms, backingFee,
					locked, outpoints,
				)

				return arkchannel.VTXOBinding{}, err

			case oorbridge.PreparationAccepted:
				if len(lookup.InputOutpoints) > 0 &&
					!sameChannelPreparationInputs(
						lookup.InputOutpoints,
						outpoints,
					) {

					rpcServer.unlockSelectedVTXOsBestEffort(
						context.WithoutCancel(ctx),
						locked,
					)
				}

				return arkchannel.VTXOBinding{}, err
			}
		}

		return fail(err)
	}
	if prepared.Existing {
		rpcServer.unlockSelectedVTXOsBestEffort(ctx, locked)
	}

	return prepared.Binding, nil
}

// reconcileArkChannelPreparation owns a canceled caller's selected locks until
// the registry proves either a durable owner or a failed/no-session admission.
func (s *Server) reconcileArkChannelPreparation(ctx context.Context,
	controller *oorbridge.Controller, terms arkchannel.Terms,
	backingFee btcutil.Amount, locked *wallet.SelectAndLockVTXOsResponse,
	selected []wire.OutPoint) {

	rpcServer := &RPCServer{server: s}
	unlockVTXOs := rpcServer.unlockSelectedVTXOsBestEffort
	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), submittedOORCleanupTimeout,
	)
	go func() {
		defer cancel()

		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			lookup, err := controller.LookupChannelPreparation(
				cleanupCtx, terms, backingFee,
			)
			if err == nil {
				switch lookup.Status {
				case oorbridge.PreparationAbsent:
					unlockVTXOs(
						cleanupCtx, locked,
					)

					return

				case oorbridge.PreparationPending:
					// The accepted registry request may
					// still create the durable session that
					// owns these locks.

				case oorbridge.PreparationPrepared:
					if !sameChannelPreparationInputs(
						lookup.InputOutpoints, selected,
					) {

						unlockVTXOs(
							cleanupCtx, locked,
						)
					}

					return

				case oorbridge.PreparationAccepted:
					if len(lookup.InputOutpoints) > 0 &&
						!sameChannelPreparationInputs(
							lookup.InputOutpoints,
							selected,
						) {

						unlockVTXOs(
							cleanupCtx, locked,
						)
					}

					return
				}
			}

			select {
			case <-cleanupCtx.Done():
				return

			case <-ticker.C:
			}
		}
	}()
}

// sameChannelPreparationInputs compares selections as sets so an idempotent
// winner with different inputs cannot retain a retry's fresh wallet locks.
func sameChannelPreparationInputs(first, second []wire.OutPoint) bool {
	if len(first) != len(second) {
		return false
	}
	counts := make(map[wire.OutPoint]int, len(first))
	for _, outpoint := range first {
		counts[outpoint]++
	}
	for _, outpoint := range second {
		if counts[outpoint] == 0 {
			return false
		}
		counts[outpoint]--
	}

	return true
}
