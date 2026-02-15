package db

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

func isPrintableUTF8(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}

	for _, r := range string(b) {
		if unicode.IsControl(r) {
			return false
		}
	}

	return true
}

func ownerForDisplay(owner []byte) string {
	if len(owner) == 0 {
		return ""
	}

	if isPrintableUTF8(owner) {
		return string(owner)
	}

	return fmt.Sprintf("hex:%x", owner)
}
