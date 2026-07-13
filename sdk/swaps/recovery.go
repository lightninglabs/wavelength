package swaps

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//nolint:ll
const (
	// recoveryDirectionPay is the SDK pay-side daemon recovery direction.
	recoveryDirectionPay = daemonrpc.
				VHTLCRecoveryDirection_VHTLC_RECOVERY_DIRECTION_PAY

	// recoveryDirectionReceive is the SDK receive-side daemon recovery
	// direction.
	recoveryDirectionReceive = daemonrpc.
					VHTLCRecoveryDirection_VHTLC_RECOVERY_DIRECTION_RECEIVE

	// recoveryActionClaim is the receive-side unilateral claim action.
	recoveryActionClaim = daemonrpc.
				VHTLCRecoveryAction_VHTLC_RECOVERY_ACTION_CLAIM

	// recoveryActionRefundWithoutReceiver is the pay-side sender-only
	// unilateral refund action.
	recoveryActionRefundWithoutReceiver = daemonrpc.
						VHTLCRecoveryAction_VHTLC_RECOVERY_ACTION_REFUND_WITHOUT_RECEIVER

	// recoveryStateUnspecified represents a missing or unknown recovery
	// row.
	recoveryStateUnspecified = daemonrpc.
					VHTLCRecoveryState_VHTLC_RECOVERY_STATE_UNSPECIFIED

	// recoveryStateArmed means the row is dormant and cooperative
	// settlement can still win without on-chain escalation.
	recoveryStateArmed = daemonrpc.
				VHTLCRecoveryState_VHTLC_RECOVERY_STATE_ARMED

	// recoveryStateUnrollStarted means daemon-owned unroll execution has
	// started.
	recoveryStateUnrollStarted = daemonrpc.
					VHTLCRecoveryState_VHTLC_RECOVERY_STATE_UNROLL_STARTED

	// recoveryStateCompleted means the daemon completed on-chain recovery.
	recoveryStateCompleted = daemonrpc.
				VHTLCRecoveryState_VHTLC_RECOVERY_STATE_COMPLETED

	// recoveryStateCancelled means cooperative settlement cancelled
	// recovery.
	recoveryStateCancelled = daemonrpc.
				VHTLCRecoveryState_VHTLC_RECOVERY_STATE_CANCELLED

	// recoveryStateFailed means recovery hit a terminal daemon-side error.
	recoveryStateFailed = daemonrpc.
				VHTLCRecoveryState_VHTLC_RECOVERY_STATE_FAILED

	// recoveryReasonServerClaimObserved explains cancellation when the pay
	// side observes the server's preimage claim.
	recoveryReasonServerClaimObserved = "server claim observed"

	// recoveryReasonServerClaimBeforeRefund explains cancellation when the
	// pay side finds the server claim while reconciling refund.
	recoveryReasonServerClaimBeforeRefund = "server claim observed " +
		"before refund"

	// recoveryReasonRefundOutputIndexed explains cancellation when the
	// cooperative refund output is already indexed.
	recoveryReasonRefundOutputIndexed = "cooperative refund output indexed"

	// recoveryReasonRefundSessionCompleted explains cancellation when the
	// daemon's durable OOR session status confirms cooperative refund
	// completion before refund output indexing catches up.
	recoveryReasonRefundSessionCompleted = "cooperative refund session " +
		"completed"

	// recoveryReasonRefundSpendObserved explains cancellation when the pay
	// side observes the vHTLC spent by the refund before the refund output
	// itself is indexed.
	recoveryReasonRefundSpendObserved = "cooperative refund spend observed"

	// recoveryReasonClaimAccepted explains cancellation when the daemon
	// accepted the cooperative claim OOR.
	recoveryReasonClaimAccepted = "cooperative claim accepted"

	// recoveryReasonClaimIndexed explains cancellation when the cooperative
	// claim spend is already indexed.
	recoveryReasonClaimIndexed = "cooperative claim indexed"

	// recoveryReasonFundingRejected explains cancellation of a pay-side
	// refund recovery when the funding OOR was rejected by the operator, so
	// the vHTLC never existed and there is nothing to recover.
	recoveryReasonFundingRejected = "funding OOR rejected by operator"

	// DefaultRecoveryMaxFeeRateSatPerKW caps SDK-armed vHTLC exit spends at
	// 100 sat/vbyte. Operators can still clamp lower through the daemon's
	// unroll fee cap; this value prevents an armed recovery row from being
	// uncapped if the lower layer is configured permissively.
	DefaultRecoveryMaxFeeRateSatPerKW int32 = 25_000

	// recoverySignerKeyIndex is the fixed identity-key index used by the
	// daemon for Ark/OOR signing. The key family is
	// keychain.KeyFamilyNodeKey; keeping the locator explicit lets the
	// unroll signer reconstruct the same public key after restart.
	recoverySignerKeyIndex int32 = 0

	// DefaultRecoveryCooperativeFailureGracePeriod is how long the SDK
	// keeps retrying cooperative vHTLC settlement after the first observed
	// cooperative send failure before automatic on-chain recovery may
	// start.
	DefaultRecoveryCooperativeFailureGracePeriod = time.Hour

	// DefaultRecoveryMinMarginBlocks is the block-height safety margin that
	// lets claim recovery override the wall-clock grace period before a
	// refund locktime can make waiting unsafe.
	DefaultRecoveryMinMarginBlocks = uint32(12)
)

