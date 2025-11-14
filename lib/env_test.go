package lib_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/ark/lib"
	"github.com/lightninglabs/ark/lib/types"
	"github.com/lightninglabs/ark/lib/wallets"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
)

var testParams = &chaincfg.RegressionNetParams

const (
	waitTime     = time.Second * 20
	waitPollTime = time.Millisecond * 200

	oneBtc = btcutil.SatoshiPerBitcoin
)

// operator represents a mock operator for testing.
type operator struct {
	wallet *wallet

	lib.ArkServer
}

// env provides a mock environment for testing.
type env struct {
	t             *testing.T
	miner         *BitcoindMiner
	operator      *operator
	walletCounter int
}

// getMinerClient returns the RPC client from the bitcoind miner
func (e *env) getMinerClient() *rpcclient.Client {
	return e.miner.Client
}

// sendOutputsToMiner sends outputs using the bitcoind miner
func (e *env) sendOutputsToMiner(outputs []*wire.TxOut, feeRate btcutil.Amount) (*chainhash.Hash, error) {
	return e.miner.SendOutputs(outputs, feeRate)
}

// generateBlocks generates blocks using the bitcoind miner
func (e *env) generateBlocks(numBlocks uint32) ([]*chainhash.Hash, error) {
	return e.miner.Generate(numBlocks)
}

// newEnvironment sets up a new test environment using bitcoind.
func newEnvironment(t *testing.T) *env {
	// Create bitcoind backend for all tests to support package submission
	bitcoindMiner := NewBitcoindMiner(
		t, testParams, []string{"--txindex"}, true, 25,
	)

	// Mine blocks for SegWit activation.
	numBlocks := testParams.MinerConfirmationWindow * 2
	_, err := bitcoindMiner.Generate(numBlocks)
	require.NoError(t, err)

	e := &env{
		t:     t,
		miner: bitcoindMiner,
	}

	// Set up an Ark operator.
	wallet := e.newWallet()

	op, err := lib.NewOperator(&lib.OperatorConfig{
		VTXOTreeRadix:      2,
		ConnectorTreeRadix: 4,
		BoardingExitDelay:  10,
		VTXOExitDelay:      10,
		SweepDelay:         20,
		Wallet: wallets.NewOperatorLNDWallet(
			testParams, wallet, wallet, wallet,
		),
		FeeEstimator: chainfee.NewStaticEstimator(2500, 0),
		Chain:        e,
		DustLimit:    btcutil.Amount(546),
		TargetConf:   6,
	})
	require.NoError(t, err)

	e.operator = &operator{
		wallet:    wallet,
		ArkServer: op,
	}

	return e
}

// wallet is a mock wallet for testing. It will be used to create the various
// wallet interfaces.
type wallet struct {
	lnwallet.WalletController
	keychain.SecretKeyRing
	input.Signer
	rpcClient *rpcclient.Client
}

// BroadcastTransaction implements package transaction broadcasting using bitcoind.
func (w *wallet) BroadcastTransaction(_ context.Context, txs ...string) (string, error) {
	if w.rpcClient == nil {
		return "", fmt.Errorf("rpc client not available")
	}

	// Use the bitcoind broadcast method for package support
	if err := w.broadcast(txs...); err != nil {
		return "", err
	}

	// Return the txid of the last transaction (similar to LndBitcoindWallet)
	if len(txs) == 0 {
		return "", fmt.Errorf("no transactions to broadcast")
	}

	// Parse the last transaction to get its hash
	var tx wire.MsgTx
	err := tx.Deserialize(hex.NewDecoder(strings.NewReader(txs[len(txs)-1])))
	if err != nil {
		return "", fmt.Errorf("failed to parse transaction: %w", err)
	}

	return tx.TxHash().String(), nil
}

