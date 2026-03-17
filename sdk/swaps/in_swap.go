package swaps

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// PayViaLightning performs a complete Ark-to-Lightning swap in a
// single blocking call. It creates an in-swap with the server,
// builds and funds a vHTLC, then waits for the server to claim the
// vHTLC (which means the Lightning invoice was paid).
func (c *SwapClient) PayViaLightning(ctx context.Context,
	invoice string, maxFeeSat uint64) (*PayResult, error) {

	// Get our identity and operator keys.
	clientKey, err := c.daemon.GetIdentityPubkey(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"get client pubkey: %w", err,
		)
	}

	// Request swap parameters from the server.
	cfg, err := c.server.CreateInSwap(
		ctx, invoice, maxFeeSat, clientKey,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"create in-swap: %w", err,
		)
	}

	c.log.InfoS(ctx, "In-swap created",
		btclog.Hex("hash", cfg.PaymentHash[:]),
		slog.Int64("amount_sat", cfg.AmountSat),
		slog.Uint64("fee_sat", cfg.FeeSat),
	)

	operatorKey, err := c.daemon.GetOperatorPubkey(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"get operator pubkey: %w", err,
		)
	}

	// The LN payment hash is already SHA256(preimage), which is
	// the format the vHTLC script expects for OP_SHA256 verification.
	preimageHash := cfg.PaymentHash[:]

	// Build the vHTLC policy. For in-swaps, client is sender
	// and server is receiver.
	policy, err := arkscript.NewVHTLCPolicy(
		arkscript.VHTLCOpts{
			Sender:       clientKey,
			Receiver:     cfg.ServerPubkey,
			Server:       operatorKey,
			PreimageHash: preimageHash,
			RefundLocktime: cfg.VHTLCConfig.
				RefundLocktime,
			UnilateralClaimDelay: cfg.VHTLCConfig.
				UnilateralClaimDelay,
			UnilateralRefundDelay: cfg.VHTLCConfig.
				UnilateralRefundDelay,
			UnilateralRefundWithoutReceiverDelay: cfg.
				VHTLCConfig.
				UnilateralRefundWithoutReceiverDelay,
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"build vHTLC policy: %w", err,
		)
	}

	// Get the vHTLC pkScript.
	vhtlcPkScript, err := policy.PkScript()
	if err != nil {
		return nil, fmt.Errorf(
			"get vHTLC pkScript: %w", err,
		)
	}

	// Fund the vHTLC via OOR.
	txid, err := c.daemon.SendOOR(
		ctx, vhtlcPkScript, cfg.AmountSat,
	)
	if err != nil {
		return nil, fmt.Errorf("fund vHTLC: %w", err)
	}

	c.log.InfoS(ctx, "vHTLC funded",
		btclog.Hex("hash", cfg.PaymentHash[:]),
		slog.String("txid", txid),
	)

	// Wait for the server to claim the vHTLC (which means it
	// paid the invoice).
	err = c.waitForVHTLCSpent(
		ctx, vhtlcPkScript, cfg.Expiry,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"wait for claim: %w", err,
		)
	}

	c.log.InfoS(ctx, "In-swap completed",
		btclog.Hex("hash", cfg.PaymentHash[:]),
	)

	return &PayResult{
		PaymentHash:      cfg.PaymentHash,
		FundingSessionID: txid,
		FeeSat:           cfg.FeeSat,
	}, nil
}

// waitForVHTLCSpent polls the daemon's VTXOs until the vHTLC with
// the given pkScript is no longer live (was spent by the server
// claiming it with the preimage).
func (c *SwapClient) waitForVHTLCSpent(ctx context.Context,
	pkScript []byte, expiry time.Time) error {

	pkScriptHex := hex.EncodeToString(pkScript)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
			if time.Now().After(expiry) {
				return fmt.Errorf("swap expired")
			}

			vtxos, err := c.daemon.ListLiveVTXOs(ctx)
			if err != nil {
				continue
			}

			// If the vHTLC is no longer live, the server
			// claimed it.
			found := false
			for _, vtxo := range vtxos {
				vtxoHex := hex.EncodeToString(
					vtxo.PkScript,
				)
				if vtxoHex == pkScriptHex {
					found = true

					break
				}
			}

			if !found {
				return nil
			}
		}
	}
}
