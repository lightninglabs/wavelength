//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestDiagnosticOORRecordsRetainUnknownAndFailedSessions verifies that
// optional package proof and swap linkage never erase operational status.
func TestDiagnosticOORRecordsRetainUnknownAndFailedSessions(t *testing.T) {
	completed := waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED
	failed := waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED
	outgoing := waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	incoming := waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING
	claimTxid := chainhash.Hash{1, 2, 3}
	listedClaim := claimTxid.String()
	page := &waverpc.ListOORSessionsResponse{
		NextPageToken: "older",
		Sessions: []*waverpc.OORSessionInfo{
			{
				SessionId: "ancestor",
				Direction: incoming,
				Status:    completed,
			},
			{
				SessionId: "legacy",
				Direction: incoming,
				Status:    completed,
			},
			{
				SessionId: listedClaim,
				Direction: outgoing,
				Status:    completed,
			},
			{
				SessionId:     "failed",
				Direction:     incoming,
				Status:        failed,
				FailureReason: "operator timed out",
			},
			{
				SessionId: "send",
				Direction: outgoing,
				Status:    completed,
			},
			{
				SessionId: "unknown",
				Direction: outgoing,
				Status:    completed,
			},
		},
	}
	packages := map[string]db.OORDiagnosticPackage{
		"ancestor": {
			RecoveryAncestor: true,
		}, "legacy": {},
		"failed": {
			RecoveryAncestor: true,
		},
	}
	claim := &swapclientrpc.SwapSummary{
		Direction:      swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE,
		ClaimSessionId: oor.SessionID(claimTxid).String(),
		PaymentHash:    "parent",
		AmountSat:      800,
		Invoice:        "private invoice",
		Preimage:       "proof of payment",
	}
	readAmount := func(_ context.Context, id string, outgoing bool) (*int64,
		error) {

		switch {
		case id == listedClaim:
			if outgoing {
				return nil, errors.New("unreadable request")
			}
			amount := int64(1200)

			return &amount, nil

		case id == "send":
			amount := int64(1800)

			return &amount, nil

		case id == "unknown":
			return nil, errors.New("unreadable proof")

		default:
			return nil, nil
		}
	}
	records := describeOORRecords(
		t.Context(), page.Sessions, packages,
		[]*swapclientrpc.SwapSummary{claim}, readAmount,
	)
	raw, err := marshalDiagnostic(page, &diagnosticOORPage{
		records: records, SwapEvidenceUnavailable: true,
	})
	require.NoError(t, err)
	var decoded struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Len(t, decoded.Sessions, 6)
	require.Contains(t, string(raw), `"next_page_token":"older"`)
	require.Contains(t, string(raw), `"swap_evidence_unavailable":true`)
	require.JSONEq(
		t, `{"kind":"recovery_package"}`,
		string(decoded.Sessions[0]["diagnostic"]),
	)
	require.JSONEq(
		t, `{"kind":"stored_package"}`,
		string(decoded.Sessions[1]["diagnostic"]),
	)
	require.NotContains(t, decoded.Sessions[2], "amount_sat")
	require.Contains(t, string(raw), `"claimed_amount_sat":"1200"`)
	require.Contains(
		t, string(decoded.Sessions[2]["diagnostic"]),
		`"amount_read_failed":true`,
	)
	require.NotContains(
		t, string(decoded.Sessions[2]["diagnostic"]),
		"claimed_amount_read_failed",
	)
	require.Contains(
		t, string(decoded.Sessions[2]["diagnostic"]),
		`"amount_sat":"800"`,
	)
	require.Contains(t, string(raw), `"payment_hash":"parent"`)
	require.NotContains(t, string(raw), claim.Invoice)
	require.NotContains(t, string(raw), claim.Preimage)
	require.NotEmpty(t, claim.Preimage)
	require.NotContains(t, decoded.Sessions[3], "diagnostic")
	require.JSONEq(
		t, `"operator timed out"`,
		string(decoded.Sessions[3]["failure_reason"]),
	)
	require.JSONEq(t, `"1800"`, string(decoded.Sessions[4]["amount_sat"]))
	require.NotContains(t, decoded.Sessions[5], "amount_sat")
	require.JSONEq(
		t, `{"amount_read_failed":true}`,
		string(decoded.Sessions[5]["diagnostic"]),
	)
}

// TestDiagnosticClaimSwapsRequiresUniqueReceiveEvidence rejects ambiguous
// references and non-claim IDs as explanations for ordinary OOR transfers.
func TestDiagnosticClaimSwapsRequiresUniqueReceiveEvidence(t *testing.T) {
	receive := swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE
	pay := swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY
	want := &swapclientrpc.SwapSummary{
		Direction: receive, ClaimSessionId: "ok", PaymentHash: "receive",
	}
	claims := diagnosticClaimSwaps([]*swapclientrpc.SwapSummary{
		want,
		{Direction: receive, ClaimSessionId: "duplicate", PaymentHash: "a"},
		{Direction: receive, ClaimSessionId: "duplicate", PaymentHash: "b"},
		{Direction: receive, ClaimSessionId: "duplicate", PaymentHash: "c"},
		{Direction: pay, ClaimSessionId: "pay", PaymentHash: "pay"},
		{Direction: pay, FundingSessionId: "funding", PaymentHash: "fund"},
	})
	require.Equal(t, map[string]*swapclientrpc.SwapSummary{
		"ok": want, "duplicate": nil,
	}, claims)
}

// TestDiagnosticCreditLedgerDirectionKeepsWireValue verifies the bridge
// publishes lowercase ledger directions alongside protobuf operation states.
func TestDiagnosticCreditLedgerDirectionKeepsWireValue(t *testing.T) {
	raw, err := marshalDiagnostic(&swapclientrpc.ListCreditsResponse{
		Operations: []*swapclientrpc.CreditOperation{{
			OperationId: "op",
			State: swapclientrpc.
				CreditOperationState_CREDIT_OPERATION_STATE_DEBITED,
		}},
		LedgerEntries: []*swapclientrpc.CreditLedgerEntry{{
			OperationId: "op", Direction: "debit", AmountSat: 40,
		}},
	}, nil)
	require.NoError(t, err)
	require.Contains(
		t, string(raw), `"state":"CREDIT_OPERATION_STATE_DEBITED"`,
	)
	require.Contains(t, string(raw), `"direction":"debit"`)
	require.Contains(t, string(raw), `"amount_sat":"40"`)
}
