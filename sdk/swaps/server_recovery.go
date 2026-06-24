package swaps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

const maxDaemonRecoveryInt32 = uint32(1<<31 - 1)

type malformedRecoverableSwapError struct {
	err error
}

// Error returns the underlying malformed recoverable row error.
func (e *malformedRecoverableSwapError) Error() string {
	return e.err.Error()
}

// Unwrap returns the underlying malformed recoverable row error.
func (e *malformedRecoverableSwapError) Unwrap() error {
	return e.err
}

// malformedRecoverableSwap marks an error as local to one server row.
func malformedRecoverableSwap(err error) error {
	if err == nil {
		return nil
	}

	return &malformedRecoverableSwapError{err: err}
}

// isMalformedRecoverableSwap reports whether restore can skip the row.
func isMalformedRecoverableSwap(err error) bool {
	var malformed *malformedRecoverableSwapError

	return errors.As(err, &malformed)
}

// SwapserverRecoveryResult summarizes vHTLC recovery rows restored from the
// swapserver-owned discovery API.
type SwapserverRecoveryResult struct {
	// RecoveredVHTLCs counts recoverable rows returned by the swapserver
	// that had vHTLC metadata the SDK could validate.
	RecoveredVHTLCs uint32

	// RecoveredVHTLCRefunds counts pay-side refund recovery rows armed
	// from recoverable in-swaps.
	RecoveredVHTLCRefunds uint32

	// RecoveredVHTLCClaims counts receive-side claim recovery rows armed
	// from recoverable out-swaps.
	RecoveredVHTLCClaims uint32
}

// RecoverSwapserverVHTLCs discovers swapserver-owned recoverable vHTLC rows and
// arms daemon-owned recovery for live outputs.
func (c *SwapClient) RecoverSwapserverVHTLCs(ctx context.Context) (
	*SwapserverRecoveryResult, error) {

	if c == nil || c.server == nil || c.daemon == nil {
		return &SwapserverRecoveryResult{}, nil
	}

	clientKey, err := c.daemon.IdentityPubKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("get recovery identity pubkey: %w", err)
	}

	ownerProof, err := newSwapOwnerProof(
		ctx, c.daemon, clientKey, swapRecoveryAuthList,
		c.currentTime().Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("create recovery owner proof: %w", err)
	}

	rows, err := c.server.ListRecoverableSwaps(ctx, ownerProof)
	if err != nil {
		return nil, fmt.Errorf("list recoverable swaps: %w", err)
	}

	result := &SwapserverRecoveryResult{}
	for _, row := range rows {
		recovered, err := c.recoverSwapserverVHTLC(
			ctx, clientKey, row,
		)
		if err != nil {
			if !isMalformedRecoverableSwap(err) {
				return nil, err
			}

			c.log.WarnS(ctx, "Skipping malformed recoverable swap",
				err,
				btclog.Hex(
					"payment_hash", row.GetPaymentHash(),
				),
				slog.String(
					"direction",
					row.GetDirection().String(),
				),
			)

			continue
		}
		if !recovered {
			continue
		}

		result.RecoveredVHTLCs++
		switch row.GetDirection() {
		case swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN:

			result.RecoveredVHTLCRefunds++

		case swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT:

			result.RecoveredVHTLCClaims++

		case swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_UNSPECIFIED: //nolint:ll
		}
	}

	return result, nil
}

// recoverSwapserverVHTLC validates one server row against the vHTLC policy and
// arms recovery only when the daemon/indexer still reports a live output.
func (c *SwapClient) recoverSwapserverVHTLC(ctx context.Context,
	clientKey *btcec.PublicKey, row *swaprpc.RecoverableSwap) (bool,
	error) {

	if row == nil || len(row.GetVhtlcPkScript()) == 0 {
		return false, nil
	}

	paymentHash, _, pkScript, err := recoverableSwapPolicy(row)
	if err != nil {
		return false, malformedRecoverableSwap(err)
	}

	live, err := c.daemon.FindLiveVTXOByPkScript(ctx, pkScript)
	if err != nil {
		return false, fmt.Errorf("query live recoverable vHTLC %x: %w",
			paymentHash[:], err)
	}
	if live == nil {
		return false, nil
	}

	switch row.GetDirection() {
	case swaprpc.
		RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN:
		return true, c.armRecoveredInSwapRefund(
			ctx, row, paymentHash, live,
		)

	case swaprpc.
		RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT:
		return true, c.armRecoveredOutSwapClaim(
			ctx, clientKey, row, paymentHash, live,
		)

	case swaprpc.
		RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_UNSPECIFIED:
		return false, nil

	default:
		return false, nil
	}
}

