package chainsource

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// awaitTimeout is the per-step wait used by the reorg lifecycle tests. It
// is generous enough to absorb scheduling jitter on overloaded CI machines
// but still short enough that a hung actor surfaces as a fast failure.
const awaitTimeout = 5 * time.Second

// TestConfActorReorgAwareForwardsFullLifecycle drives the full
// Confirmed -> Reorged -> Confirmed -> Done sequence through a reorg-aware
// ConfActor and asserts each event is forwarded to the correct notify ref
// in order, and that the actor releases the backend registration after
// Done.
func TestConfActorReorgAwareForwardsFullLifecycle(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	txHash := chainhash.Hash{0x01}
	confNotifier := actor.NewChannelTellOnlyRef[ConfirmationEvent](
		"conf-notify", 10,
	)
	reorgNotifier := actor.NewChannelTellOnlyRef[ConfReorgedEvent](
		"conf-reorged", 10,
	)
	doneNotifier := actor.NewChannelTellOnlyRef[ConfDoneEvent](
		"conf-done", 10,
	)

	var confRef actor.TellOnlyRef[ConfirmationEvent] = confNotifier
	var reorgRef actor.TellOnlyRef[ConfReorgedEvent] = reorgNotifier
	var doneRef actor.TellOnlyRef[ConfDoneEvent] = doneNotifier

	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:      "test-conf-reorg-lifecycle",
		Txid:          &txHash,
		PkScript:      []byte{0x00, 0x14},
		TargetConfs:   1,
		NotifyActor:   fn.Some(confRef),
		NotifyReorged: fn.Some(reorgRef),
		NotifyDone:    fn.Some(doneRef),
	})
	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	confResp, ok := resp.(*RegisterConfResponse)
	require.True(t, ok)

	// Reorg-aware mode is actor-only, so the response must not carry a
	// Future.
	require.Nil(t, confResp.Future)

	// 1. First confirmation on the canonical chain.
	blockHash1 := chainhash.Hash{0xaa}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash1,
		BlockHeight: 100,
		Tx:          wire.NewMsgTx(2),
	}

	event1, ok := confNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for first ConfirmationEvent")
	require.Equal(t, int32(100), event1.BlockHeight)
	require.Equal(t, blockHash1, event1.BlockHash)

	// 2. Reorg evicts that confirmation.
	backend.confReorgedChan <- uint64(0)

	reorgEvt, ok := reorgNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for ConfReorgedEvent")
	require.Equal(t, txHash, reorgEvt.Txid)

	// 3. Transaction re-confirms in a different block on the new tip.
	blockHash2 := chainhash.Hash{0xbb}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash2,
		BlockHeight: 101,
		Tx:          wire.NewMsgTx(2),
	}

	event2, ok := confNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for re-ConfirmationEvent")
	require.Equal(t, int32(101), event2.BlockHeight)
	require.Equal(t, blockHash2, event2.BlockHash)

	// 4. Registration matures past reorg safety; backend fires Done.
	backend.confDoneChan <- struct{}{}

	doneEvt, ok := doneNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for ConfDoneEvent")
	require.Equal(t, txHash, doneEvt.Txid)

	// After Done the actor must release the registration.
	require.Eventually(t, func() bool {
		return backend.confCancelled.Load() >= 1
	}, awaitTimeout, 10*time.Millisecond,
		"registration Cancel was never invoked after Done")
}