// broadcast broadcasts one or more transactions to the network. If only a
// single transaction is provided, we use sendrawtransaction. If multiple
// transactions are provided, we use submitpackage.
func (w *wallet) broadcast(txs ...string) error {
	if len(txs) == 1 {
		var tx wire.MsgTx
		err := tx.Deserialize(hex.NewDecoder(strings.NewReader(txs[0])))
		if err != nil {
			return err
		}

		// Use the RPC client to send the transaction
		_, err = w.rpcClient.SendRawTransaction(&tx, true)
		return err
	}

	// Use submitpackage for multiple transactions
	var lastError error
	for i := 0; i < 3; i++ { // Reduced tries for testing
		res, err := w.submitPackage(txs)
		if err != nil {
			// If it's a permanent error, don't retry
			if strings.Contains(err.Error(), "TX decode failed") ||
				strings.Contains(err.Error(), "missing inputs") ||
				strings.Contains(err.Error(), "bad-txns-inputs-missingorspent") ||
				strings.Contains(err.Error(), "non-mandatory-script-verify-flag") {
				return err
			}
			return err
		}
		if strings.Contains(res.PackageMsg, "success") {
			return nil
		}
		// Store the package message for debugging
		if res.PackageMsg != "" {
			lastError = fmt.Errorf("package submission failed: %s", res.PackageMsg)
		}
		// For testing purposes, don't sleep as long
		time.Sleep(100 * time.Millisecond)
	}

	if lastError != nil {
		return lastError
	}
	return fmt.Errorf("package submission failed after retries")
}

// submitPackage submits a package of transactions using Bitcoin Core's submitpackage RPC.
func (w *wallet) submitPackage(rawTxs []string) (*btcjson.SubmitPackageResult, error) {
	cmd := btcjson.NewJsonSubmitPackageCmd(rawTxs, nil, nil)

	future := w.rpcClient.SendCmd(cmd)
	res, err := rpcclient.ReceiveFuture(future)
	if err != nil {
		return nil, err
	}

	submitPackage := &btcjson.SubmitPackageResult{}
	err = submitPackage.UnmarshalJSON(res)
	if err != nil {
		return nil, err
	}

	return submitPackage, nil
}

// newWallet creates a new test wallet using bitcoind.
func (e *env) newWallet() *wallet {
	t := e.t

	// Use bitcoind connection approach like the test harness
	rpcConfig := e.miner.RPCConfig()

	// Create bitcoind connection with ZMQ support
	bitcoindConfig := &chain.BitcoindConfig{
		ChainParams: testParams,
		Host:        rpcConfig.Host,
		User:        rpcConfig.User,
		Pass:        rpcConfig.Pass,
		ZMQConfig: &chain.ZMQConfig{
			ZMQBlockHost:    e.miner.ZMQBlock,
			ZMQTxHost:       e.miner.ZMQTx,
			ZMQReadDeadline: 5 * time.Second,
		},
	}

	bitcoindConn, err := chain.NewBitcoindConn(bitcoindConfig)
	require.NoError(t, err)

	err = bitcoindConn.Start()
	require.NoError(t, err)

	chainClient := bitcoindConn.NewBitcoindClient()
	err = chainClient.Start()
	require.NoError(t, err)

	// Wait for client to be current
	for !chainClient.IsCurrent() {
		time.Sleep(100 * time.Millisecond)
	}

	// Create RPC client for package submission - use same config as miner
	rpcClient := e.getMinerClient()

	// Create wallet with unique seed for each wallet in this test
	e.walletCounter++
	seed := fmt.Sprintf("test-seed-%s-%02d", t.Name(), e.walletCounter)
	// Ensure seed is exactly 32 bytes
	if len(seed) > 32 {
		seed = seed[:32]
	} else if len(seed) < 32 {
		// Pad to 32 bytes
		seedBytes := make([]byte, 32)
		copy(seedBytes, []byte(seed))
		seed = string(seedBytes)
	}

	// Create wallet.
	config := btcwallet.Config{
		PrivatePass:    []byte("test-pass"),
		HdSeed:         []byte(seed),
		NetParams:      testParams,
		CoinType:       testParams.HDCoinType,
		ChainSource:    chainClient, // Use bitcoind client
		RecoveryWindow: 2,
		LoaderOptions: []btcwallet.LoaderOption{
			btcwallet.LoaderWithLocalWalletDB(
				t.TempDir(), false, time.Minute,
			),
		},
	}

	blockCache := blockcache.NewBlockCache(10000)
	btcWallet, err := btcwallet.New(config, blockCache)
	require.NoError(t, err)

	require.NoError(t, btcWallet.Start())

	keyRing := keychain.NewBtcWalletKeyRing(
		btcWallet.InternalWallet(), keychain.CoinTypeTestnet,
	)

	return &wallet{
		WalletController: btcWallet,
		SecretKeyRing:    keyRing,
		Signer:           btcWallet,
		rpcClient:        rpcClient,
	}
}

