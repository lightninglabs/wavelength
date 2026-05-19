package darepoclicommands

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidatePaymentHashHappyPath confirms a canonical 64-hex payment
// hash passes the validator.
func TestValidatePaymentHashHappyPath(t *testing.T) {
	t.Parallel()

	hash := strings.Repeat("ab", 32)
	require.NoError(t, validatePaymentHash(hash))
}

// TestValidatePaymentHashRejectsAgentHallucinations exhaustively
// covers the agent-hallucination patterns the validator catches
// before the daemon ever sees the payment_hash: empty, wrong length,
// embedded whitespace, query/fragment suffix, non-hex chars.
func TestValidatePaymentHashRejectsAgentHallucinations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"empty",
			"",
			"required",
		},
		{
			"short",
			strings.Repeat("a", 32),
			"64 hex chars",
		},
		{
			"long",
			strings.Repeat("a", 65),
			"64 hex chars",
		},
		{
			"trailing space",
			strings.Repeat("a", 64) + " ",
			"whitespace",
		},
		{
			"query suffix",
			strings.Repeat("a", 60) + "?fmt",
			"query/fragment",
		},
		{
			"non-hex char",
			strings.Repeat("a", 63) + "z",
			"non-hex",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePaymentHash(tc.in)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestValidateInvoiceRejectsAgentHallucinations confirms the invoice
// pre-screen catches the patterns the daemon's bech32 decoder would
// otherwise have to produce confusing errors for: whitespace,
// embedded NULs and control bytes, query/fragment suffixes.
func TestValidateInvoiceRejectsAgentHallucinations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"empty",
			"",
			"required",
		},
		{
			"whitespace",
			"lnbcrt100 1pwlqxyz",
			"whitespace",
		},
		{
			"control char",
			"lnbcrt100\x01pwlqxyz",
			"control",
		},
		{
			"query suffix",
			"lnbcrt100?fmt=json",
			"query/fragment",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInvoice(tc.in)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestValidateInvoiceAcceptsCanonical confirms a plain BOLT-11
// invoice passes — the validator is intentionally lax beyond
// rejecting the early garbage class; bech32 decoding is the daemon's
// job.
func TestValidateInvoiceAcceptsCanonical(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateInvoice("lnbcrt100u1pwlqxyz"))
}
