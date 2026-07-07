package btcwbackend

import (
	"context"
	"testing"

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
