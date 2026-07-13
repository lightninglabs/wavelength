package lwwallet

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/stretchr/testify/require"
)

// awaitNotification pulls the next notification from the chain
// service or fails the test on timeout.
func awaitNotification(t *testing.T, s *EsploraChainService) interface{} {
	t.Helper()

	select {
	case n := <-s.Notifications():
		return n

	case <-time.After(reorgTestTimeout):
		t.Fatalf("timed out waiting for chain notification")

		return nil
	}
}

// drainUntilConnected drains notifications until a BlockConnected
// event at the given height is observed (or the test times out).
// Used to skip over the startup ClientConnected and initial connected
// events so the reorg-specific assertions can run on a known cursor.
func drainUntilConnected(t *testing.T, s *EsploraChainService, height int32) {
	t.Helper()

	deadline := time.After(reorgTestTimeout)

	for {
		select {
		case n := <-s.Notifications():
			conn, ok := n.(chain.BlockConnected)
			if !ok {
				continue
			}
			if conn.Block.Height == height {
				return
			}

		case <-deadline:
			t.Fatalf("timed out waiting for BlockConnected at %d",
				height)
		}
	}
}

// TestEsploraChainServiceReorgEmitsBlockDisconnected pins the property
// that a chain reorg surfaces chain.BlockDisconnected notifications
// before btcwallet sees the BlockConnected events that announce the
// new canonical chain, AND that the disconnect events arrive in
// newest-height-first order (the order btcwallet's disconnectBlock
// expects when walking back from its cached tip). Both ordering
// properties are load-bearing: without disconnect-before-connect,
// btcwallet would refuse the rollback because the cached hash at
// each height would already be overwritten; without
// newest-first, btcwallet's per-step rollback could trip on a
// height it hasn't seen disconnected yet.
func TestEsploraChainServiceReorgEmitsBlockDisconnected(t *testing.T) {
	t.Parallel()

	chainModel := newFakeChain(t, 100, "svc-reorg-100")
	chainModel.extend("svc-reorg-101")
	chainModel.extend("svc-reorg-102")

	srv := fakeChainServer(t, chainModel)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	tipPoller := NewTipPoller(
		esplora, 20*time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, tipPoller.Start())
	t.Cleanup(tipPoller.Stop)

	service := NewEsploraChainService(
		esplora, tipPoller, btclog.Disabled,
	)
	require.NoError(t, service.Start(t.Context()))
	t.Cleanup(func() {
		service.Stop()
		service.WaitForShutdown()
	})

	// First notification is always ClientConnected.
	first := awaitNotification(t, service)
	_, ok := first.(chain.ClientConnected)
	require.True(t, ok, "expected ClientConnected first, got %T", first)

	// Drain forward to a known tip + a couple of new blocks so the
	// service has a known set of recently-emitted connected blocks
	// before the reorg fires.
	chainModel.extend("svc-reorg-103")
	drainUntilConnected(t, service, 103)

	// Stash hashes of the blocks the reorg is about to invalidate.
	oldHash102 := chainModel.blocks[102].hash
	oldHash103 := chainModel.blocks[103].hash

	// Rewrite heights 102..103 with a new chain. The tip stays at
	// 103 but with a different hash, so the poller detects a
	// same-height drift at 103 and walks back to fork point 101.
	chainModel.rewriteFrom(102, "svc-reorg-new")
	newHash102 := chainModel.blocks[102].hash
	newHash103 := chainModel.blocks[103].hash

	// Drain notifications and pin the strict ordering. Each
	// BlockConnected for a new-chain hash must arrive AFTER both
	// BlockDisconnected events for the old chain — the unified
	// chain stream guarantees this by serializing reorg + tip
	// events through a single producer-ordered channel.
	disconnectsSeen := 0
	gotDisconnected := []chainhash.Hash{}
	gotConnectedNew := map[chainhash.Hash]struct{}{}
	deadline := time.After(reorgTestTimeout)
	for len(gotConnectedNew) < 2 {
		select {
		case n := <-service.Notifications():
			switch ev := n.(type) {
			case chain.BlockDisconnected:
				gotDisconnected = append(
					gotDisconnected, ev.Block.Hash,
				)
				disconnectsSeen++

			case chain.BlockConnected:
				meta := wtxmgr.BlockMeta(ev)
				if meta.Hash != newHash102 &&
					meta.Hash != newHash103 {

					continue
				}

				require.Equal(
					t, 2, disconnectsSeen, "BlockConnect"+
						"ed for replacement chain "+
						"arrived before all "+
						"BlockDisconnected events: "+
						"saw %d disconnects so far",
					disconnectsSeen,
				)
				gotConnectedNew[meta.Hash] = struct{}{}
			}

		case <-deadline:
			t.Fatalf("timed out: disconnects=%v connected_new=%v",
				gotDisconnected, gotConnectedNew)
		}
	}

	require.Equal(
		t, oldHash103, gotDisconnected[0], "first "+
			"BlockDisconnected should be the newest old-chain "+
			"height (103) for btcwallet's per-step walk-back",
	)
	require.Equal(
		t, oldHash102, gotDisconnected[1],
		"second BlockDisconnected should be height 102",
	)

	_, sawConn102 := gotConnectedNew[newHash102]
	_, sawConn103 := gotConnectedNew[newHash103]
	require.True(t, sawConn102,
		"BlockConnected for new 102 not emitted")
	require.True(t, sawConn103,
		"BlockConnected for new 103 not emitted")
}
