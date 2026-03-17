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

// InSwapState represents the client-side in-swap state.
type InSwapState int

const (
	// InSwapCreated indicates the swap has been created but not
	// yet funded.
	InSwapCreated InSwapState = iota

	// InSwapFundingVHTLC indicates the client is funding the
	// vHTLC via OOR.
	InSwapFundingVHTLC

	// InSwapWaitingForPayment indicates the vHTLC is funded and
	// the client is waiting for the server to claim it.
	InSwapWaitingForPayment

	// InSwapCompleted indicates the swap completed successfully.
	InSwapCompleted

	// InSwapFailed indicates the swap failed.
	InSwapFailed
)

// String returns a human-readable representation of the in-swap
// state.
func (s InSwapState) String() string {
	switch s {
	case InSwapCreated:
		return "Created"
	case InSwapFundingVHTLC:
		return "FundingVHTLC"
	case InSwapWaitingForPayment:
		return "WaitingForPayment"
	case InSwapCompleted:
		return "Completed"
	case InSwapFailed:
		return "Failed"
	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

// SendPayment pays a Lightning invoice by funding a vHTLC that the
// swap server will claim after paying the invoice. This is the
// main entry point for in-swaps (Ark -> Lightning).
func (c *SwapClient) SendPayment(ctx context.Context,
	invoice string, maxFeeSat uint64) error {

	// Get our identity and operator keys.
	clientKey, err := c.daemon.GetIdentityPubkey(ctx)
	if err != nil {
		return fmt.Errorf("get client pubkey: %w", err)
	}

	// Request swap parameters from the server.
	cfg, err := c.server.CreateInSwap(
		ctx, invoice, maxFeeSat, clientKey,
	)
	if err != nil {
		return fmt.Errorf("create in-swap: %w", err)
	}

	c.log.InfoS(ctx, "In-swap created",
		btclog.Hex("hash", cfg.PaymentHash[:]),
		slog.Int64("amount_sat", cfg.AmountSat),
		slog.Uint64("fee_sat", cfg.FeeSat),
	)

	operatorKey, err := c.daemon.GetOperatorPubkey(ctx)
	if err != nil {
		return fmt.Errorf("get operator pubkey: %w", err)
	}

	// Build preimage hash from payment hash.
	preimageHash := arkscript.Hash160(cfg.PaymentHash[:])

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
		return fmt.Errorf("build vHTLC policy: %w", err)
	}

	// Get the vHTLC pkScript.
	vhtlcPkScript, err := policy.PkScript()
	if err != nil {
		return fmt.Errorf("get vHTLC pkScript: %w", err)
	}

	// Fund the vHTLC via OOR.
	txid, err := c.daemon.SendOOR(
		ctx, vhtlcPkScript, cfg.AmountSat,
	)
	if err != nil {
		return fmt.Errorf("fund vHTLC: %w", err)
	}

	c.log.InfoS(ctx, "vHTLC funded",
		btclog.Hex("hash", cfg.PaymentHash[:]),
		slog.String("txid", txid),
	)

	// Wait for the server to claim the vHTLC (which means it
	// paid the invoice).
	err = c.waitForVHTLCSpent(ctx, vhtlcPkScript, cfg.Expiry)
	if err != nil {
		return fmt.Errorf("wait for claim: %w", err)
	}

	c.log.InfoS(ctx, "In-swap completed",
		btclog.Hex("hash", cfg.PaymentHash[:]),
	)

	return nil
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
