package waved

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// scaledEstimateFn prices a quote as a pure function of the request so
// tests covering several distinct (amount, remaining-blocks) pairs can
// assert each row was matched to its own quote: liquidity scales with
// amount, the other components are fixed.
func scaledEstimateFn(
	req *arkrpc.EstimateFeeRequest) *arkrpc.EstimateFeeResponse {

	liquidity := req.AmountSat / 1_000

	return &arkrpc.EstimateFeeResponse{
		LiquidityFeeSat: liquidity,
		OnchainShareSat: 25,
		MarginSat:       100,
		TotalFeeSat:     liquidity + 125,
	}
}

// newRefreshEstimateServer wires the minimal daemon surface the refresh
// dry-run preview needs — SQL VTXO store, chain tip, operator
// connection — and deliberately nothing wallet-related: the preview
// must work without a ready wallet.
func newRefreshEstimateServer(t *testing.T, svc *fakeArkService,
	height int32) (*RPCServer, *db.VTXOPersistenceStore) {

	t.Helper()

	sqlDB := db.NewTestDB(t)
	roundDB := db.NewTransactionExecutor(
		sqlDB.BaseDB,
		func(tx *sql.Tx) db.RoundStore {
			return sqlDB.WithTx(tx)
		},
		btclog.Disabled,
	)
	vtxoStore := db.NewVTXOPersistenceStore(
		roundDB, clock.NewDefaultClock(),
	)

	s := &Server{
		serverConn: newBufconnClient(t, svc),
		log:        btclog.Disabled,
		chainBackend: &heightOnlyChainBackend{
			height: height,
		},
		vtxoStore: vtxoStore,
	}

	return &RPCServer{server: s}, vtxoStore
}

// newRefreshEstimateVTXO builds a persistable live VTXO descriptor with
// the amount and batch expiry the estimate under test needs.
func newRefreshEstimateVTXO(t *testing.T, hashByte byte, amount btcutil.Amount,
	batchExpiry int32) *vtxo.Descriptor {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay uint32 = 10

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		clientKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
	require.NoError(t, err)

	pkScript, err := template.PkScript()
	require.NoError(t, err)

	tapScript, err := arkscript.VTXOTapScript(
		clientKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	var outpointHash chainhash.Hash
	outpointHash[0] = hashByte

	var commitmentTxID chainhash.Hash
	commitmentTxID[0] = hashByte
	commitmentTxID[1] = 0xc0

	return &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  outpointHash,
			Index: uint32(hashByte),
		},
		Amount:         amount,
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Index: uint32(hashByte),
			},
		},
		OperatorKey:    operatorKey.PubKey(),
		TapScript:      tapScript,
		RoundID:        fmt.Sprintf("refresh-est-round-%x", hashByte),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    batchExpiry,
		RelativeExpiry: exitDelay,
		CreatedHeight:  1,
		Status:         vtxo.VTXOStatusLive,
	}
}

// outpointStr renders a descriptor's outpoint in the "txid:index" wire
// format the RPC uses.
func outpointStr(desc *vtxo.Descriptor) string {
	return fmt.Sprintf("%s:%d", desc.Outpoint.Hash, desc.Outpoint.Index)
}

