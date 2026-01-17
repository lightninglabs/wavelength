package mailboxclient_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/db"
	mailboxclient "github.com/lightninglabs/darepo-client/mailbox/client"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/stretchr/testify/require"
)

// TestClient_SQLCStoreSurvivesRestart verifies that a response persisted to the
// SQL store can be consumed after the client is restarted.
func TestClient_SQLCStoreSurvivesRestart(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	edge := &fakeMailboxServiceClient{mb: mb}

	mux := mailboxrpc.NewServeMux()
	arkrpc.RegisterArkServiceMailboxServer(mux, &testArkServer{
		resp: &arkrpc.GetInfoResponse{
			Version:     "v-test",
			Network:     "regtest",
			BlockHeight: 123,
		},
	})

	operatorCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	operatorErr := make(chan error, 1)
	go func() {
		operatorErr <- runOperator(operatorCtx, edge, "operator", mux)
	}()

	dbPath := filepath.Join(t.TempDir(), "mailboxclient.db")
	store1, err := db.NewSqliteStore(&db.SqliteConfig{
		DatabaseFileName: dbPath,
		SkipMigrations:   false,
	}, btclog.Disabled)
	require.NoError(t, err)

	cfg := mailboxclient.DefaultConfig()
	cfg.Edge = edge
	cfg.Store = mailboxclient.NewSQLCStore(store1.Queries)
	cfg.LocalMailboxID = "client-1"
	cfg.RemoteMailboxID = "operator"
	cfg.ProtocolVersion = 1
	cfg.PullWaitTimeout = 20 * time.Millisecond

	rpc, err := mailboxclient.New(cfg)
	require.NoError(t, err)

	correlationID := "corr-1"
	_, _, err = rpc.SendRPC(
		t.Context(),
		"arkrpc.ArkService",
		"GetInfo",
		&arkrpc.GetInfoRequest{},
		mailboxrpc.RPCOptions{
			CorrelationID:  correlationID,
			IdempotencyKey: correlationID,
		},
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, ok, err := cfg.Store.GetResponse(
			t.Context(), cfg.LocalMailboxID, correlationID,
		)
		require.NoError(t, err)

		return ok
	}, 2*time.Second, 10*time.Millisecond)

	rpc.Stop()
	require.NoError(t, store1.DB.Close())

	store2, err := db.NewSqliteStore(&db.SqliteConfig{
		DatabaseFileName: dbPath,
		SkipMigrations:   false,
	}, btclog.Disabled)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store2.DB.Close())
	}()

	cfg.Store = mailboxclient.NewSQLCStore(store2.Queries)
	rpc2, err := mailboxclient.New(cfg)
	require.NoError(t, err)
	defer rpc2.Stop()

	out := new(arkrpc.GetInfoResponse)
	err = rpc2.AwaitRPC(t.Context(), correlationID, out)
	require.NoError(t, err)
	require.Equal(t, "v-test", out.Version)
	require.Equal(t, "regtest", out.Network)
	require.Equal(t, uint32(123), out.BlockHeight)

	cancel()
	require.NoError(t, <-operatorErr)
}
