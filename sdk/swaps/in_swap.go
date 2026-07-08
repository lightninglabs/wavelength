package swaps

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	loopfsm "github.com/lightninglabs/loop/fsm"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	refundLocktimeNearBlocks      = uint32(3)
	refundLocktimeMaxPollMultiple = uint32(30)
	refundLocktimeMaxPollInterval = time.Minute
)

// PayState identifies the client-side lifecycle state of an Ark-to-Lightning
// pay flow.
type PayState uint8

const (
	// PayStateCreated means the local SDK session exists but has not yet
	// requested swap parameters from the server.
	PayStateCreated PayState = iota

	// PayStateSwapCreated means the server has accepted the invoice and
	// returned the vHTLC parameters, but the client has not funded it yet.
	PayStateSwapCreated

	// PayStateFundingInitiated means the client durably recorded funding
	// intent and is reconciling or submitting the OOR transfer that funds
	// the vHTLC.
	PayStateFundingInitiated

	// PayStateVHTLCFunded means the expected vHTLC is indexed as live.
	PayStateVHTLCFunded

	// PayStateWaitingForClaim means the server can claim the funded vHTLC
	// after paying the Lightning invoice, and the client is waiting for the
	// claim preimage to become indexed.
	PayStateWaitingForClaim

	// PayStateCompleted means the client observed a valid claim preimage
	// for the payment hash.
	PayStateCompleted

	// PayStateExpired means the negotiated swap deadline elapsed before the
	// client funded a vHTLC or before any accepted funding attempt needed
	// recovery.
	PayStateExpired

	// PayStateRefundInitiated means the funded vHTLC timed out and the
	// client durably recorded refund intent before submitting or
	// reconciling the timeout refund spend.
	PayStateRefundInitiated

	// PayStateRefunded means the client observed or submitted a timeout
	// refund spend for the funded vHTLC. This is terminal because the
	// Lightning invoice was not paid, but the client's Ark funds were
	// recovered.
	PayStateRefunded

	// PayStateNeedsIntervention means the client observed anomalous
	// funding or claim data that should be preserved for operator
	// inspection instead of being collapsed into a generic failure.
	PayStateNeedsIntervention

	// PayStateFailed means the local client flow hit an unrecoverable
	// error.
	PayStateFailed
)

