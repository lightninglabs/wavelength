package waveclicommands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
)

// sendResult is the compact summary printed after a send settles. It distills
// the verbose inspection trace down to the fields a caller actually needs: the
// amount that left the wallet, the fee, and, for a Lightning send, the payment
// preimage that proves the invoice was paid. The full ledger, VTXO, and swap
// breakdown stays available on demand via `da activity inspect <id>`.
type sendResult struct {
	// Status is the short terminal status label (e.g. COMPLETE, PENDING).
	Status string `json:"status"`

	// Kind is the short activity kind label (SEND for this verb).
	Kind string `json:"kind"`

	// AmountSat is the signed amount that left the wallet in satoshis.
	AmountSat int64 `json:"amount_sat"`

	// FeeSat is the fee paid for the send in satoshis.
	FeeSat int64 `json:"fee_sat"`

	// Settlement is the short settlement label (e.g. IN_ARK, LIGHTNING) for
	// swap-backed sends, omitted when the send did not run over a swap.
	Settlement string `json:"settlement,omitempty"`

	// Destination is the counterparty summary the daemon recorded for the
	// send (invoice prefix or onchain address).
	Destination string `json:"destination,omitempty"`

	// PaymentHash is the Lightning payment hash for invoice sends.
	PaymentHash string `json:"payment_hash,omitempty"`

	// Preimage is the hex payment preimage, the proof of payment for a
	// completed Lightning send. It is empty until the swap reveals it.
	Preimage string `json:"preimage,omitempty"`

	// Txid is the settling transaction id for onchain sends.
	Txid string `json:"txid,omitempty"`

	// VtxoOutpoint is the settling vHTLC/VTXO outpoint when known.
	VtxoOutpoint string `json:"vtxo_outpoint,omitempty"`

	// ID is the wallet entry id, the handle for `da activity inspect`.
	ID string `json:"id"`
}

// sendResultFromEntry builds the compact summary from a wallet entry alone,
// used both for the dispatched (still-pending) receipt and as the base for the
// settled summary.
func sendResultFromEntry(entry *wavewalletrpc.WalletEntry) sendResult {
	res := sendResult{
		Status:      formatEntryStatus(entry.GetStatus()),
		Kind:        formatEntryKind(entry.GetKind()),
		AmountSat:   entry.GetAmountSat(),
		FeeSat:      entry.GetFeeSat(),
		Destination: entry.GetCounterparty(),
		ID:          entry.GetId(),
	}

	if progress := entry.GetProgress(); progress != nil {
		res.PaymentHash = progress.GetPaymentHash()
		res.Txid = progress.GetTxid()
		res.VtxoOutpoint = progress.GetVtxoOutpoint()
	}

	// A freshly dispatched entry may not carry a progress payment hash yet,
	// so fall back to the one echoed on the original invoice request.
	if res.PaymentHash == "" {
		if ln := entry.GetRequest().GetLightningInvoice(); ln != nil {
			res.PaymentHash = ln.GetPaymentHash()
		}
	}

	return res
}

// sendResultFromInspection builds the settled summary from a terminal
// inspection response, folding in the swap preimage and settlement type when
// the send ran over a swap.
func sendResultFromInspection(
	resp *wavewalletrpc.InspectActivityResponse) sendResult {

	res := sendResultFromEntry(resp.GetEntry())

	if swap := resp.GetSwap(); swap != nil {
		res.Preimage = swap.GetPreimage()
		res.Settlement = trimSettlementType(swap.GetSettlementType())
		if res.PaymentHash == "" {
			res.PaymentHash = swap.GetPaymentHash()
		}
	}

	return res
}

// printSendResult writes the compact send summary to w as pretty JSON so a
// shell pipeline can consume it while the phase transitions stay on stderr.
// Callers pass the command's stdout so tests can capture the output.
func printSendResult(w io.Writer, res sendResult) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal send result: %w", err)
	}

	fmt.Fprintln(w, string(data))

	return nil
}

// trimSettlementType strips the swap settlement enum prefix down to its short
// label (SWAP_SETTLEMENT_TYPE_IN_ARK -> IN_ARK).
func trimSettlementType(s string) string {
	return strings.TrimPrefix(s, "SWAP_SETTLEMENT_TYPE_")
}