// TestRefreshDryRunEstimateAllPath covers the selection=all preview: the
// estimate must price every live VTXO from the daemon's own store view
// (no caller-supplied amounts or lifetimes) — and must do so without any
// wallet wiring, since the dry-run path short-circuits before the
// wallet-ready gate.
func TestRefreshDryRunEstimateAllPath(t *testing.T) {
	t.Parallel()

	const height = int32(900)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	descA := newRefreshEstimateVTXO(t, 0x01, 100_000, 1_000)
	descB := newRefreshEstimateVTXO(t, 0x02, 250_000, 1_400)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), descA))
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), descB))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "preview", resp.Status)
	require.ElementsMatch(
		t, []string{outpointStr(descA), outpointStr(descB)},
		resp.QueuedOutpoints,
	)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.Empty(t, est.EstimateError)
	require.False(
		t, est.FreeRefreshEligible,
		"no operator terms cached, so no free window applies",
	)
	require.Len(t, est.Outpoints, 2)

	rows := make(map[string]*waverpc.OutpointFeeEstimate)
	for _, row := range est.Outpoints {
		rows[row.Outpoint] = row
	}

	rowA := rows[outpointStr(descA)]
	require.NotNil(t, rowA)
	require.Equal(t, int64(100_000), rowA.AmountSat)
	require.Equal(
		t, uint32(100), rowA.RemainingBlocks,
		"remaining = batch expiry 1000 - height 900",
	)
	require.Equal(t, int64(100), rowA.LiquidityFeeSat)
	require.Equal(t, int64(25), rowA.OnchainShareSat)
	require.Equal(t, int64(100), rowA.MarginSat)
	require.Equal(t, int64(225), rowA.TotalFeeSat)
	require.False(t, rowA.InFreeRefreshWindow)

	rowB := rows[outpointStr(descB)]
	require.NotNil(t, rowB)
	require.Equal(t, int64(250_000), rowB.AmountSat)
	require.Equal(t, uint32(500), rowB.RemainingBlocks)
	require.Equal(t, int64(375), rowB.TotalFeeSat)

	require.Equal(
		t, int64(600), est.GetEstimatedTotalFeeSat(),
		"total is the sum of the per-outpoint quotes",
	)

	// Distinct (amount, remaining) pairs require distinct quotes.
	require.Equal(t, 2, svc.estimateFeeCalls)
}

// TestRefreshDryRunEstimateExplicitOutpointLookup covers the explicit
// selection path: the daemon must resolve amount and remaining lifetime
// from its store — the manual amount / remaining-blocks inputs of the
// standalone fees estimate command must not be needed — and must ask
// the operator for a refresh (not boarding) quote.
func TestRefreshDryRunEstimateExplicitOutpointLookup(t *testing.T) {
	t.Parallel()

	const height = int32(700)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	desc := newRefreshEstimateVTXO(t, 0x03, 42_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						outpointStr(desc),
					},
				},
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "preview", resp.Status)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.Len(t, est.Outpoints, 1)
	require.Equal(t, int64(42_000), est.Outpoints[0].AmountSat)
	require.Equal(t, uint32(300), est.Outpoints[0].RemainingBlocks)

	require.NotNil(t, svc.lastRequest)
	require.False(
		t, svc.lastRequest.IsBoarding,
		"refresh quotes must not be priced as boarding",
	)
	require.Equal(t, int64(42_000), svc.lastRequest.AmountSat)
	require.Equal(t, uint32(300), svc.lastRequest.RemainingBlocks)
}

// TestRefreshDryRunEstimateUnknownOutpoint verifies an explicit outpoint
// the store does not know is a caller mistake (InvalidArgument naming
// the outpoint), not a silent echo: the previous behavior echoed any
// well-formed outpoint, which let a typo'd preview read as valid.
func TestRefreshDryRunEstimateUnknownOutpoint(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, _ := newRefreshEstimateServer(t, svc, 700)

	missing := newRefreshEstimateVTXO(t, 0x04, 10_000, 1_000)

	_, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						outpointStr(missing),
					},
				},
			},
			DryRun: true,
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), outpointStr(missing))
}

// TestRefreshDryRunEstimateDedupesQuotes verifies VTXOs sharing an
// (amount, remaining-blocks) pair share one operator round-trip: a
// wallet-wide dry run over a round's fan-out must not turn into one
// RPC per VTXO.
func TestRefreshDryRunEstimateDedupesQuotes(t *testing.T) {
	t.Parallel()

	const height = int32(500)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	// Two identical (amount, expiry) pairs plus one distinct.
	descA := newRefreshEstimateVTXO(t, 0x05, 50_000, 1_000)
	descB := newRefreshEstimateVTXO(t, 0x06, 50_000, 1_000)
	descC := newRefreshEstimateVTXO(t, 0x07, 80_000, 1_000)
	for _, desc := range []*vtxo.Descriptor{descA, descB, descC} {
		require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))
	}

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.Len(t, est.Outpoints, 3)
	require.Equal(
		t, 2, svc.estimateFeeCalls,
		"two distinct (amount, remaining) pairs, two quotes",
	)

	// The deduped pair still carries per-row components.
	rows := make(map[string]*waverpc.OutpointFeeEstimate)
	for _, row := range est.Outpoints {
		rows[row.Outpoint] = row
	}
	require.Equal(
		t, rows[outpointStr(descA)].TotalFeeSat,
		rows[outpointStr(descB)].TotalFeeSat,
	)
	require.Equal(t, int64(175), rows[outpointStr(descA)].TotalFeeSat)
	require.Equal(t, int64(205), rows[outpointStr(descC)].TotalFeeSat)
}

