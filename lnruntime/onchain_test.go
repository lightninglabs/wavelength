package lnruntime

import (
	"testing"

	"github.com/lightningnetwork/lnd/chainio"
	"github.com/stretchr/testify/require"
)

// blockbeatQueueRecorder records each independent dispatcher queue.
type blockbeatQueueRecorder struct {
	queues [][]chainio.Consumer
}

// RegisterQueue implements blockbeatQueueRegistrar.
func (r *blockbeatQueueRecorder) RegisterQueue(consumers []chainio.Consumer) {
	r.queues = append(
		r.queues,
		append(
			[]chainio.Consumer(nil), consumers...,
		),
	)
}

// namedBlockbeatConsumer is sufficient to identify queue membership.
type namedBlockbeatConsumer string

// Name implements chainio.Consumer.
func (c namedBlockbeatConsumer) Name() string {
	return string(c)
}

// ProcessBlock implements chainio.Consumer.
func (namedBlockbeatConsumer) ProcessBlock(chainio.Blockbeat) error {
	return nil
}

// TestRegisterOnchainBlockConsumers verifies Ark materialization cannot delay
// sweeper and publisher delivery while preserving their ordering dependency.
func TestRegisterOnchainBlockConsumers(t *testing.T) {
	t.Parallel()

	chainArbitrator := namedBlockbeatConsumer("chain arbitrator")
	utxoSweeper := namedBlockbeatConsumer("sweeper")
	txPublisher := namedBlockbeatConsumer("publisher")
	recorder := &blockbeatQueueRecorder{}

	registerOnchainBlockConsumers(
		recorder, chainArbitrator, utxoSweeper, txPublisher,
	)

	require.Equal(t, [][]chainio.Consumer{
		{chainArbitrator},
		{utxoSweeper, txPublisher},
	}, recorder.queues)
}

var _ blockbeatQueueRegistrar = (*blockbeatQueueRecorder)(nil)
var _ chainio.Consumer = namedBlockbeatConsumer("")
