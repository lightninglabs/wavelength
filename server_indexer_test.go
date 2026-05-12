package darepo

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
	storedb "github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/mailbox"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// TestSetupIndexerSubsystemUsesDurableMailboxStore verifies that the
// operator mailbox is backed by SQL storage, not process-local memory.
func TestSetupIndexerSubsystemUsesDurableMailboxStore(t *testing.T) {
	t.Parallel()

	server := newTestIndexerServer(t)
	server.cfg.Mailbox.MaxEnvelopesPerMailbox = 1

	err := server.setupIndexerSubsystem(t.Context())
	require.NoError(t, err)
	defer server.stopIndexerSubsystem(t.Context())

	require.IsType(
		t, (*storedb.MailboxEnvelopeStore)(nil), server.mailboxStore,
	)

	_, err = server.mailboxStore.Append(
		t.Context(), testIndexerMailboxEnvelope("alice", "msg-1"),
	)
	require.NoError(t, err)

	_, err = server.mailboxStore.Append(
		t.Context(), testIndexerMailboxEnvelope("alice", "msg-2"),
	)
	require.Error(t, err)
}

// TestSetupIndexerSubsystemPreservesMailboxSequence verifies that a
// restarted operator continues server mailbox cursors from persisted
// SQL state.
func TestSetupIndexerSubsystemPreservesMailboxSequence(t *testing.T) {
	t.Parallel()

	sqlStore := storedb.NewTestDB(t)
	store := newTestIndexerStore(sqlStore.BaseDB)

	server1 := newTestIndexerServerWithStore(store)
	err := server1.setupIndexerSubsystem(t.Context())
	require.NoError(t, err)

	seq1, err := server1.mailboxStore.Append(
		t.Context(), testIndexerMailboxEnvelope("alice", "msg-1"),
	)
	require.NoError(t, err)
	require.Equal(t, uint64(1), seq1)
	server1.stopIndexerSubsystem(t.Context())

	server2 := newTestIndexerServerWithStore(store)
	err = server2.setupIndexerSubsystem(t.Context())
	require.NoError(t, err)
	defer server2.stopIndexerSubsystem(t.Context())

	seq2, err := server2.mailboxStore.Append(
		t.Context(), testIndexerMailboxEnvelope("alice", "msg-2"),
	)
	require.NoError(t, err)
	require.Equal(t, uint64(2), seq2)
}

// newTestIndexerServer returns a server with a test SQL database for
// exercising indexer subsystem wiring.
func newTestIndexerServer(t testing.TB) *Server {
	t.Helper()

	return newTestIndexerServerWithStore(
		newTestIndexerStore(
			storedb.NewTestDB(t).BaseDB,
		),
	)
}

// newTestIndexerStore wraps the given SQL test handle in the daemon's
// unified DB store.
func newTestIndexerStore(base *storedb.BaseDB) *storedb.Store {
	return storedb.NewStore(
		base.DB, base.Queries, base.Backend(), btclog.Disabled,
		clock.NewDefaultClock(),
	)
}

// newTestIndexerServerWithStore returns a server using the given SQL
// store without starting unrelated daemon subsystems.
func newTestIndexerServerWithStore(store *storedb.Store) *Server {
	cfg := DefaultConfig()
	cfg.Loggers = nil

	return &Server{
		cfg: cfg,
		db:  store,
		log: btclog.Disabled,
	}
}

// testIndexerMailboxEnvelope returns a minimal mailbox envelope for
// exercising daemon mailbox store wiring.
func testIndexerMailboxEnvelope(recipient, msgID string) *mailbox.Envelope {
	return &mailbox.Envelope{
		Recipient:       recipient,
		MsgId:           msgID,
		Sender:          "test-sender",
		ProtocolVersion: 1,
		Body: &anypb.Any{
			TypeUrl: "type.googleapis.com/test.Message",
			Value:   []byte("payload"),
		},
	}
}