// TestConfActorSynthesizesDoneFromFinalityDepth verifies that a
// reorg-aware ConfActor with a non-zero FinalityDepth fires
// ConfDoneEvent on its own once enough blocks have been observed past
// the first Confirmed event, even when the backend's Done channel
// never fires. This closes the lndclient gRPC gap, where lnd's
// internal "past reorg-safety depth" signal does not survive the
// transport.
func TestConfActorSynthesizesDoneFromFinalityDepth(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	const finalityDepth = 6
	confActor := NewConfActor(ConfActorConfig{
		Backend:       backend,
		FinalityDepth: finalityDepth,
	})
	defer confActor.Stop()

	txHash := chainhash.Hash{0x01}
	confNotifier := actor.NewChannelTellOnlyRef[ConfirmationEvent](
		"conf-notify", 10,
	)
	reorgNotifier := actor.NewChannelTellOnlyRef[ConfReorgedEvent](
		"conf-reorged", 10,
	)
	doneNotifier := actor.NewChannelTellOnlyRef[ConfDoneEvent](
		"conf-done", 10,
	)

	var confRef actor.TellOnlyRef[ConfirmationEvent] = confNotifier
	var reorgRef actor.TellOnlyRef[ConfReorgedEvent] = reorgNotifier
	var doneRef actor.TellOnlyRef[ConfDoneEvent] = doneNotifier

	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:      "test-conf-finality-depth",
		Txid:          &txHash,
		PkScript:      []byte{0x00, 0x14},
		TargetConfs:   1,
		NotifyActor:   fn.Some(confRef),
		NotifyReorged: fn.Some(reorgRef),
		NotifyDone:    fn.Some(doneRef),
	})
	require.True(t, result.IsOk())

	// 1. First confirmation at height 100. This arms the height-based
	// finality synthesizer.
	blockHash1 := chainhash.Hash{0xaa}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash1,
		BlockHeight: 100,
		Tx:          wire.NewMsgTx(2),
	}
	_, ok := confNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for ConfirmationEvent")

	// 2. Push blocks up to height 104. Inclusive depth at that point
	// is 5 (heights 100..104), one short of the finality threshold,
	// so ConfDoneEvent MUST NOT have fired yet.
	for height := int32(101); height <=
		int32(100+finalityDepth-2); height++ {

		backend.epochChan <- &BlockEpoch{Height: height}
	}
	_, ok = doneNotifier.AwaitMessage(50 * time.Millisecond)
	require.False(t, ok, "ConfDoneEvent fired before finality depth")

	// 3. One more block brings the inclusive depth to exactly
	// FinalityDepth (heights 100..105). Done must fire now.
	backend.epochChan <- &BlockEpoch{
		Height: 100 + int32(finalityDepth) - 1,
	}
	doneEvt, ok := doneNotifier.AwaitMessage(awaitTimeout)
	require.True(
		t, ok,
		"ConfDoneEvent never fired despite reaching finality depth",
	)
	require.Equal(t, txHash, doneEvt.Txid)

	// 4. After Done the actor exits and releases the registration.
	require.Eventually(t, func() bool {
		return backend.confCancelled.Load() >= 1
	}, awaitTimeout, 10*time.Millisecond,
		"registration Cancel was never invoked after synthesized "+
			"Done")
}

// TestConfActorDiscardsStaleReorgBySeq verifies that a reorg signal which
// lost a cross-channel race to a newer re-confirmation is discarded by
// sequence number. The re-confirmation (seq 3) is observed before the
// older reorg (seq 2); because Confirmed and Reorged arrive on separate
// channels the actor cannot order them by arrival, so it must order them
// by Seq and ignore the stale reorg. The proof is that height-based
// finality still fires against the re-confirmation height — if the stale
// reorg had been applied it would have reset confirmHeight to 0 and Done
// would never synthesize.
func TestConfActorDiscardsStaleReorgBySeq(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	const finalityDepth = 6
	confActor := NewConfActor(ConfActorConfig{
		Backend:       backend,
		FinalityDepth: finalityDepth,
	})
	defer confActor.Stop()

	txHash := chainhash.Hash{0x04}
	confNotifier := actor.NewChannelTellOnlyRef[ConfirmationEvent](
		"conf-notify", 10,
	)
	reorgNotifier := actor.NewChannelTellOnlyRef[ConfReorgedEvent](
		"conf-reorged", 10,
	)
	doneNotifier := actor.NewChannelTellOnlyRef[ConfDoneEvent](
		"conf-done", 10,
	)

	var confRef actor.TellOnlyRef[ConfirmationEvent] = confNotifier
	var reorgRef actor.TellOnlyRef[ConfReorgedEvent] = reorgNotifier
	var doneRef actor.TellOnlyRef[ConfDoneEvent] = doneNotifier

	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:      "test-conf-stale-reorg-seq",
		Txid:          &txHash,
		PkScript:      []byte{0x00, 0x14},
		TargetConfs:   1,
		NotifyActor:   fn.Some(confRef),
		NotifyReorged: fn.Some(reorgRef),
		NotifyDone:    fn.Some(doneRef),
	})
	require.True(t, result.IsOk())

	// 1. First confirmation at height 100, seq 1.
	blockHash1 := chainhash.Hash{0xaa}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash1,
		BlockHeight: 100,
		Tx:          wire.NewMsgTx(2),
		Seq:         1,
	}
	_, ok := confNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for first ConfirmationEvent")

	// 2. The re-confirmation (seq 3) is delivered before the older reorg
	// (seq 2) — the cross-channel race. It arms finality at height 101.
	blockHash2 := chainhash.Hash{0xbb}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash2,
		BlockHeight: 101,
		Tx:          wire.NewMsgTx(2),
		Seq:         3,
	}
	_, ok = confNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for re-ConfirmationEvent")

	// 3. The stale reorg (seq 2 <= 3) arrives late and must be discarded:
	// no ConfReorgedEvent is delivered and confirmHeight is untouched.
	backend.confReorgedChan <- uint64(2)
	_, ok = reorgNotifier.AwaitMessage(50 * time.Millisecond)
	require.False(t, ok, "stale reorg (seq 2) was not discarded")

	// 4. Drive blocks to the finality depth past height 101. Done must
	// fire, proving confirmHeight survived the stale reorg.
	for height := int32(102); height <=
		int32(101+finalityDepth)-1; height++ {

		backend.epochChan <- &BlockEpoch{Height: height}
	}
	doneEvt, ok := doneNotifier.AwaitMessage(awaitTimeout)
	require.True(
		t, ok, "ConfDoneEvent never fired; stale reorg wrongly "+
			"reset confirmHeight",
	)
	require.Equal(t, txHash, doneEvt.Txid)
}

