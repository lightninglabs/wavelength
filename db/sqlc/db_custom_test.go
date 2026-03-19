package sqlc

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQueriesWithTxPreservesBackend verifies that rebinding a Queries handle
// to a transaction keeps the backend type metadata needed by backend-specific
// query helpers.
func TestQueriesWithTxPreservesBackend(t *testing.T) {
	t.Parallel()

	queries := NewSqlite(&sql.DB{})
	txQueries := queries.WithTx(&sql.Tx{})

	require.Equal(t, BackendTypeSqlite, txQueries.Backend())
}
