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

	// recoveryReasonRefundAccepted explains cancellation when the daemon
	// accepted the cooperative refund OOR.
	recoveryReasonRefundAccepted = "cooperative refund accepted"

	// recoveryReasonClaimAccepted explains cancellation when the daemon
	// accepted the cooperative claim OOR.
	recoveryReasonClaimAccepted = "cooperative claim accepted"

	// recoveryReasonClaimIndexed explains cancellation when the cooperative
	// claim spend is already indexed.
	recoveryReasonClaimIndexed = "cooperative claim indexed"

	// defaultRecoveryMaxFeeRateSatPerKW caps SDK-armed vHTLC exit spends at
	// 100 sat/vbyte. Operators can still clamp lower through the daemon's
	// unroll fee cap; this value prevents an armed recovery row from being
	// uncapped if the lower layer is configured permissively.
	defaultRecoveryMaxFeeRateSatPerKW int32 = 25_000

	// recoverySignerKeyIndex is the fixed identity-key index used by the
	// daemon for Ark/OOR signing. The key family is
	// keychain.KeyFamilyNodeKey; keeping the locator explicit lets the
	// unroll signer reconstruct the same public key after restart.
	recoverySignerKeyIndex int32 = 0
)

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
			MaxFeeRateSatPerKw: defaultRecoveryMaxFeeRateSatPerKW,
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
			int(defaultRecoveryMaxFeeRateSatPerKW),
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
			MaxFeeRateSatPerKw: defaultRecoveryMaxFeeRateSatPerKW,
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
			int(defaultRecoveryMaxFeeRateSatPerKW),
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