// recoveredArmParams turns a swapserver row into the daemon arm request shape
// after checking that server uint32 script parameters fit the daemon int32 API.
func recoveredArmParams(row *swaprpc.RecoverableSwap, paymentHash lntypes.Hash,
	live *VTXOInfo, direction daemonrpc.VHTLCRecoveryDirection,
	action daemonrpc.VHTLCRecoveryAction,
	destinationScript []byte) (armVHTLCRecoveryParams, error) {

	refundLocktime, err := recoveryInt32Field(
		"refund_locktime", row.GetRefundLocktime(),
	)
	if err != nil {
		return armVHTLCRecoveryParams{}, err
	}
	claimDelay, err := recoveryInt32Field(
		"unilateral_claim_delay", row.GetUnilateralClaimDelay(),
	)
	if err != nil {
		return armVHTLCRecoveryParams{}, err
	}
	refundDelay, err := recoveryInt32Field(
		"unilateral_refund_delay", row.GetUnilateralRefundDelay(),
	)
	if err != nil {
		return armVHTLCRecoveryParams{}, err
	}
	refundWithoutReceiverDelay, err := recoveryInt32Field(
		"unilateral_refund_without_receiver_delay",
		row.GetUnilateralRefundWithoutReceiverDelay(),
	)
	if err != nil {
		return armVHTLCRecoveryParams{}, err
	}

	params := armVHTLCRecoveryParams{
		RequestID: recoveryRequestID(
			recoveryDirectionString(direction), paymentHash, action,
		),
		PaymentHash:   paymentHash,
		Direction:     direction,
		Action:        action,
		VTXOOutpoint:  live.Outpoint,
		VTXOAmountSat: live.AmountSat,
		SenderPubkey: append(
			[]byte(nil), row.GetSenderPubkey()...,
		),
		ReceiverPubkey: append(
			[]byte(nil), row.GetReceiverPubkey()...,
		),
		ServerPubkey: append(
			[]byte(nil), row.GetOperatorPubkey()...,
		),
		RefundLocktime:        refundLocktime,
		UnilateralClaimDelay:  claimDelay,
		UnilateralRefundDelay: refundDelay,
		DestinationScript: append(
			[]byte(nil), destinationScript...,
		),
		MaxFeeRateSatKW: DefaultRecoveryMaxFeeRateSatPerKW,
	}
	params.UnilateralRefundWithoutReceiverDelay = refundWithoutReceiverDelay

	return params, nil
}

// recoveryDirectionString maps daemon recovery directions to SDK request-id
// direction labels.
func recoveryDirectionString(
	direction daemonrpc.VHTLCRecoveryDirection) string {

	switch direction {
	case recoveryDirectionPay:
		return string(SwapDirectionPay)

	case recoveryDirectionReceive:
		return string(SwapDirectionReceive)

	default:
		return direction.String()
	}
}

// recoveryInt32Field validates that server uint32 script fields fit the daemon
// recovery API before conversion.
func recoveryInt32Field(name string, value uint32) (int32, error) {
	if value > maxDaemonRecoveryInt32 {
		return 0, fmt.Errorf("%s exceeds int32", name)
	}

	return int32(value), nil
}

// existingRecoverableVHTLC returns a matching non-terminal daemon recovery row
// for a deterministic restore request id, when one already exists.
func (c *SwapClient) existingRecoverableVHTLC(ctx context.Context,
	expected armVHTLCRecoveryParams) (*daemonrpc.VHTLCRecoveryStatus,
	error) {

	resp, err := c.daemon.ListVHTLCRecoveries(
		ctx, &daemonrpc.ListVHTLCRecoveriesRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("list vhtlc recoveries: %w", err)
	}

	for _, status := range resp.GetStatuses() {
		if status.GetRequestId() != expected.RequestID ||
			recoveryStatusTerminal(status.GetState()) {

			continue
		}

		if err := validateExistingRecovery(
			status, expected,
		); err != nil {
			return nil, malformedRecoverableSwap(err)
		}

		return status, nil
	}

	return nil, nil
}

