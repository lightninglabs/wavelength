package swaps

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

// restoreFundedSwapSession persists a normal settlement session at a funded
// state. Recovery admission is cheap; its worker, not the restore RPC, owns
// cooperative retries, refund maturity, and policy-gated exit escalation.
func (c *SwapClient) restoreFundedSwapSession(ctx context.Context,
	row *swaprpc.RecoverableSwap, hash lntypes.Hash, live *VTXOInfo,
	direction waverpc.VHTLCRecoveryDirection,
	action waverpc.VHTLCRecoveryAction) error {

	if live.Outpoint == "" || live.AmountSat <= 0 {
		return malformedRecoverableSwap(
			fmt.Errorf("invalid funded vHTLC"),
		)
	}
	// The advertised vHTLC total includes attached credit. Never replace
	// that expectation with an underfunded live output and reveal the
	// invoice preimage for less than the server promised.
	if direction == recoveryDirectionReceive &&
		uint64(live.AmountSat) != row.GetAmountSat() {
		return malformedRecoverableSwap(
			fmt.Errorf(
				"funded vHTLC amount %d does not match "+
					"advertised amount %d", live.AmountSat,
				row.GetAmountSat(),
			),
		)
	}
	_, policy, pkScript, err := recoverableSwapPolicy(row)
	if err != nil {
		return malformedRecoverableSwap(err)
	}
	template, err := encodeVHTLCPolicyTemplate(policy)
	if err != nil {
		return malformedRecoverableSwap(err)
	}
	params, err := recoveredArmParams(
		row, hash, live, direction, action, nil,
	)
	if err != nil {
		return malformedRecoverableSwap(err)
	}
	existing, err := c.existingRecoverableVHTLC(ctx, params)
	if err != nil {
		return err
	}

	var preimage *lntypes.Preimage
	if direction == recoveryDirectionReceive {
		preimage, err = recoverOutSwapPreimage(
			ctx, c.daemon, hash, row.GetPreimageRecoveryParams(),
		)
		if err != nil {
			return err
		}
	}

	// The cooperative output needs the owner key as well as its script.
	// Existing daemon jobs retain their own on-chain destination;
	// allocating a cooperative destination must not re-arm or mutate that
	// obligation.
	destination, err := c.daemon.AllocateReceiveScript(ctx, "swap-restore")
	if err != nil {
		return fmt.Errorf("allocate recovered swap destination: %w",
			err)
	}
	if destination == nil || len(destination.PkScript) == 0 ||
		len(destination.PubKeyXOnly) != 32 {
		return fmt.Errorf("recovered swap destination is incomplete")
	}
	var recoveryID string
	if existing != nil {
		recoveryID = existing.GetRecoveryId()
	}
	// The script builder above has already checked these encodings. Parse
	// participant keys again only to populate the existing session model.
	sender, err := btcec.ParsePubKey(row.GetSenderPubkey())
	if err != nil {
		return malformedRecoverableSwap(err)
	}
	receiver, err := btcec.ParsePubKey(row.GetReceiverPubkey())
	if err != nil {
		return malformedRecoverableSwap(err)
	}
	operator, err := btcec.ParsePubKey(row.GetOperatorPubkey())
	if err != nil {
		return malformedRecoverableSwap(err)
	}
	config := VHTLCConfig{
		RefundLocktime:        row.GetRefundLocktime(),
		UnilateralClaimDelay:  row.GetUnilateralClaimDelay(),
		UnilateralRefundDelay: row.GetUnilateralRefundDelay(),
		UnilateralRefundWithoutReceiverDelay: row.
			GetUnilateralRefundWithoutReceiverDelay(),
	}
	created := c.currentTime()
	if direction == recoveryDirectionPay {
		config.SwapServerPubkey = row.GetReceiverPubkey()
		session := &paySession{
			client: c, state: PayStateRefundInitiated,
			createdAt: created, updatedAt: created,
			cfg: &InSwapConfig{
				PaymentHash: hash, AmountSat: live.AmountSat,
				ServerPubkey: receiver, VHTLCConfig: config,
				Expiry:         row.GetExpiresAt().AsTime(),
				SettlementType: SettlementTypeLightning,
			},
			clientPubKey: sender, serverPubKey: receiver,
			operatorPubKey: operator, vhtlcPolicy: policy,
			vhtlcPkScript: pkScript, vhtlcPolicyTemplate: template,
			vhtlcOutpoint:       live.Outpoint,
			vhtlcAmount:         live.AmountSat,
			refundReceivePubKey: destination.PubKeyXOnly,
			refundReceiveScript: destination.PkScript,
			refundRecoveryID:    recoveryID,
		}

		// Admit before arming. A competing normal creator must never
		// inherit a daemon job with the losing restore destination.
		if err := session.persistRestored(ctx); err != nil {
			return err
		}
		if session.state != PayStateRefundInitiated {
			return nil
		}

		return session.ensurePayRefundRecoveryArmed(ctx)
	}

	config.SwapServerPubkey = row.GetSenderPubkey()
	session := &ReceiveSession{
		client: c, state: ReceiveStateVHTLCFunded,
		PaymentHash: hash, Preimage: *preimage,
		amountSat:          btcutil.Amount(live.AmountSat),
		requestedAmountSat: row.GetAmountSat(),
		expectedVHTLCSat:   row.GetAmountSat(),
		createdAt:          created, updatedAt: created,
		deadline:     row.GetExpiresAt().AsTime(),
		clientPubKey: receiver, swapServerPubKey: sender,
		operatorPubKey: operator, vhtlcConfig: config,
		settlementType: SettlementTypeLightning,
		vhtlcPolicy:    policy, vhtlcPolicyTemplate: template,
		vhtlcPkScript: pkScript, vhtlcOutpoint: live.Outpoint,
		vhtlcAmount:        live.AmountSat,
		claimReceivePubKey: destination.PubKeyXOnly,
		claimReceiveScript: destination.PkScript,
		claimRecoveryID:    recoveryID,
		forfeitBindingGate: newReceiveForfeitBindingGate(),
	}

	if err := session.persistRestored(ctx); err != nil {
		return err
	}
	if session.state != ReceiveStateVHTLCFunded &&
		session.state != ReceiveStateClaimInitiated {
		return nil
	}

	return session.ensureReceiveClaimRecoveryArmed(ctx)
}
