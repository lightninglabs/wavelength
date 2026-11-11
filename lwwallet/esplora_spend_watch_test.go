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

// installSpendWatchBlock adds a single-transaction block to the synchronous
// HTTP fixture. No poller is running; tests explicitly deliver its tip event.
func installSpendWatchBlock(stub *rawBlockStubChain, height int32,
	tx *wire.MsgTx) {

	stub.mu.Lock()
	defer stub.mu.Unlock()
	block := &wire.MsgBlock{
		Header: wire.BlockHeader{
			Version: 1, PrevBlock: stub.hashAt[height-1],
			MerkleRoot: tx.TxHash(),
			Timestamp:  time.Unix(int64(height)*600, 0),
		},
		Transactions: []*wire.MsgTx{
			tx,
		},
	}
	hash := block.BlockHash()
	stub.blockAt[height] = block
	stub.hashAt[height] = hash
	stub.heightFor[hash] = height
	stub.tipHeight = height
}

// readSpendWatchBlock drains the ordered filtered/block notification pair.
func readSpendWatchBlock(t *testing.T,
	service *EsploraChainService) chain.FilteredBlockConnected {

	t.Helper()
	var filtered chain.FilteredBlockConnected
	for i := range 2 {
		select {
		case n := <-service.Notifications():
			if i == 0 {
				var ok bool
				filtered, ok = n.(chain.FilteredBlockConnected)
				require.True(t, ok)
			} else {
				_, ok := n.(chain.BlockConnected)
				require.True(t, ok)
			}

		case <-time.After(time.Second):
			t.Fatal("missing wallet block notification")
		}
	}

	return filtered
}

// TestLiveWatchesIncludeInputOnlySpends covers restored coins with no address
// watches, historical and live output discovery, negative isolation, and
// retaining spend recognition for reorg replay.
func TestLiveWatchesIncludeInputOnlySpends(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{
		"restored outpoint only", "historical output", "live output",
		"unrelated input",
	} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			stub := newRawBlockStubChain(100)
			addr, err := btcaddr.NewAddressWitnessPubKeyHash(
				make([]byte, 20), &chaincfg.RegressionNetParams,
			)
			require.NoError(t, err)
			script, err := txscript.PayToAddrScript(addr)
			require.NoError(t, err)
			funding := wire.NewMsgTx(2)
			previous := wire.OutPoint{Hash: chainhash.Hash{1}}
			funding.AddTxIn(wire.NewTxIn(&previous, nil, nil))
			funding.AddTxOut(wire.NewTxOut(50_000, script))
			coin := wire.OutPoint{Hash: funding.TxHash()}
			var addresses []btcaddr.Address
			coins := make(map[wire.OutPoint]btcaddr.Address)
			switch mode {
			case "historical output":
				installSpendWatchBlock(stub, 100, funding)
				addresses = []btcaddr.Address{addr}

			case "live output":
				addresses = []btcaddr.Address{addr}

			default:
				coins[coin] = addr
			}
			server := mockEsploraServer(t, stub.handler(t))
			service := NewEsploraChainService(
				NewEsploraClient(server.URL, btclog.Disabled),
				nil, btclog.Disabled,
			)
			t.Cleanup(func() {
				service.Stop()
				service.WaitForShutdown()
			})
			start := stub.hashFor(100)
			seedCursor(service, 100, start)
			require.NoError(
				t, service.Rescan(&start, addresses, coins),
			)
			readSpendWatchBlock(t, service)
			height := int32(101)
			if mode == "live output" {
				installSpendWatchBlock(stub, height, funding)
				service.processTipEvent(
					t.Context(), tipEventFor(stub, height),
				)
				filtered := readSpendWatchBlock(t, service)
				require.Len(t, filtered.RelevantTxs, 1)
				height++
			}
			spend := wire.NewMsgTx(2)
			input := coin
			if mode == "unrelated input" {
				input.Hash[0] ^= 1
			}
			spend.AddTxIn(wire.NewTxIn(&input, nil, nil))
			spend.AddTxOut(
				wire.NewTxOut(
					49_000, []byte{txscript.OP_TRUE},
				),
			)
			installSpendWatchBlock(stub, height, spend)
			service.processTipEvent(
				t.Context(), tipEventFor(stub, height),
			)
			filtered := readSpendWatchBlock(t, service)
			if mode == "unrelated input" {
				require.Empty(t, filtered.RelevantTxs)

				return
			}
			require.Len(t, filtered.RelevantTxs, 1)
			require.Equal(
				t, spend.TxHash(), filtered.RelevantTxs[0].Hash,
			)
			require.True(
				t, service.txMatchesLiveWatches(spend, nil),
				"spent watch must survive reorg replay",
			)
			service.mu.Lock()
			_, foreign := service.watchedOutpoints[wire.OutPoint{
				Hash: spend.TxHash(),
			}]
			service.mu.Unlock()
			require.False(
				t, foreign, "do not watch external outputs",
			)
		})
	}
}
