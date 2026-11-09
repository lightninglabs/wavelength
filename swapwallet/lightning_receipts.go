//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// creditReceiptReader borrows one original admission without listing history.
type creditReceiptReader interface {
	// ReadCreditOperationForReceipt returns the original local admission.
	ReadCreditOperationForReceipt(context.Context,
		string) (*db.CreditOperationRecord, error)
}

// lookupLightningReceipt checks typed persisted sources, then uses the normal
// canonical sink. It never interprets raw OOR, refresh, deposit or asset
// carrier activity as a received Bitcoin invoice amount.
func (r *Runtime) lookupLightningReceipt(ctx context.Context,
	ref waved.LightningReceiptReference, after int64) (
	*waved.LightningReceipt, error) {

	if r.deps == nil || r.deps.ActivityStore == nil {
		return nil, fmt.Errorf("canonical receipt store unavailable")
	}
	var entry *wavewalletrpc.WalletEntry
	switch ref.Source {
	case waved.LightningReceiptSwap:
		response, err := r.deps.SwapService.GetSwap(
			ctx, &swapclientrpc.GetSwapRequest{
				PaymentHash: ref.ID,
			},
		)
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		summary := response.GetSwap()
		if summary.GetDirection() !=
			swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE {
			return nil, nil
		}
		if summary.GetPaymentHash() != ref.ID {
			return nil, fmt.Errorf("receipt source identity " +
				"mismatch")
		}
		hash, err := hex.DecodeString(summary.GetPaymentHash())
		if err != nil {
			return nil, fmt.Errorf("invalid receipt payment "+
				"hash: %w", err)
		}
		if err := r.validateReceiptInvoice(
			summary.GetInvoice(), hash, summary.GetAmountSat(),
		); err != nil {
			return nil, err
		}
		entry = swapEntryFromSummary(
			summary, "", "",
			wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		)

	case waved.LightningReceiptCredit:
		reader, ok := r.deps.RPCServer.(creditReceiptReader)
		if !ok {
			return nil, fmt.Errorf("credit receipt lookup " +
				"unavailable")
		}
		record, err := reader.ReadCreditOperationForReceipt(ctx, ref.ID)
		if errors.Is(err, db.ErrCreditOperationNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if record.Kind != db.CreditOpKindReceive {
			return nil, nil
		}
		if record.OpID != ref.ID {
			return nil, fmt.Errorf("credit receipt identity " +
				"mismatch")
		}
		if err := r.validateReceiptInvoice(
			record.Invoice, record.PaymentHash, record.AmountSat,
		); err != nil {
			return nil, err
		}
		entry, ok = creditEntryFromSummary(credit.CreditOpSummary{
			OpID: record.OpID, Kind: credit.KindReceive,
			State:     credit.State(record.State),
			AmountSat: record.AmountSat,
			LastError: record.LastError,
		})
		if !ok {
			return nil, fmt.Errorf("credit receipt projection " +
				"unavailable")
		}
		// A delayed first projection must retain the source admission's
		// original time, rather than making historical funds look new.
		entry.CreatedAtUnix = record.CreatedAt.Unix()
		invoiceRequest := &wavewalletrpc.LightningInvoiceRequest{
			Invoice:     record.Invoice,
			PaymentHash: hex.EncodeToString(record.PaymentHash),
		}
		variant := &wavewalletrpc.WalletEntryRequest_LightningInvoice{
			LightningInvoice: invoiceRequest,
		}
		entry.Request = &wavewalletrpc.WalletEntryRequest{
			Request: variant,
		}
		entry.Progress.PaymentHash = hex.EncodeToString(
			record.PaymentHash,
		)

	default:
		return nil, fmt.Errorf("unsupported receipt source")
	}
	if entry.GetCreatedAtUnix() <= after {
		return nil, nil
	}
	var projectErr error
	if ref.Source == waved.LightningReceiptCredit {
		pending := entry.GetStatus() ==
			wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING
		_, projectErr = r.projectCreditEntry(ctx, entry, pending)
	} else {
		_, projectErr = r.projectEmitLocked(ctx, entry)
	}
	if projectErr != nil {
		return nil, projectErr
	}
	row, err := r.deps.ActivityStore.GetEntry(ctx, entry.GetId())
	if err != nil {
		return nil, err
	}
	current, err := rowToWalletEntry(row)
	if err != nil {
		return nil, err
	}
	if current.GetKind() != wavewalletrpc.EntryKind_ENTRY_KIND_RECV ||
		current.GetAmountSat() != entry.GetAmountSat() {
		return nil, fmt.Errorf("canonical receipt disagrees with " +
			"source")
	}

	state := receiptState(entry.GetStatus())
	// Source and projection reads are separate snapshots. If another owner
	// advances one between them, defer presentation until they agree.
	if entry.GetStatus() != current.GetStatus() {
		state = "pending"
	}

	return &waved.LightningReceipt{
		Reference: ref, CanonicalID: current.GetId(),
		State:         state,
		AmountSat:     entry.GetAmountSat(),
		CreatedAtUnix: entry.GetCreatedAtUnix(),
	}, nil
}

// validateReceiptInvoice binds the typed source principal to a Bitcoin invoice
// on this wallet network. Carrier values and attached-credit funding legs never
// supply an amount for this API.
func (r *Runtime) validateReceiptInvoice(encoded string, hash []byte,
	amount int64) error {

	if amount <= 0 || amount > btcutil.MaxSatoshi || len(hash) != 32 ||
		r.deps.ChainParams == nil {
		return fmt.Errorf("invalid Bitcoin receipt principal or " +
			"identity")
	}
	invoice, err := zpay32.Decode(encoded, r.deps.ChainParams)
	if err != nil {
		return fmt.Errorf("decode receipt invoice: %w", err)
	}
	if invoice.PaymentHash == nil || invoice.MilliSat == nil ||
		!bytes.Equal(invoice.PaymentHash[:], hash) ||
		*invoice.MilliSat != lnwire.MilliSatoshi(amount*1000) {
		return fmt.Errorf("receipt invoice disagrees with durable " +
			"source")
	}

	return nil
}

// receiptState keeps the presentation contract independent of protobuf enums.
func receiptState(state wavewalletrpc.EntryStatus) string {
	switch state {
	case wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE:
		return "complete"

	case wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED:
		return "failed"

	default:
		return "pending"
	}
}
