//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/rpc/oorpb"
	"github.com/lightninglabs/darepo/oor"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// stubLineageVBytesEstimator is a deterministic LineageVBytesEstimator
// used by the cap-rejection systest. It returns a fixed vbytes value
// regardless of the inputs/PSBTs supplied, letting the test drive any
// submit over the operator's configured cap without needing real
// chain-built lineages large enough to trip the threshold.
type stubLineageVBytesEstimator struct {
	vbytes uint32
	calls  int
}

// EstimateOORLineageVBytes implements oor.LineageVBytesEstimator with
// the configured fixed vbytes return.
func (s *stubLineageVBytesEstimator) EstimateOORLineageVBytes(
	_ context.Context, _ []wire.OutPoint,
	_ *psbt.Packet, _ []*psbt.Packet) (uint32, error) {

	s.calls++

	return s.vbytes, nil
}

// TestOORLineageCapTypedRejectE2E is the H-1 end-to-end systest: when
// the operator's MaxOORLineageVBytes cap rejects a submit, the typed
// proto SubmitPackageRejection (carrying OOR_REJECT_LINEAGE_TOO_LARGE)
// must reach the client over real production transport so the client
// side can recover the typed error via
// oorpb.ParseSubmitPackageResponse / errors.As(*SubmitRejectedError).
//
// Before H-1, the actor's *FailedState branch returned only a generic
// Go error string and pushed nothing to the client; the client would
// have hung waiting for a response that never arrived. This test
// fails closed against that regression by:
//
//  1. Configuring a tight cap (1 vB) plus a stub estimator that
//     reports an arbitrarily-large vbytes value, so any well-formed
//     submit trips the cap on the operator side.
//  2. Driving Alice's OOR submit through real clientconn transport.
//  3. Asserting the InstrumentedMailbox transcript records a
//     server-to-client SubmitPackageResponse whose decoded body
//     carries the Rejection oneof branch, with Code ==
//     OOR_REJECT_LINEAGE_TOO_LARGE.
//  4. Asserting Alice's source VTXO stays LIVE — the cap check runs
//     before LockInputsReq, so a phantom forfeit transition cannot
//     fire.
//  5. Asserting NO FinalizePackageRequest C2S envelope appears, since
//     the session never reaches CoSigned.
func TestOORLineageCapTypedRejectE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OOR lineage cap e2e test in short mode")
	}

	t.Parallel()

	const (
		boardingAmount = btcutil.Amount(100_000)
		roundTimeout   = 120 * time.Second
		rejectTimeout  = 30 * time.Second
		// stubVBytes is well above the configured cap so any submit
		// trips the threshold deterministically.
		stubVBytes uint32 = 50_000
		tightCap   uint32 = 1
	)

	estimator := &stubLineageVBytesEstimator{vbytes: stubVBytes}

	h := NewE2EHarness(t, WithOORDriverMutator(func(cfg *oor.DriverCfg) {
		cfg.MaxOORLineageVBytes = tightCap
		cfg.LineageVBytesEstimator = estimator
	}))
	h.Start()
	h.FundServerWallet(btcutil.Amount(1_000_000))

	alice := NewTestClient(h)
	bob := NewTestClient(h)

	ctx := t.Context()

	// Phase 1: Alice boards and gets a live VTXO from a confirmed
	// round so she has something to spend in the cap-rejected
	// submit.
	boardClientIntoConfirmedRound(
		ctx, t, h, alice, boardingAmount, roundTimeout,
	)

	aliceVTXOs, err := alice.ListVTXOs(ctx)
	require.NoError(t, err, "alice: list vtxos after round")
	require.NotEmpty(t, aliceVTXOs,
		"alice must have VTXOs after round")

	var totalAmount btcutil.Amount
	for _, v := range aliceVTXOs {
		totalAmount += v.Amount
	}

	// Phase 2: Get Bob's recipient pkScript so Alice has a
	// well-formed recipient to submit to.
	bobPkScript, err := bob.OORReceivePkScript()
	require.NoError(t, err, "bob: derive P2TR pkScript")

	// Clear the transcript so the rejection's S2C
	// SubmitPackageResponse stands out from boarding-phase noise.
	h.Transcript().Clear()

	// Phase 3: Alice submits the OOR. The submit is well-formed
	// (it would succeed under the default cap); the operator
	// rejects it because the stub estimator reports vbytes well
	// above the configured 1 vB cap.
	//
	// SendOOR returns nil because the submit reaches the server
	// successfully — the rejection arrives asynchronously via the
	// SubmitPackageResponse the actor pushes back through
	// clientconn after FailedState.
	err = alice.SendOOR(ctx, t, bobPkScript, totalAmount)
	require.NoError(t, err, "alice: send OOR (submit reaches server)")

	// Phase 4: Wait for the typed rejection to appear in the
	// transcript. The InstrumentedMailbox records every S2C
	// envelope; the cap-reject path produces exactly one
	// SubmitPackageResponse whose decoded body carries the
	// Rejection oneof branch.
	var rejection *oorpb.SubmitPackageRejection
	require.Eventually(t, func() bool {
		entries := h.Transcript().Entries()
		for _, entry := range entries {
			if entry.Direction != ServerToClient {
				continue
			}
			if entry.MsgType != "SubmitPackageResponse" {
				continue
			}
			if entry.Envelope == nil ||
				entry.Envelope.Body == nil {

				continue
			}

			msg, unmarshalErr := anypb.UnmarshalNew(
				entry.Envelope.Body,
				proto.UnmarshalOptions{},
			)
			if unmarshalErr != nil {
				continue
			}

			resp, ok := msg.(*oorpb.SubmitPackageResponse)
			if !ok {
				continue
			}

			rej, ok := resp.Result.(*oorpb.SubmitPackageResponse_Rejection)
			if !ok {
				// Success-branch responses indicate the
				// cap check did not fire; keep polling
				// until the typed rejection arrives.
				continue
			}

			rejection = rej.Rejection

			return true
		}

		return false
	}, rejectTimeout, 250*time.Millisecond,
		"client must observe an S2C SubmitPackageResponse carrying "+
			"the typed Rejection oneof branch")

	// The estimator must have been called: otherwise the cap
	// check did not run on the operator side, in which case the
	// rejection we just observed would have come from a
	// different code path (the test fixture is only valid when
	// our injected estimator drove the rejection).
	require.GreaterOrEqual(t, estimator.calls, 1,
		"stub estimator must have driven the cap rejection")

	// The typed code is the load-bearing assertion: future
	// client-side fallback (#248) routes on
	// OOR_REJECT_LINEAGE_TOO_LARGE without string-matching the
	// human-readable reason. A regression that emits the rejection
	// without the typed code would compile-pass but break the
	// downstream classifier silently.
	require.NotNil(t, rejection)
	require.Equal(t,
		oorpb.OORRejectCode_OOR_REJECT_LINEAGE_TOO_LARGE,
		rejection.Code,
		"server's rejection must carry the typed cap code")
	require.NotEmpty(t, rejection.Reason,
		"rejection reason must surface the offending vbytes "+
			"and cap so operators can diagnose")
	require.NotEmpty(t, rejection.SessionId,
		"rejection must echo session_id so the client EventRouter "+
			"can route the failure rather than stall the cursor")

	// Phase 5: The cap check runs in handleValidateSubmit BEFORE
	// LockInputsReq (per oor/CLAUDE.md "submit validation
	// precedes VTXO locking" invariant). Alice's source VTXO
	// must remain LIVE indefinitely under the tight cap — no
	// phantom forfeit transition can happen.
	liveAfter, err := alice.ListLiveVTXOs(ctx)
	require.NoError(t, err, "alice: list live vtxos after rejection")
	require.NotEmpty(t, liveAfter,
		"alice's VTXO must remain LIVE under cap rejection — the "+
			"rejection path runs before LockInputsReq so no "+
			"forfeit transition can fire")

	// Phase 6: The session never reached CoSigned, so no
	// FinalizePackageRequest can be enqueued. Asserting absence
	// catches a regression where a partial-progress path
	// accidentally lets the FSM continue past the failure.
	for _, entry := range h.Transcript().Entries() {
		require.NotEqual(t, "FinalizePackageRequest", entry.MsgType,
			"a cap-rejected session must never reach finalize")
	}
}