// TestRefreshDryRunEstimateFreeWindow covers the waiver-eligible
// selection: every VTXO inside the operator's advertised free-refresh
// window zeroes the selection total while the per-outpoint rows keep
// the ordinary paid quote, so the caller sees what the waiver saves.
func TestRefreshDryRunEstimateFreeWindow(t *testing.T) {
	t.Parallel()

	const height = int32(950)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)
	r.server.storeOperatorTerms(&types.OperatorTerms{
		FreeRefreshWindowBlocks: 100,
	})

	// Remaining lifetimes 50 and 90: both inside the 100-block
	// window.
	descA := newRefreshEstimateVTXO(t, 0x08, 100_000, 1_000)
	descB := newRefreshEstimateVTXO(t, 0x09, 200_000, 1_040)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), descA))
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), descB))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.True(t, est.FreeRefreshEligible)
	require.NotNil(
		t, est.EstimatedTotalFeeSat,
		"the waiver's zero total is explicit, not absent",
	)
	require.Zero(
		t, est.GetEstimatedTotalFeeSat(),
		"waiver-eligible selection previews as free",
	)

	for _, row := range est.Outpoints {
		require.True(t, row.InFreeRefreshWindow, row.Outpoint)
		require.Positive(
			t, row.TotalFeeSat, "rows keep the ordinary paid "+
				"quote so the caller sees what the waiver "+
				"saves",
		)
	}
}

// TestRefreshDryRunEstimateWindowBrokenByOneOutpoint verifies the
// waiver's all-or-nothing rule: one out-of-window VTXO in the selection
// prices the whole refresh as paid, mirroring the operator's seal-time
// isFreeRefresh predicate.
func TestRefreshDryRunEstimateWindowBrokenByOneOutpoint(t *testing.T) {
	t.Parallel()

	const height = int32(950)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)
	r.server.storeOperatorTerms(&types.OperatorTerms{
		FreeRefreshWindowBlocks: 100,
	})

	// Remaining lifetimes 50 (inside) and 250 (outside).
	inside := newRefreshEstimateVTXO(t, 0x0a, 100_000, 1_000)
	outside := newRefreshEstimateVTXO(t, 0x0b, 200_000, 1_200)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), inside))
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), outside))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.False(t, est.FreeRefreshEligible)
	require.Positive(t, est.GetEstimatedTotalFeeSat())

	rows := make(map[string]*waverpc.OutpointFeeEstimate)
	for _, row := range est.Outpoints {
		rows[row.Outpoint] = row
	}
	require.True(t, rows[outpointStr(inside)].InFreeRefreshWindow)
	require.False(t, rows[outpointStr(outside)].InFreeRefreshWindow)
}

// TestRefreshDryRunEstimateOperatorDown verifies the degraded mode the
// issue requires: with the operator unreachable the preview still
// returns (dry_run stays usable as a validity probe), the locally
// known facts stay populated, and estimate_error tells the caller the
// fee numbers are absent rather than zero.
func TestRefreshDryRunEstimateOperatorDown(t *testing.T) {
	t.Parallel()

	const height = int32(900)

	svc := &fakeArkService{
		estimateFeeErr: status.Error(
			codes.Unavailable, "operator down",
		),
	}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	desc := newRefreshEstimateVTXO(t, 0x0c, 100_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err, "estimate failure must not fail dry_run")
	require.Equal(t, "preview", resp.Status)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.NotEmpty(t, est.EstimateError)
	require.NotContains(
		t, est.EstimateError, "operator down",
		"upstream error text must not leak to the caller",
	)
	require.Nil(
		t, est.EstimatedTotalFeeSat, "a degraded paid estimate "+
			"leaves the total absent so it can not be misread "+
			"as free",
	)
	require.Len(t, est.Outpoints, 1)

	// Locally known facts survive the degradation.
	require.Equal(t, int64(100_000), est.Outpoints[0].AmountSat)
	require.Equal(t, uint32(100), est.Outpoints[0].RemainingBlocks)
	require.Zero(t, est.Outpoints[0].TotalFeeSat)
}