// TestConfActorDiscardsStaleConfirmBySeq verifies the opposite race: a
// re-confirmation that lost a cross-channel race to a newer reorg is
// discarded by sequence number. The reorg (seq 3) is observed before the
// stale confirmation (seq 2); the actor must ignore the confirmation and
// leave the watch unconfirmed, so height-based finality must NOT fire.
// Without sequence ordering the stale confirmation would set a non-zero
// confirmHeight and synthesize a false Done for a tx that is gone.
func TestConfActorDiscardsStaleConfirmBySeq(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	const finalityDepth = 6
	confActor := NewConfActor(ConfActorConfig{
		Backend:       backend,
		FinalityDepth: finalityDepth,
	})
	defer confActor.Stop()

	txHash := chainhash.Hash{0x05}
	confNotifier := actor.NewChannelTellOnlyRef[ConfirmationEvent](
		"conf-notify", 10,
	)
	reorgNotifier := actor.NewChannelTellOnlyRef[ConfReorgedEvent](
		"conf-reorged", 10,
	)
	doneNotifier := actor.NewChannelTellOnlyRef[ConfDoneEvent](
		"conf-done", 10,
	)

	var confRef actor.TellOnlyRef[ConfirmationEvent] = confNotifier
	var reorgRef actor.TellOnlyRef[ConfReorgedEvent] = reorgNotifier
	var doneRef actor.TellOnlyRef[ConfDoneEvent] = doneNotifier

	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:      "test-conf-stale-confirm-seq",
		Txid:          &txHash,
		PkScript:      []byte{0x00, 0x14},
		TargetConfs:   1,
		NotifyActor:   fn.Some(confRef),
		NotifyReorged: fn.Some(reorgRef),
		NotifyDone:    fn.Some(doneRef),
	})
	require.True(t, result.IsOk())

	// 1. First confirmation at height 100, seq 1.
	blockHash1 := chainhash.Hash{0xaa}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash1,
		BlockHeight: 100,
		Tx:          wire.NewMsgTx(2),
		Seq:         1,
	}
	_, ok := confNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for first ConfirmationEvent")

	// 2. The newer reorg (seq 3) is observed first and resets the watch.
	backend.confReorgedChan <- uint64(3)
	_, ok = reorgNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for ConfReorgedEvent")

	// 3. The stale re-confirmation (seq 2 <= 3) arrives late and must be
	// discarded: no ConfirmationEvent is delivered and the watch stays
	// unconfirmed.
	blockHash2 := chainhash.Hash{0xbb}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash2,
		BlockHeight: 101,
		Tx:          wire.NewMsgTx(2),
		Seq:         2,
	}
	_, ok = confNotifier.AwaitMessage(50 * time.Millisecond)
	require.False(t, ok, "stale confirmation (seq 2) was not discarded")

	// 4. Drive many blocks well past any finality window. Done must NOT
	// fire because confirmHeight was never re-armed.
	for height := int32(101); height <= int32(120); height++ {
		backend.epochChan <- &BlockEpoch{Height: height}
	}
	_, ok = doneNotifier.AwaitMessage(100 * time.Millisecond)
	require.False(
		t, ok, "ConfDoneEvent fired for a reorged-out tx; stale "+
			"confirmation was wrongly applied",
	)
}