// fundWallet funds the given wallet with the specified amount by sending coins
// from the miner.
func (e *env) fundWallet(wallet lnwallet.WalletController, amt int64) {
	t := e.t

	addr, err := wallet.NewAddress(
		lnwallet.WitnessPubKey, false, lnwallet.DefaultAccountName,
	)
	require.NoError(t, err)

	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	// Send coins to wallet from miner.
	_, err = e.sendOutputsToMiner([]*wire.TxOut{{
		Value:    amt,
		PkScript: script,
	}}, 1)
	require.NoError(t, err)

	// Mine blocks to confirm the transaction.
	_, err = e.generateBlocks(2)
	require.NoError(t, err)

	// Wait for wallet to see the funds.
	waitFor(t, func() bool {
		utxos, err := wallet.ListUnspentWitness(0, 999999, "")
		if err != nil {
			return false
		}
		return len(utxos) > 0
	})
}

func (e *env) fundAddr(addr btcutil.Address, amt int64) {
	t := e.t

	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	// Send coins to wallet from miner.
	_, err = e.sendOutputsToMiner([]*wire.TxOut{{
		Value:    amt,
		PkScript: script,
	}}, 1)
	require.NoError(t, err)

	// Mine blocks to confirm the transaction.
	_, err = e.generateBlocks(2)
	require.NoError(t, err)
}

func (e *env) broadcastTx(tx *wire.MsgTx, mine uint32) {
	t := e.t

	err := e.operator.wallet.CheckMempoolAcceptance(tx)
	require.NoError(t, err)

	// Serialize transaction for broadcasting
	var buf strings.Builder
	err = tx.Serialize(hex.NewEncoder(&buf))
	require.NoError(t, err)

	// Submit the transaction using the new BroadcastTransaction method
	_, err = e.operator.wallet.BroadcastTransaction(context.Background(), buf.String())
	require.NoError(t, err)

	if mine > 0 {
		_, err = e.generateBlocks(mine)
		require.NoError(t, err)
	}
}

type client struct {
	t      *testing.T
	env    *env
	wallet *wallet

	lib.ArkClient
}

// newClient creates a new test client.
func (e *env) newClient() *client {
	wallet := e.newWallet()

	arkClient := lib.NewClient(
		testParams, wallets.NewClientLndWallet(
			testParams, wallet, wallet, wallet,
		), e,
	)

	return &client{
		t:         e.t,
		env:       e,
		wallet:    wallet,
		ArkClient: arkClient,
	}
}

func (c *client) prepBoarding(amt int64) *lib.BoardingRequest {
	// Request the operator's terms.
	terms, err := c.env.operator.Terms()
	require.NoError(c.t, err)

	// Using the terms, let the client derive a new boarding address.
	addr, err := c.NewBoardingAddress(terms)
	require.NoError(c.t, err)

	// Fund the boarding address.
	c.env.fundAddr(addr, amt)

	// Wait for the client to detect the boarding UTXO.
	boardingUTXO := c.waitForBoardingUTXO(addr)

	req, err := c.CreateBoardingRequest(boardingUTXO)
	require.NoError(c.t, err)

	return req
}

func (c *client) waitForBoardingUTXO(addr btcutil.Address) *types.BoardingUTXO {
	t := c.t

	var utxo *types.BoardingUTXO
	waitFor(t, func() bool {
		var err error
		utxos, err := c.ListBoardingUTXOs()
		if err != nil {
			return false
		}
		for _, bUtxo := range utxos {
			if bUtxo.Address.Address.EncodeAddress() ==
				addr.EncodeAddress() {

				utxo = bUtxo
				break
			}
		}

		return err == nil && utxo != nil
	})

	return utxo
}