// TestRefreshDryRunEstimateOperatorDownFreeWindow verifies the
// locally computed waiver survives operator unavailability: a
// waiver-eligible selection still previews as free (with
// estimate_error set for the missing per-outpoint quotes).
func TestRefreshDryRunEstimateOperatorDownFreeWindow(t *testing.T) {
	t.Parallel()

	const height = int32(950)

	svc := &fakeArkService{
		estimateFeeErr: status.Error(
			codes.Unavailable, "operator down",
		),
	}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)
	r.server.storeOperatorTerms(&types.OperatorTerms{
		FreeRefreshWindowBlocks: 100,
	})

	desc := newRefreshEstimateVTXO(t, 0x0d, 100_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.True(t, est.FreeRefreshEligible)
	require.NotNil(t, est.EstimatedTotalFeeSat)
	require.Zero(t, est.GetEstimatedTotalFeeSat())
	require.NotEmpty(t, est.EstimateError)
}

// TestRefreshDryRunEstimateExpiredClampsRemaining verifies an already
// expired VTXO quotes at a clamped remaining lifetime of 1 instead of
// 0: the operator treats zero as "use the full sweep-delay lifetime",
// which would massively over-quote. An expired VTXO is also never
// waiver-eligible.
func TestRefreshDryRunEstimateExpiredClampsRemaining(t *testing.T) {
	t.Parallel()

	const height = int32(2_000)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)
	r.server.storeOperatorTerms(&types.OperatorTerms{
		FreeRefreshWindowBlocks: 100,
	})

	// Batch expiry far behind the tip.
	desc := newRefreshEstimateVTXO(t, 0x0e, 100_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.Len(t, est.Outpoints, 1)
	require.Equal(t, uint32(1), est.Outpoints[0].RemainingBlocks)
	require.False(t, est.Outpoints[0].InFreeRefreshWindow)
	require.False(t, est.FreeRefreshEligible)

	require.NotNil(t, svc.lastRequest)
	require.Equal(t, uint32(1), svc.lastRequest.RemainingBlocks)
}

// TestRefreshDryRunEstimateRejectsNonsenseQuote verifies a negative
// operator quote is treated like an unreachable operator instead of
// being summed into a misleading total.
func TestRefreshDryRunEstimateRejectsNonsenseQuote(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{
		response: &arkrpc.EstimateFeeResponse{
			TotalFeeSat: -1,
		},
	}
	r, vtxoStore := newRefreshEstimateServer(t, svc, 900)

	desc := newRefreshEstimateVTXO(t, 0x0f, 100_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.NotEmpty(t, est.EstimateError)
	require.Nil(t, est.EstimatedTotalFeeSat)
	require.Len(t, est.Outpoints, 1)
	require.Zero(t, est.Outpoints[0].TotalFeeSat)
}

// TestRefreshDryRunEmptyAllReturnsPreview pins the reordered empty
// case: selection=all over an empty store previews as "preview" (it
// used to report "queued" because the empty-target early return ran
// before the dry-run branch) and attaches no vacuous fee estimate a
// caller could misread as "free".
func TestRefreshDryRunEmptyAllReturnsPreview(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, _ := newRefreshEstimateServer(t, svc, 900)

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "preview", resp.Status)
	require.Empty(t, resp.QueuedOutpoints)
	require.Nil(t, resp.FeeEstimate)
	require.Zero(t, svc.estimateFeeCalls)
}

