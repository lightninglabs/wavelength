package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/indexer"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
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

	// Derive mailbox IDs from the test key's public key and
	// the server's operator key.
	localMailboxID := serverconn.PubKeyMailboxID(keyDesc.PubKey)

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

	// Fetch the operator's pubkey via direct gRPC to derive
	// the remote mailbox ID.
	arkClient := arkrpc.NewArkServiceClient(edgeConn)
	infoResp, err := arkClient.GetInfo(
		h.T.Context(), &arkrpc.GetInfoRequest{},
	)
	require.NoError(h.T, err, "fetch operator info for indexer")
	require.NotEmpty(h.T, infoResp.Pubkey, "operator pubkey empty")

	operatorPubKey, err := btcec.ParsePubKey(infoResp.Pubkey)
	require.NoError(h.T, err, "parse operator pubkey")

	remoteMailboxID := serverconn.PubKeyMailboxID(operatorPubKey)
	remoteMailboxID = serverconn.CompoundMailboxID(
		remoteMailboxID, localMailboxID,
	)

	authSig, err := signIndexerMailboxAuth(
		h.T.Context(), daemon.LND.Client.Signer, *keyDesc,
		remoteMailboxID,
	)
	require.NoError(h.T, err, "sign indexer mailbox auth")

	connCfg := serverconn.DefaultConnectorConfig()
	connCfg.Edge = mailboxpb.NewMailboxServiceClient(edgeConn)
	connCfg.LocalMailboxID = localMailboxID
	connCfg.RemoteMailboxID = remoteMailboxID
	connCfg.ProtocolVersion = 1
	connCfg.Store = deliveryStore
	connCfg.AuthSignature = authSig
	connCfg.InitAuthHeader()

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
		T:             h.T,
		Indexer:       idxClient,
		runtime:       runtime,
		runtimeCancel: runtimeCancel,
		edgeConn:      edgeConn,
		sqliteStore:   sqliteStore,
	}

	h.T.Cleanup(indexerClient.Stop)

	// Wait until the mailbox-backed indexer client can complete a
	// basic query through the operator. This flushes the compound
	// mailbox routing setup before callers start polling for
	// recipient events.
	require.Eventually(h.T, func() bool {
		ctx, cancel := context.WithTimeout(
			h.T.Context(), defaultSmallTimeout,
		)
		defer cancel()

		_, err := idxClient.ListMyReceiveScripts(ctx)

		return err == nil
	}, defaultTimeout, pollInterval,
		"indexer test client never became query-ready")

	return indexerClient
}

// signIndexerMailboxAuth computes the tagged Schnorr signature that proves
// ownership of the indexer test client's mailbox identity key.
func signIndexerMailboxAuth(ctx context.Context,
	signer lndclient.SignerClient, keyDesc keychain.KeyDescriptor,
	recipientMailboxID string) (*schnorr.Signature, error) {

	msg := serverconn.MailboxAuthMessage(
		keyDesc.PubKey, recipientMailboxID,
	)
	tag := []byte(serverconn.MailboxAuthTagStr)

	rawSig, err := signer.SignMessage(
		ctx, msg, keyDesc.KeyLocator,
		lndclient.SignSchnorr(nil),
		withIndexerHarnessSchnorrTag(tag),
	)
	if err != nil {
		return nil, err
	}

	return schnorr.ParseSignature(rawSig)
}

// withIndexerHarnessSchnorrTag applies a BIP-340 tag to lnd's SignMessage
// request.
func withIndexerHarnessSchnorrTag(
	tag []byte) lndclient.SignMessageOption {

	return func(req *signrpc.SignMessageReq) {
		req.Tag = tag
	}
}
