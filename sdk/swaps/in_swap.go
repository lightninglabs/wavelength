package swaps

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightningnetwork/lnd/lntypes"
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
	// Build the vHTLC policy. For in-swaps, client is sender
	// and server is receiver.
	policy, err := arkscript.NewVHTLCPolicy(
		arkscript.VHTLCOpts{
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

	policyTemplate, err := encodeVHTLCPolicyTemplate(policy)
	if err != nil {
		return nil, fmt.Errorf(
			"encode vHTLC policy: %w", err,
		)
	}

	// Fund the vHTLC via OOR with its semantic policy template so the
	// authoritative indexer can authorize both swap participants.
	txid, err := c.daemon.SendOORWithPolicy(
		ctx, cfg.AmountSat, policyTemplate,
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

	preimage, err := c.waitForSpentVTXOPreimage(
		ctx, cfg.PaymentHash, vhtlcPkScript, cfg.Expiry,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"wait for spent vhtlc preimage: %w", err,
		)
	}

	return &PayResult{
		PaymentHash:      cfg.PaymentHash,
		Preimage:         *preimage,
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

	ticker := time.NewTicker(c.waitPollInterval)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if time.Now().After(expiry) {
			return fmt.Errorf("swap expired")
		}

		vtxos, err := c.daemon.ListLiveVTXOs(ctx)
		if err == nil {
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

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
		}
	}
}

// waitForSpentVTXOPreimage polls until the authoritative indexer exposes the
// spent vHTLC's finalized checkpoint PSBTs and one yields the expected
// hashlock preimage.
func (c *SwapClient) waitForSpentVTXOPreimage(ctx context.Context,
	paymentHash lntypes.Hash, pkScript []byte,
	expiry time.Time) (*lntypes.Preimage, error) {

	pkScriptHex := hex.EncodeToString(pkScript)
	ticker := time.NewTicker(c.waitPollInterval)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if time.Now().After(expiry) {
			return nil, fmt.Errorf("swap expired")
		}

		spentVTXOs, err := c.daemon.ListSpentVTXOs(ctx)
		if err == nil {
			preimage, err := findMatchingPreimageInVTXOs(
				spentVTXOs, pkScriptHex, paymentHash,
			)
			if err != nil {
				return nil, err
			}

			if preimage != nil {
				return preimage, nil
			}
		}

		vtxo, err := c.daemon.FindSpentVTXOByPkScript(
			ctx, pkScript,
		)
		if err == nil && vtxo != nil {
			preimage, err := findMatchingPreimageInVTXO(
				vtxo, paymentHash,
			)
			if err != nil {
				return nil, err
			}

			if preimage != nil {
				return preimage, nil
			}

			if vtxo.SpentByTxid != "" {
				pkg, err := c.daemon.
					GetIndexedOORSessionByTxid(
						ctx, pkScript, vtxo.SpentByTxid,
					)
				if err != nil {
					return nil, err
				}

				preimage, err =
					findMatchingPreimageInCheckpoints(pkg,
						paymentHash,
					)
				if err != nil {
					return nil, err
				}

				if preimage != nil {
					return preimage, nil
				}
			}
		}

		liveVTXOs, err := c.daemon.ListLiveVTXOs(ctx)
		if err == nil {
			for i := range liveVTXOs {
				preimage, err := findMatchingPreimageInVTXO(
					&liveVTXOs[i], paymentHash,
				)
				if err != nil {
					return nil, err
				}

				if preimage != nil {
					return preimage, nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-ticker.C:
		}
	}
}

// findMatchingPreimageInVTXOs scans VTXOs matching pkScriptHex for a
// preimage matching paymentHash.
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

	for i := range pkg.FinalCheckpointPSBTs {
		preimage, err := extractPreimageFromCheckpoint(
			pkg.FinalCheckpointPSBTs[i],
		)
		if err != nil {
			return nil, fmt.Errorf(
				"extract preimage from checkpoint: %w",
				err,
			)
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
		FinalCheckpointPSBTs: vtxo.FinalCheckpointPSBTs,
	}, paymentHash)
}
