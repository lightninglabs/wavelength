package actor

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWithoutTxStripsTransaction verifies that WithoutTx shadows a parent
// context's transaction entry so that TxFromContext returns (nil, false) and
// HasTx returns false. This is the core invariant that prevents the sender's
// transaction from leaking into cross-actor mailbox enqueues.
func TestWithoutTxStripsTransaction(t *testing.T) {
	t.Parallel()

	// Start with a context that carries a transaction. A typed nil
	// *sql.Tx is sufficient: WithTx stores an interface value with
	// dynamic type *sql.Tx, so HasTx and TxFromContext both report
	// a transaction is present.
	ctx := WithTx(context.Background(), (*sql.Tx)(nil))
	require.True(t, HasTx(ctx),
		"context should carry a tx after WithTx")

	tx, ok := TxFromContext(ctx)
	require.True(t, ok, "TxFromContext should return ok=true")
	require.Nil(t, tx, "tx value should be nil (typed nil)")

	// WithoutTx should shadow the parent's entry with an untyped nil,
	// causing both HasTx and TxFromContext to report no transaction.
	stripped := WithoutTx(ctx)
	require.False(t, HasTx(stripped),
		"HasTx should return false after WithoutTx")

	tx, ok = TxFromContext(stripped)
	require.False(t, ok,
		"TxFromContext should return ok=false after WithoutTx")
	require.Nil(t, tx)
}

// TestWithoutTxPreservesOtherValues verifies that WithoutTx only strips the
// transaction key and leaves other context values intact.
func TestWithoutTxPreservesOtherValues(t *testing.T) {
	t.Parallel()

	type customKey struct{}

	ctx := context.WithValue(context.Background(), customKey{}, "preserved")
	ctx = WithTx(ctx, (*sql.Tx)(nil))

	stripped := WithoutTx(ctx)

	// Transaction should be stripped.
	require.False(t, HasTx(stripped))

	// Other values should be preserved.
	val := stripped.Value(customKey{})
	require.Equal(t, "preserved", val)
}

// TestWithoutTxOnCleanContext verifies that WithoutTx is safe to call on a
// context that never had a transaction attached.
func TestWithoutTxOnCleanContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	require.False(t, HasTx(ctx))

	// Stripping a non-existent tx should be a no-op.
	stripped := WithoutTx(ctx)
	require.False(t, HasTx(stripped))

	tx, ok := TxFromContext(stripped)
	require.False(t, ok)
	require.Nil(t, tx)
}

// TestWithoutOutboxIDStripsPropagatedID verifies that internal async callbacks
// can shadow a propagated outbox ID so they don't accidentally reuse CDC
// deduplication identity from the inbound durable delivery.
func TestWithoutOutboxIDStripsPropagatedID(t *testing.T) {
	t.Parallel()

	type customKey struct{}

	ctx := context.WithValue(context.Background(), customKey{}, "preserved")
	ctx = WithOutboxID(ctx, "outbox-123")

	id, ok := OutboxIDFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "outbox-123", id)

	stripped := WithoutOutboxID(ctx)

	id, ok = OutboxIDFromContext(stripped)
	require.False(t, ok)
	require.Empty(t, id)

	val := stripped.Value(customKey{})
	require.Equal(t, "preserved", val)
}