// TestConfActorReorgAwareRejectsWithoutNotifyActor verifies that opting in
// to reorg-aware mode without an actor-mode confirmation ref is rejected at
// admission. Allowing it would silently drop every re-confirmation after
// the first, since a Future can only complete once.
func TestConfActorReorgAwareRejectsWithoutNotifyActor(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	txHash := chainhash.Hash{0x02}
	reorgNotifier := actor.NewChannelTellOnlyRef[ConfReorgedEvent](
		"conf-reorged", 1,
	)
	var reorgRef actor.TellOnlyRef[ConfReorgedEvent] = reorgNotifier

	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:      "test-conf-reorg-no-notify",
		Txid:          &txHash,
		PkScript:      []byte{0x00, 0x14},
		TargetConfs:   1,
		NotifyReorged: fn.Some(reorgRef),
	})
	require.True(t, result.IsErr())
	_, err := result.Unpack()
	require.ErrorContains(
		t, err,
		"reorg/done notifications require actor-mode NotifyActor",
	)
}

// TestConfActorLegacyExitsAfterFirstConfirm ensures legacy Actor-mode
// subscribers (no NotifyReorged / NotifyDone) keep their historical
// single-shot contract even when the backend would later emit Reorged or
// Done. The actor must cancel the registration after the first
// confirmation.
func TestConfActorLegacyExitsAfterFirstConfirm(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	txHash := chainhash.Hash{0x03}
	notifier := actor.NewChannelTellOnlyRef[ConfirmationEvent](
		"conf-notify", 10,
	)
	var confRef actor.TellOnlyRef[ConfirmationEvent] = notifier

	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:    "test-conf-legacy-single-shot",
		Txid:        &txHash,
		PkScript:    []byte{0x00, 0x14},
		TargetConfs: 1,
		NotifyActor: fn.Some(confRef),
	})
	require.True(t, result.IsOk())

	// First confirmation goes through.
	blockHash := chainhash.Hash{0x10}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash,
		BlockHeight: 200,
		Tx:          wire.NewMsgTx(2),
	}

	event, ok := notifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for first ConfirmationEvent")
	require.Equal(t, int32(200), event.BlockHeight)

	// Actor must have exited after the first confirmation, releasing the
	// registration.
	require.Eventually(t, func() bool {
		return backend.confCancelled.Load() >= 1
	}, awaitTimeout, 10*time.Millisecond,
		"legacy ConfActor did not cancel registration after first "+
			"confirmation")

	// No further events should reach the notifier.
	_, ok = notifier.AwaitMessage(50 * time.Millisecond)
	require.False(
		t, ok, "legacy ConfActor delivered an unexpected second event",
	)
}

