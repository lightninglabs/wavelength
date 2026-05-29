package darepoclicommands

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestValidateDestinationRejectsAgentHallucinations confirms the
// destination validator catches the agent-hallucination patterns the
// CLI specifically guards against: embedded query params / fragments,
// whitespace, and empty strings.
func TestValidateDestinationRejectsAgentHallucinations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		dest   string
		errIs  string
		expect bool
	}{
		// Happy paths.
		{
			"lnbcrt100u1pwlqxyz...",
			"",
			true,
		},
		{
			"bcrt1q0123abc",
			"",
			true,
		},

		// Empty.
		{
			"",
			"required",
			false,
		},

		// Embedded query / fragment.
		{
			"lnbcrt100?fields=amt",
			"query/fragment",
			false,
		},
		{
			"bcrt1q0123#alias",
			"query/fragment",
			false,
		},

		// Whitespace.
		{
			"bcrt1q0123 ",
			"whitespace",
			false,
		},
		{
			"\tlnbcrt100",
			"whitespace",
			false,
		},
	}
	for _, tc := range cases {
		err := validateDestination(tc.dest)
		if tc.expect {
			require.NoError(t, err, "dest=%q", tc.dest)

			continue
		}
		require.Error(t, err, "dest=%q", tc.dest)
		require.ErrorContains(t, err, tc.errIs, "dest=%q", tc.dest)
	}
}

// TestValidateFreeTextRejectsControlCharacters confirms note/memo
// validation rejects control characters but accepts ordinary UTF-8.
func TestValidateFreeTextRejectsControlCharacters(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateFreeText("--note", ""))
	require.NoError(t, validateFreeText("--note", "hello world"))
	require.NoError(t, validateFreeText("--note", "café ☕"))

	// Tab is a control char.
	err := validateFreeText("--note", "tab\there")
	require.ErrorContains(t, err, "control character")

	// Bell.
	err = validateFreeText("--memo", "bell\x07here")
	require.ErrorContains(t, err, "control character")

	// DEL (0x7f) is a control char too.
	err = validateFreeText("--note", "del\x7fchar")
	require.ErrorContains(t, err, "control character")
}

// TestDryRunInvoiceDetailsAcceptsMatchingNetwork confirms offchain dry-run
// validation fully decodes a BOLT-11 invoice on the daemon network and returns
// the human-confirmable amount, hash, and expiry fields for the preview.
func TestDryRunInvoiceDetailsAcceptsMatchingNetwork(t *testing.T) {
	t.Parallel()

	created := time.Unix(1_700_000_000, 0)
	invoice, paymentHash := testBolt11Invoice(
		t, &chaincfg.SigNetParams, 12_345, created,
	)

	preview, err := dryRunInvoiceDetails(invoice, "signet")
	require.NoError(t, err)
	require.Equal(t, "signet", preview.Network)
	require.EqualValues(t, 12_345, preview.AmountSat)
	require.Equal(t, paymentHash, preview.PaymentHash)
	require.Equal(t, created.Unix(), preview.CreatedAtUnix)
	require.Equal(
		t, created.Add(30*time.Minute).Unix(), preview.ExpiresAtUnix,
	)
	require.EqualValues(t, 1_800, preview.ExpirySeconds)
}

// TestDryRunInvoiceDetailsRejectsWrongNetwork confirms dry-run rejects a
// syntactically valid invoice when its BOLT-11 HRP is bound to a different
// chain than the daemon reports.
func TestDryRunInvoiceDetailsRejectsWrongNetwork(t *testing.T) {
	t.Parallel()

	invoice, _ := testBolt11Invoice(
		t, &chaincfg.MainNetParams, 1_000, time.Unix(1, 0),
	)

	_, err := dryRunInvoiceDetails(invoice, "signet")
	require.ErrorContains(
		t, err,
		`invoice HRP "lnbc" is for mainnet; daemon is on signet`,
	)
}

// TestDryRunInvoiceDetailsRejectsMalformedInvoice confirms dry-run no longer
// treats a non-decodable invoice-looking string as validated just because the
// destination is non-empty.
func TestDryRunInvoiceDetailsRejectsMalformedInvoice(t *testing.T) {
	t.Parallel()

	_, err := dryRunInvoiceDetails("lntbs1asdfasdfasdfasdf", "signet")
	require.ErrorContains(t, err, "decode invoice")
}

// TestValidateOutpointEnforcesShape confirms the outpoint validator
// requires the canonical txid:vout form and rejects malformed input
// up front so the daemon never sees obvious garbage.
func TestValidateOutpointEnforcesShape(t *testing.T) {
	t.Parallel()

	good := "abcdef01" + "abcdef01abcdef01abcdef01abcdef01abcdef01" +
		"abcdef01abcdef01" + ":0"
	require.NoError(t, validateOutpoint(good))

	cases := []struct {
		op    string
		errIs string
	}{
		{
			"",
			"required",
		},
		{
			"abc",
			"txid:vout",
		},
		{
			"abc:0:1",
			"txid:vout",
		},
		{
			"shorttxid:0",
			"64 hex",
		},
		// Non-hex character in txid (length 64, 'g' at position 0).
		{
			"g" +
				"abcdef01abcdef01abcdef01abcdef01" +
				"abcdef01abcdef01abcdef01abcdef0" + ":0",
			"non-hex",
		},
		// Empty vout.
		{
			"abcdef01abcdef01abcdef01abcdef01abcdef01" +
				"abcdef01abcdef01abcdef01:",
			"vout is empty",
		},
	}
	for _, tc := range cases {
		err := validateOutpoint(tc.op)
		require.Error(t, err, "op=%q", tc.op)
		require.ErrorContains(t, err, tc.errIs, "op=%q", tc.op)
	}
}

