//go:build !js || !wasm

package db

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// sqliteNativeDSN preserves the daemon's ordinary DSN and uses a properly
// escaped file URI for read-only inspection of an existing native database.
func sqliteNativeDSN(cfg SQLiteOpenConfig, options url.Values) (string, error) {
	if !cfg.ReadOnly {
		return fmt.Sprintf("%s?%s", cfg.DatabaseFileName,
			options.Encode()), nil
	}

	path, err := filepath.Abs(cfg.DatabaseFileName)
	if err != nil {
		return "", fmt.Errorf("resolve SQLite path: %w", err)
	}

	return sqliteReadOnlyFileURI(filepath.ToSlash(path), options), nil
}

// sqliteReadOnlyFileURI encodes an absolute slash-separated native path. A
// Windows drive must be part of the URI path, not interpreted as an authority.
func sqliteReadOnlyFileURI(path string, options url.Values) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	options.Set("mode", "ro")
	uri := url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: options.Encode(),
	}

	return uri.String()
}
