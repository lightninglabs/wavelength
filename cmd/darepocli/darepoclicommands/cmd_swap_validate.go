package darepoclicommands

import (
	"fmt"
	"strings"
)

// validatePaymentHash enforces the canonical 64-char hex payment-hash
// shape before a swap RPC ever sees it. Agents routinely paste
// payment hashes copied out of invoices or block explorers and
// occasionally include surrounding whitespace, a leading 0x prefix,
// or a stray query suffix; catching those locally keeps the daemon's
// error surface focused on real not-found cases.
func validatePaymentHash(s string) error {
	if s == "" {
		return fmt.Errorf("payment_hash is required")
	}
	if strings.ContainsAny(s, " \t\n\r?#") {
		return fmt.Errorf("payment_hash contains whitespace or "+
			"query/fragment characters (got %q)", s)
	}
	if len(s) != 64 {
		return fmt.Errorf("payment_hash must be 64 hex chars (got "+
			"%d in %q)", len(s), s)
	}
	for _, c := range s {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') ||
			(c >= 'A' && c <= 'F')
		if !isHex {
			return fmt.Errorf("payment_hash contains non-hex "+
				"character %q", c)
		}
	}

	return nil
}

// validateInvoice rejects obvious agent-hallucination patterns in a
// BOLT-11 invoice string before the daemon attempts the authoritative
// parse. We do NOT decode the bech32 envelope here: the daemon owns
// invoice validation and the CLI just trims an early class of garbage
// (whitespace, embedded NULs, query-string suffixes) so the daemon's
// error surface stays focused on legitimate decode failures.
func validateInvoice(s string) error {
	if s == "" {
		return fmt.Errorf("invoice is required")
	}
	if strings.ContainsAny(s, " \t\n\r") {
		return fmt.Errorf("invoice contains whitespace; got %q", s)
	}
	if strings.ContainsAny(s, "?#") {
		return fmt.Errorf("invoice contains query/fragment characters "+
			"(got %q)", s)
	}
	for i, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invoice contains control character "+
				"at byte %d (0x%02x)", i, r)
		}
	}

	return nil
}
