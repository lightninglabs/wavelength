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
	"github.com/lightningnetwork/lnd/lntypes"
)

const defaultReceiveExpirySeconds = 3600

// ReceiveSession holds one prepared Lightning->Ark swap receive flow.
type ReceiveSession struct {
	// Invoice is the BOLT-11 payment request the payer must pay.
	Invoice string

	// Preimage is the fixed preimage committed into both the invoice and
	// the expected vHTLC claim path.
	Preimage lntypes.Preimage

	// PaymentHash is the Lightning payment hash for this receive flow.
	PaymentHash lntypes.Hash

	client        *SwapClient
	vhtlcPolicy   *arkscript.VHTLCPolicy
	vhtlcPkScript []byte
}

// ReceiveViaLightning performs a complete Lightning-to-Ark swap in a
// single blocking call. It generates a preimage, requests a route
// hint and vHTLC config from the swap server, creates a signed
// invoice, waits for the server to fund the vHTLC, and claims the
// funds into the client's wallet.
func (c *SwapClient) ReceiveViaLightning(ctx context.Context,
	amountSat btcutil.Amount) (*ReceiveResult, error) {

	session, err := c.StartReceiveViaLightning(ctx, amountSat)
	if err != nil {
		return nil, err
	}

	return session.Wait(ctx)
}

// StartReceiveViaLightning prepares a Lightning->Ark receive flow and returns
// the invoice plus claim context needed to complete it after payment begins.
func (c *SwapClient) StartReceiveViaLightning(ctx context.Context,
	amountSat btcutil.Amount) (*ReceiveSession, error) {

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
		ctx, clientKey, defaultReceiveExpirySeconds,
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

	// The LN payment hash is already SHA256(preimage), which is
	// the format the vHTLC script expects for OP_SHA256 verification.
	policy, err := arkscript.NewVHTLCPolicy(
		arkscript.VHTLCOpts{
			Sender:       serverKey,
			Receiver:     clientKey,
			Server:       operatorKey,
			PreimageHash: hash,
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

	return &ReceiveSession{
		Invoice:       string(inv.PaymentRequest),
		Preimage:      preimage,
		PaymentHash:   hash,
		client:        c,
		vhtlcPolicy:   policy,
		vhtlcPkScript: pkScript,
	}, nil
}

// Wait blocks until the swap server funds the expected vHTLC, then claims it
// into the client's wallet.
func (s *ReceiveSession) Wait(ctx context.Context) (*ReceiveResult, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("receive session must be provided")
	}

	outpoint, amount, err := s.WaitForFunding(ctx)
	if err != nil {
		return nil, err
	}

	return s.Claim(ctx, outpoint, amount)
}

// WaitForFunding blocks until the swap server funds the expected vHTLC and
// returns the outpoint plus amount of the live vHTLC.
func (s *ReceiveSession) WaitForFunding(ctx context.Context) (string, int64,
	error) {

	if s == nil || s.client == nil {
		return "", 0, fmt.Errorf("receive session must be provided")
	}

	outpoint, amount, err := s.client.waitForVHTLC(
		ctx, s.vhtlcPkScript,
	)
	if err != nil {
		return "", 0, fmt.Errorf("wait for vHTLC: %w", err)
	}

	return outpoint, amount, nil
}

// Claim submits the vHTLC claim for a funded out-swap into a fresh wallet-
// owned receive script.
func (s *ReceiveSession) Claim(ctx context.Context, outpoint string,
	amount int64) (*ReceiveResult, error) {

	if s == nil || s.client == nil {
		return nil, fmt.Errorf("receive session must be provided")
	}

	err := s.client.claimReceiveVHTLC(
		ctx, s.PaymentHash, s.Preimage, s.vhtlcPolicy,
		s.vhtlcPkScript, outpoint, amount,
	)
	if err != nil {
		return nil, err
	}

	return &ReceiveResult{
		Invoice:      s.Invoice,
		Preimage:     s.Preimage,
		PaymentHash:  s.PaymentHash,
		VTXOOutpoint: outpoint,
		AmountSat:    amount,
	}, nil
}

// VHTLCInfo returns the scripts for the expected incoming vHTLC and its
// preimage sweep path.
func (s *ReceiveSession) VHTLCInfo() (*ReceiveVHTLCInfo, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("receive session must be provided")
	}

	claimPath, err := s.vhtlcPolicy.ClaimPath(s.Preimage)
	if err != nil {
		return nil, fmt.Errorf("build claim path: %w", err)
	}

	return &ReceiveVHTLCInfo{
		PkScript: append([]byte(nil), s.vhtlcPkScript...),
		ClaimScript: append(
			[]byte(nil), claimPath.WitnessScript...,
		),
	}, nil
}