// TestResolveOffchainFlagDefaultsToOffchain confirms absence of either
// flag implies offchain (the default for send/recv).
func TestResolveOffchainFlagDefaultsToOffchain(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().Bool("offchain", false, "")
	cmd.Flags().Bool("onchain", false, "")

	offchain, err := resolveOffchainFlag(cmd)
	require.NoError(t, err)
	require.True(t, offchain)
}

// TestResolveOffchainFlagOnchainOverridesDefault confirms --onchain
// flips the default.
func TestResolveOffchainFlagOnchainOverridesDefault(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().Bool("offchain", false, "")
	cmd.Flags().Bool("onchain", false, "")
	require.NoError(t, cmd.Flags().Set("onchain", "true"))

	offchain, err := resolveOffchainFlag(cmd)
	require.NoError(t, err)
	require.False(t, offchain)
}

// TestResolveOffchainFlagRejectsConflict confirms the validator
// rejects both flags at once.
func TestResolveOffchainFlagRejectsConflict(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().Bool("offchain", false, "")
	cmd.Flags().Bool("onchain", false, "")
	require.NoError(t, cmd.Flags().Set("offchain", "true"))
	require.NoError(t, cmd.Flags().Set("onchain", "true"))

	_, err := resolveOffchainFlag(cmd)
	require.ErrorIs(t, err, errOffchainOnchainConflict)
}

// TestParseListView confirms each accepted view string maps cleanly
// onto the proto enum and unknown values are rejected.
func TestParseListView(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want walletdkrpc.ListView
		bad  bool
	}{
		{
			"",
			walletdkrpc.ListView_LIST_VIEW_ACTIVITY,
			false,
		},
		{
			"activity",
			walletdkrpc.ListView_LIST_VIEW_ACTIVITY,
			false,
		},
		{
			"ACTIVITY",
			walletdkrpc.ListView_LIST_VIEW_ACTIVITY,
			false,
		},
		{
			"vtxos",
			walletdkrpc.ListView_LIST_VIEW_VTXOS,
			false,
		},
		{
			"vtxo",
			walletdkrpc.ListView_LIST_VIEW_VTXOS,
			false,
		},
		{
			"onchain",
			walletdkrpc.ListView_LIST_VIEW_ONCHAIN,
			false,
		},
		{
			"on-chain",
			walletdkrpc.ListView_LIST_VIEW_ONCHAIN,
			false,
		},
		{
			"junk",
			walletdkrpc.ListView_LIST_VIEW_UNSPECIFIED,
			true,
		},
	}
	for _, tc := range cases {
		v, err := parseListView(tc.in)
		if tc.bad {
			require.Error(t, err, "in=%q", tc.in)

			continue
		}
		require.NoError(t, err, "in=%q", tc.in)
		require.Equal(t, tc.want, v, "in=%q", tc.in)
	}
}

// TestParseEntryKindAcceptsCanonicalForms confirms the kind parser
// accepts the canonical names plus the obvious aliases ("receive" for
// "recv").
func TestParseEntryKindAcceptsCanonicalForms(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want walletdkrpc.EntryKind
		bad  bool
	}{
		{
			"send",
			walletdkrpc.EntryKind_ENTRY_KIND_SEND,
			false,
		},
		{
			"recv",
			walletdkrpc.EntryKind_ENTRY_KIND_RECV,
			false,
		},
		{
			"receive",
			walletdkrpc.EntryKind_ENTRY_KIND_RECV,
			false,
		},
		{
			"deposit",
			walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
			false,
		},
		{
			"exit",
			walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
			false,
		},
		{
			"",
			walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			true,
		},
		{
			"junk",
			walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			true,
		},
	}
	for _, tc := range cases {
		k, err := parseEntryKind(tc.in)
		if tc.bad {
			require.Error(t, err, "in=%q", tc.in)

			continue
		}
		require.NoError(t, err, "in=%q", tc.in)
		require.Equal(t, tc.want, k, "in=%q", tc.in)
	}
}

// TestWalletCommandsRejectUnexpectedArgs confirms wallet verbs do not silently
// ignore positional input. This is especially important for secret-bearing
// commands where an argv typo can leak sensitive material to shell history.
func TestWalletCommandsRejectUnexpectedArgs(t *testing.T) {
	t.Parallel()

	commands := []*cobra.Command{
		newBalanceCmd(),
		newCreateCmd(),
		newUnlockCmd(),
		newRecvCmd(),
		newActivityCmd(),
		newExitCmd(),
		newExitStatusCmd(),
	}

	for _, cmd := range commands {
		require.Error(t, cmd.Args(cmd, []string{"unexpected"}), cmd.Use)
	}
}

// testBolt11Invoice creates a signed BOLT-11 invoice for CLI-only dry-run
// validation tests.
func testBolt11Invoice(t *testing.T, params *chaincfg.Params, amountSat uint64,
	created time.Time) (string, string) {

	t.Helper()

	var paymentHash [32]byte
	for i := range paymentHash {
		paymentHash[i] = byte(i + 1)
	}

	invoice, err := zpay32.NewInvoice(
		params, paymentHash, created,
		zpay32.Amount(
			lnwire.MilliSatoshi(amountSat*1000),
		),
		zpay32.Description("dry run"),
		zpay32.Expiry(30*time.Minute),
	)
	require.NoError(t, err)

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	paymentRequest, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			return ecdsa.SignCompact(privKey, msg, true), nil
		},
	})
	require.NoError(t, err)

	return paymentRequest, "0102030405060708090a0b0c0d0e0f101112131415161" +
		"718191a1b1c1d1e1f20"
}