// RecoveryPolicy controls automatic escalation from cooperative vHTLC retry to
// daemon-owned on-chain recovery. Arming is still immediate and cheap; this
// policy only gates the expensive unroll transition.
type RecoveryPolicy struct {
	// AutoEscalate allows the SDK to start on-chain recovery without a
	// manual caller command once the grace/deadline policy says waiting
	// longer is unsafe or unproductive.
	AutoEscalate bool

	// CooperativeFailureGracePeriod is measured from the first cooperative
	// send failure observed by the current process. While this period is
	// open, the SDK keeps retrying cooperative settlement unless deadline
	// pressure overrides the wait.
	CooperativeFailureGracePeriod time.Duration

	// MinRecoveryMarginBlocks is the minimum block margin preserved before
	// a refund locktime. Claim recovery may override the grace period when
	// the current height plus this margin reaches the refund locktime.
	MinRecoveryMarginBlocks uint32

	// MaxFeeRateSatPerKW caps the final recovery exit-spend fee rate. The
	// recovery row stores this cap at arm time so restart and manual
	// escalation cannot accidentally use a looser later default.
	MaxFeeRateSatPerKW int32
}

// DefaultRecoveryPolicy returns the production SDK recovery escalation policy.
func DefaultRecoveryPolicy() RecoveryPolicy {
	gracePeriod := DefaultRecoveryCooperativeFailureGracePeriod
	marginBlocks := DefaultRecoveryMinMarginBlocks
	feeCap := DefaultRecoveryMaxFeeRateSatPerKW

	return RecoveryPolicy{
		AutoEscalate:                  false,
		CooperativeFailureGracePeriod: gracePeriod,
		MinRecoveryMarginBlocks:       marginBlocks,
		MaxFeeRateSatPerKW:            feeCap,
	}
}

// WithDefaults fills numeric unset policy fields with the production recovery
// policy. AutoEscalate is intentionally preserved so callers can disable
// automatic on-chain recovery with RecoveryPolicy{AutoEscalate: false}.
func (p RecoveryPolicy) WithDefaults() RecoveryPolicy {
	if p.MinRecoveryMarginBlocks == 0 {
		p.MinRecoveryMarginBlocks = DefaultRecoveryMinMarginBlocks
	}
	if p.MaxFeeRateSatPerKW == 0 {
		p.MaxFeeRateSatPerKW = DefaultRecoveryMaxFeeRateSatPerKW
	}

	return p
}

