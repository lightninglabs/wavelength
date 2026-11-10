package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/db"
)

// LightningReceiptSource identifies the durable owner of an invoice receive.
type LightningReceiptSource string

const (
	// LightningReceiptSwap selects an ordinary persisted Lightning receive.
	LightningReceiptSwap LightningReceiptSource = "lightning_swap"

	// LightningReceiptCredit selects a sat-denominated credit receive.
	LightningReceiptCredit LightningReceiptSource = "credit_receive"
)

// LightningReceiptReference is an enrolled invoice's local source identity.
// It is never taken from a remote notification payload.
type LightningReceiptReference struct {
	Source LightningReceiptSource `json:"source"`
	ID     string                 `json:"id"`
}

// LightningReceipt records the current committed source and canonical state.
// Only state complete is eligible for amount presentation. Original source
// creation time, rather than a backfill timestamp, bounds opt-in eligibility.
type LightningReceipt struct {
	Reference     LightningReceiptReference `json:"reference"`
	CanonicalID   string                    `json:"canonical_id"`
	State         string                    `json:"state"`
	AmountSat     int64                     `json:"amount_sat"`
	CreatedAtUnix int64                     `json:"created_at_unix"`
}

// LightningReceiptRequest selects at most 64 explicitly enrolled sources.
// Creation must be strictly after CreatedAfterUnix; equality is ambiguous at
// the source stores' one-second timestamp precision and is suppressed.
type LightningReceiptRequest struct {
	References       []LightningReceiptReference `json:"references"`
	CreatedAfterUnix int64                       `json:"created_after_unix"`
}

// LightningReceiptLookup is the wallet layer's targeted normalization owner.
// A nil receipt excludes an unknown, old or unsupported source. It performs
// no full-history scan or remote payment-state mutation.
type LightningReceiptLookup func(context.Context,
	LightningReceiptReference, int64) (*LightningReceipt, error)

// RegisterLightningReceiptLookup publishes the local wallet-layer adapter.
// Registrar cleanup removes it before destroying the runtime it borrows.
func (r *RPCServer) RegisterLightningReceiptLookup(
	lookup LightningReceiptLookup) {

	r.server.lightningReceiptMu.Lock()
	defer r.server.lightningReceiptMu.Unlock()
	r.server.lightningReceiptLookup = lookup
}

// ReadCreditOperationForReceipt reads one original credit admission. This is
// an in-process adapter, not a new remote RPC or diagnostic full-list call.
func (r *RPCServer) ReadCreditOperationForReceipt(ctx context.Context,
	id string) (*db.CreditOperationRecord, error) {

	if err := r.server.WaitForWalletServicesReady(ctx); err != nil {
		return nil, err
	}
	if r.server.db == nil {
		return nil, fmt.Errorf("credit receipt store unavailable")
	}
	store := db.NewStore(
		r.server.db.DB, r.server.db.Queries, r.server.db.Backend(),
		r.server.subLogger(db.Subsystem),
	)

	return db.NewCreditOperationStore(store, r.server.clk).GetOperation(
		ctx, id,
	)
}

// ReadLightningReceipts checks a finite set of local invoice admissions. It
// does not discover arbitrary incoming transfers or establish global source
// completeness. The caller must retain the daemon until its SDK call joins.
func (s *Server) ReadLightningReceipts(ctx context.Context,
	request LightningReceiptRequest) ([]LightningReceipt, error) {

	if len(request.References) == 0 || len(request.References) > 64 ||
		request.CreatedAfterUnix < 0 {
		return nil, fmt.Errorf("receipt lookup requires 1..64 " +
			"references and a nonnegative creation boundary")
	}
	for _, ref := range request.References {
		if (ref.Source != LightningReceiptSwap &&
			ref.Source != LightningReceiptCredit) ||
			len(ref.ID) == 0 || len(ref.ID) > 128 {
			return nil, fmt.Errorf("invalid local receipt " +
				"reference")
		}
	}
	if err := s.WaitForWalletServicesReady(ctx); err != nil {
		return nil, err
	}
	s.lightningReceiptMu.RLock()
	defer s.lightningReceiptMu.RUnlock()
	if s.lightningReceiptLookup == nil {
		return nil, fmt.Errorf("lightning receipt lookup unavailable")
	}
	result := make([]LightningReceipt, 0, len(request.References))
	for _, ref := range request.References {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		receipt, err := s.lightningReceiptLookup(
			ctx, ref, request.CreatedAfterUnix,
		)
		if err != nil {
			return nil, err
		}
		if receipt != nil {
			result = append(result, *receipt)
		}
	}

	return result, nil
}