// TestRefreshVTXOsSelectionRequiredBeforeWalletGate verifies
// pure-argument validation outranks the wallet-ready gate, matching
// the LeaveVTXOs ordering rule: a request-shape bug must surface as
// InvalidArgument regardless of wallet state.
func TestRefreshVTXOsSelectionRequiredBeforeWalletGate(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	_, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{},
	)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestRefreshVTXOsRealPathStillRequiresWallet pins that only the
// dry-run preview moved ahead of the wallet-ready gate: a real refresh
// on an unready wallet still fails before any queuing.
func TestRefreshVTXOsRealPathStillRequiresWallet(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	_, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						"deadbeefdeadbeefdeadbeef" +
							"deadbeefdeadbeefdead" +
							"beefdeadbeef00000000" +
							":0",
					},
				},
			},
		},
	)
	require.Error(t, err)
	require.NotEqual(
		t, codes.InvalidArgument, status.Code(err),
		"a well-formed real refresh must fail on wallet readiness, "+
			"not argument validation",
	)
}

// errChainBackend is a chain backend whose BestBlock always fails, for
// exercising the chain-height-unavailable degrade path.
type errChainBackend struct {
	chainsource.ChainBackend
}

func (b *errChainBackend) BestBlock(context.Context) (int32, chainhash.Hash,
	error) {

	return 0, chainhash.Hash{}, fmt.Errorf("backend still syncing")
}

// TestRefreshDryRunEstimateChainHeightUnavailable verifies the first
// degrade mode: a failing chain backend yields amount-only rows, an
// estimate error, no operator calls, an absent total — and a preview
// that still succeeds, keeping dry_run usable as a validity probe while
// the backend syncs.
func TestRefreshDryRunEstimateChainHeightUnavailable(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, 0)
	r.server.chainBackend = &errChainBackend{}

	desc := newRefreshEstimateVTXO(t, 0x10, 100_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err, "chain failure must not fail dry_run")
	require.Equal(t, "preview", resp.Status)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.Contains(t, est.EstimateError, "chain height")
	require.Nil(t, est.EstimatedTotalFeeSat)
	require.False(t, est.FreeRefreshEligible)
	require.Len(t, est.Outpoints, 1)
	require.Equal(t, int64(100_000), est.Outpoints[0].AmountSat)
	require.Zero(t, est.Outpoints[0].RemainingBlocks)
	require.Zero(
		t, svc.estimateFeeCalls,
		"no operator quote without a chain tip",
	)
}

// TestRefreshDryRunEstimatePartialQuoteFailure verifies the
// all-or-nothing degrade when the operator fails partway through a
// multi-quote selection: no row may keep components from the quotes
// that succeeded before the failure, because estimate_error tells the
// caller the components are absent.
func TestRefreshDryRunEstimatePartialQuoteFailure(t *testing.T) {
	t.Parallel()

	const height = int32(500)

	svc := &fakeArkService{
		responseFn: scaledEstimateFn,
		errFn: func(req *arkrpc.EstimateFeeRequest) error {
			if req.AmountSat == 80_000 {
				return status.Error(
					codes.Unavailable, "quote two down",
				)
			}

			return nil
		},
	}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	descA := newRefreshEstimateVTXO(t, 0x11, 50_000, 1_000)
	descB := newRefreshEstimateVTXO(t, 0x12, 80_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), descA))
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), descB))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.NotEmpty(t, est.EstimateError)
	require.Nil(t, est.EstimatedTotalFeeSat)

	// ListLiveVTXOs enumeration order is storage-dependent, so the
	// failing quote may be attempted first (one call) or second
	// (two calls); either way the failure must abort further
	// quoting.
	require.GreaterOrEqual(t, svc.estimateFeeCalls, 1)
	require.LessOrEqual(t, svc.estimateFeeCalls, 2)

	require.Len(t, est.Outpoints, 2)
	for _, row := range est.Outpoints {
		require.Zero(t, row.LiquidityFeeSat, row.Outpoint)
		require.Zero(t, row.OnchainShareSat, row.Outpoint)
		require.Zero(t, row.MarginSat, row.Outpoint)
		require.Zero(t, row.TotalFeeSat, row.Outpoint)
	}
}