// String returns a human-readable pay state name.
func (s PayState) String() string {
	switch s {
	case PayStateCreated:
		return "Created"

	case PayStateSwapCreated:
		return "SwapCreated"

	case PayStateFundingInitiated:
		return "FundingInitiated"

	case PayStateVHTLCFunded:
		return "VHTLCFunded"

	case PayStateWaitingForClaim:
		return "WaitingForClaim"

	case PayStateCompleted:
		return "Completed"

	case PayStateExpired:
		return "Expired"

	case PayStateRefundInitiated:
		return "RefundInitiated"

	case PayStateRefunded:
		return "Refunded"

	case PayStateNeedsIntervention:
		return "NeedsIntervention"

	case PayStateFailed:
		return "Failed"

	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

// IsTerminal returns true if no more pay lifecycle work should run.
func (s PayState) IsTerminal() bool {
	return s == PayStateCompleted ||
		s == PayStateExpired ||
		s == PayStateRefunded ||
		s == PayStateNeedsIntervention ||
		s == PayStateFailed
}

// payEvent identifies one transition edge in the pay FSM.
type payEvent = loopfsm.EventType

const (
	// payEventAdvance asks the current Loop FSM state to reconcile the pay
	// session against daemon, server, and indexer state.
	payEventAdvance = loopfsm.EventType("OnAdvance")

	// payEventSwapCreated records that the server returned the in-swap
	// configuration and the SDK derived the matching vHTLC script.
	payEventSwapCreated = loopfsm.EventType("OnSwapCreated")

	// payEventFundingInitiated records durable funding intent before the
	// client submits or retries the OOR funding transfer.
	payEventFundingInitiated = loopfsm.EventType("OnFundingInitiated")

	// payEventVHTLCFunded records that the expected vHTLC is live.
	payEventVHTLCFunded = loopfsm.EventType("OnVHTLCFunded")

	// payEventWaitForClaim records that the funded vHTLC can now be claimed
	// by the server after it pays the Lightning invoice.
	payEventWaitForClaim = loopfsm.EventType("OnWaitForClaim")

	// payEventCompleted records terminal success.
	payEventCompleted = loopfsm.EventType("OnCompleted")

	// payEventExpired records terminal expiry.
	payEventExpired = loopfsm.EventType("OnExpired")

	// payEventRefundInitiated records durable refund intent before the
	// client submits or reconciles a timeout refund spend.
	payEventRefundInitiated = loopfsm.EventType("OnRefundInitiated")

	// payEventRefunded records terminal recovery of the timed-out vHTLC.
	payEventRefunded = loopfsm.EventType("OnRefunded")

	// payEventNeedsIntervention records a terminal anomalous state that
	// requires operator inspection.
	payEventNeedsIntervention = loopfsm.EventType("OnNeedsIntervention")

	// payEventFailed records terminal local failure.
	payEventFailed = loopfsm.EventType("OnFailed")
)

// payTransitions is the complete transition descriptor for client-side
// Ark-to-Lightning payments.
var payTransitions = map[PayState]map[payEvent]PayState{
	PayStateCreated: {
		payEventSwapCreated:       PayStateSwapCreated,
		payEventExpired:           PayStateExpired,
		payEventNeedsIntervention: PayStateNeedsIntervention,
		payEventFailed:            PayStateFailed,
	},
	PayStateSwapCreated: {
		payEventFundingInitiated:  PayStateFundingInitiated,
		payEventVHTLCFunded:       PayStateVHTLCFunded,
		payEventCompleted:         PayStateCompleted,
		payEventExpired:           PayStateExpired,
		payEventRefundInitiated:   PayStateRefundInitiated,
		payEventNeedsIntervention: PayStateNeedsIntervention,
		payEventFailed:            PayStateFailed,
	},
	PayStateFundingInitiated: {
		payEventVHTLCFunded:       PayStateVHTLCFunded,
		payEventCompleted:         PayStateCompleted,
		payEventExpired:           PayStateExpired,
		payEventRefundInitiated:   PayStateRefundInitiated,
		payEventNeedsIntervention: PayStateNeedsIntervention,
		payEventFailed:            PayStateFailed,
	},
	PayStateVHTLCFunded: {
		payEventWaitForClaim:      PayStateWaitingForClaim,
		payEventCompleted:         PayStateCompleted,
		payEventExpired:           PayStateExpired,
		payEventRefundInitiated:   PayStateRefundInitiated,
		payEventNeedsIntervention: PayStateNeedsIntervention,
		payEventFailed:            PayStateFailed,
	},
	PayStateWaitingForClaim: {
		payEventCompleted:         PayStateCompleted,
		payEventExpired:           PayStateExpired,
		payEventRefundInitiated:   PayStateRefundInitiated,
		payEventRefunded:          PayStateRefunded,
		payEventNeedsIntervention: PayStateNeedsIntervention,
		payEventFailed:            PayStateFailed,
	},
	PayStateRefundInitiated: {
		payEventCompleted:         PayStateCompleted,
		payEventRefunded:          PayStateRefunded,
		payEventNeedsIntervention: PayStateNeedsIntervention,
		payEventFailed:            PayStateFailed,
	},
}

// paySession owns one blocking Ark-to-Lightning payment flow.
type paySession struct {
	client *SwapClient

	invoice      string
	maxFeeSat    uint64
	maxCreditSat uint64
	state        PayState
	createdAt    time.Time
	updatedAt    time.Time

	cfg                 *InSwapConfig
	vhtlcPolicy         *arkscript.VHTLCPolicy
	vhtlcPkScript       []byte
	vhtlcPolicyTemplate []byte
	vhtlcOutpoint       string
	vhtlcAmount         int64
	fundingSessionID    string
	refundReceivePubKey []byte
	refundReceiveScript []byte
	// refundSessionID stores the daemon OOR session id when this process
	// submitted the refund, or the observed spender txid when a resume
	// adopts an already-indexed refund spend.
	refundSessionID         string
	refundRecoveryID        string
	refundRecoveryFailureAt time.Time
	preimage                *lntypes.Preimage
	interventionReason      string
	clientPubKey            *btcec.PublicKey
	operatorPubKey          *btcec.PublicKey
	serverPubKey            *btcec.PublicKey
}

// PaySession is the exported alias for the client-side Ark-to-Lightning swap
// session type. A session is not safe for concurrent method calls; callers
// should not call Wait, State, or other methods from multiple goroutines at
// the same time.
type PaySession = paySession

// State returns the current client-side pay lifecycle state.
func (s *paySession) State() PayState {
	if s == nil {
		return PayStateFailed
	}

	return s.state
}

// PaymentHash returns the Lightning payment hash for this pay session.
//
// The value becomes available once the swap server has accepted the payment
// request and the session has enough durable identity to be resumed later. A
// daemon-owned background executor can use it as the worker key without
// re-parsing the BOLT-11 invoice or reaching into unexported session config.
func (s *paySession) PaymentHash() lntypes.Hash {
	if s == nil || s.cfg == nil {
		return lntypes.Hash{}
	}

	return s.cfg.PaymentHash
}

// InterventionReason returns the durable operator-facing reason for a
// NeedsIntervention pay session.
func (s *paySession) InterventionReason() string {
	if s == nil || s.state != PayStateNeedsIntervention {
		return ""
	}

	return s.interventionReason
}

// TerminalReason returns the durable explanation for a terminal pay session.
func (s *paySession) TerminalReason() string {
	if s == nil {
		return ""
	}

	return s.interventionReason
}

// terminalErr converts one terminal pay state into the public blocking API
// error returned by runUntil.
func (s *paySession) terminalErr() error {
	if s == nil {
		return fmt.Errorf("pay session must be provided")
	}

	switch s.state {
	case PayStateExpired:
		return errSwapExpired

	case PayStateRefunded:
		return ErrSwapRefunded

	case PayStateNeedsIntervention:
		return newInterventionError(s.interventionReason, nil)

	case PayStateFailed:
		return newFailureError(s.interventionReason, nil)

	default:
		return fmt.Errorf("pay session stopped in terminal state %s",
			s.state)
	}
}

// markExpired persists terminal expiry for the pay session and returns the
// canonical expiry error.
func (s *paySession) markExpired(ctx context.Context, reason string) error {
	if s == nil {
		return errSwapExpired
	}

	if s.state != PayStateExpired {
		s.client.log.InfoS(ctx, "Pay swap expired",
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.String("reason", reason),
		)

		err := s.mutateAndPersist(ctx, func() error {
			return s.transition(payEventExpired)
		})
		if err != nil {
			return err
		}
	}

	return errSwapExpired
}

// initiateRefund records timeout-refund intent for a funded pay-side vHTLC.
func (s *paySession) initiateRefund(ctx context.Context, reason string) error {
	if s == nil {
		return errSwapExpired
	}

	if s.state == PayStateRefundInitiated {
		return nil
	}

	s.client.log.InfoS(ctx, "Pay swap refund initiated",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("reason", reason),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.Int64("amount_sat", s.vhtlcAmount),
	)

	return s.mutateAndPersist(ctx, func() error {
		s.interventionReason = reason

		return s.transition(payEventRefundInitiated)
	})
}

// needsIntervention persists one anomalous pay-side condition and returns the
// canonical intervention error for the blocking API surface.
func (s *paySession) needsIntervention(ctx context.Context, reason string,
	cause error, mutate func()) error {

	if s == nil {
		return newInterventionError(reason, cause)
	}

	s.client.log.WarnS(ctx, "Pay swap needs intervention",
		cause,
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("reason", reason),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.Int64("amount_sat", s.vhtlcAmount),
	)

	err := s.mutateAndPersist(ctx, func() error {
		if mutate != nil {
			mutate()
		}

		s.interventionReason = reason
		if s.state == PayStateNeedsIntervention {
			return nil
		}

		return s.transition(payEventNeedsIntervention)
	})
	if err != nil {
		return err
	}

	return newInterventionError(reason, cause)
}

// failTerminal persists one safely-classified pay-side terminal failure and
// returns the canonical failure error for the blocking API surface.
func (s *paySession) failTerminal(ctx context.Context, reason string,
	cause error, mutate func()) error {

	if s == nil {
		return newFailureError(reason, cause)
	}

	s.client.log.WarnS(ctx, "Pay swap failed",
		cause,
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("reason", reason),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.Int64("amount_sat", s.vhtlcAmount),
	)

	err := s.mutateAndPersist(ctx, func() error {
		if mutate != nil {
			mutate()
		}

		s.interventionReason = reason
		if s.state == PayStateFailed {
			return nil
		}

		return s.transition(payEventFailed)
	})
	if err != nil {
		return err
	}

	return newFailureError(reason, cause)
}

// PayViaLightning performs a complete Ark-to-Lightning swap in a single
// blocking call. The client creates an in-swap with the server, funds the
// server-claimable vHTLC, then waits until the authoritative indexer exposes
// the vHTLC claim preimage.
func (c *SwapClient) PayViaLightning(ctx context.Context, invoice string,
	maxFeeSat uint64) (*PayResult, error) {

	session, err := c.StartPayViaLightning(ctx, invoice, maxFeeSat)
	if err != nil {
		return nil, err
	}

	return session.Wait(ctx)
}

// StartPayViaLightning creates an Ark-to-Lightning pay session and advances it
// until the server has accepted the swap and the client derived the exact
// vHTLC script it must fund.
func (c *SwapClient) StartPayViaLightning(ctx context.Context, invoice string,
	maxFeeSat uint64) (*PaySession, error) {

	return c.StartPayViaLightningWithCredits(ctx, invoice, maxFeeSat, 0)
}

// StartPayViaLightningWithCredits creates an Ark-to-Lightning pay session and
// allows the swap server to reserve up to maxCreditSat credits.
func (c *SwapClient) StartPayViaLightningWithCredits(ctx context.Context,
	invoice string, maxFeeSat uint64, maxCreditSat uint64) (*PaySession,
	error) {

	session := &paySession{
		client:       c,
		invoice:      invoice,
		maxFeeSat:    maxFeeSat,
		maxCreditSat: maxCreditSat,
		state:        PayStateCreated,
	}

	if err := session.runUntil(ctx, PayStateSwapCreated); err != nil {
		return nil, err
	}

	return session, nil
}

// Wait blocks until the server either claims the funded vHTLC with the
// expected preimage or the pay session reaches a terminal error state.
func (s *paySession) Wait(ctx context.Context) (*PayResult, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("pay session must be provided")
	}

	if err := s.runUntil(ctx, PayStateCompleted); err != nil {
		return nil, err
	}

	return &PayResult{
		PaymentHash:      s.cfg.PaymentHash,
		Preimage:         *s.preimage,
		FundingSessionID: s.fundingSessionID,
		FeeSat:           s.cfg.FeeSat,
	}, nil
}

// runUntil advances the pay FSM until the target or a terminal state is
// reached.
func (s *paySession) runUntil(ctx context.Context, target PayState) error {
	machine := newPayLoopFSM(s, target)

	for s.state != target {
		if s.state.IsTerminal() {
			if s.state == PayStateCompleted {
				return nil
			}

			return s.terminalErr()
		}

		if err := machine.advance(ctx); err != nil {
			return err
		}
	}

	return nil
}

// transition applies one pay FSM event to the current state.
func (s *paySession) transition(event payEvent) error {
	next, ok := payTransitions[s.state][event]
	if !ok {
		return fmt.Errorf("invalid pay transition %s -> %s", s.state,
			event)
	}

	s.state = next

	return nil
}

// createSwap requests in-swap parameters from the server and derives the
// exact vHTLC script the client must fund.
func (s *paySession) createSwap(ctx context.Context) error {
	clientKey, err := s.client.daemon.IdentityPubKey(ctx)
	if err != nil {
		return fmt.Errorf("get client pubkey: %w", err)
	}

	var cfg *InSwapConfig
	if server, ok := s.client.server.(interface {
		CreateInSwapWithCredits(context.Context, string, uint64,
			*btcec.PublicKey, []byte, uint64) (*InSwapConfig, error)
	}); ok {

		cfg, err = server.CreateInSwapWithCredits(
			ctx, s.invoice, s.maxFeeSat, clientKey,
			clientKey.SerializeCompressed(), s.maxCreditSat,
		)
	} else {
		cfg, err = s.client.server.CreateInSwap(
			ctx, s.invoice, s.maxFeeSat, clientKey,
		)
	}
	if err != nil {
		return fmt.Errorf("create in-swap: %w", err)
	}
	if cfg == nil {
		return fmt.Errorf("in-swap config is required")
	}
	if cfg.SettlementType == "" {
		cfg.SettlementType = SettlementTypeLightning
	}

	err = validateInSwapQuote(
		s.invoice, s.maxFeeSat, cfg, s.client.chainParams,
	)
	if err != nil {
		return fmt.Errorf("validate in-swap quote: %w", err)
	}

	if cfg.SettlementType == SettlementTypeCredit {
		var creditAppliedSat uint64
		if cfg.CreditQuote != nil {
			creditAppliedSat = cfg.CreditQuote.CreditAppliedSat
		}

		s.client.log.InfoS(ctx, "Credit in-swap completed",
			btclog.Hex("hash", cfg.PaymentHash[:]),
			slog.Uint64("credit_applied_sat", creditAppliedSat),
			slog.Time("deadline", cfg.Expiry),
		)

		return s.mutateAndPersist(ctx, func() error {
			if s.createdAt.IsZero() {
				s.createdAt = s.client.currentTime()
			}
			s.cfg = cfg
			s.preimage = cfg.Preimage
			s.clientPubKey = clientKey
			s.operatorPubKey = clientKey
			s.serverPubKey = clientKey
			if err := s.transition(
				payEventSwapCreated,
			); err != nil {
				return err
			}

			return s.transition(payEventCompleted)
		})
	}

	operatorKey, err := s.client.daemon.OperatorPubKey(ctx)
	if err != nil {
		return fmt.Errorf("get operator pubkey: %w", err)
	}

	// The LN payment hash is already SHA256(preimage), which is the value
	// the vHTLC script commits to for its hashlock branch.
	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:       clientKey,
		Receiver:     cfg.ServerPubkey,
		Server:       operatorKey,
		PreimageHash: cfg.PaymentHash,
		RefundLocktime: cfg.VHTLCConfig.
			RefundLocktime,
		UnilateralClaimDelay: cfg.VHTLCConfig.
			UnilateralClaimDelay,
		UnilateralRefundDelay: cfg.VHTLCConfig.
			UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: cfg.
			VHTLCConfig.
			UnilateralRefundWithoutReceiverDelay,
	})
	if err != nil {
		return fmt.Errorf("build vHTLC policy: %w", err)
	}

	pkScript, err := policy.PkScript()
	if err != nil {
		return fmt.Errorf("get vHTLC pkScript: %w", err)
	}

	policyTemplate, err := encodeVHTLCPolicyTemplate(policy)
	if err != nil {
		return fmt.Errorf("encode vHTLC policy: %w", err)
	}

	s.client.log.InfoS(ctx, "In-swap created",
		btclog.Hex("hash", cfg.PaymentHash[:]),
		slog.Int64("amount_sat", cfg.AmountSat),
		slog.Uint64("fee_sat", cfg.FeeSat),
		slog.String("settlement_type", string(cfg.SettlementType)),
		slog.Time("deadline", cfg.Expiry),
	)

	return s.mutateAndPersist(ctx, func() error {
		if s.createdAt.IsZero() {
			s.createdAt = s.client.currentTime()
		}
		s.cfg = cfg
		s.vhtlcPolicy = policy
		s.vhtlcPkScript = pkScript
		s.vhtlcPolicyTemplate = policyTemplate
		s.clientPubKey = clientKey
		s.operatorPubKey = operatorKey
		s.serverPubKey = cfg.ServerPubkey

		return s.transition(payEventSwapCreated)
	})
}

