//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"encoding/json"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// diagnosticOORRecord keeps the existing requested/received amount separate
// from the optional presentation facts about a stored package or swap leg.
type diagnosticOORRecord struct {
	amount  *int64
	details *diagnosticOORDetails
}

// diagnosticOORDetails is additive mobile JSON. Missing or unfamiliar kinds
// must leave a row visible. A claim amount is the output received into Ark.
type diagnosticOORDetails struct {
	Kind             string          `json:"kind,omitempty"`
	ClaimedAmountSat *int64          `json:"claimed_amount_sat,string,omitempty"`
	RelatedSwap      json.RawMessage `json:"related_swap,omitempty"`
	AmountReadFailed bool            `json:"amount_read_failed,omitempty"`
}

const (
	diagnosticStoredPackage   = "stored_package"
	diagnosticRecoveryPackage = "recovery_package"
	diagnosticLightningClaim  = "lightning_receive_claim"
)

// diagnosticOORRecords enriches a finite page with local evidence. An amount
// lookup or optional evidence failure leaves the status list usable.
func diagnosticOORRecords(ctx context.Context, client *wavewalletdk.Client,
	page *waverpc.ListOORSessionsResponse) map[string]diagnosticOORRecord {

	if len(page.GetSessions()) == 0 {
		return nil
	}
	ids := make([]string, 0, len(page.GetSessions()))
	for _, session := range page.GetSessions() {
		ids = append(ids, session.GetSessionId())
	}
	packages, _ := client.OORDiagnosticPackages(ctx, ids)
	swaps, _ := client.SwapRPC().ListSwaps(
		ctx, &swapclientrpc.ListSwapsRequest{},
	)

	return describeOORRecords(
		ctx, page.GetSessions(), packages, swaps.GetSwaps(),
		client.OORAmount,
	)
}

// describeOORRecords uses exact session identity for claim links and preserves
// the phase, failure and ordinary amount regardless of enrichment.
func describeOORRecords(ctx context.Context, sessions []*waverpc.OORSessionInfo,
	packages map[string]db.OORDiagnosticPackage,
	swaps []*swapclientrpc.SwapSummary,
	readAmount func(context.Context, string, bool) (*int64, error)) map[string]diagnosticOORRecord {

	claims := diagnosticClaimSwaps(swaps)
	records := make(map[string]diagnosticOORRecord, len(sessions))
	for _, session := range sessions {
		id := session.GetSessionId()
		var record diagnosticOORRecord
		var details diagnosticOORDetails

		completed := session.GetStatus() ==
			waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED &&
			session.GetFailureReason() == ""
		pkg, packageOnly := packages[id]
		claim := claims[id]
		switch {
		case completed && claim != nil:
			related, err := (protojson.MarshalOptions{
				UseProtoNames: true, EmitUnpopulated: true,
			}).Marshal(diagnosticSwap(claim))
			if err == nil {
				details.Kind = diagnosticLightningClaim
				details.RelatedSwap = related
			}

		case completed && packageOnly && pkg.RecoveryAncestor:
			details.Kind = diagnosticRecoveryPackage

		case completed && packageOnly:
			details.Kind = diagnosticStoredPackage
		}

		direction := session.GetDirection()
		if !packageOnly && direction !=
			waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_UNSPECIFIED {

			outgoing := direction ==
				waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
			amount, err := readAmount(ctx, id, outgoing)
			if err == nil {
				record.amount = amount
			}
			details.AmountReadFailed = err != nil
		}
		if details.Kind == diagnosticLightningClaim {
			if direction ==
				waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING {

				details.ClaimedAmountSat = record.amount
			} else {
				amount, err := readAmount(ctx, id, false)
				if err == nil {
					details.ClaimedAmountSat = amount
				}
				details.AmountReadFailed = err != nil
			}
		}

		if details.Kind != "" || details.AmountReadFailed {
			record.details = &details
		}
		records[id] = record
	}

	return records
}

// diagnosticClaimSwaps indexes only unambiguous receive claim IDs. A
// conflicting wallet record leaves the OOR session visible with its original
// meaning.
func diagnosticClaimSwaps(
	swaps []*swapclientrpc.SwapSummary) map[string]*swapclientrpc.SwapSummary {

	claims := make(map[string]*swapclientrpc.SwapSummary)
	for _, swap := range swaps {
		id := swap.GetClaimSessionId()
		if id == "" || swap.GetPaymentHash() == "" ||
			swap.GetDirection() !=
				swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE {

			continue
		}

		if _, exists := claims[id]; exists {
			claims[id] = nil
			continue
		}
		claims[id] = swap
	}

	return claims
}

// diagnosticSwap removes the invoice and proof of payment from both Lightning
// diagnostics and any copy linked from a claim.
func diagnosticSwap(
	swap *swapclientrpc.SwapSummary) *swapclientrpc.SwapSummary {

	if swap == nil {
		return nil
	}
	public := proto.Clone(swap).(*swapclientrpc.SwapSummary)
	public.Preimage = ""
	public.Invoice = ""

	return public
}
