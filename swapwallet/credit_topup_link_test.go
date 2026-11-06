//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// testTopupSessionID returns a deterministic 32-byte OOR session id and its
// raw hex encoding.
func testTopupSessionID(t *testing.T) ([]byte, string) {
	t.Helper()

	raw := make([]byte, chainhash.HashSize)
	for i := range raw {
		raw[i] = byte(i + 1)
	}

	return raw, hex.EncodeToString(raw)
}

// topupLedgerRow builds the OOR-send ledger row shape produced by a credit
// top-up transfer.
func topupLedgerRow(sessionID []byte, amountSat int64,
) *waverpc.TransactionHistoryEntry {

	return &waverpc.TransactionHistoryEntry{
		Type:          "oor",
		Subtype:       ledger.EventVTXOSent,
		DebitAccount:  ledger.AccountTransfersOut,
		CreditAccount: ledger.AccountVTXOBalance,
		AmountSat:     amountSat,
		SessionId:     sessionID,
		EntryId:       46,
	}
}

// TestCollectCreditTopupLinksLabelsTopupRow verifies the history merger can
// resolve a credit pay operation from the raw ledger row of its top-up OOR
// transfer, in either hex orientation, and relabels the entry so it no longer
// reads as an unexplained outgoing transfer (issue #989).
func TestCollectCreditTopupLinksLabelsTopupRow(t *testing.T) {
	t.Parallel()

	sessionID, sessionHex := testTopupSessionID(t)
	const payHash = "f87105d14f3c955ea2253dd0dd81a80db71e6f098df97fd2" +
		"c80350a6f0968338"

	registry := &fakeCreditRegistry{
		listResp: &credit.ListCreditOpsResponse{
			Ops: []credit.CreditOpSummary{
				{
					OpID:         "op-topup",
					OpKey:        "pay:" + payHash,
					Kind:         credit.KindPay,
					AmountSat:    500,
					TopupSat:     1_000,
					OORSessionID: sessionHex,
				},
				{
					// A receive op with an OOR session
					// must not produce a link.
					OpID:         "op-recv",
					OpKey:        "recv:aa",
					Kind:         credit.KindReceive,
					OORSessionID: "ff",
				},
			},
		},
	}

	h := &history{
		deps: &Deps{
			CreditRegistry: registry,
		},
		runtime: &Runtime{},
	}

	links := h.collectCreditTopupLinks(t.Context())
	require.NotEmpty(t, links)

	// The receive op must not contribute a link despite carrying an OOR
	// session id.
	require.NotContains(t, links, "ff")

	// The registry may have recorded either hex orientation of the
	// session id; both must resolve.
	reversed, err := chainhash.NewHash(sessionID)
	require.NoError(t, err)
	for _, key := range []string{sessionHex, reversed.String()} {
		link, ok := links[strings.ToLower(key)]
		require.True(t, ok, "missing link for %s", key)
		require.Equal(t, payHash, link.paymentHash)
		require.Equal(t, int64(1_000), link.topupSat)
	}

	row := topupLedgerRow(sessionID, 1_000)
	link, ok := creditTopupLinkForRow(row, row.GetAmountSat(), links)
	require.True(t, ok)

	entry, ok := walletEntryFromLedgerRow(row)
	require.True(t, ok)
	decorateCreditTopupEntry(entry, link)

	require.Equal(t, creditCounterparty, entry.GetCounterparty())
	require.Equal(
		t, creditTopupPhaseLabel, entry.GetProgress().GetPhaseLabel(),
	)
	require.Equal(t, payHash, entry.GetProgress().GetPaymentHash())

	// The amount stays the real VTXO outflow; the surplus above the paid
	// amount remains represented by the credit balance, not the feed.
	require.Equal(t, int64(-1_000), entry.GetAmountSat())
}