func waitFor(t *testing.T, condition func() bool) {
	require.Eventually(t, condition, waitTime, waitPollTime)
}

func TestPackageBroadcast(t *testing.T) {
	// Create a standalone bitcoind miner just for testing package submission
	bitcoindMiner := NewBitcoindMiner(t, testParams, []string{"--txindex"}, true, 25)

	// Mine some blocks so we have coins to work with (coinbase maturity requires 100 blocks)
	_, err := bitcoindMiner.Generate(101)
	require.NoError(t, err)

	// Create mock wallet with bitcoind client for package testing
	wallet := &wallet{
		rpcClient: bitcoindMiner.Client,
	}

	// Get a coinbase UTXO to spend from the bitcoind wallet
	unspentOutputs, err := bitcoindMiner.Client.ListUnspent()
	require.NoError(t, err)
	require.Greater(t, len(unspentOutputs), 0, "need at least one UTXO")

	utxo := unspentOutputs[0]

	// Parse the UTXO TxID
	utxoHash, err := chainhash.NewHashFromStr(utxo.TxID)
	require.NoError(t, err)

	// Create addresses for the transaction outputs
	addr1, err := bitcoindMiner.Client.GetNewAddress("")
	require.NoError(t, err)
	addr2, err := bitcoindMiner.Client.GetNewAddress("")
	require.NoError(t, err)

	// Build first transaction - parent transaction (spends coinbase,
	// creates two outputs).
	parentTx := wire.NewMsgTx(2)

	// Add input from coinbase UTXO.
	parentTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  *utxoHash,
			Index: utxo.Vout,
		},
		Sequence: wire.MaxTxInSequenceNum,
	})

	// Add two outputs (split the coinbase amount).
	scriptAddr1, err := txscript.PayToAddrScript(addr1)
	require.NoError(t, err)
	scriptAddr2, err := txscript.PayToAddrScript(addr2)
	require.NoError(t, err)

	// Split amount roughly in half (minus fees).
	halfAmount := int64(utxo.Amount*btcutil.SatoshiPerBitcoin) / 2
	feeAmount := int64(10000) // 0.0001 BTC fee

	parentTx.AddTxOut(&wire.TxOut{
		Value:    halfAmount - feeAmount,
		PkScript: scriptAddr1,
	})
	parentTx.AddTxOut(&wire.TxOut{
		Value:    halfAmount - feeAmount,
		PkScript: scriptAddr2,
	})

	// Sign the parent transaction
	miner := bitcoindMiner.Client
	parentTxSigned, complete, err := miner.SignRawTransactionWithWallet(parentTx)
	require.NoError(t, err)
	require.True(t, complete, "parent transaction should be fully signed")

	// Convert parent to hex for broadcasting
	var parentBuf strings.Builder
	err = parentTxSigned.Serialize(hex.NewEncoder(&parentBuf))
	require.NoError(t, err)
	parentHex := parentBuf.String()

	// Test single transaction broadcast (should use sendrawtransaction)
	txid, err := wallet.BroadcastTransaction(context.Background(), parentHex)
	require.NoError(t, err)
	require.NotEmpty(t, txid)

	// Mine a block to confirm the parent (for the single transaction test)
	_, err = bitcoindMiner.Generate(1)
	require.NoError(t, err)

	// Now test package submission with an unconfirmed parent-child transaction pair
	// Get a fresh UTXO for package testing
	unspentOutputs2, err := bitcoindMiner.Client.ListUnspent()
	require.NoError(t, err)
	require.Greater(t, len(unspentOutputs2), 1, "need at least two UTXOs")

	// Find a different UTXO for package testing
	var utxo2 btcjson.ListUnspentResult
	for _, u := range unspentOutputs2 {
		if u.TxID != utxo.TxID {
			utxo2 = u
			break
		}
	}

	// Parse the second UTXO TxID
	utxo2Hash, err := chainhash.NewHashFromStr(utxo2.TxID)
	require.NoError(t, err)

	// Create package parent transaction with low fee
	packageParentTx := wire.NewMsgTx(2)
	packageParentTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  *utxo2Hash,
			Index: utxo2.Vout,
		},
		Sequence: wire.MaxTxInSequenceNum,
	})

	addr4, err := bitcoindMiner.Client.GetNewAddress("")
	require.NoError(t, err)
	scriptAddr4, err := txscript.PayToAddrScript(addr4)
	require.NoError(t, err)

	packageParentAmount := int64(utxo2.Amount * btcutil.SatoshiPerBitcoin)
	lowFee := int64(5000) // 0.00005 BTC - very low fee
	packageParentTx.AddTxOut(&wire.TxOut{
		Value:    packageParentAmount - lowFee,
		PkScript: scriptAddr4,
	})

	// Sign package parent
	packageParentSigned, complete, err := bitcoindMiner.Client.SignRawTransactionWithWallet(packageParentTx)
	require.NoError(t, err)
	require.True(t, complete, "package parent should be fully signed")

	// Create child transaction that spends from the unconfirmed parent with higher fee (CPFP)
	addr5, err := bitcoindMiner.Client.GetNewAddress("")
	require.NoError(t, err)

	// Child pays high fee to make the package attractive to miners
	highFee := int64(60000) // 0.0006 BTC - high fee for child to boost parent+child package
	childOutputAmount := packageParentAmount - lowFee - highFee

	// Create the child transaction inputs and outputs
	childInputs := []btcjson.TransactionInput{{
		Txid: packageParentSigned.TxHash().String(),
		Vout: 0,
	}}
	childOutputs := map[btcutil.Address]btcutil.Amount{
		addr5: btcutil.Amount(childOutputAmount),
	}

	// Create the raw child transaction
	packageChildRaw, err := bitcoindMiner.Client.CreateRawTransaction(childInputs, childOutputs, nil)
	require.NoError(t, err)

	// Sign the child transaction, providing the parent's output details
	parentOutputAmount := btcutil.Amount(packageParentAmount - lowFee).ToBTC()
	packageChildSigned, complete, err := bitcoindMiner.Client.SignRawTransactionWithWallet2(packageChildRaw, []btcjson.RawTxWitnessInput{
		{
			Txid:         packageParentSigned.TxHash().String(),
			Vout:         0,
			ScriptPubKey: hex.EncodeToString(scriptAddr4),
			Amount:       &parentOutputAmount,
		},
	})
	require.NoError(t, err)
	require.True(t, complete, "child transaction should be fully signed")

	// Convert package transactions to hex strings
	var packageParentBuf strings.Builder
	err = packageParentSigned.Serialize(hex.NewEncoder(&packageParentBuf))
	require.NoError(t, err)
	packageParentHex := packageParentBuf.String()

	var packageChildBuf strings.Builder
	err = packageChildSigned.Serialize(hex.NewEncoder(&packageChildBuf))
	require.NoError(t, err)
	packageChildHex := packageChildBuf.String()

	// Test package broadcast (should use submitpackage)
	t.Log("Testing package submission with unconfirmed parent-child transaction pair (should use submitpackage)")

	// This demonstrates real CPFP: parent has low fee, child has high fee
	// Together they form a valid package that miners will accept
	packageTxid, err := wallet.BroadcastTransaction(context.Background(), packageParentHex, packageChildHex)
	require.NoError(t, err)
	require.NotEmpty(t, packageTxid)
	t.Logf("Successfully broadcast transaction package, child txid: %s", packageTxid)
	t.Log("SUCCESS: Package broadcast worked perfectly!")

	// Verify the transactions are in mempool
	mempool, err := bitcoindMiner.Client.GetRawMempool()
	require.NoError(t, err)

	parentHash := packageParentSigned.TxHash()
	childHash := packageChildSigned.TxHash()

	var foundParent, foundChild bool
	for _, mempoolTxHash := range mempool {
		if mempoolTxHash.IsEqual(&parentHash) {
			foundParent = true
		}
		if mempoolTxHash.IsEqual(&childHash) {
			foundChild = true
		}
	}

	require.True(t, foundParent, "parent transaction should be in mempool")
	require.True(t, foundChild, "child transaction should be in mempool")
	t.Log("Both parent and child transactions are in mempool - CPFP package submission successful!")
}

