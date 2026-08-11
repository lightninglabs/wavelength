package arkchannel

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/round"
)

// RoundCompletion advances receive channels when their exact Ark round
// confirms and executes the resulting virtual-lnd activation.
type RoundCompletion struct {
	coordinator *Coordinator
	executor    ActionExecutor
}

// NewRoundCompletion constructs the confirmed-round adapter.
func NewRoundCompletion(coordinator *Coordinator,
	executor ActionExecutor) (*RoundCompletion, error) {

	if coordinator == nil {
		return nil, fmt.Errorf("channel coordinator is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("channel action executor is required")
	}

	return &RoundCompletion{
		coordinator: coordinator,
		executor:    executor,
	}, nil
}

// RoundConfirmed advances every local receive intent bound to this round.
func (c *RoundCompletion) RoundConfirmed(ctx context.Context,
	roundID round.RoundID, txID chainhash.Hash, _ round.ConfInfo) error {

	records, err := c.coordinator.ListNonTerminal(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		source := record.Snapshot.Source
		if source == nil || source.RoundID != roundID.String() ||
			source.CommitmentTxID != txID {

			continue
		}

		_, actions, err := c.coordinator.Apply(
			ctx, record.Snapshot.Terms.ID, &RoundConfirmed{
				RoundID:        roundID.String(),
				CommitmentTxID: txID,
			},
		)
		if err != nil {
			return err
		}
		for _, action := range actions {
			if err := c.executor.Execute(
				ctx, record.Snapshot.Terms.ID, action,
			); err != nil {
				return err
			}
		}
	}

	return nil
}

var _ round.CompletionObserver = (*RoundCompletion)(nil)
