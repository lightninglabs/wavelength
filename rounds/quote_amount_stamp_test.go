package rounds

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/stretchr/testify/require"
)

// TestApplyQuoteAmountsStampsVTXODescriptor is a direct regression
// test for the bug that left the commitment-tx builder with
// zero-sat VTXO descriptors when the client marked its change
// output with IsChange=true and a zero target amount. After the
// stamper runs, reg.VTXODescriptors[key].Amount must carry the
// quote's residual — otherwise the tree the server builds would
// not match the quote the server fanned out to the client, and
// client-side leaf validation at CommitmentTxReceivedState would
// fail. Positional alignment between Quote.VTXOAmounts and
// IntentVTXOReqs is the contract the stamper relies on.
func TestApplyQuoteAmountsStampsVTXODescriptor(t *testing.T) {
	t.Parallel()

	// Build two VTXO requests: position 0 is a fixed non-change
	// target echoed verbatim, position 1 is the change output the
	// quote fills with the residual.
	fixedReq := newTestVTXORequest(t, btcutil.Amount(400_000), false)
	changeReq := newTestVTXORequest(t, btcutil.Amount(0), true)

	fixedKey := signingKeyVertex(fixedReq)
	changeKey := signingKeyVertex(changeReq)

	reg := &ClientRegistration{
		IntentVTXOReqs: []*types.VTXORequest{
			fixedReq,
			changeReq,
		},
		VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{
			fixedKey: {
				// Stale intent-time value; the stamper must
				// overwrite with the quote's positional entry.
				Amount: btcutil.Amount(400_000),
			},
			changeKey: {
				// Zero at intent time — this is the sentinel
				// that would produce a 0-sat tree leaf if the
				// stamper failed to overwrite.
				Amount: btcutil.Amount(0),
			},
		},
	}

	// A quote that echoes the fixed target and stamps 599_745 on
	// the change slot (the residual after the fictional fee).
	q := &Quote{
		VTXOAmounts: []btcutil.Amount{
			btcutil.Amount(400_000),
			btcutil.Amount(599_745),
		},
		RejectReason: QuoteReasonOK,
	}

	applyQuoteAmountsToRegistration(reg, q)

	require.Equal(
		t, btcutil.Amount(400_000),
		reg.VTXODescriptors[fixedKey].Amount,
		"fixed descriptor amount must echo the quote's value",
	)
	require.Equal(
		t, btcutil.Amount(599_745),
		reg.VTXODescriptors[changeKey].Amount, "change descriptor "+
			"must carry the residual, not the intent-time zero "+
			"that would produce a 0-sat leaf",
	)
}

// TestApplyQuoteAmountsStampsLeaveOutputs verifies the LeaveOutputs
// side of the stamper: LeaveOutputs[i].Value must be rewritten to
// match Quote.LeaveAmounts[i] before the commitment-tx builder
// materializes the on-chain leave outputs. Same failure mode as
// the VTXO path but for cooperative-leave amounts.
func TestApplyQuoteAmountsStampsLeaveOutputs(t *testing.T) {
	t.Parallel()

	// Two leave outputs: one fixed target, one change. Intent-time
	// values are both zero to simulate the client leaving the
	// server to fill them at seal time.
	reg := &ClientRegistration{
		IntentLeaveReqs: []*types.LeaveRequest{
			{
				Output: &wire.TxOut{
					Value: 0,
				},
				IsChange: false,
			},
			{
				Output: &wire.TxOut{
					Value: 0,
				},
				IsChange: true,
			},
		},
		LeaveOutputs: []*wire.TxOut{
			{
				Value: 0,
			},
			{
				Value: 0,
			},
		},
	}

	q := &Quote{
		LeaveAmounts: []btcutil.Amount{
			btcutil.Amount(250_000),
			btcutil.Amount(749_745),
		},
		RejectReason: QuoteReasonOK,
	}

	applyQuoteAmountsToRegistration(reg, q)

	require.Equal(t, int64(250_000), reg.LeaveOutputs[0].Value)
	require.Equal(t, int64(749_745), reg.LeaveOutputs[1].Value)
}

// TestApplyQuoteAmountsNilSafeNoOps confirms the stamper degrades
// gracefully when either side is nil. This matters during test
// harness setup paths and during reseals where the reg map may
// have been drained: the stamper must not panic on nil inputs.
func TestApplyQuoteAmountsNilSafeNoOps(t *testing.T) {
	t.Parallel()

	// Nil reg: must not panic.
	applyQuoteAmountsToRegistration(nil, &Quote{
		VTXOAmounts: []btcutil.Amount{btcutil.Amount(100)},
	})

	// Nil quote: must not panic, and must leave reg untouched.
	reg := &ClientRegistration{
		IntentVTXOReqs: []*types.VTXORequest{
			newTestVTXORequest(t, btcutil.Amount(42), true),
		},
	}
	applyQuoteAmountsToRegistration(reg, nil)
	require.Nil(
		t, reg.VTXODescriptors,
		"nil quote must not fabricate descriptor entries",
	)
}

// TestApplyQuoteAmountsShortSliceStopsEarly verifies the stamper's
// bounds check: if Quote.VTXOAmounts has fewer entries than
// IntentVTXOReqs (a builder bug we want to catch, not paper over),
// the stamper stamps what it can and leaves the rest alone rather
// than panicking with an index-out-of-range.
func TestApplyQuoteAmountsShortSliceStopsEarly(t *testing.T) {
	t.Parallel()

	req0 := newTestVTXORequest(t, btcutil.Amount(100), false)
	req1 := newTestVTXORequest(t, btcutil.Amount(200), true)

	key0 := signingKeyVertex(req0)
	key1 := signingKeyVertex(req1)

	reg := &ClientRegistration{
		IntentVTXOReqs: []*types.VTXORequest{
			req0,
			req1,
		},
		VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{
			key0: {
				Amount: 100,
			},
			key1: {
				Amount: 200,
			},
		},
	}

	// Only one entry in VTXOAmounts — simulates a malformed quote.
	// The stamper must stamp position 0 and leave position 1 at
	// its current value rather than panic.
	q := &Quote{
		VTXOAmounts: []btcutil.Amount{
			btcutil.Amount(999),
		},
		RejectReason: QuoteReasonOK,
	}

	applyQuoteAmountsToRegistration(reg, q)

	require.Equal(t, btcutil.Amount(999), reg.VTXODescriptors[key0].Amount)
	require.Equal(
		t, btcutil.Amount(200), reg.VTXODescriptors[key1].Amount,
		"out-of-range position must be left untouched, not panic",
	)
}