// recoveryEscalationDecision is the policy result for one cooperative vHTLC
// send failure.
type recoveryEscalationDecision struct {
	// Escalate is true when the caller should start daemon-owned unroll
	// now.
	Escalate bool

	// Trigger explains why escalation is allowed or why it was skipped.
	Trigger string

	// FirstFailureAt is when this process first observed cooperative
	// failure for the recovery row.
	FirstFailureAt time.Time

	// NextRetryAt is when the grace-period retry window opens. It is zero
	// when escalation is immediate or automatic escalation is disabled.
	NextRetryAt time.Time

	// CurrentHeight is the block height used for deadline checks.
	CurrentHeight uint32

	// DeadlineHeight is the claim/refund deadline, when one exists.
	DeadlineHeight uint32

	// RemainingBlocks is the signed distance between the current height and
	// DeadlineHeight. It is negative when the deadline has already passed.
	RemainingBlocks int32
}

// decideRecoveryEscalation evaluates the automatic escalation policy after a
// cooperative vHTLC send failure.
func decideRecoveryEscalation(policy RecoveryPolicy, firstFailureAt time.Time,
	now time.Time, currentHeight uint32,
	deadlineHeight uint32) recoveryEscalationDecision {

	policy = policy.WithDefaults()
	decision := recoveryEscalationDecision{
		FirstFailureAt: firstFailureAt,
		CurrentHeight:  currentHeight,
		DeadlineHeight: deadlineHeight,
	}
	if deadlineHeight != 0 {
		decision.RemainingBlocks = int32(deadlineHeight) -
			int32(currentHeight)
	}
	if !policy.AutoEscalate {
		decision.Trigger = "auto_escalate_disabled"

		return decision
	}

	if deadlineHeight != 0 &&
		currentHeight+policy.MinRecoveryMarginBlocks >= deadlineHeight {

		decision.Escalate = true
		decision.Trigger = "deadline_margin"

		return decision
	}

	if policy.CooperativeFailureGracePeriod <= 0 {
		decision.Escalate = true
		decision.Trigger = "grace_disabled"

		return decision
	}

	readyAt := firstFailureAt.Add(policy.CooperativeFailureGracePeriod)
	decision.NextRetryAt = readyAt
	if !now.Before(readyAt) {
		decision.Escalate = true
		decision.Trigger = "grace_elapsed"

		return decision
	}

	decision.Trigger = "within_grace_period"

	return decision
}

// recoveryRequestID returns the caller-owned idempotency key used when arming
// vHTLC recovery. It is deterministic from the SDK swap identity and action so
// retries after crash or RPC timeout return the original daemon recovery row.
func recoveryRequestID(direction string, paymentHash lntypes.Hash,
	action daemonrpc.VHTLCRecoveryAction) string {

	return fmt.Sprintf("sdk-swaps:%s:%x:%s", direction, paymentHash[:],
		action.String())
}

// recoverySignerFamily returns the daemon identity key family used by the
// client-side vHTLC sender/receiver key.
func recoverySignerFamily() int32 {
	return int32(keychain.KeyFamilyNodeKey)
}

// pubKeyBytesForRecovery serializes a required compressed public key for an
// arm request.
func pubKeyBytesForRecovery(pubKey *btcec.PublicKey,
	name string) ([]byte, error) {

	if pubKey == nil {
		return nil, fmt.Errorf("%s pubkey is required", name)
	}

	return pubKey.SerializeCompressed(), nil
}