// ensureFundingStillSafe refuses to submit or retry funding once the local
// wall-clock funding deadline is effectively exhausted or the vHTLC refund
// locktime is already imminent.
func (s *paySession) ensureFundingStillSafe(ctx context.Context) error {
	now := s.client.currentTime()
	if !s.cfg.Expiry.IsZero() &&
		!now.Add(s.client.fundingExpiryBuffer).Before(s.cfg.Expiry) {
		return s.markExpired(
			ctx, fmt.Sprintf("funding deadline %s is too close "+
				"or already reached", s.cfg.Expiry),
		)
	}

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return fmt.Errorf("get block height: %w", err)
	}

	if height+s.client.refundLocktimeBuffer >=
		s.cfg.VHTLCConfig.RefundLocktime {
		return s.failTerminal(
			ctx, fmt.Sprintf("refund locktime %d is too close at "+
				"height %d", s.cfg.VHTLCConfig.RefundLocktime,
				height),
			nil,
			nil,
		)
	}

	return nil
}

// fundOrAdoptVHTLC reconciles already-indexed state before submitting funding,
// then records funding intent once the daemon accepts the OOR transfer.
func (s *paySession) fundOrAdoptVHTLC(ctx context.Context) error {
	funded, err := s.observeLiveVHTLC(ctx)
	switch {
	// A fresh in-swap has not funded its vHTLC yet, so the script is not
	// registered with the authoritative indexer and the pre-funding adopt
	// query is expected to be rejected on the first pass. Log it at debug
	// so it is not mistaken for the genuine receive-side registration loop
	// tracked in #538; only real query failures warrant a warning.
	case err != nil && isUnregisteredScriptErr(err):
		s.client.log.DebugS(
			ctx,
			"In-swap vHTLC not yet registered before funding",
			slog.String("err", err.Error()),
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
		)

	case err != nil:
		s.client.log.WarnS(
			ctx,
			"Unable to query in-swap vHTLC before funding",
			err,
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
		)
	}
	if s.state == PayStateRefundInitiated {
		return nil
	}
	if funded {
		return s.mutateAndPersist(ctx, func() error {
			return s.transition(payEventVHTLCFunded)
		})
	}

	if err := s.ensureFundingStillSafe(ctx); err != nil {
		return err
	}

	if s.state == PayStateSwapCreated {
		if err := s.mutateAndPersist(ctx, func() error {
			return s.transition(payEventFundingInitiated)
		}); err != nil {
			return err
		}
	}

	return s.ensureFundingSubmitted(ctx, true)
}