func (e *env) GetPrevOutputs(tx *wire.MsgTx) (map[wire.OutPoint]*wire.TxOut, error) {
	result := make(map[wire.OutPoint]*wire.TxOut)

	for _, txIn := range tx.TxIn {
		// Fetch the previous transaction
		prevTxHash := &txIn.PreviousOutPoint.Hash
		prevTx, err := e.getMinerClient().GetRawTransaction(prevTxHash)
		if err != nil {
			return nil, fmt.Errorf("failed to get previous transaction %v: %w", prevTxHash, err)
		}

		// Get the specific output
		outputIndex := txIn.PreviousOutPoint.Index
		if outputIndex >= uint32(len(prevTx.MsgTx().TxOut)) {
			return nil, fmt.Errorf("output index %d out of range for transaction %v",
				outputIndex, prevTxHash)
		}

		result[txIn.PreviousOutPoint] = prevTx.MsgTx().TxOut[outputIndex]
	}

	return result, nil
}

func (e *env) GetUTXO(op *wire.OutPoint) (*wire.TxOut, bool, int64, error) {
	txout, err := e.getMinerClient().GetTxOut(&op.Hash, op.Index, false)
	if err != nil {
		return nil, false, 0, fmt.Errorf("failed to get "+
			"transaction %s: %w", op, err)
	} else if txout == nil {
		// The UTXO has been spent.
		return nil, true, 0, nil
	}

	pkScript, err := hex.DecodeString(txout.ScriptPubKey.Hex)
	if err != nil {
		return nil, false, 0, err
	}

	// We'll ensure we properly convert the amount given in BTC to
	// satoshis.
	amt, err := btcutil.NewAmount(txout.Value)
	if err != nil {
		return nil, false, 0, err
	}

	return &wire.TxOut{
		Value:    int64(amt),
		PkScript: pkScript,
	}, false, txout.Confirmations, nil
}

