package waved

import (
	"context"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeArkService is a minimal in-memory ArkService that returns
// a canned EstimateFee response. Used by tests to exercise the
// client's quoteOperatorFee helper without spinning up the
// real operator.
type fakeArkService struct {
	arkrpc.UnimplementedArkServiceServer

	// response is the canned EstimateFee reply. Tests mutate
	// this between calls to simulate utilization-driven rate
	// changes.
	response *arkrpc.EstimateFeeResponse

	// estimateFeeErr, when non-nil, is returned from EstimateFee
	// instead of the canned response. Used by degraded-mode tests
	// to simulate an operator that is reachable for GetInfo but
	// fails the fee quote.
	estimateFeeErr error

	// responseFn, when non-nil, computes the EstimateFee reply from
	// the request instead of returning the canned response. Used by
	// tests that quote several distinct (amount, remaining-blocks)
	// pairs in one RPC and need per-pair replies.
	responseFn func(*arkrpc.EstimateFeeRequest) *arkrpc.EstimateFeeResponse

	// errFn, when non-nil, lets a test fail EstimateFee selectively
	// per request (e.g. only the second distinct quote of a
	// selection). A nil return falls through to responseFn / the
	// canned response.
	errFn func(*arkrpc.EstimateFeeRequest) error

	// estimateFeeCalls counts EstimateFee invocations so tests can
	// assert quote deduplication.
	estimateFeeCalls int

	// getInfoResponse is the canned GetInfo reply. Optional;
	// tests that don't exercise the fetchOperatorTerms path can
	// leave it nil, in which case GetInfo falls through to the
	// embedded Unimplemented server and returns an error.
	getInfoResponse *arkrpc.GetInfoResponse

	// getInfoErr, when non-nil, is returned from GetInfo instead
	// of the canned response. Used to exercise the degraded-mode
	// fetchOperatorTerms-failed branch.
	getInfoErr error

	// lastRequest records the most recent EstimateFeeRequest
	// so tests can assert what the client asked for.
	lastRequest *arkrpc.EstimateFeeRequest
}

// EstimateFee records the request and returns the canned reply
// (or an error, if the test injected one).
func (f *fakeArkService) EstimateFee(_ context.Context,
	req *arkrpc.EstimateFeeRequest) (*arkrpc.EstimateFeeResponse, error) {

	f.lastRequest = req
	f.estimateFeeCalls++

	if f.estimateFeeErr != nil {
		return nil, f.estimateFeeErr
	}

	if f.errFn != nil {
		if err := f.errFn(req); err != nil {
			return nil, err
		}
	}

	if f.responseFn != nil {
		return f.responseFn(req), nil
	}

	return f.response, nil
}

// GetInfo returns the canned reply (or an injected error). When
// both fields are nil the call falls through to the embedded
// Unimplemented server, which itself returns an error — useful
// for tests that want fetchOperatorTerms to fail via the default.
func (f *fakeArkService) GetInfo(ctx context.Context,
	req *arkrpc.GetInfoRequest) (*arkrpc.GetInfoResponse, error) {

	if f.getInfoErr != nil {
		return nil, f.getInfoErr
	}

	if f.getInfoResponse != nil {
		return f.getInfoResponse, nil
	}

	return f.UnimplementedArkServiceServer.GetInfo(ctx, req)
}

// newBufconnClient builds a *grpc.ClientConn wired to an
// in-process arkrpc server backed by the given fakeArkService.
// Returns the conn and a cleanup func to stop the server.
func newBufconnClient(t *testing.T, svc *fakeArkService) *grpc.ClientConn {
	t.Helper()

	const bufSize = 1024 * 1024
	lis := bufconn.Listen(bufSize)

	srv := grpc.NewServer()
	arkrpc.RegisterArkServiceServer(srv, svc)

	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}

	conn, err := grpc.DialContext(
		t.Context(),
		"bufconn", grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err, "dial bufconn")
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// TestQuoteOperatorFeeRoundTripsCongestionSpread verifies that
// the client's quoteOperatorFee helper returns exactly the
// TotalFeeSat the server reports, across a baseline quote and
// a subsequent utilization-driven quote.
//
// This is the client-side half of the congestion round-trip
// invariant in issue #263 Phase C.4 (4): a client using
// EstimateFee to size its boarding / refresh must observe the
// same TotalFeeSat the operator booked, so there is no drift
// when utilization changes between two client calls.
func TestQuoteOperatorFeeRoundTripsCongestionSpread(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{
		response: &arkrpc.EstimateFeeResponse{
			LiquidityFeeSat:     100,
			OnchainShareSat:     10,
			MarginSat:           50,
			TotalFeeSat:         160,
			EffectiveAnnualRate: 0.05,
		},
	}

	// Construct a Server with only the fields quoteOperatorFee
	// touches: serverConn (non-nil) and no log requirement.
	s := &Server{
		serverConn: newBufconnClient(t, svc),
		log:        btclog.Disabled,
	}

	// Baseline quote.
	got, err := s.quoteOperatorFee(
		t.Context(), 100_000, true /* isBoarding */, 0,
	)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(160), got)

	require.NotNil(t, svc.lastRequest)
	require.Equal(
		t, int64(100_000), svc.lastRequest.AmountSat,
	)
	require.True(t, svc.lastRequest.IsBoarding)
	require.Equal(t, uint32(0), svc.lastRequest.RemainingBlocks)

	// Simulate a congestion-driven bump in the operator-side
	// schedule. The fake returns a higher fee on the next
	// EstimateFee call.
	svc.response = &arkrpc.EstimateFeeResponse{
		LiquidityFeeSat:     800,
		OnchainShareSat:     10,
		MarginSat:           50,
		TotalFeeSat:         860,
		EffectiveAnnualRate: 0.25,
	}

	got, err = s.quoteOperatorFee(
		t.Context(), 100_000, false /* isBoarding */, 144,
	)
	require.NoError(t, err)
	require.Equal(
		t, btcutil.Amount(860), got, "client must report exactly "+
			"the server's TotalFeeSat, no local caching",
	)
	require.Equal(
		t, uint32(144), svc.lastRequest.RemainingBlocks,
		"RemainingBlocks must propagate",
	)
}

// TestQuoteOperatorFeeNilServerConn asserts the guard for the
// case where the daemon hasn't dialed the operator yet. Without
// the guard the helper would panic inside the arkrpc client
// on a nil conn; the guard turns the panic into a clean
// Unavailable error so upstream callers can fall back.
func TestQuoteOperatorFeeNilServerConn(t *testing.T) {
	t.Parallel()

	s := &Server{
		serverConn: nil,
		log:        btclog.Disabled,
	}

	_, err := s.quoteOperatorFee(
		t.Context(), 100_000, true, 0,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not initialized")
}
