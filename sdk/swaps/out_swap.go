package swaps

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightningnetwork/lnd/lntypes"
)

// OutSwapState represents the client-side out-swap state.
type OutSwapState int

const (
	// OutSwapWaitingForFunding indicates the swap is waiting for
	// the swap server to fund the vHTLC.
	OutSwapWaitingForFunding OutSwapState = iota

	// OutSwapClaimingVHTLC indicates the client is claiming the
	// vHTLC with the preimage.
	OutSwapClaimingVHTLC

	// OutSwapCompleted indicates the swap completed successfully.
	OutSwapCompleted

	// OutSwapFailed indicates the swap failed.
	OutSwapFailed
)

// String returns a human-readable representation of the out-swap
// state.
func (s OutSwapState) String() string {
	switch s {
	case OutSwapWaitingForFunding:
		return "WaitingForFunding"
	case OutSwapClaimingVHTLC:
		return "ClaimingVHTLC"
	case OutSwapCompleted:
		return "Completed"
	case OutSwapFailed:
		return "Failed"
	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

// OutSwap represents a client-side out-swap (Lightning -> Ark).
// The client receives a VTXO by claiming a vHTLC funded by the
// swap server.
type OutSwap struct {
	// PaymentHash is the SHA-256 payment hash for the swap.
	PaymentHash lntypes.Hash

	// Preimage is the preimage that unlocks the payment hash.
	Preimage lntypes.Preimage

	// AmountSat is the swap amount in satoshis.
	AmountSat int64

	// State is the current state of the out-swap.
	State OutSwapState

	// VHTLCConfig holds the vHTLC parameters for this swap.
	VHTLCConfig VHTLCConfig
}

// RequestRouteHint asks the swap server for a route hint that can
// be embedded in a Lightning invoice. The returned route hint
// directs payments through the swap server's virtual channel.
func (c *SwapClient) RequestRouteHint(ctx context.Context,
	vhtlcPubkey *btcec.PublicKey,
	expirySeconds uint32) (*RouteHint, error) {

	hint, err := c.server.RequestChannelID(
		ctx, vhtlcPubkey, expirySeconds,
	)
	if err != nil {
		return nil, fmt.Errorf("request route hint: %w", err)
	}

	c.log.InfoS(ctx, "Received route hint from swap server",
		slog.Uint64("channel_id", hint.ChannelID),
	)

	return hint, nil
}

// ClaimOutSwap claims a vHTLC that was funded by the swap server
// after an HTLC was intercepted. The client uses the preimage
// from the original invoice to spend the vHTLC via the claim path.
func (c *SwapClient) ClaimOutSwap(ctx context.Context,
	intercept HtlcIntercept,
	preimage lntypes.Preimage) error {

	// Get our identity and operator keys.
	clientKey, err := c.daemon.GetIdentityPubkey(ctx)
	if err != nil {
		return fmt.Errorf("get client pubkey: %w", err)
	}

	operatorKey, err := c.daemon.GetOperatorPubkey(ctx)
	if err != nil {
		return fmt.Errorf("get operator pubkey: %w", err)
	}

	// Parse the swap server's pubkey from the config.
	serverKey, err := btcec.ParsePubKey(
		intercept.VHTLCConfig.SwapServerPubkey,
	)
	if err != nil {
		return fmt.Errorf("parse server pubkey: %w", err)
	}

	// Build the preimage hash (HASH160 of the payment hash).
	preimageHash := arkscript.Hash160(
		intercept.PaymentHash[:],
	)

	// Build the vHTLC policy matching what the server created.
	// Server is the sender, client is the receiver.
	policy, err := arkscript.NewVHTLCPolicy(
		arkscript.VHTLCOpts{
			Sender:   serverKey,
			Receiver: clientKey,
			Server:   operatorKey,
			PreimageHash: preimageHash,
			RefundLocktime: intercept.VHTLCConfig.
				RefundLocktime,
			UnilateralClaimDelay: intercept.VHTLCConfig.
				UnilateralClaimDelay,
			UnilateralRefundDelay: intercept.VHTLCConfig.
				UnilateralRefundDelay,
			UnilateralRefundWithoutReceiverDelay: intercept.
				VHTLCConfig.
				UnilateralRefundWithoutReceiverDelay,
		},
	)
	if err != nil {
		return fmt.Errorf("build vHTLC policy: %w", err)
	}

	// Get the claim path with the preimage.
	claimPath, err := policy.ClaimPath(preimage[:])
	if err != nil {
		return fmt.Errorf("build claim path: %w", err)
	}

	// Get the vHTLC pkScript.
	vhtlcPkScript, err := policy.PkScript()
	if err != nil {
		return fmt.Errorf("get vHTLC pkScript: %w", err)
	}

	// Wait for the vHTLC to appear in our VTXOs.
	outpoint, amount, err := c.waitForVHTLC(
		ctx, vhtlcPkScript,
	)
	if err != nil {
		return fmt.Errorf("wait for vHTLC: %w", err)
	}

	// Get a fresh receive script for the claimed funds.
	receiveScript, err := c.daemon.NewOORReceiveScript(ctx)
	if err != nil {
		return fmt.Errorf("get receive script: %w", err)
	}

	c.log.InfoS(ctx, "vHTLC found, claiming",
		btclog.Hex("hash", intercept.PaymentHash[:]),
		slog.String("outpoint", outpoint),
		slog.Int64("amount", amount),
	)

	// Claim via SendOOR with custom input.
	_, err = c.daemon.SendOORWithCustomInputs(
		ctx,
		receiveScript,
		amount,
		[]CustomInput{{
			Outpoint:           outpoint,
			AmountSat:          amount,
			PkScript:           vhtlcPkScript,
			SpendWitnessScript: claimPath.WitnessScript,
			SpendControlBlock:  claimPath.ControlBlock,
			ConditionWitness: [][]byte{
				preimage[:],
			},
		}},
	)
	if err != nil {
		return fmt.Errorf("claim vHTLC: %w", err)
	}

	c.log.InfoS(ctx, "vHTLC claimed successfully",
		btclog.Hex("hash", intercept.PaymentHash[:]),
	)

	return nil
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