// ensurePayRefundRecoveryArmed stores a dormant refund-without-receiver
// recovery row for the funded pay-side vHTLC. The row is armed before refund
// locktime pressure so escalation later only flips existing durable state.
func (s *paySession) ensurePayRefundRecoveryArmed(ctx context.Context) error {
	if s.refundRecoveryID != "" {
		return nil
	}
	if s.cfg == nil {
		return fmt.Errorf("pay swap config is required")
	}
	if s.vhtlcOutpoint == "" || s.vhtlcAmount <= 0 {
		return fmt.Errorf("funded pay vHTLC is required")
	}
	if _, err := s.refundPubKey(ctx); err != nil {
		return err
	}
	if len(s.refundReceiveScript) == 0 {
		return fmt.Errorf("refund destination script is required")
	}

	sender, err := pubKeyBytesForRecovery(s.clientPubKey, "sender")
	if err != nil {
		return err
	}
	receiver, err := pubKeyBytesForRecovery(s.serverPubKey, "receiver")
	if err != nil {
		return err
	}
	server, err := pubKeyBytesForRecovery(s.operatorPubKey, "server")
	if err != nil {
		return err
	}

	resp, err := s.client.daemon.ArmVHTLCRecovery(
		ctx, &daemonrpc.ArmVHTLCRecoveryRequest{
			RequestId: recoveryRequestID(
				string(SwapDirectionPay), s.cfg.PaymentHash,
				recoveryActionRefundWithoutReceiver,
			),
			SwapId: append(
				[]byte(nil), s.cfg.PaymentHash[:]...,
			),
			Direction:      recoveryDirectionPay,
			Action:         recoveryActionRefundWithoutReceiver,
			VtxoOutpoint:   s.vhtlcOutpoint,
			VtxoAmountSat:  s.vhtlcAmount,
			SenderPubkey:   sender,
			ReceiverPubkey: receiver,
			ServerPubkey:   server,
			RefundLocktime: int32(
				s.cfg.VHTLCConfig.RefundLocktime,
			),
			UnilateralClaimDelay: int32(
				s.cfg.VHTLCConfig.UnilateralClaimDelay,
			),
			UnilateralRefundDelay: int32(
				s.cfg.VHTLCConfig.UnilateralRefundDelay,
			),
			UnilateralRefundWithoutReceiverDelay: int32(
				s.cfg.VHTLCConfig.
					UnilateralRefundWithoutReceiverDelay,
			),
			PreimageHash: append(
				[]byte(nil), s.cfg.PaymentHash[:]...,
			),
			SignerKeyFamily: recoverySignerFamily(),
			SignerKeyIndex:  recoverySignerKeyIndex,
			DestinationScript: append(
				[]byte(nil), s.refundReceiveScript...,
			),
			MaxFeeRateSatPerKw: s.client.recoveryPolicy.
				MaxFeeRateSatPerKW,
		},
	)
	if err != nil {
		return fmt.Errorf("arm pay refund recovery: %w", err)
	}
	if resp.GetRecoveryId() == "" {
		return fmt.Errorf("arm pay refund recovery returned empty id")
	}

	s.client.log.InfoS(ctx, "Pay refund recovery armed",
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("recovery_id", resp.GetRecoveryId()),
		slog.Bool("created", resp.GetCreated()),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.Int64("amount_sat", s.vhtlcAmount),
		slog.Uint64(
			"refund_locktime",
			uint64(s.cfg.VHTLCConfig.RefundLocktime),
		),
		slog.Uint64(
			"ark_csv", uint64(
				s.cfg.VHTLCConfig.
					UnilateralRefundWithoutReceiverDelay,
			),
		),
		slog.Int(
			"max_fee_rate_sat_per_kw",
			int(s.client.recoveryPolicy.MaxFeeRateSatPerKW),
		),
	)

	return s.mutateAndPersist(ctx, func() error {
		s.refundRecoveryID = resp.GetRecoveryId()

		return nil
	})
}