// TestSpendActorReorgAwareForwardsFullLifecycle drives the spend lifecycle
// (Spend -> Reorged -> Spend -> Done) through a reorg-aware SpendActor and
// asserts every event is forwarded to the correct notify ref in order.
func TestSpendActorReorgAwareForwardsFullLifecycle(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	outpoint := wire.OutPoint{Hash: chainhash.Hash{0x11}, Index: 0}

	spendNotifier := actor.NewChannelTellOnlyRef[SpendEvent](
		"spend-notify", 10,
	)
	reorgNotifier := actor.NewChannelTellOnlyRef[SpendReorgedEvent](
		"spend-reorged", 10,
	)
	doneNotifier := actor.NewChannelTellOnlyRef[SpendDoneEvent](
		"spend-done", 10,
	)

	var spendRef actor.TellOnlyRef[SpendEvent] = spendNotifier
	var reorgRef actor.TellOnlyRef[SpendReorgedEvent] = reorgNotifier
	var doneRef actor.TellOnlyRef[SpendDoneEvent] = doneNotifier

	result := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID:      "test-spend-reorg-lifecycle",
		Outpoint:      &outpoint,
		PkScript:      []byte{0x00, 0x14},
		NotifyActor:   fn.Some(spendRef),
		NotifyReorged: fn.Some(reorgRef),
		NotifyDone:    fn.Some(doneRef),
	})
	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	spendResp, ok := resp.(*RegisterSpendResponse)
	require.True(t, ok)
	require.Nil(t, spendResp.Future)

	// 1. First spend confirms.
	spendingTx1 := wire.NewMsgTx(2)
	hash1 := spendingTx1.TxHash()
	backend.spendChan <- &SpendDetail{
		SpentOutPoint:  &outpoint,
		SpenderTxHash:  &hash1,
		SpendingTx:     spendingTx1,
		SpendingHeight: 150,
	}

	spend1, ok := spendNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for first SpendEvent")
	require.Equal(t, outpoint, spend1.Outpoint)
	require.Equal(t, int32(150), spend1.SpendingHeight)
	require.Equal(t, hash1, spend1.SpendingTxid)

	// 2. Reorg evicts that spend.
	backend.spendReorgedChan <- uint64(0)

	reorgEvt, ok := reorgNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for SpendReorgedEvent")
	require.Equal(t, outpoint, reorgEvt.Outpoint)

	// 3. A different spender wins the new chain.
	spendingTx2 := wire.NewMsgTx(2)
	spendingTx2.AddTxIn(&wire.TxIn{Sequence: 1})
	hash2 := spendingTx2.TxHash()
	require.NotEqual(t, hash1, hash2)
	backend.spendChan <- &SpendDetail{
		SpentOutPoint:  &outpoint,
		SpenderTxHash:  &hash2,
		SpendingTx:     spendingTx2,
		SpendingHeight: 151,
	}

	spend2, ok := spendNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for re-SpendEvent")
	require.Equal(t, outpoint, spend2.Outpoint)
	require.Equal(t, int32(151), spend2.SpendingHeight)
	require.Equal(t, hash2, spend2.SpendingTxid)

	// 4. Registration matures past reorg safety.
	backend.spendDoneChan <- struct{}{}

	doneEvt, ok := doneNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for SpendDoneEvent")
	require.Equal(t, outpoint, doneEvt.Outpoint)

	require.Eventually(t, func() bool {
		return backend.spendCancelled.Load() >= 1
	}, awaitTimeout, 10*time.Millisecond,
		"registration Cancel was never invoked after Done")
}

// TestSpendActorSynthesizesDoneFromFinalityDepth mirrors the conf-side
// height-based finality test for the spend watch. A reorg-aware
// SpendActor with non-zero FinalityDepth fires SpendDoneEvent on its
// own once enough blocks have been observed past the first Spend,
// closing the same lndclient gRPC gap that ConfActor closes for
// confirmations.
func TestSpendActorSynthesizesDoneFromFinalityDepth(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	const finalityDepth = 6
	spendActor := NewSpendActor(SpendActorConfig{
		Backend:       backend,
		FinalityDepth: finalityDepth,
	})
	defer spendActor.Stop()

	outpoint := wire.OutPoint{Hash: chainhash.Hash{0x11}, Index: 0}

	spendNotifier := actor.NewChannelTellOnlyRef[SpendEvent](
		"spend-notify", 10,
	)
	reorgNotifier := actor.NewChannelTellOnlyRef[SpendReorgedEvent](
		"spend-reorged", 10,
	)
	doneNotifier := actor.NewChannelTellOnlyRef[SpendDoneEvent](
		"spend-done", 10,
	)

	var spendRef actor.TellOnlyRef[SpendEvent] = spendNotifier
	var reorgRef actor.TellOnlyRef[SpendReorgedEvent] = reorgNotifier
	var doneRef actor.TellOnlyRef[SpendDoneEvent] = doneNotifier

	result := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID:      "test-spend-finality-depth",
		Outpoint:      &outpoint,
		PkScript:      []byte{0x00, 0x14},
		NotifyActor:   fn.Some(spendRef),
		NotifyReorged: fn.Some(reorgRef),
		NotifyDone:    fn.Some(doneRef),
	})
	require.True(t, result.IsOk())

	// 1. First spend confirms at height 150. This arms the
	// height-based finality synthesizer.
	spendingTx := wire.NewMsgTx(2)
	hash := spendingTx.TxHash()
	backend.spendChan <- &SpendDetail{
		SpentOutPoint:  &outpoint,
		SpenderTxHash:  &hash,
		SpendingTx:     spendingTx,
		SpendingHeight: 150,
	}
	_, ok := spendNotifier.AwaitMessage(awaitTimeout)
	require.True(t, ok, "timeout waiting for SpendEvent")

	// 2. Push blocks up to height 154. Inclusive depth at that point
	// is 5 (heights 150..154), one short of the finality threshold.
	// SpendDoneEvent MUST NOT have fired yet.
	for height := int32(151); height <=
		int32(150+finalityDepth-2); height++ {

		backend.epochChan <- &BlockEpoch{Height: height}
	}
	_, ok = doneNotifier.AwaitMessage(50 * time.Millisecond)
	require.False(t, ok, "SpendDoneEvent fired before finality depth")

	// 3. One more block brings the inclusive depth to exactly
	// FinalityDepth (heights 150..155). Done must fire now.
	backend.epochChan <- &BlockEpoch{
		Height: 150 + int32(finalityDepth) - 1,
	}
	doneEvt, ok := doneNotifier.AwaitMessage(awaitTimeout)
	require.True(
		t, ok,
		"SpendDoneEvent never fired despite reaching finality depth",
	)
	require.Equal(t, outpoint, doneEvt.Outpoint)

	require.Eventually(t, func() bool {
		return backend.spendCancelled.Load() >= 1
	}, awaitTimeout, 10*time.Millisecond,
		"registration Cancel was never invoked after synthesized "+
			"Done")
}

