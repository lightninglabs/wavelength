package waved

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightningnetwork/lnd/clock"
)

const (
	defaultArkChannelPrePONRTimeout      = 10 * time.Minute
	defaultArkChannelPrePONRScanInterval = 30 * time.Second
	arkChannelPrePONRExpiryReason        = "channel preparation expired " +
		"before OOR commit"
)

// prePONRResultController exposes the result-bearing abort used to distinguish
// a released reservation from an OOR whose commit already won the durable gate.
type prePONRResultController interface {
	ValidatePreparedOOR(context.Context, arkchannel.Terms,
		arkchannel.VTXOBinding) error

	AbortPreparedOORResult(context.Context, arkchannel.ID, arkchannel.Terms,
		arkchannel.VTXOBinding,
		string) (oorbridge.TerminalResult, error)
}

// withArkChannelControllerDefaults installs process-owned clocks.
func withArkChannelControllerDefaults(
	cfg ArkChannelControllerConfig) ArkChannelControllerConfig {

	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}

	return cfg
}

// startPrePONRReaper starts one controller-lifetime maintenance loop. Client
// controllers start this before lnd or the hub is available so an abandoned
// local wallet reservation can still expire autonomously.
func (c *NativeArkChannelController) startPrePONRReaper(
	parent context.Context) {

	if c.reaperCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	c.reaperCancel = cancel
	c.reaperWG.Add(1)
	go func() {
		defer c.reaperWG.Done()

		for {
			c.mu.RLock()
			service := c.service
			c.mu.RUnlock()
			err := c.maintainPrePONRChannels(
				ctx, service, c.cfg.Clock.Now(),
			)
			_ = tolerateNativeArkChannelFailures(
				ctx, err, c.cfg.Log,
				"Ark channel pre-PONR maintenance failed",
			)

			select {
			case <-ctx.Done():
				return

			case <-c.cfg.Clock.TickAfter(
				defaultArkChannelPrePONRScanInterval,
			):
			}
		}
	}()
}

// maintainPrePONRChannels reconciles only deterministic OOR keys, then expires
// local-funder work whose commit action has not become durable. It never calls
// the wallet input-selection path.
func (c *NativeArkChannelController) maintainPrePONRChannels(
	ctx context.Context, service *arkchannel.Service, now time.Time) error {

	records, err := c.coordinator.ListNonTerminal(ctx)
	if err != nil {
		return err
	}
	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		if err := c.maintainPrePONRChannel(
			ctx, service, record, now,
		); err != nil {

			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err:       err,
			})
		}
	}
	if len(failures) != 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// maintainPrePONRChannel repairs the crash windows around one local OOR
// preparation and records a definitive abort before lnd cleanup is resumed.
func (c *NativeArkChannelController) maintainPrePONRChannel(ctx context.Context,
	service *arkchannel.Service, record arkchannel.Record,
	now time.Time) error {

	snapshot := record.Snapshot
	if snapshot.Terms.Funder != c.party ||
		(!snapshot.OORPreparationStarted && snapshot.Source == nil) {
		return nil
	}
	if snapshot.Phase != arkchannel.PhaseRequested &&
		snapshot.Phase != arkchannel.PhaseNegotiating &&
		snapshot.Phase != arkchannel.PhaseCancelling {
		return nil
	}

	controller, err := c.prePONRController()
	if err != nil {
		return err
	}
	if snapshot.Source == nil {
		if c.cfg.LookupOOR == nil {
			return fmt.Errorf("Ark channel OOR lookup is " +
				"unavailable")
		}
		lookup, err := c.cfg.LookupOOR(
			ctx, snapshot.Terms, arkchannel.DefaultBackingFee,
		)
		if err != nil {
			return err
		}
		switch lookup.Status {
		case oorbridge.PreparationAbsent:
			if !prePONRExpired(record, now) {
				return nil
			}
			expiry := &arkchannel.ExpirePrePONR{
				Reason: arkChannelPrePONRExpiryReason,
			}
			_, _, err := c.coordinator.Apply(
				ctx, snapshot.Terms.ID, expiry,
			)

			return err

		case oorbridge.PreparationPending:
			return nil

		case oorbridge.PreparationAccepted:
			return fmt.Errorf("channel OOR advanced without a " +
				"durable source binding")

		case oorbridge.PreparationPrepared:
			if err := controller.ValidatePreparedOOR(
				ctx, snapshot.Terms, lookup.Binding,
			); err != nil {
				return err
			}
			record, _, err = c.coordinator.Apply(
				ctx, snapshot.Terms.ID, &arkchannel.BindVTXO{
					Binding: lookup.Binding,
				},
			)
			if err != nil {
				return err
			}
			snapshot = record.Snapshot

		default:
			return fmt.Errorf("unknown channel OOR preparation "+
				"status %d", lookup.Status)
		}
	}

	// A client-funded promotion records the recovered source locally before
	// replaying the idempotent peer binding. This prevents a second local
	// OOR selection if the peer is temporarily unavailable.
	if c.party == arkchannel.PartyClient && c.remote != nil &&
		snapshot.Source != nil &&
		(snapshot.Phase == arkchannel.PhaseRequested ||
			snapshot.Phase == arkchannel.PhaseNegotiating) {

		if _, err := c.remote.BindPreparedOOR(
			ctx, snapshot.Terms.ID, *snapshot.Source,
		); err != nil {
			return err
		}
	}

	if snapshot.Phase != arkchannel.PhaseCancelling {
		if !prePONRExpired(record, now) {
			return nil
		}
		record, _, err = c.coordinator.Apply(
			ctx, snapshot.Terms.ID, &arkchannel.ExpirePrePONR{
				Reason: arkChannelPrePONRExpiryReason,
			},
		)
		if err != nil {
			return err
		}
		snapshot = record.Snapshot
		if snapshot.Phase != arkchannel.PhaseCancelling {
			return nil
		}
	}

	result, err := controller.AbortPreparedOORResult(
		ctx, snapshot.Terms.ID, snapshot.Terms, *snapshot.Source,
		snapshot.Failure,
	)
	if err != nil {
		return err
	}
	if result.Finalized {
		return fmt.Errorf("channel OOR commit won while pre-PONR " +
			"cancellation was pending")
	}
	if err := result.Validate(); err != nil {
		return err
	}
	_, _, err = c.coordinator.Apply(
		ctx, snapshot.Terms.ID, &arkchannel.OORAborted{
			SessionID: snapshot.Source.OORSessionID,
			Reason:    result.Reason,
		},
	)
	if err != nil {
		return err
	}
	if service == nil {
		return nil
	}
	_, err = service.ResumeChannelAction(ctx, snapshot.Terms.ID)

	return err
}

// prePONRController returns the OOR controller owned by this endpoint's funder.
func (c *NativeArkChannelController) prePONRController() (
	prePONRResultController, error) {

	var candidate arkchannel.OORTransferController
	if c.cfg.FundingOOR != nil {
		candidate = c.cfg.FundingOOR
	} else if c.cfg.OOR != nil {
		candidate = c.cfg.OOR
	}
	controller, ok := candidate.(prePONRResultController)
	if !ok {
		return nil, fmt.Errorf("result-bearing Ark channel OOR " +
			"controller is required")
	}

	return controller, nil
}

// prePONRExpired reports whether the durable preparation lease elapsed.
func prePONRExpired(record arkchannel.Record, now time.Time) bool {
	if record.PrePONRStartedAt.IsZero() ||
		now.Before(record.PrePONRStartedAt) {
		return false
	}

	return now.Sub(record.PrePONRStartedAt) >=
		defaultArkChannelPrePONRTimeout
}
