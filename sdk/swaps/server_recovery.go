package swaps

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

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
			return nil, err
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
		}
	}

	return result, nil
}

// recoverSwapserverVHTLC validates one server row against the vHTLC policy and
// arms recovery only when the daemon/indexer still reports a live output.
func (c *SwapClient) recoverSwapserverVHTLC(ctx context.Context,
	clientKey *btcec.PublicKey, row *swaprpc.RecoverableSwap) (bool, error) {

	if row == nil || len(row.GetVhtlcPkScript()) == 0 {
		return false, nil
	}

	paymentHash, _, pkScript, err := recoverableSwapPolicy(row)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(pkScript, row.GetVhtlcPkScript()) {
		return false, fmt.Errorf("recoverable swap %x pkScript mismatch",
			paymentHash[:])
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

	default:
		return false, nil
	}
}

// armRecoveredInSwapRefund installs a pay-side daemon recovery row that uses
// the refund-without-receiver path instead of sending the Lightning payment.
func (c *SwapClient) armRecoveredInSwapRefund(ctx context.Context,
	row *swaprpc.RecoverableSwap, paymentHash lntypes.Hash,
	live *VTXOInfo) error {

	destination, err := c.daemon.AllocateReceiveScript(
		ctx, "vhtlc-recovery-refund",
	)
	if err != nil {
		return fmt.Errorf("allocate refund destination: %w", err)
	}

	_, err = c.daemon.ArmVHTLCRecovery(
		ctx, &daemonrpc.ArmVHTLCRecoveryRequest{
			RequestId: recoveryRequestID(
				string(SwapDirectionPay), paymentHash,
				recoveryActionRefundWithoutReceiver,
			),
			SwapId:         append([]byte(nil), paymentHash[:]...),
			Direction:      recoveryDirectionPay,
			Action:         recoveryActionRefundWithoutReceiver,
			VtxoOutpoint:   live.Outpoint,
			VtxoAmountSat:  live.AmountSat,
			SenderPubkey:   append([]byte(nil), row.GetSenderPubkey()...),
			ReceiverPubkey: append([]byte(nil), row.GetReceiverPubkey()...),
			ServerPubkey:   append([]byte(nil), row.GetOperatorPubkey()...),
			RefundLocktime: int32(row.GetRefundLocktime()),
			UnilateralClaimDelay: int32(
				row.GetUnilateralClaimDelay(),
			),
			UnilateralRefundDelay: int32(
				row.GetUnilateralRefundDelay(),
			),
			UnilateralRefundWithoutReceiverDelay: int32(
				row.GetUnilateralRefundWithoutReceiverDelay(),
			),
			PreimageHash:      append([]byte(nil), paymentHash[:]...),
			SignerKeyFamily:   recoverySignerFamily(),
			SignerKeyIndex:    recoverySignerKeyIndex,
			DestinationScript: destination.PkScript,
			MaxFeeRateSatPerKw: c.recoveryPolicy.
				MaxFeeRateSatPerKW,
		},
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
		return fmt.Errorf("open recovered out-swap preimage: %w", err)
	}

	destination, err := c.daemon.AllocateReceiveScript(
		ctx, "vhtlc-recovery-claim",
	)
	if err != nil {
		return fmt.Errorf("allocate claim destination: %w", err)
	}

	resp, err := c.daemon.ArmVHTLCRecovery(
		ctx, &daemonrpc.ArmVHTLCRecoveryRequest{
			RequestId: recoveryRequestID(
				string(SwapDirectionReceive), paymentHash,
				recoveryActionClaim,
			),
			SwapId:        append([]byte(nil), paymentHash[:]...),
			Direction:     recoveryDirectionReceive,
			Action:        recoveryActionClaim,
			VtxoOutpoint:  live.Outpoint,
			VtxoAmountSat: live.AmountSat,
			SenderPubkey: append(
				[]byte(nil), row.GetSenderPubkey()...,
			),
			ReceiverPubkey: append(
				[]byte(nil), row.GetReceiverPubkey()...,
			),
			ServerPubkey: append(
				[]byte(nil), row.GetOperatorPubkey()...,
			),
			RefundLocktime: int32(row.GetRefundLocktime()),
			UnilateralClaimDelay: int32(
				row.GetUnilateralClaimDelay(),
			),
			UnilateralRefundDelay: int32(
				row.GetUnilateralRefundDelay(),
			),
			UnilateralRefundWithoutReceiverDelay: int32(
				row.GetUnilateralRefundWithoutReceiverDelay(),
			),
			PreimageHash:      append([]byte(nil), paymentHash[:]...),
			SignerKeyFamily:   recoverySignerFamily(),
			SignerKeyIndex:    recoverySignerKeyIndex,
			DestinationScript: destination.PkScript,
			MaxFeeRateSatPerKw: c.recoveryPolicy.
				MaxFeeRateSatPerKW,
		},
	)
	if err != nil {
		return fmt.Errorf("arm recovered out-swap claim: %w", err)
	}
	if resp.GetRecoveryId() == "" {
		return fmt.Errorf("arm recovered out-swap claim returned empty id")
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
func recoverableSwapPolicy(row *swaprpc.RecoverableSwap) (
	lntypes.Hash, *arkscript.VHTLCPolicy, []byte, error) {

	paymentHash, err := lntypes.MakeHash(row.GetPaymentHash())
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf(
			"parse recoverable payment hash: %w", err,
		)
	}

	sender, err := btcec.ParsePubKey(row.GetSenderPubkey())
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf(
			"parse recoverable sender key: %w", err,
		)
	}
	receiver, err := btcec.ParsePubKey(row.GetReceiverPubkey())
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf(
			"parse recoverable receiver key: %w", err,
		)
	}
	server, err := btcec.ParsePubKey(row.GetOperatorPubkey())
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf(
			"parse recoverable operator key: %w", err,
		)
	}

	preimageHash := paymentHash
	if len(row.GetPreimageHash()) != 0 {
		preimageHash, err = lntypes.MakeHash(row.GetPreimageHash())
		if err != nil {
			return lntypes.Hash{}, nil, nil, fmt.Errorf(
				"parse recoverable preimage hash: %w", err,
			)
		}
	}
	if preimageHash != paymentHash {
		return lntypes.Hash{}, nil, nil, fmt.Errorf(
			"recoverable preimage hash mismatch",
		)
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
		return lntypes.Hash{}, nil, nil, fmt.Errorf(
			"build recoverable vHTLC policy: %w", err,
		)
	}

	pkScript, err := policy.PkScript()
	if err != nil {
		return lntypes.Hash{}, nil, nil, fmt.Errorf(
			"build recoverable vHTLC pkScript: %w", err,
		)
	}
	if len(row.GetVhtlcPkScript()) != 0 &&
		!bytes.Equal(pkScript, row.GetVhtlcPkScript()) {

		return lntypes.Hash{}, nil, nil, fmt.Errorf(
			"recoverable vHTLC pkScript mismatch",
		)
	}

	return paymentHash, policy, pkScript, nil
}