// waitForFundedVHTLC waits until either the funded vHTLC becomes live or the
// server claims it quickly enough that the preimage is indexed first.
func (s *paySession) waitForFundedVHTLC(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		preimage, spentVHTLC, err :=
			s.client.waitForInSwapClaimObservation(
				ctx, s.cfg.PaymentHash, s.vhtlcPkScript,
			)
		if err != nil {
			s.client.log.DebugS(
				ctx,
				"Unable to query in-swap claim preimage",
				slog.String("err", err.Error()),
				btclog.Hex("hash", s.cfg.PaymentHash[:]),
			)
		}
		if preimage != nil {
			if err := cancelVHTLCRecovery(
				ctx, s.client.daemon, s.refundRecoveryID,
				recoveryReasonServerClaimObserved, "",
			); err != nil {
				return newRetryableActionError(err)
			}

			return s.mutateAndPersist(ctx, func() error {
				s.preimage = preimage

				return s.transition(payEventCompleted)
			})
		}
		if spentVHTLC != nil {
			reason := "funded vHTLC spent without claim preimage"
			if spentVHTLC.SpentByTxID != "" {
				reason = fmt.Sprintf("%s (spender %s)", reason,
					spentVHTLC.SpentByTxID)
			}

			return s.needsIntervention(ctx, reason, nil, func() {
				s.vhtlcOutpoint = spentVHTLC.Outpoint
				s.vhtlcAmount = spentVHTLC.AmountSat
			})
		}

		funded, err := s.observeLiveVHTLC(ctx)
		if err != nil {
			s.client.log.DebugS(
				ctx,
				"Unable to query in-swap vHTLC",
				slog.String("err", err.Error()),
				btclog.Hex("hash", s.cfg.PaymentHash[:]),
			)
		}
		if s.state == PayStateRefundInitiated {
			return nil
		}
		if funded {
			return s.mutateAndPersist(ctx, func() error {
				return s.transition(payEventVHTLCFunded)
			})
		}

		if err := s.ensureFundingStillSafe(ctx); err != nil {
			return err
		}

		if err := s.ensureFundingSubmitted(ctx, false); err != nil {
			return err
		}

		if err := s.waitForNextPoll(ctx); err != nil {
			if errors.Is(err, errSwapExpired) {
				if s.fundingSessionID != "" {
					return waitForFixedPoll(
						ctx, s.client.waitPollInterval,
					)
				}

				return s.mutateAndPersist(ctx, func() error {
					return s.transition(payEventExpired)
				})
			}

			return err
		}
	}
}

// ensureFundingSubmitted records or retries the pay-side OOR funding send.
// When a session resumes in FundingInitiated without a persisted session id,
// the method waits through a short ambiguity window before retrying the send so
// an accepted-but-not-yet-indexed funding attempt is not duplicated
// immediately after restart.
func (s *paySession) ensureFundingSubmitted(ctx context.Context,
	allowImmediate bool) error {

	if s.fundingSessionID != "" {
		if s.vhtlcOutpoint != "" && s.vhtlcAmount > 0 {
			return s.markVHTLCFundedFromLocalMetadata(ctx)
		}

		return nil
	}

	if !allowImmediate {
		grace := s.client.fundingResumeGracePeriod
		lastUpdate := s.updatedAt
		if grace > 0 && !lastUpdate.IsZero() &&
			s.client.currentTime().Before(lastUpdate.Add(grace)) {
			return nil
		}
	}

	result, err := s.client.daemon.SendOORWithPolicyDetails(
		ctx, s.cfg.AmountSat, s.vhtlcPolicyTemplate,
	)
	if err != nil {
		// A retry can race with a funding attempt that was accepted but
		// not yet observed by this SDK instance. Reconcile once before
		// surfacing the send failure.
		funded, reconcileErr := s.observeLiveVHTLC(ctx)
		if reconcileErr != nil {
			s.client.log.DebugS(
				ctx,
				"Unable to reconcile in-swap vHTLC after "+
					"funding error",
				slog.String("err", reconcileErr.Error()),
				btclog.Hex("hash", s.cfg.PaymentHash[:]),
			)
		}
		if s.state == PayStateRefundInitiated {
			return nil
		}
		if funded {
			return s.mutateAndPersist(ctx, func() error {
				return s.transition(payEventVHTLCFunded)
			})
		}

		return fmt.Errorf("fund vHTLC: %w", err)
	}
	if result == nil || result.SessionID == "" {
		return fmt.Errorf("fund vHTLC: daemon returned empty OOR " +
			"session id")
	}

	s.client.log.InfoS(ctx, "In-swap vHTLC funding submitted",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("txid", result.SessionID),
		slog.String("outpoint", result.RecipientOutpoint),
	)

	if err := s.persistFundingResult(
		ctx, result.SessionID, result.RecipientOutpoint,
		s.cfg.AmountSat,
	); err != nil {
		return newRetryableActionError(err)
	}
	if result.RecipientOutpoint == "" {
		return nil
	}

	return s.markVHTLCFundedFromLocalMetadata(ctx)
}

// markVHTLCFundedFromLocalMetadata records progress from a locally known
// funding outpoint. This lets retries recover even if the OOR metadata was
// persisted but the subsequent state transition did not make it to the swap
// DB.
func (s *paySession) markVHTLCFundedFromLocalMetadata(
	ctx context.Context) error {

	s.client.log.InfoS(ctx, "In-swap vHTLC funded from local OOR metadata",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.Int64("amount_sat", s.vhtlcAmount),
	)

	if err := s.ensurePayRefundRecoveryArmed(ctx); err != nil {
		return newRetryableActionError(err)
	}

	return s.mutateAndPersist(ctx, func() error {
		switch s.state {
		case PayStateVHTLCFunded, PayStateWaitingForClaim:
			return nil

		case PayStateSwapCreated, PayStateFundingInitiated:
			return s.transition(payEventVHTLCFunded)

		default:
			// The funding paths only call this helper before claim
			// handling starts. Leave any unexpected later state
			// alone instead of manufacturing a stale transition.
			return nil
		}
	})
}