// BitcoindMiner wraps a dockerized bitcoind instance that supports submitpackage
type BitcoindMiner struct {
	*dockertest.Resource
	Client   *rpcclient.Client
	Host     string
	ZMQBlock string
	ZMQTx    string
	pool     *dockertest.Pool
	dataDir  string
}

// NewBitcoindMiner creates a new dockerized bitcoind miner similar to unittest.NewMiner
// but using real Bitcoin Core that supports submitpackage RPC
func NewBitcoindMiner(t *testing.T, _ *chaincfg.Params, extraArgs []string,
	_ bool, _ int) *BitcoindMiner {

	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	// Create temporary data directory
	dataDir := t.TempDir()
	btcDataDir := filepath.Join(dataDir, "bitcoin")
	require.NoError(t, os.MkdirAll(btcDataDir, 0755))

	// Prepare bitcoind command based on harness pattern with ZMQ
	cmd := []string{
		"-regtest",
		"-txindex=1",
		"-fallbackfee=0.00001",
		"-minrelaytxfee=0.00000500",
		"-rpcuser=admin1",
		"-rpcpassword=123",
		"-rpcallowip=0.0.0.0/0",
		"-rpcbind=0.0.0.0",
		"-zmqpubrawblock=tcp://0.0.0.0:28332",
		"-zmqpubrawtx=tcp://0.0.0.0:28333",
		"-printtoconsole",
	}
	cmd = append(cmd, extraArgs...)

	// Start bitcoind container with ZMQ ports
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "lightninglabs/bitcoin-core",
		Tag:        "29",
		Cmd:        cmd,
		Mounts: []string{
			fmt.Sprintf("%s:/home/user/.bitcoin", btcDataDir),
		},
		ExposedPorts: []string{"18443/tcp", "28332/tcp", "28333/tcp"},
	}, func(hc *docker.HostConfig) {
		hc.PortBindings = map[docker.Port][]docker.PortBinding{
			"18443/tcp": {{HostIP: "0.0.0.0", HostPort: ""}},
			"28332/tcp": {{HostIP: "0.0.0.0", HostPort: ""}},
			"28333/tcp": {{HostIP: "0.0.0.0", HostPort: ""}},
		}
	})
	require.NoError(t, err)

	// Get mapped ports
	rpcPort := resource.GetPort("18443/tcp")
	zmqBlockPort := resource.GetPort("28332/tcp")
	zmqTxPort := resource.GetPort("28333/tcp")

	host := net.JoinHostPort("127.0.0.1", rpcPort)
	zmqBlock := fmt.Sprintf("tcp://127.0.0.1:%s", zmqBlockPort)
	zmqTx := fmt.Sprintf("tcp://127.0.0.1:%s", zmqTxPort)

	// Create RPC client
	connCfg := &rpcclient.ConnConfig{
		Host:         host,
		User:         "admin1",
		Pass:         "123",
		HTTPPostMode: true,
		DisableTLS:   true,
	}

	// Retry connection until bitcoind is ready
	var client *rpcclient.Client
	require.Eventually(t, func() bool {
		client, err = rpcclient.New(connCfg, nil)
		if err != nil {
			return false
		}
		// Test connection
		_, err = client.GetBlockCount()
		return err == nil
	}, 30*time.Second, 500*time.Millisecond, "bitcoind should be responsive")

	// Create default wallet if none exists (following harness pattern)
	// Use RawRequest to call listwallets and createwallet
	walletsResp, err := client.RawRequest("listwallets", nil)
	require.NoError(t, err)

	var walletList []string
	err = json.Unmarshal(walletsResp, &walletList)
	require.NoError(t, err)

	if len(walletList) == 0 {
		walletParams := []json.RawMessage{json.RawMessage(`"default"`)}
		_, err = client.RawRequest("createwallet", walletParams)
		require.NoError(t, err)
	}

	miner := &BitcoindMiner{
		Resource: resource,
		Client:   client,
		Host:     host,
		ZMQBlock: zmqBlock,
		ZMQTx:    zmqTx,
		pool:     pool,
		dataDir:  dataDir,
	}

	// Setup cleanup
	t.Cleanup(func() {
		if client != nil {
			client.Shutdown()
		}
		_ = pool.Purge(resource)
	})

	return miner
}

