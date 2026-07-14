package db

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/constraints"
)

var (
	// MaxValidSQLTime is the maximum valid time that can be rendered as a
	// time string and can be used for comparisons in SQL.
	MaxValidSQLTime = time.Date(9999, 12, 31, 23, 59, 59, 999999, time.UTC)
)

// sqlInt64 turns a numerical integer type into the NullInt64 that sql/sqlc
// uses when an integer field can be permitted to be NULL.  We use the
// constraints.Integer constraint here which maps to all signed and unsigned
// integer types.
func sqlInt64[T constraints.Integer](num T) sql.NullInt64 {
	return sql.NullInt64{
		Int64: int64(num),
		Valid: true,
	}
}

// sqlInt32 turns a numerical integer type into the NullInt32 that sql/sqlc
// uses when an integer field can be permitted to be NULL.  We use the
// constraints.Integer constraint here which maps to all signed and unsigned
// integer types.
func sqlInt32[T constraints.Integer](num T) sql.NullInt32 {
	return sql.NullInt32{
		Int32: int32(num),
		Valid: true,
	}
}

// sqlInt16 turns a numerical integer type into the NullInt16 that sql/sqlc
// uses when an integer field can be permitted to be NULL.  We use the
// constraints.Integer constraint here which maps to all signed and unsigned
// integer types.
//
//nolint:unused
func sqlInt16[T constraints.Integer](num T) sql.NullInt16 {
	return sql.NullInt16{
		Int16: int16(num),
		Valid: true,
	}
}

// sqlBool turns a boolean into the NullBool that sql/sqlc uses when a boolean
// field can be permitted to be NULL.
//
//nolint:unused
func sqlBool(b bool) sql.NullBool {
	return sql.NullBool{
		Bool:  b,
		Valid: true,
	}
}

// sqlStr turns a string into the NullString that sql/sqlc uses when a string
// can be permitted to be NULL.
func sqlStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{
		String: s,
		Valid:  true,
	}
}

// extractSQLInt64 turns a NullInt64 into a numerical type. This can be useful
// when reading directly from the database, as this function handles extracting
// the inner value from the "option"-like struct.
//
//nolint:unused
func extractSQLInt64[T constraints.Integer](num sql.NullInt64) T {
	return T(num.Int64)
}

// extractSQLInt32 turns a NullInt32 into a numerical type. This can be useful
// when reading directly from the database, as this function handles extracting
// the inner value from the "option"-like struct.
//
//nolint:unused
func extractSQLInt32[T constraints.Integer](num sql.NullInt32) T {
	return T(num.Int32)
}

// extractSQLInt16 turns a NullInt16 into a numerical type. This can be useful
// when reading directly from the database, as this function handles extracting
// the inner value from the "option"-like struct.
//
//nolint:unused
func extractSQLInt16[T constraints.Integer](num sql.NullInt16) T {
	return T(num.Int16)
}

// extractBool turns a NullBool into a boolean. This can be useful when reading
// directly from the database, as this function handles extracting the inner
// value from the "option"-like struct.
//
//nolint:unused
func extractBool(b sql.NullBool) bool {
	return b.Bool
}

// fMapKeys extracts the set of keys from a map, applies the function f to each
// element and returns the results in a new slice.
//
//nolint:unused
func fMapKeys[K comparable, V, R any](m map[K]V, f func(K) R) []R {
	keys := make([]R, 0, len(m))
	for k := range m {
		r := f(k)
		keys = append(keys, r)
	}

	return keys
}

// fMap takes an input slice, and applies the function f to each element,
// yielding a new slice.
//
//nolint:unused
func fMap[T1, T2 any](s []T1, f func(T1) T2) []T2 {
	r := make([]T2, len(s))
	for i, v := range s {
		r[i] = f(v)
	}

	return r
}

// mergeMap adds all the values that are in map b to map a.
//
//nolint:unused
func mergeMap[K comparable, V any](a, b map[K]V) map[K]V {
	for k, v := range b {
		a[k] = v
	}

	return a
}

// noError1 calls a function with 1 argument and verifies that no error is
// returned. If the error is nil, then the value is returned.
//
//nolint:unused
func noError1[T any, Q any](t *testing.T, f func(Q) (T, error), args Q) T {
	v, err := f(args)
	require.NoError(t, err)

	return v
}

// parseCoalesceNumericType parses a value that is expected to be a numeric
// value into a numeric type.
//
//nolint:unused
func parseCoalesceNumericType[T constraints.Integer](value any) (T, error) {
	switch typedValue := value.(type) {
	case int64:
		return T(typedValue), nil

	case int32:
		return T(typedValue), nil

	case int16:
		return T(typedValue), nil

	case int8:
		return T(typedValue), nil

	case string:
		parsedValue, err := strconv.ParseInt(typedValue, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("unable to parse value '%v' as "+
				"number: %w", value, err)
		}

		return T(parsedValue), nil

	default:
		return 0, fmt.Errorf("unexpected column type '%T' to parse "+
			"value '%v' as number", value, value)
	}
}

// InsertTestdata reads the given file and inserts its content into the given
// database. The file should contain valid SQL statements.
func InsertTestdata(t *testing.T, db *BaseDB, filePath string) {
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, &BaseTxOptions{readOnly: false})
	require.NoError(t, err)

	// Test helper reading a caller-specified fixture path.
	testData, err := os.ReadFile(filePath) //nolint:gosec // G304
	require.NoError(t, err)

	testDataStr := string(testData)

	// If we're using Postgres, we need to convert the SQLite hex literals
	// (X'<hex>') to Postgres hex literals ('\x<hex>').
	if db.Backend() == sqlc.BackendTypePostgres {
		rex := regexp.MustCompile(`X'([0-9a-f]+?)'`)
		testDataStr = rex.ReplaceAllString(testDataStr, `'\x$1'`)
	}

	_, err = tx.Exec(testDataStr)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}
