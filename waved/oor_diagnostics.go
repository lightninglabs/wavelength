package waved

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/oor"
)

// ReadOORAmount reads public transfer value without materializing PSBTs.
// Outgoing amounts use the immutable requested-recipient proof, excluding
// wallet change. Incoming amounts sum only persisted local created outputs.
// Legacy or not-yet-persisted amounts remain unknown (nil), never zero.
func (s *Server) ReadOORAmount(ctx context.Context, sessionID string,
	outgoing bool) (*int64, error) {

	id, err := chainhash.NewHashFromStr(sessionID)
	if err != nil {
		return nil, err
	}
	if outgoing {
		if s.oorSessionStore == nil {
			return nil, nil
		}
		attempt, err := s.oorSessionStore.GetDispatchAttemptBySessionID(
			ctx, *id,
		)
		if errors.Is(err, db.ErrOORDispatchAttemptNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}

		return diagnosticRequestedAmount(attempt.RequestData)
	}
	if s.db == nil {
		return nil, nil
	}
	rows, err := s.db.Queries.ListOORVTXOBindingsBySession(ctx, id[:])
	if err != nil {
		return nil, err
	}

	return diagnosticReceivedAmount(rows), nil
}

// diagnosticRequestedAmount decodes the caller's recipients, not all outputs.
func diagnosticRequestedAmount(raw []byte) (*int64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	recipients, err := oor.OutgoingReplayRecipients(raw)
	if err != nil {
		return nil, fmt.Errorf("read OOR amount: %w", err)
	}
	var total int64
	for _, recipient := range recipients {
		total += int64(recipient.Value)
	}

	return &total, nil
}

// diagnosticReceivedAmount uses local created bindings, never consumed inputs.
// A missing value leaves the whole amount unknown rather than showing a
// subtotal.
func diagnosticReceivedAmount(
	rows []sqlc.ListOORVTXOBindingsBySessionRow) *int64 {

	var total int64
	found := false
	created := int32(types.OORPackageLinkKindCreatedOutput)
	for _, row := range rows {
		if row.LinkKind != created {
			continue
		}
		if row.ValueSat <= 0 {
			return nil
		}
		found = true
		total += row.ValueSat
	}
	if !found {
		return nil
	}

	return &total
}
