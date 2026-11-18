package swaps

import (
	"path/filepath"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestReceiveRequestIDSurvivesRestart verifies the request ID and memo are
// stored on the admitted receive row, survive a store reopen and later
// progress updates, and that a second receive cannot claim the same ID.
func TestReceiveRequestIDSurvivesRestart(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	cfg := &SqliteStoreConfig{
		DatabaseFileName: filepath.Join(
			t.TempDir(), DefaultSqliteDatabaseFileName,
		),
	}
	store, err := NewSqliteStore(cfg, btclog.Disabled)
	require.NoError(t, err)
	client := NewSwapClientWithStore(nil, nil, nil, nil, store)

	// Admission is the first write of the row. A later progress update
	// must keep the request ID and memo it recorded.
	session := admissionReceiveSession(t, client, false)
	session.requestID = "req-1"
	session.memo = "coffee"
	require.NoError(t, session.persist(ctx))
	session.state = ReceiveStateVHTLCFunded
	require.NoError(t, session.persist(ctx))
	require.NoError(t, store.Close())

	store, err = NewSqliteStore(cfg, btclog.Disabled)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})
	client = NewSwapClientWithStore(nil, nil, nil, nil, store)

	summary, err := client.ReceiveSummaryByRequestID(ctx, "req-1")
	require.NoError(t, err)
	require.Equal(t, session.PaymentHash, summary.PaymentHash)
	require.Equal(t, "normal-invoice", summary.Invoice)
	require.Equal(t, "coffee", summary.Memo)
	require.Equal(t, int64(42_000), summary.AmountSat)

	// A different receive under the same ID is refused by the store, so
	// one request ID can never own two invoices.
	other := admissionReceiveSession(t, client, false)
	other.Preimage = lntypes.Preimage{203}
	other.PaymentHash = other.Preimage.Hash()
	other.requestID = "req-1"
	require.Error(t, other.persist(ctx))

	_, err = client.ReceiveSummaryByRequestID(ctx, "unknown")
	require.ErrorIs(t, err, ErrSwapSummaryNotFound)
	_, err = client.ReceiveSummaryByRequestID(ctx, "")
	require.ErrorIs(t, err, ErrSwapSummaryNotFound)
}
