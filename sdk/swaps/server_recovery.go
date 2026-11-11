package swaps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	// RecoveredVHTLCs counts durable live swap sessions whose recovery was
	// armed or reused successfully. Pending rows also survive arm errors
	// for runtime retry; these counters do not mean settlement completed.
	RecoveredVHTLCs uint32

	// RecoveredVHTLCRefunds counts pay-side refund recovery rows armed
	// from recoverable in-swaps.
	RecoveredVHTLCRefunds uint32

	// RecoveredVHTLCClaims counts receive-side claim recovery rows armed
	// from recoverable out-swaps.
	RecoveredVHTLCClaims uint32
}

// RecoverSwapserverVHTLCs discovers swapserver-owned recoverable vHTLC rows and
// restores durable sessions for live outputs. The caller must resume pending
// sessions under its service lifetime; the daemon runtime schedules this from
// its recovery backend without extending the restore request deadline.
func (c *SwapClient) RecoverSwapserverVHTLCs(ctx context.Context) (
	*SwapserverRecoveryResult, error) {

	if c == nil || c.server == nil || c.daemon == nil {
		return &SwapserverRecoveryResult{}, nil
	}
	select {
	case c.restoreGate <- struct{}{}:
		defer func() {
			<-c.restoreGate
		}()

	case <-ctx.Done():
		return &SwapserverRecoveryResult{}, ctx.Err()
	}

	clientKey, err := c.daemon.IdentityPubKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("get recovery identity pubkey: %w", err)
	}

	pages, ok := c.server.(RecoverableSwapPageServerConn)
	if !ok {
		return &SwapserverRecoveryResult{}, nil
	}
	info, err := pages.GetSwapRecoveryInfo(ctx)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return &SwapserverRecoveryResult{}, nil
		}

		return nil, fmt.Errorf("get recovery signing audience: %w", err)
	}
	if c.chainParams == nil || c.chainParams.GenesisHash == nil {
		return nil, fmt.Errorf("recovery network is not configured")
	}
	if !bytes.Equal(
		info.GetNetworkGenesisHash(), c.chainParams.GenesisHash[:],
	) {
		return nil, fmt.Errorf("recovery server network mismatch")
	}
	if len(info.GetServerIdentityPubkey()) != 33 {
		return nil, fmt.Errorf("recovery server identity is not " +
			"compressed")
	}
	serverKey, err := btcec.ParsePubKey(info.GetServerIdentityPubkey())
	if err != nil {
		return nil, fmt.Errorf("parse recovery server identity: %w",
			err)
	}

	result := &SwapserverRecoveryResult{}
	var rowErrors []error
	var token []byte
	seenTokens := make(map[string]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(rowErrors, err)...)
		}
		const pageSize = uint32(100)
		proof, err := newSwapListOwnerProof(
			ctx, c.daemon, clientKey, serverKey,
			*c.chainParams.GenesisHash, pageSize, token,
			c.currentTime().Unix(),
		)
		if err != nil {
			return result, errors.Join(append(rowErrors, err)...)
		}
		page, err := pages.ListRecoverableSwapsPage(
			ctx, proof, pageSize, token,
		)
		if err != nil {
			if status.Code(err) == codes.Unimplemented &&
				len(token) == 0 {
				return result, nil
			}

			return result, errors.Join(append(rowErrors, err)...)
		}
		if page == nil || len(page.GetSwaps()) > int(pageSize) {
			return result, errors.Join(
				append(
					rowErrors, fmt.Errorf("invalid "+
						"recovery page size"),
				)...,
			)
		}
		if len(page.GetSwaps()) == 0 &&
			len(page.GetNextPageToken()) != 0 {
			return result, errors.Join(
				append(
					rowErrors, fmt.Errorf("empty "+
						"recovery page has a "+
						"continuation"),
				)...,
			)
		}
		for _, row := range page.GetSwaps() {
			recovered, err := c.recoverSwapserverVHTLC(
				ctx, row, clientKey,
			)
			if err != nil {
				if ctx.Err() != nil {
					return result, errors.Join(
						append(
							rowErrors, err,
							ctx.Err(),
						)...,
					)
				}
				if !isMalformedRecoverableSwap(err) {
					rowErrors = append(rowErrors, err)
				}

				c.log.DebugS(ctx, "Skipping unrecovered swap",
					slog.Any("err", err),
					btclog.Hex(
						"payment_hash",
						row.GetPaymentHash(),
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

			result.record(row)
		}
		next := page.GetNextPageToken()
		if len(next) == 0 {
			return result, errors.Join(rowErrors...)
		}
		if len(next) > 128 {
			return result, errors.Join(
				append(
					rowErrors, fmt.Errorf("oversized "+
						"recovery page token"),
				)...,
			)
		}
		if _, repeated := seenTokens[string(next)]; repeated {
			return result, errors.Join(
				append(
					rowErrors, fmt.Errorf("repeated "+
						"recovery page token"),
				)...,
			)
		}
		seenTokens[string(next)] = struct{}{}
		token = append([]byte(nil), next...)
	}
}

// record counts a newly persisted settlement obligation, not its completion.
func (r *SwapserverRecoveryResult) record(row *swaprpc.RecoverableSwap) {
	r.RecoveredVHTLCs++
	switch row.GetDirection() {
	case swaprpc.RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN:
		r.RecoveredVHTLCRefunds++

	case swaprpc.RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT:
		r.RecoveredVHTLCClaims++

	case swaprpc.RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_UNSPECIFIED: //nolint:ll
	}
}

// recoverSwapserverVHTLC validates one server row against the vHTLC policy and
// arms recovery only when the daemon/indexer still reports a live output.
func (c *SwapClient) recoverSwapserverVHTLC(ctx context.Context,
	row *swaprpc.RecoverableSwap, clientKey *btcec.PublicKey) (bool,
	error) {

	if row == nil || len(row.GetVhtlcPkScript()) == 0 {
		return false, nil
	}

	paymentHash, _, pkScript, err := recoverableSwapPolicy(row)
	if err != nil {
		return false, malformedRecoverableSwap(err)
	}
	var participant []byte
	switch row.GetDirection() {
	case swaprpc.RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN:
		participant = row.GetSenderPubkey()

	case swaprpc.RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT:
		participant = row.GetReceiverPubkey()

	default:
		return false, malformedRecoverableSwap(
			fmt.Errorf("unknown recovery direction"),
		)
	}
	if clientKey == nil ||
		!bytes.Equal(participant, clientKey.SerializeCompressed()) {
		return false, malformedRecoverableSwap(
			fmt.Errorf("recovery participant is not this wallet"),
		)
	}
	if c.store == nil || c.store.queries == nil {
		return false, fmt.Errorf("durable swap store is required for " +
			"restore")
	}
	// Existing local state owns settlement, including manual intervention.
	// Never replace a running session or turn a terminal row into a new
	// send.
	_, err = c.GetSwapSummary(ctx, paymentHash)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, ErrSwapSummaryNotFound) {
		return false, err
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
			ctx, row, paymentHash, live,
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
	live *VTXOInfo, direction waverpc.VHTLCRecoveryDirection,
	action waverpc.VHTLCRecoveryAction,
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
func recoveryDirectionString(direction waverpc.VHTLCRecoveryDirection) string {
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
	expected armVHTLCRecoveryParams) (*waverpc.VHTLCRecoveryStatus, error) {

	resp, err := c.daemon.GetVHTLCRecoveryStatus(
		ctx, &waverpc.GetVHTLCRecoveryStatusRequest{
			RequestId: expected.RequestID,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("look up vhtlc recovery request: %w",
			err)
	}
	if !resp.GetFound() {
		return nil, nil
	}
	status := resp.GetStatus()
	if status.GetRequestId() != expected.RequestID {
		return nil, malformedRecoverableSwap(
			fmt.Errorf("existing recovery request id mismatch"),
		)
	}
	if err := validateExistingRecovery(status, expected); err != nil {
		return nil, malformedRecoverableSwap(err)
	}
	if recoveryStatusTerminal(status.GetState()) {
		return nil, fmt.Errorf("recovery %s is terminal (%s); inspect "+
			"the existing job before retrying",
			status.GetRecoveryId(), status.GetState())
	}

	return status, nil
}

// recoveryStatusTerminal reports whether a daemon recovery row should not be
// reused for restore arming.
func recoveryStatusTerminal(state waverpc.VHTLCRecoveryState) bool {
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
func validateExistingRecovery(status *waverpc.VHTLCRecoveryStatus,
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

// armRecoveredInSwapRefund restores a refund-only durable pay session. Its
// worker never requests a quote, funds a vHTLC, or sends a Lightning payment.
func (c *SwapClient) armRecoveredInSwapRefund(ctx context.Context,
	row *swaprpc.RecoverableSwap, paymentHash lntypes.Hash,
	live *VTXOInfo) error {

	return c.restoreFundedSwapSession(
		ctx, row, paymentHash, live, recoveryDirectionPay,
		recoveryActionRefundWithoutReceiver,
	)
}

// armRecoveredOutSwapClaim restores a durable receive session that attempts
// cooperative settlement before its normal policy-gated on-chain fallback.
func (c *SwapClient) armRecoveredOutSwapClaim(ctx context.Context,
	row *swaprpc.RecoverableSwap, paymentHash lntypes.Hash,
	live *VTXOInfo) error {

	return c.restoreFundedSwapSession(
		ctx, row, paymentHash, live, recoveryDirectionReceive,
		recoveryActionClaim,
	)
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
