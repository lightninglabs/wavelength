package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolveSqliteSynchronous verifies that the configured synchronous level
// is normalized and validated: an empty value resolves to the safe default,
// each valid level passes through unchanged, and an unknown value is rejected
// so a typo surfaces at startup rather than silently weakening durability.
func TestResolveSqliteSynchronous(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{
			name:  "empty resolves to default normal",
			value: "",
			want:  defaultSqliteSynchronous,
		},
		{
			name:  "full passes through",
			value: SqliteSynchronousFull,
			want:  SqliteSynchronousFull,
		},
		{
			name:  "normal passes through",
			value: SqliteSynchronousNormal,
			want:  SqliteSynchronousNormal,
		},
		{
			name:  "off passes through",
			value: SqliteSynchronousOff,
			want:  SqliteSynchronousOff,
		},
		{
			name:  "uppercase normalizes to lowercase",
			value: "NORMAL",
			want:  SqliteSynchronousNormal,
		},
		{
			name:  "mixed case normalizes to lowercase",
			value: "Full",
			want:  SqliteSynchronousFull,
		},
		{
			name:    "unknown value is rejected",
			value:   "fsync",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveSqliteSynchronous(tc.value)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