// recoveryStatusTerminal reports whether a daemon recovery row should not be
// reused for restore arming.
func recoveryStatusTerminal(state daemonrpc.VHTLCRecoveryState) bool {
	switch state {
	case recoveryStateCompleted, recoveryStateCancelled,
		recoveryStateFailed:
		return true

	default:
		return false
	}
}

// validateExistingRecovery checks that a request-id match is the same vHTLC
// recovery row before restore reuses it.
func validateExistingRecovery(status *daemonrpc.VHTLCRecoveryStatus,
	expected armVHTLCRecoveryParams) error {

	if status.GetRecoveryId() == "" {
		return fmt.Errorf("existing recovery has empty id")
	}
	if !bytes.Equal(status.GetSwapId(), expected.PaymentHash[:]) {
		return fmt.Errorf("existing recovery swap id mismatch")
	}
	if status.GetDirection() != expected.Direction {
		return fmt.Errorf("existing recovery direction mismatch")
	}
	if status.GetAction() != expected.Action {
		return fmt.Errorf("existing recovery action mismatch")
	}
	if status.GetVtxoOutpoint() != expected.VTXOOutpoint {
		return fmt.Errorf("existing recovery outpoint mismatch")
	}
	if status.GetVtxoAmountSat() != expected.VTXOAmountSat {
		return fmt.Errorf("existing recovery amount mismatch")
	}
	if status.GetRefundLocktime() != expected.RefundLocktime ||
		status.GetUnilateralClaimDelay() !=
			expected.UnilateralClaimDelay ||
		status.GetUnilateralRefundDelay() !=
			expected.UnilateralRefundDelay ||
		status.GetUnilateralRefundWithoutReceiverDelay() !=
			expected.UnilateralRefundWithoutReceiverDelay {
		return fmt.Errorf("existing recovery vHTLC config mismatch")
	}

	return nil
}

// armRecoveredInSwapRefund installs a pay-side daemon recovery row that uses
// the refund-without-receiver path instead of sending the Lightning payment.
func (c *SwapClient) armRecoveredInSwapRefund(ctx context.Context,
	row *swaprpc.RecoverableSwap, paymentHash lntypes.Hash,
	live *VTXOInfo) error {

	params, err := recoveredArmParams(
		row, paymentHash, live, recoveryDirectionPay,
		recoveryActionRefundWithoutReceiver, nil,
	)
	if err != nil {
		return malformedRecoverableSwap(err)
	}

	existing, err := c.existingRecoverableVHTLC(ctx, params)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}

	destination, err := c.daemon.AllocateReceiveScript(
		ctx, "vhtlc-recovery-refund",
	)
	if err != nil {
		return fmt.Errorf("allocate refund destination: %w", err)
	}
	if destination == nil || len(destination.PkScript) == 0 {
		return fmt.Errorf("refund destination script is required")
	}

	params.DestinationScript = destination.PkScript
	params.MaxFeeRateSatKW = c.recoveryPolicy.MaxFeeRateSatPerKW
	_, err = c.daemon.ArmVHTLCRecovery(
		ctx, buildArmVHTLCRecoveryRequest(params),
	)
	if err != nil {
		return fmt.Errorf("arm recovered in-swap refund: %w", err)
	}

	return nil
}