// TestSpendActorReorgAwareRejectsWithoutNotifyActor mirrors the conf-side
// admission check: opting in to reorg-aware spend forwarding without a
// NotifyActor must be rejected.
func TestSpendActorReorgAwareRejectsWithoutNotifyActor(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	outpoint := wire.OutPoint{Hash: chainhash.Hash{0x21}, Index: 0}
	reorgNotifier := actor.NewChannelTellOnlyRef[SpendReorgedEvent](
		"spend-reorged", 1,
	)
	var reorgRef actor.TellOnlyRef[SpendReorgedEvent] = reorgNotifier

	result := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID:      "test-spend-reorg-no-notify",
		Outpoint:      &outpoint,
		PkScript:      []byte{0x00, 0x14},
		NotifyReorged: fn.Some(reorgRef),
	})
	require.True(t, result.IsErr())
	_, err := result.Unpack()
	require.ErrorContains(
		t, err,
		"reorg/done notifications require actor-mode NotifyActor",
	)
}

// TestSpendActorLegacyExitsAfterFirstSpend exercises the legacy Actor-mode
// path: a watch that did not opt into the reorg lifecycle
// (NotifyReorged/NotifyDone both unset) is single-shot, exiting and
// releasing its backend registration after the first spend. This mirrors
// ConfActor's legacy contract and the documented invariant in CLAUDE.md;
// without the reorgAware gate such a watch would run forever and, with
// FinalityDepth > 0, arm a block subscription it never requested.
func TestSpendActorLegacyExitsAfterFirstSpend(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	outpoint := wire.OutPoint{Hash: chainhash.Hash{0x31}, Index: 0}
	notifier := actor.NewChannelTellOnlyRef[SpendEvent](
		"spend-notify", 10,
	)
	var spendRef actor.TellOnlyRef[SpendEvent] = notifier

	result := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID:    "test-spend-legacy-single-shot",
		Outpoint:    &outpoint,
		PkScript:    []byte{0x00, 0x14},
		NotifyActor: fn.Some(spendRef),
	})
	require.True(t, result.IsOk())

	// First spend goes through.
	spendingTx := wire.NewMsgTx(2)
	hash := spendingTx.TxHash()
	backend.spendChan <- &SpendDetail{
		SpentOutPoint:  &outpoint,
		SpenderTxHash:  &hash,
		SpendingTx:     spendingTx,
		SpendingHeight: 300,
	}

	first, ok := notifier.AwaitMessage(awaitTimeout)
	require.True(t, ok)
	require.Equal(t, int32(300), first.SpendingHeight)

	// The actor must have exited after the first spend, releasing the
	// registration.
	require.Eventually(t, func() bool {
		return backend.spendCancelled.Load() >= 1
	}, awaitTimeout, 10*time.Millisecond,
		"legacy SpendActor did not cancel registration after first "+
			"spend")

	// No further events should reach the notifier.
	_, ok = notifier.AwaitMessage(50 * time.Millisecond)
	require.False(
		t, ok, "legacy SpendActor delivered an unexpected second event",
	)
}
