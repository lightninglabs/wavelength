package lwwallet

import (
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/stretchr/testify/require"
)

// TestRescanRetainsLiveChangeWatch models restart importing an existing fee
// address and change address only through Rescan. A later spend must reach
// btcwallet so it can remove the old credit and add confirmed change.
func TestRescanRetainsLiveChangeWatch(t *testing.T) {
	t.Parallel()
	stub := newRawBlockStubChain(100)
	server := mockEsploraServer(t, stub.handler(t))
	service := NewEsploraChainService(
		NewEsploraClient(server.URL, btclog.Disabled), nil,
		btclog.Disabled,
	)
	t.Cleanup(func() {
		service.Stop()
		service.WaitForShutdown()
	})
	seedCursor(service, 100, stub.hashFor(100))
	addresses := make([]btcaddr.Address, 2)
	for i := range addresses {
		var hash [20]byte
		hash[0] = byte(i + 1)
		addr, err := btcaddr.NewAddressWitnessPubKeyHash(
			hash[:], &chaincfg.RegressionNetParams,
		)
		require.NoError(t, err)
		addresses[i] = addr
	}
	start := stub.hashFor(100)
	require.NoError(t, service.Rescan(&start, addresses, nil))

	// Drain the historical scan before driving the next live block.
	for range 2 {
		select {
		case <-service.Notifications():
		case <-time.After(time.Second):
			t.Fatal("missing historical rescan notification")
		}
	}
	script, err := txscript.PayToAddrScript(addresses[1])
	require.NoError(t, err)
	funding := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
	spend := wire.NewMsgTx(2)
	spend.AddTxIn(wire.NewTxIn(&funding, nil, nil))
	spend.AddTxOut(wire.NewTxOut(49_000, script))
	block := &wire.MsgBlock{
		Header: wire.BlockHeader{
			Version: 1, PrevBlock: start,
			MerkleRoot: spend.TxHash(),
			Timestamp:  time.Unix(101*600, 0),
		},
		Transactions: []*wire.MsgTx{
			spend,
		},
	}
	hash := block.BlockHash()
	stub.mu.Lock()
	stub.blockAt[101] = block
	stub.hashAt[101] = hash
	stub.heightFor[hash] = 101
	stub.tipHeight = 101
	stub.mu.Unlock()
	service.processTipEvent(t.Context(), tipEventFor(stub, 101))
	select {
	case notification := <-service.Notifications():
		filtered, ok := notification.(chain.FilteredBlockConnected)
		require.True(t, ok)
		require.EqualValues(t, 101, filtered.Block.Height)
		require.Len(t, filtered.RelevantTxs, 1)
		require.Equal(t, spend.TxHash(), filtered.RelevantTxs[0].Hash)
		require.Equal(
			t, funding,
			filtered.RelevantTxs[0].MsgTx.TxIn[0].PreviousOutPoint,
		)

	case <-time.After(time.Second):
		t.Fatal("rescan change address missing from live notifications")
	}
}
