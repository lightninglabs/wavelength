package swaps

import (
	"context"
	"time"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

// IndexedVTXOWaiter optionally supplies bounded, proof-gated remote waits.
// The bool confirms that an empty result consumed the requested wait. A false
// bit requires the existing polling fallback. Returned rows remain subject to
// the swap's amount, script, expiry and preimage validation.
type IndexedVTXOWaiter interface {
	// WaitIndexedVTXO waits for one live or spent indexed output.
	WaitIndexedVTXO(ctx context.Context, pkScript, policy []byte,
		state waverpc.VTXOStatus, wait time.Duration) (*VTXOInfo, bool,
		error)
}

// findVTXOWithWait uses the optional remote wait without requiring older
// daemon adapters to implement it. Immediate fallback reads keep their pacing.
func (c *SwapClient) findVTXOWithWait(ctx context.Context, script,
	policy []byte, state waverpc.VTXOStatus, wait time.Duration) (*VTXOInfo,
	bool, error) {

	if waiter, ok := c.daemon.(IndexedVTXOWaiter); ok && wait > 0 {
		return waiter.WaitIndexedVTXO(ctx, script, policy, state, wait)
	}
	var info *VTXOInfo
	var err error
	if state == waverpc.VTXOStatus_VTXO_STATUS_SPENT {
		info, err = c.daemon.FindSpentVTXOByPkScript(ctx, script)
	} else {
		info, err = c.daemon.FindLiveVTXOByPkScript(ctx, script)
	}

	return info, false, err
}

// indexedWaitBudget preserves the existing retry cadence and expiry boundary.
func (c *SwapClient) indexedWaitBudget(expiry time.Time) time.Duration {
	wait := c.waitPollInterval
	if !expiry.IsZero() {
		wait = min(wait, expiry.Sub(c.currentTime()))
	}

	return max(wait, 0)
}

// waitForIndexedClaim waits only on the first spent-row lookup. A visible row
// still uses the existing bounded checkpoint catch-up reads and validation.
func (s *paySession) waitForIndexedClaim(ctx context.Context) (
	*lntypes.Preimage, *VTXOInfo, bool, error) {

	waited := false
	firstLookup := true
	preimage, spent, err := s.client.waitForClaimObservation(
		ctx, s.cfg.PaymentHash, s.vhtlcPkScript,
		func(ctx context.Context) (*VTXOInfo, error) {
			if !firstLookup {
				return s.client.daemon.FindSpentVTXOByPkScript(
					ctx, s.vhtlcPkScript,
				)
			}
			firstLookup = false
			info, supported, err := s.client.findVTXOWithWait(
				ctx, s.vhtlcPkScript, s.vhtlcPolicyTemplate,
				waverpc.VTXOStatus_VTXO_STATUS_SPENT,
				s.client.indexedWaitBudget(s.cfg.Expiry),
			)
			waited = supported && err == nil && info == nil

			return info, err
		},
	)

	return preimage, spent, waited, err
}