// TestCreditTopupLinkForRowIgnoresUnrelatedRows verifies non-top-up rows and
// empty link sets never match.
func TestCreditTopupLinkForRowIgnoresUnrelatedRows(t *testing.T) {
	t.Parallel()

	sessionID, sessionHex := testTopupSessionID(t)

	// Index both hex orientations, mirroring collectCreditTopupLinks, so
	// the shape checks below cannot pass merely because the display
	// orientation is missing from the map.
	reversed, err := chainhash.NewHash(sessionID)
	require.NoError(t, err)
	link := creditTopupLink{
		paymentHash: "aa",
		topupSat:    1_000,
	}
	links := map[string]creditTopupLink{
		sessionHex:                         link,
		strings.ToLower(reversed.String()): link,
	}

	// The matching send row resolves, so the negative cases below fail on
	// shape or amount, not on the map contents.
	_, ok := creditTopupLinkForRow(
		topupLedgerRow(sessionID, 1_000), 1_000, links,
	)
	require.True(t, ok)

	// A receive-shaped row with the same session id must not match.
	recvRow := topupLedgerRow(sessionID, 1_000)
	recvRow.Subtype = ledger.EventVTXOReceived
	recvRow.DebitAccount = ledger.AccountVTXOBalance
	recvRow.CreditAccount = ledger.AccountTransfersIn
	_, ok = creditTopupLinkForRow(recvRow, recvRow.GetAmountSat(), links)
	require.False(t, ok)

	// A send row whose outflow does not equal the recorded top-up amount
	// must not match: the session id resolved to an unrelated transfer.
	_, ok = creditTopupLinkForRow(
		topupLedgerRow(sessionID, 900), 900, links,
	)
	require.False(t, ok)

	// A send row with an unknown session id must not match.
	otherID := make([]byte, chainhash.HashSize)
	otherID[0] = 0xff
	_, ok = creditTopupLinkForRow(
		topupLedgerRow(otherID, 1_000), 1_000, links,
	)
	require.False(t, ok)

	// An empty link set short-circuits.
	_, ok = creditTopupLinkForRow(
		topupLedgerRow(sessionID, 1_000), 1_000, nil,
	)
	require.False(t, ok)
}

// TestCreditTopupWithChangeMatchesNetOutflow reproduces a small payment funded
// by a larger input and checks that only the exact net top-up is correlated.
func TestCreditTopupWithChangeMatchesNetOutflow(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		change  int64
		labeled bool
	}{
		{
			name:    "matching net top-up",
			change:  9_000,
			labeled: true,
		},
		{
			name:    "different net transfer",
			change:  8_500,
			labeled: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h, _, rpc := newHistoryFixture(t)
			session, sessionHex := testTopupSessionID(t)
			payHash := strings.Repeat("ab", 32)
			h.deps.CreditRegistry = &fakeCreditRegistry{
				listResp: &credit.ListCreditOpsResponse{
					Ops: []credit.CreditOpSummary{{
						OpID: "topup", OpKey: "pay:" +
							payHash,
						Kind: credit.KindPay, AmountSat: 999,
						TopupSat: 1_000, OORSessionID: sessionHex,
					}},
				},
			}
			send := topupLedgerRow(session, 10_000)
			send.ConfirmationStatus = "recorded"
			rpc.listTxResp = &waverpc.ListTransactionsResponse{
				Transactions: []*waverpc.TransactionHistoryEntry{
					send,
					{
						Type: "oor", Subtype: ledger.EventVTXOReceived,
						DebitAccount:       ledger.AccountVTXOBalance,
						CreditAccount:      ledger.AccountTransfersIn,
						ConfirmationStatus: "recorded",
						AmountSat:          test.change,
						Txid: testSessionString(
							t, session,
						),
						OutputIndex: 1, EntryId: 47,
					},
				},
			}
			result, err := h.List(
				t.Context(), &wavewalletrpc.ListRequest{},
			)
			require.NoError(t, err)
			entries := result.GetActivity().GetEntries()
			require.Len(t, entries, 1)
			entry := entries[0]
			require.Equal(
				t, test.change-10_000, entry.GetAmountSat(),
			)
			require.Equal(
				t, test.labeled,
				entry.GetProgress().GetPhaseLabel() == creditTopupPhaseLabel,
			)
			if test.labeled {
				require.Equal(
					t, payHash,
					entry.GetProgress().GetPaymentHash(),
				)
			} else {
				require.Empty(
					t, entry.GetProgress().GetPaymentHash(),
				)
			}
			require.Equal(t, int64(10_000), send.GetAmountSat())
		})
	}
}
