package btcwbackend

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/stretchr/testify/require"
)

// stubPackageSubmitter records package relay calls for tests.
type stubPackageSubmitter struct {
	parents []*wire.MsgTx
	child   *wire.MsgTx

	result *btcjson.SubmitPackageResult
	err    error
}

// SubmitPackage records the submitted package and returns the configured
// result.
func (s *stubPackageSubmitter) SubmitPackage(_ context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx, _ *float64) (
	*btcjson.SubmitPackageResult, error) {

	s.parents = parents
	s.child = child

	return s.result, s.err
}

// TestSubmitPackageRequiresConfiguredSubmitter verifies that btcwallet does not
// pretend individual neutrino broadcast is atomic package relay.
func TestSubmitPackageRequiresConfiguredSubmitter(t *testing.T) {
	backend := &ChainBackend{}

	err := backend.SubmitPackage(
		t.Context(), []*wire.MsgTx{wire.NewMsgTx(3)}, wire.NewMsgTx(3),
	)
	require.ErrorContains(t, err, "package submission not supported")
}

// TestSubmitPackageUsesConfiguredSubmitter verifies that btcwallet delegates
// package relay to the configured direct package submitter.
func TestSubmitPackageUsesConfiguredSubmitter(t *testing.T) {
	parent := wire.NewMsgTx(3)
	child := wire.NewMsgTx(3)
	submitter := &stubPackageSubmitter{
		result: &btcjson.SubmitPackageResult{
			PackageMsg: "success",
			TxResults:  map[string]btcjson.SubmitPackageTxResult{},
		},
	}
	backend := &ChainBackend{
		packageSubmitter: submitter,
	}

	err := backend.SubmitPackage(t.Context(), []*wire.MsgTx{parent}, child)
	require.NoError(t, err)
	require.Equal(t, []*wire.MsgTx{parent}, submitter.parents)
	require.Same(t, child, submitter.child)
}

// TestSubmitPackageRejectsPackageErrors verifies that package relay rejections
// are surfaced to txconfirm instead of being treated as broadcast success.
func TestSubmitPackageRejectsPackageErrors(t *testing.T) {
	rejectReason := "bad-txns-inputs-missingorspent"
	submitter := &stubPackageSubmitter{
		result: &btcjson.SubmitPackageResult{
			PackageMsg: "transaction failed",
			TxResults: map[string]btcjson.SubmitPackageTxResult{
				"wtxid": {
					Error: &rejectReason,
				},
			},
		},
	}
	backend := &ChainBackend{
		packageSubmitter: submitter,
	}

	err := backend.SubmitPackage(
		t.Context(), []*wire.MsgTx{wire.NewMsgTx(3)}, wire.NewMsgTx(3),
	)
	require.ErrorContains(t, err, "package not accepted")
	require.ErrorContains(t, err, rejectReason)
}

var _ chainbackends.PackageSubmitter = (*stubPackageSubmitter)(nil)

// TestWaitUntilCurrentAlreadyCurrent verifies that when the backend is
// already current, the wait returns immediately without consuming a
// tick — the steady-state (restart against an already-synced neutrino)
// must not be delayed.
func TestWaitUntilCurrentAlreadyCurrent(t *testing.T) {
	t.Parallel()

	var onWaitCalls int

	// A huge poll interval guarantees the return came from the
	// fast-path check, not from a ticker firing.
	got := waitUntilCurrent(
		func() bool { return true }, make(chan struct{}), time.Hour,
		func(int) { onWaitCalls++ },
	)

	require.True(t, got)
	require.Zero(t, onWaitCalls)
}

// TestWaitUntilCurrentBecomesCurrent verifies that the wait keeps
// polling until the backend reports current, then returns true. This is
// the fix's core behavior: the notifier is held back until neutrino has
// synced. It also asserts onWait fires once per not-current poll so
// progress logging is driven correctly.
func TestWaitUntilCurrentBecomesCurrent(t *testing.T) {
	t.Parallel()

	// Report not-current on the first two checks (the pre-loop check
	// and the first tick), then current on the third.
	var calls int
	isCurrent := func() bool {
		calls++

		return calls >= 3
	}

	var onWaitCalls int
	got := waitUntilCurrent(
		isCurrent, make(chan struct{}), time.Millisecond,
		func(int) { onWaitCalls++ },
	)

	require.True(t, got)
	require.Equal(t, 3, calls)

	// onWait fires only after the single not-current tick (the
	// pre-loop check and the final current tick don't invoke it).
	require.Equal(t, 1, onWaitCalls)
}

// TestWaitUntilCurrentStopsOnQuit verifies that closing quit unblocks a
// wait for a backend that never becomes current, so daemon shutdown
// during the initial sync wait cannot hang.
func TestWaitUntilCurrentStopsOnQuit(t *testing.T) {
	t.Parallel()

	quit := make(chan struct{})
	close(quit)

	// Never current + a huge poll interval: the only way this returns
	// is via the closed quit channel.
	got := waitUntilCurrent(
		func() bool { return false }, quit, time.Hour, nil,
	)

	require.False(t, got)
}
