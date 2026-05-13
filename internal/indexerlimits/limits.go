package indexerlimits

import "fmt"

const (
	// MaxVTXOsByScriptsCursorBytes caps the opaque cursor accepted for
	// ListVTXOsByScripts pagination. The current server cursor is a
	// 36-byte outpoint keyset cursor; this leaves room for format evolution
	// while bounding untrusted remote bytes.
	MaxVTXOsByScriptsCursorBytes = 256
)

// ValidateVTXOsByScriptsCursor rejects opaque VTXO query cursors that exceed
// the client-side resource cap.
func ValidateVTXOsByScriptsCursor(cursor []byte) error {
	if len(cursor) <= MaxVTXOsByScriptsCursorBytes {
		return nil
	}

	return fmt.Errorf("vtxo cursor length %d exceeds limit %d", len(cursor),
		MaxVTXOsByScriptsCursorBytes)
}
