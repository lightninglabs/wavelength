//go:build itest

package harness_test

import (
	"database/sql"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	_ "github.com/lib/pq"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/stretchr/testify/require"
)

var (
	// testParallelism controls how many harness tests can run in parallel.
	// Each harness test spawns multiple Docker containers (bitcoind, lnd,
	// electrs, etc.) which can be resource-intensive.
	testParallelism = flag.Int(
		"test.parallelism", 4,
		"maximum number of harness tests to run in parallel",
	)

	// testParallelismSem is a semaphore channel to limit parallel test
	// execution based on testParallelism flag.
	testParallelismSem chan struct{}
)

// ParallelN acquires a slot from the parallelism semaphore and marks the test
// as parallel. This allows controlling how many resource-intensive harness
// tests run concurrently via the -test.parallelism flag.
func ParallelN(t *testing.T) {
	t.Helper()

	t.Parallel()

	testParallelismSem <- struct{}{}
	t.Cleanup(func() {
		<-testParallelismSem
	})
}

// TestMain is the entry point for the test suite and handles flag parsing.
func TestMain(m *testing.M) {
	// Parse flags and initialize the parallelism semaphore.
	flag.Parse()
	testParallelismSem = make(chan struct{}, *testParallelism)

	// Run tests.
	os.Exit(m.Run())
}

// TestHarnessStartup tests that we can start up the harness (which includes
// bitcoind, LND and tapd) and create a new TapdHarness for a user with a
// separate lnd and tapd instance.
func TestHarnessStartup(t *testing.T) {
	ParallelN(t)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()
	opts.StartTapd = true

	h := harness.NewHarness(t, &opts)

	t.Cleanup(func() {
		h.Stop()
	})

	h.Start()
	t.Logf("LND gRPC: %s, bitcoind RPC: %s", h.LNDGRPCPort, h.BitcoindRPC)

	// Start a TapdHarness for Alice to verify tapd infrastructure works.
	t.Log("Starting TapdHarness for alice...")
	alice := h.NewTapdHarness("alice")
	t.Cleanup(func() {
		alice.Stop()
	})

	t.Logf(
		"Alice LND gRPC: %s, Alice tapd gRPC: %s", alice.LNDGRPCPort,
		alice.TapdGRPCPort,
	)
}

// TestHarnessMining tests block generation functionality including Generate
// and GenerateAndWait methods.
func TestHarnessMining(t *testing.T) {
	ParallelN(t)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()

	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()

	// Test Generate() produces the expected number of blocks.
	initialHeight := h.BlockCount()
	t.Logf("Initial block height: %d", initialHeight)

	const numBlocks = 10
	headers := h.Generate(numBlocks)
	require.Len(
		t, headers, numBlocks,
		"Generate should return correct number of block headers",
	)

	newHeight := h.BlockCount()
	require.Equal(
		t, initialHeight+uint32(numBlocks), newHeight,
		"Block height should increase by number of mined blocks",
	)

	// Test GenerateAndWait() returns block headers with txids.
	blocks := h.GenerateAndWait(5)
	require.Len(t, blocks, 5, "GenerateAndWait should return 5 blocks")

	for i, block := range blocks {
		require.NotEmpty(
			t, block.Header.Hash, "Block %d should have hash", i,
		)
		require.NotEmpty(
			t, block.TxIDs, "Block %d should have at least "+
				"coinbase tx", i,
		)
		t.Logf(
			"Block %d: hash=%s, txCount=%d", i, block.Header.Hash,
			len(block.TxIDs),
		)
	}

	finalHeight := h.BlockCount()
	require.Equal(
		t, newHeight+5, finalHeight,
		"Block height should increase by 5 after GenerateAndWait",
	)
}