// ensureReceiveClaimRecoveryArmed stores a dormant claim recovery row for the
// funded receive-side vHTLC. The raw preimage remains in the receive swap row;
// the daemon recovery row receives only the hash and resolves the preimage
// through the swap store if escalation becomes necessary.
func (s *ReceiveSession) ensureReceiveClaimRecoveryArmed(
	ctx context.Context) error {

	if s.claimRecoveryID != "" {
		return nil
	}
	if s.vhtlcOutpoint == "" || s.vhtlcAmount <= 0 {
		return fmt.Errorf("funded receive vHTLC is required")
	}
	if len(s.claimReceiveScript) == 0 {
		return fmt.Errorf("claim destination script is required")
	}

	sender, err := pubKeyBytesForRecovery(s.swapServerPubKey, "sender")
	if err != nil {
		return err
	}
	receiver, err := pubKeyBytesForRecovery(s.clientPubKey, "receiver")
	if err != nil {
		return err
	}
	server, err := pubKeyBytesForRecovery(s.operatorPubKey, "server")
	if err != nil {
		return err
	}

	resp, err := s.client.daemon.ArmVHTLCRecovery(
		ctx, &daemonrpc.ArmVHTLCRecoveryRequest{
			RequestId: recoveryRequestID(
				string(SwapDirectionReceive), s.PaymentHash,
				recoveryActionClaim,
			),
			SwapId: append(
				[]byte(nil), s.PaymentHash[:]...,
			),
			Direction:      recoveryDirectionReceive,
			Action:         recoveryActionClaim,
			VtxoOutpoint:   s.vhtlcOutpoint,
			VtxoAmountSat:  s.vhtlcAmount,
			SenderPubkey:   sender,
			ReceiverPubkey: receiver,
			ServerPubkey:   server,
			RefundLocktime: int32(
				s.vhtlcConfig.RefundLocktime,
			),
			UnilateralClaimDelay: int32(
				s.vhtlcConfig.UnilateralClaimDelay,
			),
			UnilateralRefundDelay: int32(
				s.vhtlcConfig.UnilateralRefundDelay,
			),
			UnilateralRefundWithoutReceiverDelay: int32(
				s.vhtlcConfig.
					UnilateralRefundWithoutReceiverDelay,
			),
			PreimageHash: append(
				[]byte(nil), s.PaymentHash[:]...,
			),
			SignerKeyFamily: recoverySignerFamily(),
			SignerKeyIndex:  recoverySignerKeyIndex,
			DestinationScript: append(
				[]byte(nil), s.claimReceiveScript...,
			),
			MaxFeeRateSatPerKw: s.client.recoveryPolicy.
				MaxFeeRateSatPerKW,
		},
	)
	if err != nil {
		return fmt.Errorf("arm receive claim recovery: %w", err)
	}
	if resp.GetRecoveryId() == "" {
		return fmt.Errorf("arm receive claim recovery returned empty " +
			"id")
	}

	s.client.log.InfoS(ctx, "Receive claim recovery armed",
		btclog.Hex("hash", s.PaymentHash[:]),
		slog.String("recovery_id", resp.GetRecoveryId()),
		slog.Bool("created", resp.GetCreated()),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.Int64("amount_sat", s.vhtlcAmount),
		slog.Uint64(
			"refund_locktime", uint64(s.vhtlcConfig.RefundLocktime),
		),
		slog.Uint64(
			"ark_csv", uint64(s.vhtlcConfig.UnilateralClaimDelay),
		),
		slog.Int(
			"max_fee_rate_sat_per_kw",
			int(s.client.recoveryPolicy.MaxFeeRateSatPerKW),
		),
	)

	return s.mutateAndPersist(ctx, func() error {
		s.claimRecoveryID = resp.GetRecoveryId()

		return nil
	})
}

// cancelVHTLCRecovery records that cooperative OOR settlement won before an
// armed recovery was needed. The caller decides whether cancellation failure is
// retryable for its current FSM edge.
func cancelVHTLCRecovery(ctx context.Context, daemon DaemonConn, recoveryID,
	reason, cooperativeTxid string) error {

	if recoveryID == "" {
		return nil
	}

	_, err := daemon.CancelVHTLCRecovery(
		ctx, &daemonrpc.CancelVHTLCRecoveryRequest{
			RecoveryId:      recoveryID,
			Reason:          reason,
			CooperativeTxid: cooperativeTxid,
		},
	)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}

		return fmt.Errorf("cancel vhtlc recovery %s: %w", recoveryID,
			err)
	}

	return nil
}