// waitForClaimPreimage waits until the server claim spend is indexed and the
// finalized checkpoint exposes the expected hashlock preimage.
func (s *paySession) waitForClaimPreimage(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		preimage, spentVHTLC, err :=
			s.client.waitForInSwapClaimObservation(
				ctx, s.cfg.PaymentHash, s.vhtlcPkScript,
			)
		if err != nil {
			s.client.log.DebugS(
				ctx,
				"Unable to query in-swap claim preimage",
				slog.String("err", err.Error()),
				btclog.Hex("hash", s.cfg.PaymentHash[:]),
			)
		}
		if preimage != nil {
			s.client.log.InfoS(ctx, "In-swap completed",
				btclog.Hex("hash", s.cfg.PaymentHash[:]),
			)

			if err := cancelVHTLCRecovery(
				ctx, s.client.daemon, s.refundRecoveryID,
				recoveryReasonServerClaimObserved, "",
			); err != nil {
				return newRetryableActionError(err)
			}

			return s.mutateAndPersist(ctx, func() error {
				s.preimage = preimage

				return s.transition(payEventCompleted)
			})
		}
		if spentVHTLC != nil {
			refundOutput, err := s.observeRefundOutput(ctx)
			if err != nil {
				return newRetryableActionError(
					fmt.Errorf("query in-swap refund "+
						"output after spend: %w", err),
				)
			}
			if refundOutput != nil {
				return s.markRefundOutputIndexed(
					ctx, refundOutput,
				)
			}
			if len(s.refundReceiveScript) > 0 {
				return s.markRefunded(ctx, spentVHTLC)
			}

			reason := "funded vHTLC spent without claim preimage"
			if spentVHTLC.SpentByTxID != "" {
				reason = fmt.Sprintf("%s (spender %s)", reason,
					spentVHTLC.SpentByTxID)
			}

			return s.needsIntervention(
				ctx, reason, nil, func() {
					s.vhtlcOutpoint = spentVHTLC.Outpoint
					s.vhtlcAmount = spentVHTLC.AmountSat
				},
			)
		}

		refunded, err := s.tryCooperativeRefund(ctx)
		if err != nil {
			return err
		}
		if refunded {
			return nil
		}

		height, err := s.client.daemon.BlockHeight(ctx)
		if err != nil {
			s.client.log.DebugS(
				ctx,
				"Unable to query block height while waiting "+
					"for in-swap claim",
				slog.String("err", err.Error()),
				btclog.Hex("hash", s.cfg.PaymentHash[:]),
			)
			if err := s.waitForNextPoll(ctx); err != nil {
				if errors.Is(err, errSwapExpired) {
					return s.initiateRefund(
						ctx, "claim deadline elapsed",
					)
				}

				return err
			}

			continue
		}
		if height >= s.cfg.VHTLCConfig.RefundLocktime {
			return s.initiateRefund(
				ctx, "refund locktime reached before claim",
			)
		}

		if err := s.waitForNextPoll(ctx); err != nil {
			if errors.Is(err, errSwapExpired) {
				return s.initiateRefund(
					ctx, "claim deadline elapsed",
				)
			}

			return err
		}
	}
}

// completeRefund reconciles or submits the sender-side timeout refund for a
// funded pay-side vHTLC.
func (s *paySession) completeRefund(ctx context.Context) error {
	preimage, spentVHTLC, err := s.client.waitForInSwapClaimObservation(
		ctx, s.cfg.PaymentHash, s.vhtlcPkScript,
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("query in-swap claim before refund: %w",
				err),
		)
	}
	if preimage != nil {
		s.client.log.InfoS(ctx, "In-swap completed before refund",
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
		)

		if err := cancelVHTLCRecovery(
			ctx, s.client.daemon, s.refundRecoveryID,
			recoveryReasonServerClaimBeforeRefund, "",
		); err != nil {
			return newRetryableActionError(err)
		}

		return s.mutateAndPersist(ctx, func() error {
			s.preimage = preimage

			return s.transition(payEventCompleted)
		})
	}
	refundOutput, err := s.observeRefundOutput(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("query in-swap refund output: %w", err),
		)
	}
	if refundOutput != nil {
		return s.markRefundOutputIndexed(ctx, refundOutput)
	}
	if spentVHTLC != nil {
		return s.markRefunded(ctx, spentVHTLC)
	}

	recoveryHandled, err := s.reconcilePayRefundRecovery(ctx)
	if err != nil {
		return newRetryableActionError(err)
	}
	if recoveryHandled {
		return nil
	}

	sessionHandled, err := s.reconcilePayRefundSession(ctx)
	if err != nil {
		return newRetryableActionError(err)
	}
	if sessionHandled {
		return nil
	}

	funded, err := s.observeRefundableVHTLC(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("query in-swap vHTLC before refund: %w",
				err),
		)
	}
	if !funded {
		return waitForFixedPoll(ctx, s.client.waitPollInterval)
	}

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		s.client.log.DebugS(
			ctx,
			"Unable to query block height while waiting for "+
				"in-swap refund maturity",
			slog.String("err", err.Error()),
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
		)

		return waitForFixedPoll(ctx, s.client.waitPollInterval)
	}
	if height < s.cfg.VHTLCConfig.RefundLocktime {
		wait := s.refundLocktimePollInterval(height)
		s.client.log.DebugS(ctx, "Pay swap refund not yet mature",
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.Uint64("height", uint64(height)),
			slog.Uint64(
				"refund_locktime",
				uint64(s.cfg.VHTLCConfig.RefundLocktime),
			),
			slog.Duration("next_poll", wait),
		)

		return waitForFixedPoll(ctx, wait)
	}

	refundPubKey, err := s.refundPubKey(ctx)
	if err != nil {
		return err
	}

	refundPath, err := s.vhtlcPolicy.RefundWithoutReceiverPath()
	if err != nil {
		return fmt.Errorf("build refund path: %w", err)
	}

	spendPath, err := refundPath.Encode()
	if err != nil {
		return fmt.Errorf("encode refund path: %w", err)
	}

	refundSessionID, err := s.client.daemon.SendOORWithCustomInputs(
		ctx, refundPubKey, s.vhtlcAmount, []CustomInput{{
			Outpoint:           s.vhtlcOutpoint,
			VTXOPolicyTemplate: s.vhtlcPolicyTemplate,
			SpendPath:          spendPath,
			AmountSat:          s.vhtlcAmount,
			PkScript:           s.vhtlcPkScript,
		}},
	)
	if err != nil {
		if armErr := s.ensurePayRefundRecoveryArmed(
			ctx,
		); armErr != nil {
			return newRetryableActionError(armErr)
		}

		if escalateErr := s.maybeEscalatePayRefundRecovery(
			ctx, err,
		); escalateErr != nil {
			return newRetryableActionError(escalateErr)
		}

		return waitForFixedPoll(ctx, s.client.waitPollInterval)
	}
	if refundSessionID == "" {
		return fmt.Errorf("refund vHTLC returned empty session id")
	}

	s.refundSessionID = refundSessionID
	if err := s.persist(ctx); err != nil {
		return newRetryableActionError(
			fmt.Errorf("persist pay refund session id: %w", err),
		)
	}

	s.client.log.InfoS(ctx, "Pay swap refund submitted",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("refund_session_id", refundSessionID),
	)

	return waitForFixedPoll(ctx, s.client.waitPollInterval)
}

// refundLocktimePollInterval returns a slower timeout-refund poll interval when
// the refund branch is still many blocks away from maturity. The final few
// blocks keep the normal SDK interval so the client remains responsive near the
// first spendable height.
func (s *paySession) refundLocktimePollInterval(height uint32) time.Duration {
	wait := s.client.waitPollInterval
	locktime := s.cfg.VHTLCConfig.RefundLocktime
	if wait <= 0 || height >= locktime {
		return wait
	}

	remaining := locktime - height
	if remaining <= refundLocktimeNearBlocks {
		return wait
	}

	multiple := remaining / 2
	if multiple < 2 {
		multiple = 2
	}
	if multiple > refundLocktimeMaxPollMultiple {
		multiple = refundLocktimeMaxPollMultiple
	}

	backoff := wait * time.Duration(multiple)
	if backoff > refundLocktimeMaxPollInterval {
		return refundLocktimeMaxPollInterval
	}

	return backoff
}