// TestHarnessReorg verifies the harness can invalidate the active chain tip,
// mine a longer replacement branch, and leave bitcoind on the new branch.
func TestHarnessReorg(t *testing.T) {
	ParallelN(t)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()

	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()

	h.Generate(3)
	oldTip := h.BestBlockHeader()

	reorg := h.ReorgDepth(2)
	require.Equal(t, oldTip.Hash, reorg.OldTip.Hash)
	require.Equal(t, oldTip.Height-2, reorg.ForkPoint.Height)
	require.Len(t, reorg.Disconnected, 2)
	require.Len(t, reorg.Connected, 3)
	require.Equal(t, oldTip.Hash, reorg.Disconnected[1].Hash)

	newTip := h.BestBlockHeader()
	require.Equal(
		t, reorg.Connected[len(reorg.Connected)-1].Hash, newTip.Hash,
	)
	require.Equal(t, oldTip.Height+1, newTip.Height)
	require.NotEqual(t, oldTip.Hash, newTip.Hash)

	replacedHashAtOldHeight := h.BlockHash(oldTip.Height)
	require.NotEqual(t, oldTip.Hash, replacedHashAtOldHeight)

	h.ReconsiderBlock(reorg.Disconnected[0].Hash)
}

// TestHarnessFaucet tests wallet funding functionality using the Faucet method.
func TestHarnessFaucet(t *testing.T) {
	ParallelN(t)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()

	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()

	// Get bitcoin RPC client to generate a new address.
	client, err := h.BitcoinRPCClient()
	require.NoError(t, err, "Should create RPC client")
	defer client.Shutdown()

	addr, err := client.GetNewAddress("")
	require.NoError(t, err, "GetNewAddress should succeed")
	t.Logf("Generated address: %s", addr.String())

	// Fund the address using the faucet.
	const amount = btcutil.Amount(1_000_000)
	txID := h.Faucet(addr.String(), amount)
	require.NotEmpty(t, txID, "Faucet should return txid")
	t.Logf("Faucet txid: %s", txID)

	// Verify the transaction is in the mempool.
	txIDs := h.MempoolTxIDs()
	require.Contains(t, txIDs, txID,
		"Faucet tx should be in mempool")

	// Mine a block to confirm the transaction.
	h.Generate(1)

	// Verify the transaction is no longer in the mempool.
	txIDs = h.MempoolTxIDs()
	require.NotContains(
		t, txIDs, txID, "Confirmed tx should not be in mempool",
	)
}

// TestHarnessMempool tests mempool inspection functionality including
// MempoolTxIDs and WaitMempoolTxCount.
func TestHarnessMempool(t *testing.T) {
	ParallelN(t)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()

	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()

	// Initially mempool should be empty or nearly empty.
	initialTxIDs := h.MempoolTxIDs()
	t.Logf("Initial mempool size: %d", len(initialTxIDs))

	// Get bitcoin RPC client to generate addresses.
	client, err := h.BitcoinRPCClient()
	require.NoError(t, err, "Should create RPC client")
	defer client.Shutdown()

	// Create some transactions by funding addresses.
	const numTxs = 3
	for i := range numTxs {
		addr, err := client.GetNewAddress("")
		require.NoError(t, err, "GetNewAddress should succeed")

		const amount = btcutil.Amount(100_000)
		txID := h.Faucet(addr.String(), amount)
		t.Logf("Created tx %d: %s", i+1, txID)
	}

	// Wait for all transactions to appear in mempool.
	expectedCount := len(initialTxIDs) + numTxs
	txIDs := h.WaitMempoolTxCount(expectedCount)
	require.GreaterOrEqual(
		t, len(txIDs), expectedCount, "Mempool should contain at "+
			"least %d txs", expectedCount,
	)
	t.Logf("Mempool now contains %d transactions", len(txIDs))

	// Mine blocks to clear the mempool.
	h.Generate(1)
	clearedTxIDs := h.MempoolTxIDs()
	t.Logf("Mempool after mining: %d txs", len(clearedTxIDs))
}