// RPCConfig returns RPC configuration for connecting to this bitcoind instance
func (b *BitcoindMiner) RPCConfig() struct {
	Host         string
	User         string
	Pass         string
	Certificates []byte
} {
	return struct {
		Host         string
		User         string
		Pass         string
		Certificates []byte
	}{
		Host:         b.Host,
		User:         "admin1",
		Pass:         "123",
		Certificates: nil,
	}
}

// Generate mines the specified number of blocks using generatetoaddress
func (b *BitcoindMiner) Generate(numBlocks uint32) ([]*chainhash.Hash, error) {
	// Get a new address to mine to
	addr, err := b.Client.GetNewAddress("")
	if err != nil {
		return nil, fmt.Errorf("failed to get new address: %w", err)
	}

	// Use generatetoaddress instead of generate
	return b.Client.GenerateToAddress(int64(numBlocks), addr, nil)
}

// SendOutputs creates and broadcasts a transaction with the given outputs
func (b *BitcoindMiner) SendOutputs(outputs []*wire.TxOut, _ btcutil.Amount) (*chainhash.Hash, error) {
	// Send to the first output (simplified)
	if len(outputs) > 0 {
		// Extract the address from the script
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(outputs[0].PkScript, testParams)
		if err != nil {
			return nil, fmt.Errorf("failed to extract address from script: %w", err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no addresses found in script")
		}

		amount := btcutil.Amount(outputs[0].Value)
		return b.Client.SendToAddress(addrs[0], amount)
	}

	return nil, fmt.Errorf("no outputs provided")
}

// GetClient returns the RPC client for interface compatibility
func (b *BitcoindMiner) GetClient() *rpcclient.Client {
	return b.Client
}