// TestRefreshDryRunEstimateRejectsNegativeComponent verifies the quote
// sanity check covers the component fields, not just the total: a
// negative component would break the documented invariant that a row's
// components sum to its total.
func TestRefreshDryRunEstimateRejectsNegativeComponent(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{
		response: &arkrpc.EstimateFeeResponse{
			LiquidityFeeSat: -50,
			MarginSat:       100,
			TotalFeeSat:     50,
		},
	}
	r, vtxoStore := newRefreshEstimateServer(t, svc, 900)

	desc := newRefreshEstimateVTXO(t, 0x13, 100_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.Contains(t, est.EstimateError, "invalid")
	require.Nil(t, est.EstimatedTotalFeeSat)
	require.Len(t, est.Outpoints, 1)
	require.Zero(t, est.Outpoints[0].LiquidityFeeSat)
	require.Zero(t, est.Outpoints[0].TotalFeeSat)
}

// TestRefreshDryRunEstimateWindowBoundary pins the inclusive window
// comparison against the operator's IsFreeRefreshWindow semantics: a
// VTXO at exactly the advertised window is waiver-eligible, one block
// beyond is not.
func TestRefreshDryRunEstimateWindowBoundary(t *testing.T) {
	t.Parallel()

	const height = int32(900)

	cases := []struct {
		name      string
		remaining int32
		eligible  bool
	}{
		{
			name:      "exactly at window",
			remaining: 100,
			eligible:  true,
		},
		{
			name:      "one beyond window",
			remaining: 101,
			eligible:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := &fakeArkService{
				responseFn: scaledEstimateFn,
			}
			r, vtxoStore := newRefreshEstimateServer(
				t, svc, height,
			)
			r.server.storeOperatorTerms(&types.OperatorTerms{
				FreeRefreshWindowBlocks: 100,
			})

			desc := newRefreshEstimateVTXO(
				t, 0x14, 100_000, height+tc.remaining,
			)
			require.NoError(
				t,
				vtxoStore.SaveVTXO(
					t.Context(), desc,
				),
			)

			resp, err := r.RefreshVTXOs(
				t.Context(), &waverpc.RefreshVTXOsRequest{
					Selection: &waverpc.
						RefreshVTXOsRequest_All{
						All: true,
					},
					DryRun: true,
				},
			)
			require.NoError(t, err)

			est := resp.FeeEstimate
			require.NotNil(t, est)
			require.Len(t, est.Outpoints, 1)
			require.Equal(
				t, tc.eligible,
				est.Outpoints[0].InFreeRefreshWindow,
			)
			require.Equal(
				t, tc.eligible, est.FreeRefreshEligible,
			)
		})
	}
}

// TestRefreshDryRunEstimateDuplicateOutpointCollapsed verifies a
// repeated explicit outpoint is collapsed before resolution: one row,
// one echo, one quote — never a doubled total for a single VTXO.
func TestRefreshDryRunEstimateDuplicateOutpointCollapsed(t *testing.T) {
	t.Parallel()

	const height = int32(700)

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, height)

	desc := newRefreshEstimateVTXO(t, 0x15, 50_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						outpointStr(desc),
						outpointStr(desc),
					},
				},
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)
	require.Equal(t, []string{outpointStr(desc)}, resp.QueuedOutpoints)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.Len(t, est.Outpoints, 1)
	require.Equal(
		t, int64(175), est.GetEstimatedTotalFeeSat(),
		"one VTXO, one quote, never a doubled total",
	)
	require.Equal(t, 1, svc.estimateFeeCalls)
}

// TestRefreshDryRunEstimateRejectsNonLiveOutpoint verifies an explicit
// outpoint in a non-live state is a caller mistake, mirroring the
// --all branch's LiveState filter: the real refresh can never execute
// it, so a preview that echoed and priced it would overpromise
// validity right where the CLI consent prompt trusts it.
func TestRefreshDryRunEstimateRejectsNonLiveOutpoint(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, 700)

	desc := newRefreshEstimateVTXO(t, 0x16, 50_000, 1_000)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	// SaveVTXO persists rows as live; move the fixture into its
	// non-live state explicitly.
	require.NoError(
		t,
		vtxoStore.UpdateVTXOStatus(
			t.Context(), desc.Outpoint, vtxo.VTXOStatusSpent,
		),
	)

	_, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						outpointStr(desc),
					},
				},
			},
			DryRun: true,
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "not refreshable")
	require.Zero(t, svc.estimateFeeCalls)
}

