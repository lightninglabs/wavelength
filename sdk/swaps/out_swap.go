package swaps

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// ReceiveViaLightning performs a complete Lightning-to-Ark swap in a
// single blocking call. It generates a preimage, requests a route
// hint and vHTLC config from the swap server, creates a signed
// invoice, waits for the server to fund the vHTLC, and claims the
// funds into the client's wallet.
func (c *SwapClient) ReceiveViaLightning(ctx context.Context,
	amountSat btcutil.Amount) (*ReceiveResult, error) {

	if c.invoiceGen == nil {
		return nil, fmt.Errorf(
			"invoice generator required for " +
				"ReceiveViaLightning",
		)
	}

	// Get our identity and operator keys.
	clientKey, err := c.daemon.GetIdentityPubkey(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"get client pubkey: %w", err,
		)
	}

	operatorKey, err := c.daemon.GetOperatorPubkey(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"get operator pubkey: %w", err,
		)
	}

	// Generate a random preimage locally so we can construct
	// both the invoice and the matching vHTLC.
	preimage, err := NewPreimage()
	if err != nil {
		return nil, fmt.Errorf(
			"generate preimage: %w", err,
		)
	}

	// Get a route hint and locked-in vHTLC config from the
	// swap server.
	hint, vhtlcCfg, err := c.server.RequestChannelID(
		ctx, clientKey, 3600,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"request channel ID: %w", err,
		)
	}

	c.log.InfoS(ctx, "Received route hint from swap server",
		slog.Uint64("channel_id", hint.ChannelID),
	)

	// Create a signed invoice locked to our preimage.
	inv, hash, err := c.invoiceGen.CreateInvoice(
		ctx, amountSat, "swap", hint, 0, &preimage,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"create invoice: %w", err,
		)
	}

	c.log.InfoS(ctx, "Invoice created for out-swap",
		btclog.Hex("hash", hash[:]),
		slog.Int64("amount_sat", int64(amountSat)),
	)

	// Build the expected vHTLC pkScript so we can identify
	// the funded VTXO when it appears.
	serverKey, err := btcec.ParsePubKey(
		vhtlcCfg.SwapServerPubkey,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"parse server pubkey: %w", err,
		)
	}

	preimageHash := arkscript.Hash160(hash[:])

	policy, err := arkscript.NewVHTLCPolicy(
		arkscript.VHTLCOpts{
			Sender:       serverKey,
			Receiver:     clientKey,
			Server:       operatorKey,
			PreimageHash: preimageHash,
			RefundLocktime: vhtlcCfg.
				RefundLocktime,
			UnilateralClaimDelay: vhtlcCfg.
				UnilateralClaimDelay,
			UnilateralRefundDelay: vhtlcCfg.
				UnilateralRefundDelay,
			UnilateralRefundWithoutReceiverDelay: vhtlcCfg.
				UnilateralRefundWithoutReceiverDelay,
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"build vHTLC policy: %w", err,
		)
	}

	pkScript, err := policy.PkScript()
	if err != nil {
		return nil, fmt.Errorf(
			"get vHTLC pkScript: %w", err,
		)
	}

	// Wait for the swap server to fund the vHTLC.
	outpoint, amount, err := c.waitForVHTLC(ctx, pkScript)
	if err != nil {
		return nil, fmt.Errorf(
			"wait for vHTLC: %w", err,
		)
	}

	c.log.InfoS(ctx, "vHTLC found, claiming",
		btclog.Hex("hash", hash[:]),
		slog.String("outpoint", outpoint),
		slog.Int64("amount", amount),
	)

	// Build the claim path using the preimage.
	claimPath, err := policy.ClaimPath(preimage[:])
	if err != nil {
		return nil, fmt.Errorf(
			"build claim path: %w", err,
		)
	}

	// Get a fresh receive script for the claimed funds.
	receiveScript, err := c.daemon.NewOORReceiveScript(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"get receive script: %w", err,
		)
	}

	// Claim the vHTLC via SendOOR with custom input.
	_, err = c.daemon.SendOORWithCustomInputs(
		ctx,
		receiveScript,
		amount,
		[]CustomInput{{
			Outpoint:           outpoint,
			AmountSat:          amount,
			PkScript:           pkScript,
			SpendWitnessScript: claimPath.WitnessScript,
			SpendControlBlock:  claimPath.ControlBlock,
			ConditionWitness: [][]byte{
				preimage[:],
			},
		}},
	)
	if err != nil {
		return nil, fmt.Errorf("claim vHTLC: %w", err)
	}

	c.log.InfoS(ctx, "vHTLC claimed successfully",
		btclog.Hex("hash", hash[:]),
	)

	return &ReceiveResult{
		Invoice:      string(inv.PaymentRequest),
		Preimage:     preimage,
		PaymentHash:  hash,
		VTXOOutpoint: outpoint,
		AmountSat:    amount,
	}, nil
}

// waitForVHTLC polls the daemon's VTXOs until one matching the
// given pkScript appears. Returns the outpoint and amount.
func (c *SwapClient) waitForVHTLC(ctx context.Context,
	pkScript []byte) (string, int64, error) {

	pkScriptHex := hex.EncodeToString(pkScript)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timeout := time.After(60 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()

		case <-timeout:
			return "", 0, fmt.Errorf(
				"timeout waiting for vHTLC",
			)

		case <-ticker.C:
			vtxos, err := c.daemon.ListLiveVTXOs(ctx)
			if err != nil {
				continue
			}

			for _, vtxo := range vtxos {
				vtxoHex := hex.EncodeToString(
					vtxo.PkScript,
				)
				if vtxoHex == pkScriptHex {
					return vtxo.Outpoint,
						vtxo.AmountSat, nil
				}
			}
		}
	}
}