// armRecoveredOutSwapClaim decrypts the sealed preimage and starts the
// receive-side claim recovery path for a live server-funded vHTLC.
func (c *SwapClient) armRecoveredOutSwapClaim(ctx context.Context,
	clientKey *btcec.PublicKey, row *swaprpc.RecoverableSwap,
	paymentHash lntypes.Hash, live *VTXOInfo) error {

	preimage, err := openOutSwapRecoveryBlob(
		ctx, c.daemon, clientKey, paymentHash,
		row.GetEncryptedRecoveryBlob(),
	)
	if err != nil {
		return malformedRecoverableSwap(
			fmt.Errorf("open recovered out-swap preimage: %w", err),
		)
	}

	params, err := recoveredArmParams(
		row, paymentHash, live, recoveryDirectionReceive,
		recoveryActionClaim, nil,
	)
	if err != nil {
		return malformedRecoverableSwap(err)
	}

	existing, err := c.existingRecoverableVHTLC(ctx, params)
	if err != nil {
		return err
	}
	if existing != nil {
		return c.escalateRecoveredOutSwapClaim(
			ctx, existing.GetRecoveryId(), preimage,
		)
	}

	destination, err := c.daemon.AllocateReceiveScript(
		ctx, "vhtlc-recovery-claim",
	)
	if err != nil {
		return fmt.Errorf("allocate claim destination: %w", err)
	}
	if destination == nil || len(destination.PkScript) == 0 {
		return fmt.Errorf("claim destination script is required")
	}

	params.DestinationScript = destination.PkScript
	params.MaxFeeRateSatKW = c.recoveryPolicy.MaxFeeRateSatPerKW
	resp, err := c.daemon.ArmVHTLCRecovery(
		ctx, buildArmVHTLCRecoveryRequest(params),
	)
	if err != nil {
		return fmt.Errorf("arm recovered out-swap claim: %w", err)
	}
	if resp.GetRecoveryId() == "" {
		return fmt.Errorf("arm recovered out-swap claim returned " +
			"empty id")
	}

	err = c.escalateRecoveredOutSwapClaim(
		ctx, resp.GetRecoveryId(), preimage,
	)
	if err != nil {
		return err
	}

	return nil
}

// escalateRecoveredOutSwapClaim supplies the recovered preimage to the
// daemon-owned recovery executor so it can spend the claim path.
func (c *SwapClient) escalateRecoveredOutSwapClaim(ctx context.Context,
	recoveryID string, preimage *lntypes.Preimage) error {

	_, err := c.daemon.EscalateVHTLCRecovery(
		ctx, &daemonrpc.EscalateVHTLCRecoveryRequest{
			RecoveryId:    recoveryID,
			Reason:        "swapserver recovery claim",
			ClaimPreimage: (*preimage)[:],
		},
	)
	if err != nil {
		return fmt.Errorf("escalate recovered out-swap claim: %w", err)
	}

	return nil
}

// recoverableSwapPolicy rebuilds the script policy advertised by the
// swapserver and returns the pkScript that must match indexer state.
func recoverableSwapPolicy(row *swaprpc.RecoverableSwap) (lntypes.Hash,
	*arkscript.VHTLCPolicy, []byte, error) {

	paymentHash, err := lntypes.MakeHash(row.GetPaymentHash())
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf("parse "+
			"recoverable payment hash: %w", err)
	}

	sender, err := btcec.ParsePubKey(row.GetSenderPubkey())
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf("parse "+
			"recoverable sender key: %w", err)
	}
	receiver, err := btcec.ParsePubKey(row.GetReceiverPubkey())
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf("parse "+
			"recoverable receiver key: %w", err)
	}
	server, err := btcec.ParsePubKey(row.GetOperatorPubkey())
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf("parse "+
			"recoverable operator key: %w", err)
	}

	preimageHash := paymentHash
	if len(row.GetPreimageHash()) != 0 {
		preimageHash, err = lntypes.MakeHash(row.GetPreimageHash())
		if err != nil {
			return lntypes.Hash{}, nil, nil, fmt.Errorf("parse "+
				"recoverable preimage hash: %w", err)
		}
	}
	if preimageHash != paymentHash {
		return lntypes.Hash{}, nil, nil, fmt.Errorf("recoverable " +
			"preimage hash mismatch")
	}

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                sender,
		Receiver:              receiver,
		Server:                server,
		PreimageHash:          preimageHash,
		RefundLocktime:        row.GetRefundLocktime(),
		UnilateralClaimDelay:  row.GetUnilateralClaimDelay(),
		UnilateralRefundDelay: row.GetUnilateralRefundDelay(),
		UnilateralRefundWithoutReceiverDelay: row.
			GetUnilateralRefundWithoutReceiverDelay(),
	})
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf("build "+
			"recoverable vHTLC policy: %w", err)
	}

	pkScript, err := policy.PkScript()
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf("build "+
			"recoverable vHTLC pkScript: %w", err)
	}
	if len(row.GetVhtlcPkScript()) != 0 &&
		!bytes.Equal(pkScript, row.GetVhtlcPkScript()) {
		return lntypes.Hash{}, nil, nil, fmt.Errorf("recoverable " +
			"vHTLC pkScript mismatch")
	}

	return paymentHash, policy, pkScript, nil
}