// TestHarnessEsplora tests Esplora API integration by verifying the HTTP
// endpoint is accessible and returns valid data.
func TestHarnessEsplora(t *testing.T) {
	ParallelN(t)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()

	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()

	require.NotEmpty(t, h.EsploraURL, "EsploraURL should be set")
	t.Logf("Esplora URL: %s", h.EsploraURL)

	// Test /blocks/tip/height endpoint.
	req, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, h.EsploraURL+"/blocks/tip/height",
		nil,
	)
	require.NoError(t, err, "Should create request")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Esplora endpoint should be accessible")
	defer resp.Body.Close()

	require.Equal(
		t, http.StatusOK, resp.StatusCode,
		"Esplora should return 200 OK",
	)

	// Mine some blocks and verify height increases.
	initialHeight := h.BlockCount()
	h.Generate(5)
	newHeight := h.BlockCount()

	require.Equal(
		t, initialHeight+5, newHeight, "Block height should increase",
	)
	t.Logf("Verified block height: %d -> %d", initialHeight, newHeight)
}

// TestHarnessBitcoindRPCClient tests that we can create a bitcoind RPC
// client and make calls.
func TestHarnessBitcoindRPCClient(t *testing.T) {
	ParallelN(t)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()

	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()

	// Create RPC client.
	client, err := h.BitcoinRPCClient()
	require.NoError(t, err, "Should create RPC client")
	defer client.Shutdown()

	// Test getblockchaininfo RPC call.
	info, err := client.GetBlockChainInfo()
	require.NoError(t, err, "GetBlockChainInfo should succeed")
	require.Equal(
		t, "regtest", info.Chain, "Should be on regtest network",
	)
	t.Logf("Chain: %s, Blocks: %d", info.Chain, info.Blocks)

	// Test block generation using generatetoaddress (Generate is
	// deprecated in newer bitcoind versions).
	initialBlocks := info.Blocks
	addr, err := client.GetNewAddress("")
	require.NoError(t, err, "GetNewAddress should succeed")

	_, err = client.GenerateToAddress(3, addr, nil)
	require.NoError(t, err, "GenerateToAddress should succeed")

	info, err = client.GetBlockChainInfo()
	require.NoError(t, err, "GetBlockChainInfo should succeed")
	require.Equal(
		t, initialBlocks+3, info.Blocks,
		"Block count should increase by 3",
	)
}

