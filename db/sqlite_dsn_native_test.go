//go:build !js || !wasm

package db

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSQLiteReadOnlyFileURI verifies that native drive letters stay in the
// path and reserved filename characters cannot become query or fragment data.
func TestSQLiteReadOnlyFileURI(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		uri  string
	}{
		{
			name: "posix",
			path: "/tmp/report #?.db",
			uri:  "file:///tmp/report%20%23%3F.db?mode=ro",
		},
		{
			name: "windows drive",
			path: "C:/Users/operator/report #?.db",
			uri: "file:///C:/Users/operator/" +
				"report%20%23%3F.db?mode=ro",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			uri := sqliteReadOnlyFileURI(test.path, url.Values{})
			require.Equal(t, test.uri, uri)
			parsed, err := url.Parse(uri)
			require.NoError(t, err)
			require.Empty(t, parsed.Host)
			require.Empty(t, parsed.Fragment)
			require.Equal(t, "ro", parsed.Query().Get("mode"))
		})
	}
}
