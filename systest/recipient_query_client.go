//go:build systest

package systest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	clientindexer "github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo/clientconn"
	serverdb "github.com/lightninglabs/darepo/db"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// RecipientQueryClient is a mailbox-backed standalone indexer client used by
// systests to query recipient events independently from a running daemon.
type RecipientQueryClient struct {
	t *testing.T
	h *E2EHarness

	clientID clientconn.ClientID

	indexer *clientindexer.Client
	rpc     *arkrpc.IndexerServiceMailboxClient

	runtime       *serverconn.Runtime
	runtimeCancel context.CancelFunc

	sqliteStore *clientdb.SqliteStore
}

// Stop releases all resources associated with the standalone query client.
func (c *RecipientQueryClient) Stop() {
	if c == nil {
		return
	}

	if c.runtimeCancel != nil {
		c.runtimeCancel()
	}

	if c.runtime != nil {
		c.runtime.Stop()
	}

	if c.h != nil && c.clientID != "" {
		_ = c.h.clientBridge.DeregisterClient(c.clientID)
		c.h.instrumentedMB.UnregisterClient(c.clientID)
	}

	if c.sqliteStore != nil && c.sqliteStore.DB != nil {
		_ = c.sqliteStore.DB.Close()
	}
}

// BuildListOORRecipientEventsByScriptRequest builds the proof-gated request
// body for one taproot recipient-event query.
func (c *RecipientQueryClient) BuildListOORRecipientEventsByScriptRequest(
	ctx context.Context, pkScript []byte, afterEventID uint64,
	limit uint32) (*arkrpc.ListOORRecipientEventsByScriptRequest, error) {

	if c == nil || c.indexer == nil {
		return nil, fmt.Errorf("recipient query client not initialized")
	}

	return c.indexer.BuildListOORRecipientEventsByScriptTaprootRequest(
		ctx, pkScript, afterEventID, limit,
	)
}

// ListOORRecipientEventsByRequest submits a prebuilt, proof-gated recipient
// event query through the mailbox transport.
func (c *RecipientQueryClient) ListOORRecipientEventsByRequest(
	ctx context.Context,
	req *arkrpc.ListOORRecipientEventsByScriptRequest) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	if c == nil {
		return nil, fmt.Errorf("recipient query client not initialized")
	}

	return c.rpc.ListOORRecipientEventsByScript(ctx, req)
}

// StartRecipientQueryClient starts a standalone mailbox-backed indexer query
// client bound to the provided receive-script proof key.
func (h *E2EHarness) StartRecipientQueryClient(keyDesc keychain.KeyDescriptor,
	signer clientindexer.SchnorrSigner) *RecipientQueryClient {

	h.t.Helper()

	if keyDesc.PubKey == nil {
		h.t.Fatalf("recipient query proof key is required")
	}

	localMailboxID := serverconn.PubKeyMailboxID(keyDesc.PubKey)
	clientID := clientconn.ClientID(localMailboxID)
	serverMailboxID := serverMailboxPrefix + localMailboxID

	h.instrumentedMB.RegisterMailboxPair(
		clientID, serverMailboxID, localMailboxID,
	)

	dataDir := filepath.Join(
		h.BaseDir(),
		"systest-recipient-query-clients", localMailboxID,
	)
	requireNoError(
		h.t, os.MkdirAll(dataDir, 0o755),
		"mkdir recipient query dir",
	)

	sqliteCfg := clientdb.DefaultSqliteConfig(dataDir)
	sqliteCfg.DatabaseFileName = filepath.Join(
		dataDir, "recipient-query-client.db",
	)
	sqliteStore, err := clientdb.NewSqliteStore(
		sqliteCfg, btclog.Disabled,
	)
	requireNoError(h.t, err, "create recipient query sqlite store")

	clientDeliveryStore, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqliteStore.DB, sqliteStore.Backend(), clock.NewDefaultClock(),
		btclog.Disabled,
	)
	requireNoError(h.t, err, "create recipient query delivery store")

	clientConnCfg := serverconn.DefaultConnectorConfig()
	clientConnCfg.Edge = h.instrumentedMB
	clientConnCfg.LocalMailboxID = localMailboxID
	clientConnCfg.RemoteMailboxID = serverMailboxID
	clientConnCfg.Store = clientDeliveryStore
	clientConnCfg.ProtocolVersion = 1
	clientConnCfg.PullWaitTimeout = 100 * time.Millisecond

	runtime, err := serverconn.NewRuntime(clientConnCfg)
	requireNoError(h.t, err, "create recipient query runtime")

	runtimeCtx, runtimeCancel := context.WithCancel(context.Background())
	requireNoError(
		h.t, runtime.Start(runtimeCtx),
		"start recipient query runtime",
	)

	serverDeliveryStore, err := serverdb.NewActorDeliveryStoreFromDB(
		h.sqlStore, clock.NewDefaultClock(), h.SubLogger("RQRY"),
	)
	requireNoError(h.t, err, "create server delivery store")

	serverPerClientCfg := clientconn.DefaultPerClientConfig()
	serverPerClientCfg.Edge = h.instrumentedMB
	serverPerClientCfg.LocalMailboxID = serverMailboxID
	serverPerClientCfg.RemoteMailboxID = localMailboxID
	serverPerClientCfg.Dispatchers = h.indexerOperator.Dispatchers()
	serverPerClientCfg.Store = serverDeliveryStore
	serverPerClientCfg.ProtocolVersion = 1
	serverPerClientCfg.PullWaitTimeout = 100 * time.Millisecond

	_, err = h.clientBridge.RegisterClient(
		h.ctx, clientID, serverPerClientCfg,
	)
	requireNoError(h.t, err, "register recipient query client")

	indexerClient := clientindexer.New(
		runtime.Unary(), signer, "server", localMailboxID,
		fn.None[btclog.Logger](),
	)
	rpcClient := arkrpc.NewIndexerServiceMailboxClient(runtime.Unary())

	queryClient := &RecipientQueryClient{
		t:             h.t,
		h:             h,
		clientID:      clientID,
		indexer:       indexerClient,
		rpc:           rpcClient,
		runtime:       runtime,
		runtimeCancel: runtimeCancel,
		sqliteStore:   sqliteStore,
	}

	h.t.Cleanup(queryClient.Stop)

	require.Eventually(h.t, func() bool {
		ctx, cancel := context.WithTimeout(h.ctx, defaultTimeout)
		defer cancel()

		_, listErr := indexerClient.ListMyReceiveScripts(ctx)

		return listErr == nil
	}, defaultTimeout, pollInterval, "recipient query client never "+
		"became ready")

	return queryClient
}

func requireNoError(t *testing.T, err error, msg string) {
	t.Helper()

	if err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}