// recoveryStatusState returns the current recovery state or UNSPECIFIED when
// the daemon has no row for recoveryID.
func recoveryStatusState(resp *daemonrpc.GetVHTLCRecoveryStatusResponse) (
	daemonrpc.VHTLCRecoveryState, string) {

	if resp == nil || !resp.GetFound() || resp.GetStatus() == nil {
		return recoveryStateUnspecified, ""
	}

	status := resp.GetStatus()

	return status.GetState(), status.GetLastError()
}

// escalateVHTLCRecovery asks the daemon to start a previously armed recovery
// row. A repeated call is safe because the daemon owns idempotent escalation
// semantics for already-active or terminal rows.
func escalateVHTLCRecovery(ctx context.Context, daemon DaemonConn, recoveryID,
	reason string) error {

	if recoveryID == "" {
		return nil
	}

	_, err := daemon.EscalateVHTLCRecovery(
		ctx, &daemonrpc.EscalateVHTLCRecoveryRequest{
			RecoveryId: recoveryID,
			Reason:     reason,
		},
	)
	if err != nil {
		return fmt.Errorf("escalate vhtlc recovery %s: %w", recoveryID,
			err)
	}

	return nil
}

// firstPayRefundRecoveryFailure returns the durable lower bound for when
// pay-side cooperative refund started failing. The pay FSM persists the
// RefundInitiated state before retrying the custom-input refund, so UpdatedAt
// survives restart and does not move forward while cooperative retry keeps
// failing.
func (s *paySession) firstPayRefundRecoveryFailure() time.Time {
	if !s.refundRecoveryFailureAt.IsZero() {
		return s.refundRecoveryFailureAt
	}

	first := s.updatedAt
	if first.IsZero() {
		first = s.client.now()
	}
	s.refundRecoveryFailureAt = first

	return first
}

// maybeEscalatePayRefundRecovery applies the SDK recovery policy after a
// cooperative pay-side refund send fails. It returns nil when the SDK should
// keep retrying the cooperative path instead of starting costly unroll.
func (s *paySession) maybeEscalatePayRefundRecovery(ctx context.Context,
	cause error) error {

	if s.refundRecoveryID == "" {
		return nil
	}

	reason := fmt.Sprintf("cooperative refund failed: %v", cause)
	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		s.client.log.WarnS(
			ctx,
			"Pay refund recovery height unavailable",
			err,
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.String("recovery_id", s.refundRecoveryID),
		)
	}

	decision := decideRecoveryEscalation(
		s.client.recoveryPolicy, s.firstPayRefundRecoveryFailure(),
		s.client.now(), height, 0,
	)
	if !decision.Escalate {
		s.client.log.WarnS(ctx,
			"Pay refund recovery escalation deferred", cause,
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.String("recovery_id", s.refundRecoveryID),
			slog.String("reason", reason),
			slog.String("trigger", decision.Trigger),
			slog.Time("first_failure_at", decision.FirstFailureAt),
			slog.Time("next_retry_at", decision.NextRetryAt),
			slog.Uint64(
				"current_height",
				uint64(decision.CurrentHeight),
			),
		)

		return nil
	}

	// Pay-side refund recovery has no absolute claim deadline. The grace
	// period is the only automatic trigger; operators can still use the
	// recovery CLI to escalate earlier.
	if err := escalateVHTLCRecovery(
		ctx, s.client.daemon, s.refundRecoveryID, reason,
	); err != nil {
		return err
	}

	s.client.log.WarnS(ctx, "Pay refund recovery escalated",
		cause,
		btclog.Hex("hash", s.cfg.PaymentHash[:]),
		slog.String("recovery_id", s.refundRecoveryID),
		slog.String("reason", reason),
		slog.String("trigger", decision.Trigger),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.Int64("amount_sat", s.vhtlcAmount),
		slog.Uint64("current_height", uint64(decision.CurrentHeight)),
	)

	return nil
}

