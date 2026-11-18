package lnruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/route"
)

// LookupChannelPayment reads one control-tower snapshot. A hash is shared
// across peers, so neither its preimage nor a terminal failure is attributable
// to this channel until its stored attempts identify the private leg.
func (n *NativeNode) LookupChannelPayment(ctx context.Context,
	record arkchannel.Record, hash lntypes.Hash, amount btcutil.Amount) (
	lntypes.Preimage, bool, error) {

	if err := ctx.Err(); err != nil {
		return lntypes.Preimage{}, false, err
	}
	terms := record.Snapshot.Terms
	if record.Snapshot.Backing == nil || terms.ReservedSCID == 0 ||
		amount <= 0 || amount > btcutil.MaxSatoshi {
		return lntypes.Preimage{}, false, fmt.Errorf("invalid " +
			"private channel payment lookup")
	}
	local, peer := terms.ClientNodeKey, terms.HubNodeKey
	switch n.cfg.Party {
	case arkchannel.PartyHub:
		local, peer = peer, local

	case arkchannel.PartyClient:
	default:
		return lntypes.Preimage{}, false, fmt.Errorf("unknown " +
			"channel party")
	}
	if n.cfg.IdentityKey.PubKey == nil || n.cfg.RemoteNodeKey == nil ||
		route.NewVertex(n.cfg.IdentityKey.PubKey) != local ||
		route.NewVertex(n.cfg.RemoteNodeKey) != peer {
		return lntypes.Preimage{}, false, fmt.Errorf("channel " +
			"payment endpoint does not match channel terms")
	}
	payment, err := n.runtime.Payments().ControlTower().FetchPayment(
		ctx, hash,
	)
	if errors.Is(err, paymentsdb.ErrPaymentNotInitiated) {
		return lntypes.Preimage{}, false, nil
	}
	if err != nil {
		return lntypes.Preimage{}, false, err
	}
	if !payment.Terminated() {
		return lntypes.Preimage{}, false, nil
	}
	attempts := payment.GetHTLCs()
	if len(attempts) == 0 {
		return lntypes.Preimage{}, false, fmt.Errorf("terminal " +
			"payment has no channel attempt")
	}
	msat := lnwire.NewMSatFromSatoshis(amount)
	settled := 0
	for i := range attempts {
		a := &attempts[i]
		if (a.Hash != nil && *a.Hash != hash) || !matchesPrivateRoute(
			&a.Route, local, peer, terms.ReservedSCID, msat,
		) {
			return lntypes.Preimage{}, false, fmt.Errorf(
				"payment attempt does not match private " +
					"channel")
		}
		if (a.Settle == nil) == (a.Failure == nil) {
			return lntypes.Preimage{}, false, fmt.Errorf(
				"private attempt has no unambiguous outcome")
		}
		if a.Settle != nil {
			settled++
		}
	}
	if !(settled == 1 &&
		payment.GetStatus() == paymentsdb.StatusSucceeded ||
		settled == 0 &&
			payment.GetStatus() == paymentsdb.StatusFailed) {
		return lntypes.Preimage{}, false, fmt.Errorf("inconsistent " +
			"private payment outcome")
	}
	preimage, found, err := terminalPaymentPreimage(payment)
	if found && preimage.Hash() != hash {
		return lntypes.Preimage{}, false, fmt.Errorf("private " +
			"payment preimage does not match hash")
	}

	return preimage, found, err
}

// matchesPrivateRoute checks the persisted route without consulting a live
// link: recovery can run after the original link or channel has closed.
func matchesPrivateRoute(r *route.Route, local, peer route.Vertex, scid uint64,
	amount lnwire.MilliSatoshi) bool {

	if r.SourcePubKey != local || r.TotalAmount != amount ||
		len(r.Hops) != 1 || r.Hops[0] == nil {
		return false
	}
	first := r.FirstHopAmount.Val.Int()
	hop := r.Hops[0]

	return (first == 0 || first == amount) && hop.ChannelID == scid &&
		hop.PubKeyBytes == peer && hop.AmtToForward == amount &&
		hop.AMP == nil && hop.MPP == nil
}
