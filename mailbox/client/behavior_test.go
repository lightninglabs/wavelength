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

// TestClient_ConcurrentResponsesSurviveRestart verifies that multiple in-flight
// responses survive a client restart when using a durable store.
func TestClient_ConcurrentResponsesSurviveRestart(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	edge := &fakeMailboxServiceClient{mb: mb}

	mux := mailboxrpc.NewServeMux()
	arkrpc.RegisterArkServiceMailboxServer(mux, &testArkServer{
		resp: &arkrpc.GetInfoResponse{
			Version:     "v-test",
			Network:     "regtest",
			BlockHeight: 999,
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

	corr1 := "corr-1"
	corr2 := "corr-2"

	send := func(corr string) {
		_, _, err := rpc.SendRPC(
			t.Context(),
			"arkrpc.ArkService",
			"GetInfo",
			&arkrpc.GetInfoRequest{},
			mailboxrpc.RPCOptions{
				CorrelationID:  corr,
				IdempotencyKey: corr,
			},
		)
		require.NoError(t, err)
	}

	send(corr1)
	send(corr2)

	require.Eventually(t, func() bool {
		_, ok1, err := cfg.Store.GetResponse(
			t.Context(), cfg.LocalMailboxID, corr1,
		)
		if err != nil {
			return false
		}

		_, ok2, err := cfg.Store.GetResponse(
			t.Context(), cfg.LocalMailboxID, corr2,
		)
		if err != nil {
			return false
		}

		return ok1 && ok2
	}, 3*time.Second, 10*time.Millisecond)

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
	cursor, err := cfg.Store.LoadCursor(t.Context(), cfg.LocalMailboxID)
	require.NoError(t, err)
	require.NotZero(t, cursor)

	rpc2, err := mailboxclient.New(cfg)
	require.NoError(t, err)
	defer rpc2.Stop()

	resp1 := new(arkrpc.GetInfoResponse)
	err = rpc2.AwaitRPC(t.Context(), corr1, resp1)
	require.NoError(t, err)

	resp2 := new(arkrpc.GetInfoResponse)
	err = rpc2.AwaitRPC(t.Context(), corr2, resp2)
	require.NoError(t, err)

	_, ok, err := cfg.Store.GetResponse(
		t.Context(), cfg.LocalMailboxID, corr1,
	)
	require.NoError(t, err)
	require.False(t, ok)

	_, ok, err = cfg.Store.GetResponse(
		t.Context(), cfg.LocalMailboxID, corr2,
	)
	require.NoError(t, err)
	require.False(t, ok)

	cancel()
	require.NoError(t, <-operatorErr)
}

// TestClient_CursorPersistenceIsMonotonic verifies that cursor updates are
// monotonic. This is a guard against accidental cursor regressions.
func TestClient_CursorPersistenceIsMonotonic(t *testing.T) {
	t.Parallel()

	storePath := filepath.Join(t.TempDir(), "mailboxclient.db")
	sqlStore, err := db.NewSqliteStore(&db.SqliteConfig{
		DatabaseFileName: storePath,
		SkipMigrations:   false,
	}, btclog.Disabled)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, sqlStore.DB.Close())
	}()

	store := mailboxclient.NewSQLCStore(sqlStore.Queries)

	err = store.SaveCursor(t.Context(), "mb-1", 10)
	require.NoError(t, err)

	err = store.SaveCursor(t.Context(), "mb-1", 5)
	require.NoError(t, err)

	cursor, err := store.LoadCursor(t.Context(), "mb-1")
	require.NoError(t, err)
	require.Equal(t, uint64(10), cursor)

	err = store.SaveCursor(t.Context(), "mb-1", 11)
	require.NoError(t, err)

	cursor, err = store.LoadCursor(t.Context(), "mb-1")
	require.NoError(t, err)
	require.Equal(t, uint64(11), cursor)
}