// firstReceiveClaimRecoveryFailure returns the durable lower bound for when
// receive-side cooperative claim started failing. The receive FSM persists
// ClaimInitiated before retrying the claim, so UpdatedAt survives restart and
// does not move forward while cooperative retry keeps failing.
func (s *ReceiveSession) firstReceiveClaimRecoveryFailure() time.Time {
	if !s.claimRecoveryFailureAt.IsZero() {
		return s.claimRecoveryFailureAt
	}

	first := s.updatedAt
	if first.IsZero() {
		first = s.client.now()
	}
	s.claimRecoveryFailureAt = first

	return first
}

// maybeEscalateReceiveClaimRecovery applies the SDK recovery policy after a
// cooperative receive-side claim send fails. The receive claim has a refund
// locktime deadline, so deadline margin may override the wall-clock grace
// period before the sender can reclaim the vHTLC.
func (s *ReceiveSession) maybeEscalateReceiveClaimRecovery(ctx context.Context,
	cause error) error {

	if s.claimRecoveryID == "" {
		return nil
	}

	reason := fmt.Sprintf("cooperative claim failed: %v", cause)
	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		s.client.log.WarnS(
			ctx,
			"Receive claim recovery height unavailable",
			err,
			btclog.Hex("hash", s.PaymentHash[:]),
			slog.String("recovery_id", s.claimRecoveryID),
		)
	}

	decision := decideRecoveryEscalation(
		s.client.recoveryPolicy, s.firstReceiveClaimRecoveryFailure(),
		s.client.now(), height, s.vhtlcConfig.RefundLocktime,
	)
	if !decision.Escalate {
		s.client.log.WarnS(ctx,
			"Receive claim recovery escalation deferred", cause,
			btclog.Hex("hash", s.PaymentHash[:]),
			slog.String("recovery_id", s.claimRecoveryID),
			slog.String("reason", reason),
			slog.String("trigger", decision.Trigger),
			slog.Time("first_failure_at", decision.FirstFailureAt),
			slog.Time("next_retry_at", decision.NextRetryAt),
			slog.Uint64(
				"current_height",
				uint64(decision.CurrentHeight),
			),
			slog.Uint64(
				"refund_locktime",
				uint64(decision.DeadlineHeight),
			),
			slog.Int(
				"remaining_blocks",
				int(decision.RemainingBlocks),
			),
		)

		return nil
	}

	if err := escalateVHTLCRecovery(
		ctx, s.client.daemon, s.claimRecoveryID, reason,
	); err != nil {
		return err
	}

	s.client.log.WarnS(ctx, "Receive claim recovery escalated",
		cause,
		btclog.Hex("hash", s.PaymentHash[:]),
		slog.String("recovery_id", s.claimRecoveryID),
		slog.String("reason", reason),
		slog.String("trigger", decision.Trigger),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.Int64("amount_sat", s.vhtlcAmount),
		slog.Uint64("current_height", uint64(decision.CurrentHeight)),
		slog.Uint64("refund_locktime", uint64(decision.DeadlineHeight)),
		slog.Int("remaining_blocks", int(decision.RemainingBlocks)),
	)

	return nil
}

// recoveryIsActive returns true once an armed row has moved into daemon-owned
// unroll execution and the SDK should stop attempting cooperative custom-input
// spends for the same vHTLC.
func recoveryIsActive(state daemonrpc.VHTLCRecoveryState) bool {
	switch state {
	case recoveryStateUnspecified, recoveryStateArmed,
		recoveryStateCancelled:
		return false

	default:
		return true
	}
}

