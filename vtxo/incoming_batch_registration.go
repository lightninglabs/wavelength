package vtxo

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
)

// registerIncomingBatchEvidence awaits registration for every distinct
// commitment before the descriptor becomes visible to persistence or actors.
func registerIncomingBatchEvidence(ctx context.Context,
	registrar IncomingBatchRegistrar, desc *Descriptor,
	evidence []batchcanon.BatchEvidence) error {

	if len(evidence) == 0 {
		return nil
	}
	if registrar == nil {
		return fmt.Errorf("incoming batch registrar must be provided")
	}
	if desc == nil {
		return fmt.Errorf("incoming VTXO descriptor must be provided")
	}

	ancestry := make(map[chainhash.Hash]struct{}, len(desc.Ancestry))
	for _, fragment := range desc.Ancestry {
		ancestry[fragment.CommitmentTxID] = struct{}{}
	}
	seen := make(map[chainhash.Hash]struct{}, len(evidence))
	for i := range evidence {
		item := evidence[i]
		if err := item.Validate(); err != nil {
			return fmt.Errorf("incoming batch evidence %d: %w", i,
				err)
		}
		if item.CSVExpiryDelta <= 0 {
			return fmt.Errorf("incoming batch evidence %d has "+
				"non-positive CSV expiry delta", i)
		}
		if _, ok := ancestry[item.BatchTxID]; !ok {
			return fmt.Errorf("incoming batch evidence %d names "+
				"commitment %s outside ancestry", i,
				item.BatchTxID)
		}
		if _, duplicate := seen[item.BatchTxID]; duplicate {
			return fmt.Errorf("incoming batch evidence %d "+
				"duplicates commitment %s", i, item.BatchTxID)
		}
		seen[item.BatchTxID] = struct{}{}
	}
	if len(seen) != len(ancestry) {
		return fmt.Errorf("incoming batch evidence covers %d of %d "+
			"ancestry commitments", len(seen), len(ancestry))
	}

	dependent := []wire.OutPoint{desc.Outpoint}
	for i := range evidence {
		item := evidence[i]
		if err := registrar.RegisterBatch(
			ctx, item.RegisterRequest(dependent),
		); err != nil {
			return fmt.Errorf("register incoming commitment %s: %w",
				item.BatchTxID, err)
		}
	}

	return nil
}