// claimReceiveVHTLC claims one funded vHTLC with the session preimage into a
// fresh wallet-owned OOR receive script.
func (c *SwapClient) claimReceiveVHTLC(ctx context.Context,
	paymentHash lntypes.Hash, preimage lntypes.Preimage,
	policy *arkscript.VHTLCPolicy, pkScript []byte, outpoint string,
	amount int64) error {

	c.log.InfoS(ctx, "vHTLC found, claiming",
		btclog.Hex("hash", paymentHash[:]),
		slog.String("outpoint", outpoint),
		slog.Int64("amount", amount),
	)

	claimPath, err := policy.ClaimPath(preimage)
	if err != nil {
		return fmt.Errorf("build claim path: %w", err)
	}

	policyTemplate, err := encodeVHTLCPolicyTemplate(policy)
	if err != nil {
		return fmt.Errorf("encode vHTLC policy: %w", err)
	}

	spendPath, err := claimPath.Encode()
	if err != nil {
		return fmt.Errorf("encode claim path: %w", err)
	}

	receiveInfo, err := c.daemon.NewOORReceiveScript(ctx)
	if err != nil {
		return fmt.Errorf("get receive script: %w", err)
	}

	for attempt := 1; attempt <= c.claimMaxAttempts; attempt++ {
		_, err = c.daemon.SendOORWithCustomInputs(
			ctx, receiveInfo.PubKey, amount, []CustomInput{{
				Outpoint:           outpoint,
				VTXOPolicyTemplate: policyTemplate,
				SpendPath:          spendPath,
				AmountSat:          amount,
				PkScript:           pkScript,
			}},
		)
		if err == nil {
			c.log.InfoS(ctx, "vHTLC claimed successfully",
				btclog.Hex("hash", paymentHash[:]),
			)

			return nil
		}

		if attempt == c.claimMaxAttempts {
			break
		}

		c.log.DebugS(ctx, "vHTLC claim retry scheduled",
			btclog.Hex("hash", paymentHash[:]),
			slog.Int("attempt", attempt),
			slog.String("reason", err.Error()))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.claimRetryDelay):
		}
	}

	return fmt.Errorf("claim vHTLC: %w", err)
}

// encodeVHTLCPolicyTemplate serializes the semantic vHTLC policy template in
// canonical leaf order.
func encodeVHTLCPolicyTemplate(policy *arkscript.VHTLCPolicy) ([]byte, error) {
	if policy == nil {
		return nil, fmt.Errorf("vHTLC policy is required")
	}

	if policy.Template == nil {
		return nil, fmt.Errorf("vHTLC policy template is required")
	}

	return policy.Template.Encode()
}

// waitForVHTLC polls the authoritative indexer until the expected vHTLC is
// live, then returns its outpoint and amount.
func (c *SwapClient) waitForVHTLC(ctx context.Context,
	pkScript []byte) (string, int64, error) {

	pkScriptHex := hex.EncodeToString(pkScript)

	ticker := time.NewTicker(c.waitPollInterval)
	defer ticker.Stop()

	timeout := time.NewTimer(c.waitVHTLCTimeout)
	defer timeout.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()

		case <-timeout.C:
			return "", 0, fmt.Errorf(
				"timeout waiting for vHTLC",
			)

		case <-ticker.C:
			vtxo, err := c.daemon.FindLiveVTXOByPkScript(
				ctx, pkScript,
			)
			if err != nil {
				c.log.DebugS(ctx,
					"Unable to query vHTLC state",
					err,
					slog.String("pk_script",
						pkScriptHex),
				)

				continue
			}

			if vtxo != nil {
				c.log.InfoS(ctx, "Found funded vHTLC",
					slog.String("pk_script",
						pkScriptHex),
					slog.String("outpoint",
						vtxo.Outpoint),
					slog.Int64("amount_sat",
						vtxo.AmountSat),
				)

				return vtxo.Outpoint, vtxo.AmountSat, nil
			}
		}
	}
}
