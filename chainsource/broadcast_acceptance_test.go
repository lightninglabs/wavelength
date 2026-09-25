package chainsource

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestChainSourceActorBroadcastTxRejectsAmbiguousSpentInput proves that a
// backend error that only identifies a spent input cannot establish that the
// submitted transaction is already known. Without read-only evidence, the
// actor must report publication failure.
func TestChainSourceActorBroadcastTxRejectsAmbiguousSpentInput(t *testing.T) {
	t.Parallel()

	backend := &broadcastErrorBackend{
		mockBackend: newMockBackend(),
		broadcastErr: errors.New(
			"output already spent by conflicting transaction",
		),
		mempoolErr: errors.New("testmempoolaccept not supported"),
	}

	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-broadcast-spent-input", chainSource,
	)

	tx := wire.NewMsgTx(2)
	future := ref.Ask(t.Context(), &BroadcastTxRequest{
		Tx:    tx,
		Label: "conflicting-spend",
	})

	result := future.Await(t.Context())
	require.True(t, result.IsErr())
	require.ErrorContains(
		t, result.Err(),
		"output already spent by conflicting transaction",
	)
}