// tryCooperativeRefund attempts an immediate refund before the timeout branch
// matures. The swap server only authorizes this after its Lightning payment
// attempt is terminal and no preimage is known, so an unavailable response just
// means the SDK should keep waiting for either claim or timeout.
func (s *paySession) tryCooperativeRefund(ctx context.Context) (bool, error) {
	if s.refundSessionID != "" {
		return true, s.initiateRefund(
			ctx, "cooperative refund already submitted",
		)
	}

	funded, err := s.observeRefundableVHTLC(ctx)
	if err != nil {
		return false, newRetryableActionError(
			fmt.Errorf("query in-swap vHTLC before cooperative "+
				"refund: %w", err),
		)
	}
	if !funded {
		return false, nil
	}

	refundPubKey, err := s.refundPubKey(ctx)
	if err != nil {
		return false, err
	}

	refundPath, err := s.vhtlcPolicy.RefundPath()
	if err != nil {
		return false, fmt.Errorf("build cooperative refund path: %w",
			err)
	}

	spendPath, err := refundPath.Encode()
	if err != nil {
		return false, fmt.Errorf("encode cooperative refund path: %w",
			err)
	}

	input := CustomInput{
		Outpoint:           s.vhtlcOutpoint,
		VTXOPolicyTemplate: s.vhtlcPolicyTemplate,
		SpendPath:          spendPath,
		AmountSat:          s.vhtlcAmount,
		PkScript:           s.vhtlcPkScript,
	}

	prepared, err := s.client.daemon.PrepareOORWithCustomInputs(
		ctx, refundPubKey, s.vhtlcAmount, []CustomInput{input},
	)
	if err != nil {
		return false, newRetryableActionError(
			fmt.Errorf("prepare cooperative in-swap refund: %w",
				err),
		)
	}

	preparedInput, err := preparedCustomInput(prepared, s.vhtlcOutpoint)
	if err != nil {
		return false, err
	}

	authorization, err := s.client.server.AuthorizeInSwapRefund(
		ctx, s.cfg.PaymentHash, s.vhtlcOutpoint, s.vhtlcAmount,
		s.vhtlcPolicyTemplate, spendPath, preparedInput.CheckpointPSBT,
	)
	if err != nil {
		code := status.Code(err)
		if code == codes.FailedPrecondition ||
			code == codes.Unavailable ||
			code == codes.Unimplemented {

			s.client.log.DebugS(
				ctx,
				"Cooperative in-swap refund not yet available",
				btclog.Hex("hash", s.cfg.PaymentHash[:]),
				slog.String("outpoint", s.vhtlcOutpoint),
			)

			return false, nil
		}

		return false, newRetryableActionError(
			fmt.Errorf("authorize cooperative in-swap refund: %w",
				err),
		)
	}
	if authorization == nil {
		return false, newRetryableActionError(
			fmt.Errorf("authorize cooperative in-swap refund: " +
				"empty response"),
		)
	}

	input.ExternalSignatures = []TaprootScriptSignature{
		authorization.Signature,
	}

	refundSessionID, err := s.client.daemon.SendOORWithCustomInputs(
		ctx, refundPubKey, s.vhtlcAmount, []CustomInput{input},
	)
	if err != nil {
		return false, newRetryableActionError(
			fmt.Errorf("submit cooperative in-swap refund: %w",
				err),
		)
	}
	if refundSessionID == "" {
		return false, fmt.Errorf("cooperative in-swap refund " +
			"returned empty session id")
	}

	reason := authorization.FailureReason
	if reason == "" {
		reason = "swap server safely failed Lightning payment"
	}

	s.client.log.InfoS(ctx, "Cooperative in-swap refund submitted",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.String("refund_session_id", refundSessionID),
		slog.String("reason", reason),
	)

	if err := s.persistRefundSessionID(
		ctx, refundSessionID, reason,
	); err != nil {
		return false, newRetryableActionError(
			fmt.Errorf("persist cooperative refund session id: %w",
				err),
		)
	}
	if err := s.initiateRefund(ctx, reason); err != nil {
		return false, newRetryableActionError(err)
	}

	return true, nil
}

// preparedCustomInput returns the prepared checkpoint signing data for one
// custom input outpoint.
func preparedCustomInput(prepared *PreparedOOR,
	outpoint string) (*PreparedOORCustomInput, error) {

	if prepared == nil {
		return nil, fmt.Errorf("prepared OOR package is required")
	}

	for i := range prepared.CustomInputs {
		input := &prepared.CustomInputs[i]
		if input.Outpoint != outpoint {
			continue
		}
		if len(input.CheckpointPSBT) == 0 {
			return nil, fmt.Errorf("prepared custom input %s "+
				"missing checkpoint PSBT", outpoint)
		}

		return input, nil
	}

	return nil, fmt.Errorf("prepared OOR package missing custom input %s",
		outpoint)
}

// observeRefundOutput returns the wallet output created by a refund once the
// daemon indexes the persisted refund destination. The refund output is the
// most direct proof that the client recovered funds, while the spent vHTLC
// record can lag behind or be unavailable depending on indexer timing.
func (s *paySession) observeRefundOutput(ctx context.Context) (*VTXOInfo,
	error) {

	if len(s.refundReceiveScript) == 0 {
		return nil, nil
	}

	refundOutput, err := s.client.daemon.FindLiveVTXOByPkScript(
		ctx, s.refundReceiveScript,
	)
	if err != nil || refundOutput == nil {
		return nil, err
	}
	if refundOutput.Outpoint == s.vhtlcOutpoint {
		return nil, nil
	}

	if s.vhtlcAmount != 0 && refundOutput.AmountSat != s.vhtlcAmount {
		s.client.log.WarnS(
			ctx,
			"Ignoring refund output with unexpected amount",
			nil,
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.String("outpoint", refundOutput.Outpoint),
			slog.Int64("amount", refundOutput.AmountSat),
			slog.Int64("want_amount", s.vhtlcAmount),
		)

		return nil, nil
	}

	return refundOutput, nil
}

// observeRefundableVHTLC records the live vHTLC while deliberately skipping
// the pre-refund-locktime safety check used before funding and claim waiting.
func (s *paySession) observeRefundableVHTLC(ctx context.Context) (bool, error) {
	vtxo, err := s.client.daemon.FindLiveVTXOByPkScript(
		ctx, s.vhtlcPkScript,
	)
	if err != nil {
		if isUnregisteredScriptErr(err) && s.vhtlcOutpoint != "" &&
			s.vhtlcAmount > 0 {

			s.client.log.DebugS(
				ctx,
				"Using local in-swap vHTLC metadata for refund",
				slog.String("err", err.Error()),
				btclog.Hex("hash", s.cfg.PaymentHash[:]),
				slog.String("outpoint", s.vhtlcOutpoint),
			)

			if err := s.ensurePayRefundRecoveryArmed(
				ctx,
			); err != nil {
				return false, err
			}

			return true, nil
		}

		return false, err
	}
	if vtxo == nil {
		return false, nil
	}

	err = s.mutateAndPersist(ctx, func() error {
		s.vhtlcOutpoint = vtxo.Outpoint
		s.vhtlcAmount = vtxo.AmountSat

		return nil
	})
	if err != nil {
		return false, err
	}

	if err := s.ensurePayRefundRecoveryArmed(ctx); err != nil {
		return false, err
	}

	return true, nil
}

