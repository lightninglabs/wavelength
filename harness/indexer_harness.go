package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/indexer"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// defaultIndexerProofServerID must match the operator's indexer proof
	// verifier server-id domain.
	defaultIndexerProofServerID = "arkd"
)

// IndexerTestClient is a real mailbox-backed indexer client that can issue
// proof-gated indexer RPCs against the running operator in integration tests.
type IndexerTestClient struct {
	T *testing.T

	LocalMailboxID  string
	RemoteMailboxID string

	Indexer *indexer.Client

	runtime       *serverconn.Runtime
	runtimeCancel context.CancelFunc
	edgeConn      *grpc.ClientConn
	sqliteStore   *clientdb.SqliteStore
}

// Stop gracefully stops the indexer runtime and closes all backing resources.
func (c *IndexerTestClient) Stop() {
	if c == nil {
		return
	}

	if c.runtimeCancel != nil {
		c.runtimeCancel()
	}

	if c.runtime != nil {
		c.runtime.Stop()
	}

	if c.edgeConn != nil {
		_ = c.edgeConn.Close()
	}

	if c.sqliteStore != nil && c.sqliteStore.DB != nil {
		_ = c.sqliteStore.DB.Close()
	}
}

// StartIndexerTestClient starts a mailbox-backed indexer client bound to the
// given daemon's backing wallet key descriptor.
func (h *ArkHarness) StartIndexerTestClient(daemonName string, keyFamily,
	keyIndex uint32) *IndexerTestClient {

	h.T.Helper()

	daemon := h.GetClientDaemon(daemonName)
	require.NotNil(h.T, daemon, "client daemon %q not found", daemonName)
	require.NotNil(h.T, daemon.LND,
		"indexer test client currently requires lnd backend")
	require.NotNil(h.T, daemon.LND.Client,
		"lnd services are not available for daemon %q", daemonName)

	keyDesc, err := daemon.LND.Client.WalletKit.DeriveKey(
		h.T.Context(), &keychain.KeyLocator{
			Family: keychain.KeyFamily(keyFamily),
			Index:  keyIndex,
		},
	)
	require.NoError(h.T, err, "derive key descriptor for indexer signer")

	localMailboxID := fmt.Sprintf(
		"itest-indexer-%s-%d", daemonName, time.Now().UnixNano(),
	)
	remoteMailboxID := fmt.Sprintf("server-for-%s", localMailboxID)

	dataDir := filepath.Join(
		h.BaseDir(), "indexer-test-clients", localMailboxID,
	)
	require.NoError(h.T, os.MkdirAll(dataDir, 0o755))

	sqliteCfg := clientdb.DefaultSqliteConfig(dataDir)
	sqliteCfg.DatabaseFileName = filepath.Join(
		dataDir, "indexer-test-client.db",
	)
	sqliteStore, err := clientdb.NewSqliteStore(
		sqliteCfg, btclog.Disabled,
	)
	require.NoError(h.T, err, "create indexer test sqlite store")

	deliveryStore, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqliteStore.DB, sqliteStore.Backend(), clock.NewDefaultClock(),
		btclog.Disabled,
	)
	require.NoError(h.T, err, "create indexer test delivery store")

	edgeConn, err := grpc.Dial(
		h.ArkRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(h.T, err, "dial operator mailbox edge")

	connCfg := serverconn.DefaultConnectorConfig()
	connCfg.Edge = mailboxpb.NewMailboxServiceClient(edgeConn)
	connCfg.LocalMailboxID = localMailboxID
	connCfg.RemoteMailboxID = remoteMailboxID
	connCfg.ProtocolVersion = 1
	connCfg.Store = deliveryStore

	runtime, err := serverconn.NewRuntime(connCfg)
	require.NoError(h.T, err, "create indexer mailbox runtime")

	runtimeCtx, runtimeCancel := context.WithCancel(context.Background())
	require.NoError(h.T, runtime.Start(runtimeCtx),
		"start indexer mailbox runtime")

	signer := indexer.NewLNDSchnorrSigner(
		daemon.LND.Client.Signer, *keyDesc,
	)
	idxClient := indexer.New(
		runtime.Unary(), signer, defaultIndexerProofServerID,
		localMailboxID, fn.None[btclog.Logger](),
	)

	indexerClient := &IndexerTestClient{
		T:               h.T,
		LocalMailboxID:  localMailboxID,
		RemoteMailboxID: remoteMailboxID,
		Indexer:         idxClient,
		runtime:         runtime,
		runtimeCancel:   runtimeCancel,
		edgeConn:        edgeConn,
		sqliteStore:     sqliteStore,
	}

	h.T.Cleanup(indexerClient.Stop)

	return indexerClient
}