// getVHTLCRecoveryState loads the daemon recovery row state. Missing rows are
// treated as unspecified so legacy or partially constructed sessions can
// continue through their cooperative path.
func getVHTLCRecoveryState(ctx context.Context, daemon DaemonConn,
	recoveryID string) (daemonrpc.VHTLCRecoveryState, string, error) {

	if recoveryID == "" {
		return recoveryStateUnspecified, "", nil
	}

	resp, err := daemon.GetVHTLCRecoveryStatus(
		ctx, &daemonrpc.GetVHTLCRecoveryStatusRequest{
			RecoveryId: recoveryID,
		},
	)
	if err != nil {
		return recoveryStateUnspecified, "", fmt.Errorf("get vhtlc "+
			"recovery status %s: %w", recoveryID, err)
	}

	state, lastErr := recoveryStatusState(resp)

	return state, lastErr, nil
}

// reconcilePayRefundRecovery checks whether daemon-owned pay-side recovery has
// taken over a timed-out refund. The boolean return tells the caller that the
// recovery row was relevant enough that no cooperative refund send should be
// attempted in this loop iteration.
func (s *paySession) reconcilePayRefundRecovery(ctx context.Context) (bool,
	error) {

	state, lastErr, err := getVHTLCRecoveryState(
		ctx, s.client.daemon, s.refundRecoveryID,
	)
	if err != nil {
		return false, err
	}

	switch {
	case state == recoveryStateCompleted:
		s.client.log.InfoS(ctx, "Pay refund recovery completed",
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.String("recovery_id", s.refundRecoveryID),
		)

		return true, s.mutateAndPersist(ctx, func() error {
			return s.transition(payEventRefunded)
		})

	case state == recoveryStateFailed:
		reason := "pay refund recovery failed"
		if lastErr != "" {
			reason = fmt.Sprintf("%s: %s", reason, lastErr)
		}

		return true, s.needsIntervention(ctx, reason, nil, nil)

	case recoveryIsActive(state):
		s.client.log.DebugS(ctx, "Pay refund recovery active",
			btclog.Hex("hash", s.cfg.PaymentHash[:]),
			slog.String("recovery_id", s.refundRecoveryID),
			slog.String("recovery_state", state.String()),
		)

		return true, waitForFixedPoll(
			ctx, s.client.waitPollInterval,
		)

	default:
		return false, nil
	}
}

// reconcileReceiveClaimRecovery checks whether daemon-owned receive-side
// recovery has taken over the claim path. The boolean return tells the caller
// that recovery is terminal or active and the cooperative claim send should not
// be attempted in this loop iteration.
func (s *ReceiveSession) reconcileReceiveClaimRecovery(ctx context.Context) (
	bool, error) {

	state, lastErr, err := getVHTLCRecoveryState(
		ctx, s.client.daemon, s.claimRecoveryID,
	)
	if err != nil {
		return false, err
	}

	switch {
	case state == recoveryStateCompleted:
		s.client.log.InfoS(ctx, "Receive claim recovery completed",
			btclog.Hex("hash", s.PaymentHash[:]),
			slog.String("recovery_id", s.claimRecoveryID),
		)

		return true, s.mutateAndPersist(ctx, func() error {
			return s.transition(receiveEventCompleted)
		})

	case state == recoveryStateFailed:
		reason := "receive claim recovery failed"
		if lastErr != "" {
			reason = fmt.Sprintf("%s: %s", reason, lastErr)
		}

		return true, s.failTerminal(ctx, reason, nil, nil)

	case recoveryIsActive(state):
		s.client.log.DebugS(ctx, "Receive claim recovery active",
			btclog.Hex("hash", s.PaymentHash[:]),
			slog.String("recovery_id", s.claimRecoveryID),
			slog.String("recovery_state", state.String()),
		)

		return true, waitForFixedPoll(
			ctx, s.client.waitPollInterval,
		)

	default:
		return false, nil
	}
}

// waitForFixedPoll sleeps for one SDK polling interval without consulting any
// swap- or invoice-specific deadline. Recovery reconciliation uses a fixed poll
// because once daemon-owned recovery has taken over, the SDK must keep checking
// durable recovery state even after the cooperative path's deadline has
// elapsed.
func waitForFixedPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-timer.C:
		return nil
	}
}