// reconcilePayRefundSession checks whether the daemon has durable local OOR
// status for an accepted cooperative refund. This avoids depending solely on
// refund output indexing while still keeping retry blocked until the daemon
// reports a terminal refund session.
func (s *paySession) reconcilePayRefundSession(ctx context.Context) (bool,
	error) {

	if s.refundSessionID == "" {
		return false, nil
	}

	session, err := s.client.daemon.GetOORSession(ctx, s.refundSessionID)
	if err != nil {
		return true, err
	}
	if session == nil {
		s.client.log.DebugS(ctx, "Pay refund OOR session not found",
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.String("refund_session_id", s.refundSessionID),
		)

		return true, waitForFixedPoll(ctx, s.client.waitPollInterval)
	}

	switch session.GetStatus() {
	case daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED:
		return true, s.markRefundSessionCompleted(ctx, session)

	case daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED:
		reason := session.GetFailureReason()
		s.client.log.WarnS(ctx, "Pay refund OOR session failed",
			nil,
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.String("refund_session_id", s.refundSessionID),
			slog.String("phase", session.GetPhase()),
			slog.String("reason", reason),
		)

		return false, s.mutateAndPersist(ctx, func() error {
			s.refundSessionID = ""

			return nil
		})

	default:
		s.client.log.DebugS(ctx, "Pay refund OOR session pending",
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.String("refund_session_id", s.refundSessionID),
			slog.String("phase", session.GetPhase()),
			slog.String("status", session.GetStatus().String()),
		)

		return true, waitForFixedPoll(ctx, s.client.waitPollInterval)
	}
}

// markRefundSessionCompleted persists terminal recovery after the daemon's
// durable OOR session state confirms cooperative refund completion.
func (s *paySession) markRefundSessionCompleted(ctx context.Context,
	session *daemonrpc.OORSessionInfo) error {

	s.client.log.InfoS(ctx, "Pay swap refund session completed",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("refund_session_id", s.refundSessionID),
		slog.String("phase", session.GetPhase()),
	)
	if err := cancelVHTLCRecovery(
		ctx, s.client.daemon, s.refundRecoveryID,
		recoveryReasonRefundSessionCompleted, "",
	); err != nil {
		return err
	}

	return s.mutateAndPersist(ctx, func() error {
		return s.transition(payEventRefunded)
	})
}

// markRefundOutputIndexed persists terminal recovery after the refund output
// itself becomes live in the client wallet.
func (s *paySession) markRefundOutputIndexed(ctx context.Context,
	refundOutput *VTXOInfo) error {

	if refundOutput == nil {
		return nil
	}

	s.client.log.InfoS(ctx, "Pay swap refund output indexed",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("refund_outpoint", refundOutput.Outpoint),
		slog.Int64("amount_sat", refundOutput.AmountSat),
		slog.String("refund_session_id", s.refundSessionID),
	)
	if err := cancelVHTLCRecovery(
		ctx, s.client.daemon, s.refundRecoveryID,
		recoveryReasonRefundOutputIndexed, "",
	); err != nil {
		return newRetryableActionError(err)
	}

	return s.mutateAndPersist(ctx, func() error {
		return s.transition(payEventRefunded)
	})
}

// markRefunded persists terminal recovery after the vHTLC is spent without the
// expected claim preimage during the timeout-refund phase.
func (s *paySession) markRefunded(ctx context.Context,
	spentVHTLC *VTXOInfo) error {

	if spentVHTLC == nil {
		return nil
	}

	s.client.log.InfoS(ctx, "Pay swap refunded",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("outpoint", spentVHTLC.Outpoint),
		slog.String("spender", spentVHTLC.SpentByTxID),
	)

	return s.mutateAndPersist(ctx, func() error {
		if spentVHTLC.Outpoint != "" {
			s.vhtlcOutpoint = spentVHTLC.Outpoint
		}
		if spentVHTLC.AmountSat != 0 {
			s.vhtlcAmount = spentVHTLC.AmountSat
		}
		if s.refundSessionID == "" && spentVHTLC.SpentByTxID != "" {
			s.refundSessionID = spentVHTLC.SpentByTxID
		}

		return s.transition(payEventRefunded)
	})
}

// refundPubKey returns the persisted refund destination or allocates one from
// the Ark wallet before the timeout refund is submitted.
func (s *paySession) refundPubKey(ctx context.Context) ([]byte, error) {
	if len(s.refundReceivePubKey) > 0 {
		return append([]byte(nil), s.refundReceivePubKey...), nil
	}

	receiveInfo, err := s.client.daemon.AllocateReceiveScript(
		ctx, "swap-pay-refund",
	)
	if err != nil {
		return nil, fmt.Errorf("allocate refund receive script: %w",
			err)
	}
	if receiveInfo == nil || len(receiveInfo.PubKeyXOnly) == 0 ||
		len(receiveInfo.PkScript) == 0 {
		return nil, fmt.Errorf("refund receive script is required")
	}

	refundPubKey := append([]byte(nil), receiveInfo.PubKeyXOnly...)
	refundScript := append([]byte(nil), receiveInfo.PkScript...)
	err = s.mutateAndPersist(ctx, func() error {
		s.refundReceivePubKey = refundPubKey
		s.refundReceiveScript = refundScript

		return nil
	})
	if err != nil {
		return nil, err
	}

	return append([]byte(nil), s.refundReceivePubKey...), nil
}

// observeLiveVHTLC records the live vHTLC outpoint when the authoritative
// indexer has indexed the expected script.
func (s *paySession) observeLiveVHTLC(ctx context.Context) (bool, error) {
	vtxo, err := s.client.daemon.FindLiveVTXOByPkScript(
		ctx, s.vhtlcPkScript,
	)
	if err != nil || vtxo == nil {
		return false, err
	}

	expectedAmount := s.cfg.AmountSat
	if vtxo.AmountSat != expectedAmount {
		reason := fmt.Sprintf("funded vHTLC amount %d does not match "+
			"quote %d", vtxo.AmountSat, expectedAmount)

		return false, s.mutateAndPersist(ctx, func() error {
			s.vhtlcOutpoint = vtxo.Outpoint
			s.vhtlcAmount = vtxo.AmountSat
			s.interventionReason = reason

			return s.transition(payEventRefundInitiated)
		})
	}

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return false, fmt.Errorf("get block height: %w", err)
	}
	if height+s.client.refundLocktimeBuffer >=
		s.cfg.VHTLCConfig.RefundLocktime {

		reason := fmt.Sprintf("funded vHTLC observed too close to "+
			"refund locktime %d at height %d",
			s.cfg.VHTLCConfig.RefundLocktime, height)

		return false, s.mutateAndPersist(ctx, func() error {
			s.vhtlcOutpoint = vtxo.Outpoint
			s.vhtlcAmount = vtxo.AmountSat
			s.interventionReason = reason

			return s.transition(payEventRefundInitiated)
		})
	}

	s.client.log.InfoS(ctx, "In-swap vHTLC funded",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("outpoint", vtxo.Outpoint),
		slog.Int64("amount_sat", vtxo.AmountSat),
	)

	err = s.mutateAndPersist(ctx, func() error {
		s.vhtlcOutpoint = vtxo.Outpoint
		s.vhtlcAmount = vtxo.AmountSat

		return nil
	})
	if err != nil {
		return false, err
	}

	if err := s.ensurePayRefundRecoveryArmed(ctx); err != nil {
		return false, err
	}

	return true, nil
}