// TestHarnessPostgres tests that postgres can be started and is accessible.
func TestHarnessPostgres(t *testing.T) {
	ParallelN(t)

	// Enable postgres via the flag.
	oldVal := harness.SetPostgresEnabled(true)
	defer harness.SetPostgresEnabled(oldVal)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()

	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()

	require.NotEmpty(t, h.PostgresHost, "PostgresHost should be set")
	t.Logf("Postgres host: %s", h.PostgresHost)

	// Verify postgres is functional by executing a simple query.
	// PostgresHost is already "host:port", so parse it.
	host, port, err := net.SplitHostPort(h.PostgresHost)
	require.NoError(t, err, "Should be able to parse PostgresHost")

	connStr := fmt.Sprintf("host=%s port=%s user=ark password=ark "+
		"dbname=ark sslmode=disable", host, port)

	db, err := sql.Open("postgres", connStr)
	require.NoError(t, err, "Should be able to open postgres connection")
	defer db.Close()

	// Verify connection works by running a simple query.
	var result int
	err = db.QueryRow("SELECT 1+1").Scan(&result)
	require.NoError(t, err, "Should be able to execute query")
	require.Equal(t, 2, result, "Query should return correct result")
	t.Logf("Successfully executed query, result: %d", result)

	// Verify we can create a table, insert, and query data.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS test_table (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL
		)
	`)
	require.NoError(t, err, "Should be able to create table")

	_, err = db.Exec(
		"INSERT INTO test_table (name) VALUES ($1)", "test_value",
	)
	require.NoError(t, err, "Should be able to insert data")

	var name string
	err = db.QueryRow(
		"SELECT name FROM test_table WHERE name = $1", "test_value",
	).Scan(&name)
	require.NoError(t, err, "Should be able to query data")
	require.Equal(t, "test_value", name, "Should retrieve correct data")

	t.Log("Successfully verified postgres functionality")
}

// TestHarnessMultiNode tests multi-node Lightning Network functionality
// including starting additional LND nodes and setting up channels between them.
func TestHarnessMultiNode(t *testing.T) {
	ParallelN(t)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()

	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()

	// Start two additional LND nodes (alice and bob).
	t.Log("Starting additional LND nodes...")
	alice := h.StartAdditionalLND("alice")
	require.NotNil(t, alice, "Alice LND instance should be created")
	require.Equal(t, "alice", alice.Name)
	require.NotEmpty(t, alice.GRPCPort, "Alice should have gRPC port")
	t.Logf("Alice LND gRPC: %s", alice.GRPCPort)

	bob := h.StartAdditionalLND("bob")
	require.NotNil(t, bob, "Bob LND instance should be created")
	require.Equal(t, "bob", bob.Name)
	require.NotEmpty(t, bob.GRPCPort, "Bob should have gRPC port")
	t.Logf("Bob LND gRPC: %s", bob.GRPCPort)

	// Verify we can retrieve the nodes by name.
	retrievedAlice := h.GetAdditionalLND("alice")
	require.Equal(t, alice, retrievedAlice, "Should retrieve same alice")

	retrievedBob := h.GetAdditionalLND("bob")
	require.Equal(t, bob, retrievedBob, "Should retrieve same bob")

	// Get node info to verify they're operational.
	aliceInfo, err := alice.Client.Client.GetInfo(t.Context())
	require.NoError(t, err, "Should get alice info")
	require.NotEmpty(
		t, aliceInfo.IdentityPubkey, "Alice should have pubkey",
	)
	t.Logf("Alice pubkey: %x", aliceInfo.IdentityPubkey)

	bobInfo, err := bob.Client.Client.GetInfo(t.Context())
	require.NoError(t, err, "Should get bob info")
	require.NotEmpty(t, bobInfo.IdentityPubkey, "Bob should have pubkey")
	t.Logf("Bob pubkey: %x", bobInfo.IdentityPubkey)

	// Setup a channel from alice to bob with push amount.
	t.Log("Setting up channel from alice to bob...")
	const capacitySat = 500_000
	const pushAmt = 100_000
	h.SetupChannelBetween(alice, bob, capacitySat, pushAmt)

	// Verify alice has the channel.
	aliceChannels, err := alice.Client.Client.ListChannels(
		t.Context(), false, false,
	)
	require.NoError(t, err, "Should list alice channels")
	require.Len(
		t, aliceChannels, 1, "Alice should have exactly one channel",
	)

	aliceChan := aliceChannels[0]
	require.Equal(
		t, btcutil.Amount(capacitySat), aliceChan.Capacity,
		"Channel should have correct capacity",
	)
	// Alice's local balance should be approximately capacity - pushAmt,
	// accounting for commitment transaction fees.
	expectedAliceLocal := btcutil.Amount(capacitySat - pushAmt)
	require.InDelta(
		t, float64(expectedAliceLocal), float64(aliceChan.LocalBalance),
		10000.0, // Allow 10k sats for fees
		"Alice local balance should be close to capacity minus "+
			"push amount",
	)
	t.Logf(
		"Alice channel: capacity=%d, local=%d, remote=%d",
		aliceChan.Capacity, aliceChan.LocalBalance,
		aliceChan.RemoteBalance,
	)

	// Verify bob has the channel with push amount.
	bobChannels, err := bob.Client.Client.ListChannels(
		t.Context(), false, false,
	)
	require.NoError(t, err, "Should list bob channels")
	require.Len(t, bobChannels, 1, "Bob should have exactly one channel")

	bobChan := bobChannels[0]
	require.Equal(
		t, btcutil.Amount(capacitySat), bobChan.Capacity,
		"Channel should have correct capacity",
	)
	// Bob's local balance should be approximately the push amount.
	require.InDelta(
		t, float64(pushAmt), float64(bobChan.LocalBalance),
		5000.0, // Allow 5k sats variance
		"Bob local balance should be close to push amount",
	)
	t.Logf(
		"Bob channel: capacity=%d, local=%d, remote=%d",
		bobChan.Capacity, bobChan.LocalBalance, bobChan.RemoteBalance,
	)

	t.Log("Successfully verified multi-node setup and channel creation")
}
