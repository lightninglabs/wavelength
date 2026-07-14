package waveclicommands

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseTransactionTimeFlag verifies listtransactions accepts both full
// RFC3339 timestamps and ISO date-only filters.
func TestParseTransactionTimeFlag(t *testing.T) {
	t.Parallel()

	full, err := parseTransactionTimeFlag(
		"--from", "2026-05-08T12:34:56Z",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1778243696), full)

	dateOnly, err := parseTransactionTimeFlag("--to", "2026-05-08")
	require.NoError(t, err)
	require.Equal(t, int64(1778284799), dateOnly)

	open, err := parseTransactionTimeFlag("--from", "")
	require.NoError(t, err)
	require.Zero(t, open)

	_, err = parseTransactionTimeFlag("--from", "May 8")
	require.ErrorContains(t, err, "ISO 8601")
	require.ErrorContains(t, err, "2026-05-08T00:00:00Z")
	require.ErrorContains(t, err, "2026-05-08")
}

// TestValidateTransactionTimeWindow verifies the CLI rejects inverted local
// timestamp bounds before sending a ListTransactions request.
func TestValidateTransactionTimeWindow(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateTransactionTimeWindow(10, 0))
	require.NoError(t, validateTransactionTimeWindow(0, 10))
	require.NoError(t, validateTransactionTimeWindow(10, 10))
	require.NoError(t, validateTransactionTimeWindow(10, 11))

	err := validateTransactionTimeWindow(11, 10)
	require.ErrorContains(t, err, "--from must be before")
}

// TestTransactionTypeFlagValid verifies the CLI rejects typos locally before
// sending a ListTransactions request.
func TestTransactionTypeFlagValid(t *testing.T) {
	t.Parallel()

	filters := []string{"", "boarding", "round", "oor", "sweep"}
	for _, filter := range filters {
		require.True(t, transactionTypeFlagValid(filter), filter)
	}
	require.False(t, transactionTypeFlagValid("fees"))
}

// TestMethodRegistryListTransactionsSchema verifies the public schema
// advertises listtransactions and all expected filters.
func TestMethodRegistryListTransactionsSchema(t *testing.T) {
	t.Parallel()

	method := findSchemaMethod(t, "ark.listtransactions")
	require.Equal(t, "ListTransactionsRequest", method.RequestType)
	require.Equal(t, "ListTransactionsResponse", method.ResponseType)
	require.True(t, method.JSONInput)

	params := make(map[string]schemaParam, len(method.Params))
	for _, param := range method.Params {
		params[param.Name] = param
	}

	require.Contains(t, params, "from")
	require.Contains(t, params, "to")
	require.Contains(t, params, "limit")
	require.Contains(t, params, "offset")
	require.Equal(t, []string{
		"boarding", "round", "oor", "sweep",
	}, params["type"].Values)
}