// waitForNextPoll sleeps until the next poll interval or the swap deadline,
// whichever arrives first.
func (s *paySession) waitForNextPoll(ctx context.Context) error {
	if !s.cfg.Expiry.IsZero() &&
		!s.client.currentTime().Before(s.cfg.Expiry) {
		return errSwapExpired
	}

	wait := s.client.waitPollInterval
	if !s.cfg.Expiry.IsZero() {
		until := s.cfg.Expiry.Sub(s.client.currentTime())
		if until < wait {
			wait = until
		}
	}
	if wait <= 0 {
		return errSwapExpired
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-timer.C:
		return nil
	}
}

// waitForInSwapClaimObservation gives indexed claim-spend metadata a short
// chance to catch up before treating a preimage-less spend as anomalous.
func (c *SwapClient) waitForInSwapClaimObservation(ctx context.Context,
	paymentHash lntypes.Hash, pkScript []byte) (*lntypes.Preimage,
	*VTXOInfo, error) {

	var (
		spentVHTLC  *VTXOInfo
		maxAttempts = defaultClaimPreimageLookupAttempts
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		preimage, spent, err := c.findInSwapClaimObservation(
			ctx, paymentHash, pkScript,
		)
		if err != nil || preimage != nil || spent == nil {
			return preimage, spent, err
		}

		spentVHTLC = spent
		if attempt == maxAttempts {
			break
		}

		timer := time.NewTimer(defaultClaimPreimageLookupInterval)
		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, nil, ctx.Err()

		case <-timer.C:
		}
	}

	return nil, spentVHTLC, nil
}

// findInSwapClaimObservation performs one reconciliation pass for an in-swap
// claim. It returns either a verified preimage or an authoritative spent VHTLC
// that still lacks the expected preimage material.
func (c *SwapClient) findInSwapClaimObservation(ctx context.Context,
	paymentHash lntypes.Hash, pkScript []byte) (*lntypes.Preimage,
	*VTXOInfo, error) {

	pkScriptHex := hex.EncodeToString(pkScript)

	spentVTXOs, err := c.daemon.ListSpentVTXOs(ctx)
	if err != nil {
		return nil, nil, err
	}

	preimage, err := findMatchingPreimageInVTXOs(
		spentVTXOs, pkScriptHex, paymentHash,
	)
	if err != nil {
		return nil, nil, err
	}

	if preimage != nil {
		return preimage, nil, nil
	}

	vtxo, err := c.daemon.FindSpentVTXOByPkScript(ctx, pkScript)
	if err != nil {
		return nil, nil, err
	}
	if vtxo != nil {
		preimage, err := findMatchingPreimageInVTXO(
			vtxo, paymentHash,
		)
		if err != nil {
			return nil, nil, err
		}

		if preimage != nil {
			return preimage, nil, nil
		}

		if vtxo.SpentByTxID != "" {
			pkg, err := c.daemon.GetIndexedOORSession(
				ctx, pkScript, vtxo.SpentByTxID,
			)
			if err != nil {
				return nil, nil, err
			}

			preimage, err = findMatchingPreimageInCheckpoints(
				pkg, paymentHash,
			)
			if err != nil {
				return nil, nil, err
			}

			if preimage != nil {
				return preimage, nil, nil
			}

			return nil, vtxo, nil
		}
	}

	// Some daemon versions surface the received claim output before the
	// spent vHTLC record is visible. Scan local live packages as a wallet
	// recovery fallback, but still require a payment-hash match.
	liveVTXOs, err := c.daemon.ListLiveVTXOs(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range liveVTXOs {
		preimage, err := findMatchingPreimageInVTXO(
			&liveVTXOs[i], paymentHash,
		)
		if err != nil {
			return nil, nil, err
		}

		if preimage != nil {
			return preimage, nil, nil
		}
	}

	return nil, nil, nil
}

// waitForSpentVTXOPreimage polls until the authoritative indexer exposes the
// spent vHTLC's finalized checkpoint PSBTs and one yields the expected
// hashlock preimage.
func (c *SwapClient) waitForSpentVTXOPreimage(ctx context.Context,
	paymentHash lntypes.Hash, pkScript []byte, expiry time.Time) (
	*lntypes.Preimage, error) {

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !expiry.IsZero() && !c.currentTime().Before(expiry) {
			return nil, errSwapExpired
		}

		preimage, spentVHTLC, err := c.waitForInSwapClaimObservation(
			ctx, paymentHash, pkScript,
		)
		if err != nil {
			return nil, err
		}

		if preimage != nil {
			return preimage, nil
		}
		if spentVHTLC != nil {
			return nil, newInterventionError(
				"funded vHTLC spent without claim preimage",
				nil,
			)
		}

		wait := c.waitPollInterval
		if until := expiry.Sub(c.currentTime()); !expiry.IsZero() &&
			until < wait {

			wait = until
		}
		if wait <= 0 {
			return nil, errSwapExpired
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, ctx.Err()

		case <-timer.C:
		}
	}
}

// findMatchingPreimageInVTXOs scans VTXOs matching pkScriptHex for a preimage
// matching paymentHash.
func findMatchingPreimageInVTXOs(vtxos []VTXOInfo, pkScriptHex string,
	paymentHash lntypes.Hash) (*lntypes.Preimage, error) {

	for i := range vtxos {
		if hex.EncodeToString(vtxos[i].PkScript) != pkScriptHex {
			continue
		}

		preimage, err := findMatchingPreimageInVTXO(
			&vtxos[i], paymentHash,
		)
		if err != nil {
			return nil, err
		}

		if preimage != nil {
			return preimage, nil
		}
	}

	return nil, nil
}

// findMatchingPreimageInCheckpoints scans one package's finalized checkpoints
// for a preimage matching paymentHash.
func findMatchingPreimageInCheckpoints(pkg *OORPackageInfo,
	paymentHash lntypes.Hash) (*lntypes.Preimage, error) {

	if pkg == nil {
		return nil, nil
	}

	for i := range pkg.CheckpointPSBTs {
		preimage, err := extractPreimageFromCheckpoint(
			pkg.CheckpointPSBTs[i],
		)
		if err != nil {
			return nil, fmt.Errorf("extract preimage from "+
				"checkpoint: %w", err)
		}

		if preimageMatchesHash(preimage, paymentHash) {
			return preimage, nil
		}
	}

	return nil, nil
}

// findMatchingPreimageInVTXO scans one VTXO's finalized checkpoint PSBTs for a
// preimage matching paymentHash.
func findMatchingPreimageInVTXO(vtxo *VTXOInfo,
	paymentHash lntypes.Hash) (*lntypes.Preimage, error) {

	if vtxo == nil {
		return nil, nil
	}

	return findMatchingPreimageInCheckpoints(&OORPackageInfo{
		CheckpointPSBTs: vtxo.FinalCheckpointPSBTs,
	}, paymentHash)
}