// TestRefreshDryRunNilStoreEchoesExplicit pins the store-less degrade:
// the preview echo for explicit outpoints survives (as before this
// feature) and the estimate degrades explicitly instead of silently
// vanishing.
func TestRefreshDryRunNilStoreEchoesExplicit(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	op := "1111111111111111111111111111111111111111111111111111111111" +
		"111111:0"
	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{op},
				},
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "preview", resp.Status)
	require.Len(t, resp.QueuedOutpoints, 1)

	est := resp.FeeEstimate
	require.NotNil(t, est)
	require.Contains(t, est.EstimateError, "store unavailable")
	require.Nil(t, est.EstimatedTotalFeeSat)
}

// refreshTestWallet is a minimal wallet actor recording the refresh
// requests it receives, for asserting what the real (non-dry-run)
// path dispatches.
type refreshTestWallet struct {
	mu   sync.Mutex
	reqs []*wallet.RefreshVTXOsRequest
}

func (w *refreshTestWallet) Receive(_ context.Context,
	msg wallet.WalletMsg) fn.Result[wallet.WalletResp] {

	w.mu.Lock()
	defer w.mu.Unlock()

	switch msg := msg.(type) {
	case *wallet.RefreshVTXOsRequest:
		reqCopy := *msg
		w.reqs = append(w.reqs, &reqCopy)

		return fn.Ok[wallet.WalletResp](
			&wallet.RefreshVTXOsResponse{
				RefreshingCount: len(msg.TargetOutpoints),
			},
		)

	default:
		return fn.Err[wallet.WalletResp](
			fmt.Errorf("unexpected wallet msg %T", msg),
		)
	}
}

// TestRefreshVTXOsAllQueuesLiveTargets pins the real (non-dry-run)
// --all path after the handler restructure: live VTXOs are expanded
// into explicit wallet targets, non-live rows are filtered, and the
// response echoes what the wallet actually queued.
func TestRefreshVTXOsAllQueuesLiveTargets(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, vtxoStore := newRefreshEstimateServer(t, svc, 900)

	live1 := newRefreshEstimateVTXO(t, 0x17, 40_000, 1_000)
	live2 := newRefreshEstimateVTXO(t, 0x18, 60_000, 1_000)
	pending := newRefreshEstimateVTXO(t, 0x19, 80_000, 1_000)
	for _, desc := range []*vtxo.Descriptor{live1, live2, pending} {
		require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))
	}

	// SaveVTXO persists rows as live; move the in-flight fixture
	// into PendingForfeit explicitly so the filter has something
	// to exclude.
	require.NoError(
		t,
		vtxoStore.UpdateVTXOStatus(
			t.Context(), pending.Outpoint,
			vtxo.VTXOStatusPendingForfeit,
		),
	)

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		require.NoError(t, system.Shutdown(shutdownCtx))
	})

	testWallet := &refreshTestWallet{}
	walletKey := actor.NewServiceKey[
		wallet.WalletMsg, wallet.WalletResp,
	](
		"refresh-all-test-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "refresh-all-test-wallet", testWallet,
	)

	walletReady := make(chan struct{})
	close(walletReady)
	r.server.walletReady = walletReady
	r.server.walletRef = fn.Some(walletRef)

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "queued", resp.Status)
	require.ElementsMatch(
		t, []string{outpointStr(live1), outpointStr(live2)},
		resp.QueuedOutpoints,
	)
	require.Nil(t, resp.FeeEstimate,
		"the real path attaches no estimate")

	require.Len(t, testWallet.reqs, 1)
	require.ElementsMatch(
		t, []wire.OutPoint{live1.Outpoint, live2.Outpoint},
		testWallet.reqs[0].TargetOutpoints,
		"only live VTXOs reach the wallet actor",
	)
	require.True(t, testWallet.reqs[0].ForceRefresh)
}
