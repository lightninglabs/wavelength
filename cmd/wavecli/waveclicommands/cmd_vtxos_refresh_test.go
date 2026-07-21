package waveclicommands

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// newRefreshTestCmd returns a vtxos refresh cobra command with the
// supplied stdin plumbed in so the prompt path is testable, plus the
// combined output buffer for wording assertions.
func newRefreshTestCmd(t *testing.T,
	stdin string) (*cobra.Command, *bytes.Buffer) {

	t.Helper()

	cmd := newVTXOsRefreshCmd()
	cmd.SetContext(t.Context())
	cmd.SetIn(strings.NewReader(stdin))

	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)

	return cmd, out
}

// explicitRefreshReq returns a well-formed single-outpoint refresh
// request for gate tests.
func explicitRefreshReq() *waverpc.RefreshVTXOsRequest {
	return &waverpc.RefreshVTXOsRequest{
		Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
			Outpoints: &waverpc.OutpointSelection{
				Outpoints: []string{
					"aa:0",
				},
			},
		},
	}
}

// staticPreview returns a fetchPreview stub that yields a fixed
// response and records whether it was called.
func staticPreview(called *bool, resp *waverpc.RefreshVTXOsResponse,
	err error,
) func(*waverpc.RefreshVTXOsRequest) (*waverpc.RefreshVTXOsResponse, error) {

	return func(req *waverpc.RefreshVTXOsRequest) (
		*waverpc.RefreshVTXOsResponse, error) {

		*called = true

		return resp, err
	}
}

// TestConfirmRefreshDryRunBypasses verifies a dry-run request is not
// gated: the user is previewing, not spending.
func TestConfirmRefreshDryRunBypasses(t *testing.T) {
	t.Parallel()

	cmd, _ := newRefreshTestCmd(t, "" /* no stdin */)

	req := explicitRefreshReq()
	req.DryRun = true

	var previewed bool
	require.NoError(
		t,
		confirmRefreshIfNeeded(
			cmd, req, staticPreview(&previewed, nil, nil),
		),
	)
	require.False(t, previewed)
}

// TestConfirmRefreshYesFlagBypasses verifies --yes short-circuits both
// the prompt and the extra preview round-trip for scripted use.
func TestConfirmRefreshYesFlagBypasses(t *testing.T) {
	t.Parallel()

	cmd, _ := newRefreshTestCmd(t, "" /* no stdin */)
	require.NoError(t, cmd.Flags().Set("yes", "true"))

	var previewed bool
	require.NoError(
		t,
		confirmRefreshIfNeeded(
			cmd, explicitRefreshReq(),
			staticPreview(&previewed, nil, nil),
		),
	)
	require.False(t, previewed,
		"--yes must not spend a preview round-trip")
}

// TestConfirmRefreshNonTTYRefusesPrompt is the agent-safety guard the
// issue's acceptance criteria require: a non-interactive invocation
// without --yes or --dry_run fails fast with an INVALID_ARGS envelope
// instead of blocking on a prompt an agent cannot answer.
func TestConfirmRefreshNonTTYRefusesPrompt(t *testing.T) {
	// NOT t.Parallel() — overrides the package-level stdinIsTTY
	// indirection.

	prev := stdinIsTTY
	stdinIsTTY = func(*cobra.Command) bool { return false }
	defer func() {
		stdinIsTTY = prev
	}()

	cmd := newVTXOsRefreshCmd()
	cmd.SetContext(t.Context())

	var previewed bool
	err := confirmRefreshIfNeeded(
		cmd, explicitRefreshReq(), staticPreview(&previewed, nil, nil),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--yes")
	require.Contains(t, err.Error(), "--dry_run")
	require.True(
		t, ErrorWasPrinted(err),
		"expected a printedError so main.go exits with the "+
			"INVALID_ARGS code",
	)
	require.False(t, previewed)
}

// TestConfirmRefreshPromptShowsEstimateAndAcceptsYes verifies the
// interactive path consents to a number: the preview estimate is
// printed before the prompt and "y" proceeds.
func TestConfirmRefreshPromptShowsEstimateAndAcceptsYes(t *testing.T) {
	requireInteractiveStdin(t)

	cmd, out := newRefreshTestCmd(t, "y\n")

	var previewed bool
	preview := staticPreview(
		&previewed, &waverpc.RefreshVTXOsResponse{
			QueuedOutpoints: []string{"aa:0"},
			Status:          "preview",
			FeeEstimate: &waverpc.RefreshFeeEstimate{
				EstimatedTotalFeeSat: proto.Int64(777),
				Outpoints: []*waverpc.OutpointFeeEstimate{
					{Outpoint: "aa:0"},
				},
			},
		}, nil,
	)

	require.NoError(
		t,
		confirmRefreshIfNeeded(
			cmd, explicitRefreshReq(), preview,
		),
	)
	require.True(t, previewed)
	require.Contains(t, out.String(), "777 sat")
	require.Contains(t, out.String(), "Proceed with refresh? [y/N]")
}

// TestConfirmRefreshPromptRejectsNo verifies "n" aborts before any
// dispatch, default-N posture included.
func TestConfirmRefreshPromptRejectsNo(t *testing.T) {
	requireInteractiveStdin(t)

	for _, answer := range []string{"n\n", "\n"} {
		cmd, _ := newRefreshTestCmd(t, answer)

		var previewed bool
		err := confirmRefreshIfNeeded(
			cmd, explicitRefreshReq(),
			staticPreview(&previewed, nil, nil),
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "aborted by user")
	}
}

// TestConfirmRefreshPreviewInvalidArgumentAborts verifies a preview
// rejected as InvalidArgument (unknown or malformed outpoint) aborts
// the flow: the real dispatch would reject the same request shape, so
// prompting the user to confirm it would be noise.
func TestConfirmRefreshPreviewInvalidArgumentAborts(t *testing.T) {
	requireInteractiveStdin(t)

	cmd, _ := newRefreshTestCmd(t, "y\n")

	var previewed bool
	err := confirmRefreshIfNeeded(
		cmd, explicitRefreshReq(),
		staticPreview(
			&previewed, nil, status.Error(
				codes.InvalidArgument, "unknown VTXO outpoint",
			),
		),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refresh preview rejected")
}

// TestConfirmRefreshPreviewFailureStillPrompts verifies a broken
// estimate path (operator down, old daemon) degrades to prompting
// with a warning instead of making refreshes unconfirmable — and the
// warning says a fee still applies.
func TestConfirmRefreshPreviewFailureStillPrompts(t *testing.T) {
	requireInteractiveStdin(t)

	cmd, out := newRefreshTestCmd(t, "y\n")

	var previewed bool
	require.NoError(
		t,
		confirmRefreshIfNeeded(
			cmd, explicitRefreshReq(),
			staticPreview(
				&previewed, nil, status.Error(
					codes.Unavailable, "operator down",
				),
			),
		),
	)
	require.Contains(t, out.String(), "estimate unavailable")
	require.Contains(t, out.String(), "still charged")
	require.Contains(t, out.String(), "Proceed with refresh? [y/N]")
}

// TestConfirmRefreshPreviewWithoutEstimateStillPrompts verifies a
// daemon that returns a preview without a fee estimate (an older
// daemon behind a newer CLI) still yields a warning plus the prompt,
// never a silent zero.
func TestConfirmRefreshPreviewWithoutEstimateStillPrompts(t *testing.T) {
	requireInteractiveStdin(t)

	cmd, out := newRefreshTestCmd(t, "y\n")

	var previewed bool
	require.NoError(
		t,
		confirmRefreshIfNeeded(
			cmd, explicitRefreshReq(),
			staticPreview(
				&previewed, &waverpc.RefreshVTXOsResponse{
					QueuedOutpoints: []string{"aa:0"},
					Status:          "preview",
				},
				nil,
			),
		),
	)
	require.Contains(t, out.String(), "no fee estimate")
	require.Contains(t, out.String(), "still charged")
}

// TestBuildRefreshVTXOsRequestRejectsOutpointAndAll verifies that a
// caller can't combine --outpoint and --all: before the shared
// builder, --all silently won and the outpoints were dropped.
func TestBuildRefreshVTXOsRequestRejectsOutpointAndAll(t *testing.T) {
	t.Parallel()

	_, err := buildRefreshVTXOsRequest(
		[]string{
			"aa:0",
		},
		true, false,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mutually exclusive")
}

// TestBuildRefreshVTXOsRequestRejectsNoSelection verifies that a
// caller with neither --outpoint nor --all gets a clean CLI-side
// error rather than hitting the daemon with an empty request.
func TestBuildRefreshVTXOsRequestRejectsNoSelection(t *testing.T) {
	t.Parallel()

	_, err := buildRefreshVTXOsRequest(nil, false, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "either --outpoint or --all")
}

// TestBuildRefreshVTXOsRequestBuildsSelections locks in the two
// valid selection shapes and the dry_run passthrough.
func TestBuildRefreshVTXOsRequestBuildsSelections(t *testing.T) {
	t.Parallel()

	req, err := buildRefreshVTXOsRequest(nil, true, true)
	require.NoError(t, err)
	require.True(t, req.GetAll())
	require.True(t, req.DryRun)

	req, err = buildRefreshVTXOsRequest(
		[]string{
			"aa:0", "bb:1",
		},
		false, false,
	)
	require.NoError(t, err)
	require.Equal(
		t, []string{
			"aa:0",
			"bb:1",
		},
		req.GetOutpoints().GetOutpoints(),
	)
	require.False(t, req.DryRun)
}

// TestSummarizeRefreshFeeEstimateNil verifies a response without an
// estimate (real refresh, empty selection) renders no stderr lines.
func TestSummarizeRefreshFeeEstimateNil(t *testing.T) {
	t.Parallel()

	require.Empty(t, summarizeRefreshFeeEstimate(nil))
}

// TestSummarizeRefreshFeeEstimatePaid verifies the ordinary paid
// preview renders the headline total, the selection size, and the
// advisory caveat.
func TestSummarizeRefreshFeeEstimatePaid(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		EstimatedTotalFeeSat: proto.Int64(1_234),
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0"}, {Outpoint: "bb:1"},
		},
	})
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "1234 sat")
	require.Contains(t, lines[0], "2 VTXO(s)")
	require.Contains(t, lines[0], "advisory")
	require.Contains(t, lines[0], "seal time")
}

// TestSummarizeRefreshFeeEstimateFree verifies a waiver-eligible
// selection leads with the zero fee and names the free-refresh window
// as the reason.
func TestSummarizeRefreshFeeEstimateFree(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		FreeRefreshEligible: true,
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0", TotalFeeSat: 500},
		},
	})
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "0 sat")
	require.Contains(t, lines[0], "free-refresh window")
	require.Contains(t, lines[0], "advisory")
}

// TestSummarizeRefreshFeeEstimateError verifies a degraded estimate
// warns that the numbers are absent — and still says a fee applies —
// rather than printing a zero a user could misread as free.
func TestSummarizeRefreshFeeEstimateError(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		EstimateError: "operator fee estimate unavailable",
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0"},
		},
	})
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "warning")
	require.Contains(t, lines[0], "operator fee estimate unavailable")
	require.Contains(t, lines[0], "still charged")
	require.NotContains(t, lines[0], "0 sat")
}

// TestSummarizeRefreshFeeEstimateErrorFreeWindow verifies the locally
// computed waiver still surfaces when the operator quote failed: the
// user learns the refresh is expected free even in degraded mode.
func TestSummarizeRefreshFeeEstimateErrorFreeWindow(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		EstimateError:       "operator fee estimate unavailable",
		FreeRefreshEligible: true,
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0"},
		},
	})
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "warning")
	require.Contains(t, lines[1], "expected free")
}

// TestSummarizeRefreshFeeEstimateBelowDust verifies uneconomic VTXOs in
// the selection add a count-level warning on top of the fee headline.
func TestSummarizeRefreshFeeEstimateBelowDust(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		EstimatedTotalFeeSat: proto.Int64(400),
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0", BelowDustWarning: true},
			{Outpoint: "bb:1"},
			{Outpoint: "cc:2", BelowDustWarning: true},
		},
	})
	require.Len(t, lines, 2)
	require.Contains(t, lines[1], "2 selected VTXO(s)")
	require.Contains(t, lines[1], "minimum viable")
}

// fakeRefreshDaemon is an in-process DaemonService recording refresh
// and join traffic, for wiring tests that drive the full vtxosRefresh
// command path.
type fakeRefreshDaemon struct {
	waverpc.UnimplementedDaemonServiceServer

	mu          sync.Mutex
	refreshReqs []*waverpc.RefreshVTXOsRequest
	joinCalls   int

	previewResp *waverpc.RefreshVTXOsResponse
	queuedResp  *waverpc.RefreshVTXOsResponse
}

func (f *fakeRefreshDaemon) RefreshVTXOs(_ context.Context,
	req *waverpc.RefreshVTXOsRequest) (*waverpc.RefreshVTXOsResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	reqCopy, ok := proto.Clone(req).(*waverpc.RefreshVTXOsRequest)
	if !ok {
		return nil, status.Error(codes.Internal, "clone failed")
	}
	f.refreshReqs = append(f.refreshReqs, reqCopy)

	if req.DryRun {
		if f.previewResp != nil {
			return f.previewResp, nil
		}

		return &waverpc.RefreshVTXOsResponse{
			QueuedOutpoints: []string{
				"aa:0",
			},
			Status: "preview",
		}, nil
	}

	if f.queuedResp != nil {
		return f.queuedResp, nil
	}

	return &waverpc.RefreshVTXOsResponse{
		QueuedOutpoints: []string{
			"aa:0",
		},
		Status: "queued",
	}, nil
}

func (f *fakeRefreshDaemon) JoinNextRound(_ context.Context,
	_ *waverpc.JoinNextRoundRequest) (*waverpc.JoinNextRoundResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.joinCalls++

	return &waverpc.JoinNextRoundResponse{}, nil
}

// realRefreshCalls returns how many non-dry-run refresh dispatches the
// fake daemon received.
func (f *fakeRefreshDaemon) realRefreshCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, req := range f.refreshReqs {
		if !req.DryRun {
			n++
		}
	}

	return n
}

// withFakeRefreshDaemon serves the fake over bufconn and points the
// getDaemonClient indirection at it for the duration of the test.
// Tests using it must NOT be parallel: it swaps package-level state.
func withFakeRefreshDaemon(t *testing.T, fake *fakeRefreshDaemon) {
	t.Helper()

	const bufSize = 1024 * 1024
	lis := bufconn.Listen(bufSize)

	srv := grpc.NewServer()
	waverpc.RegisterDaemonServiceServer(srv, fake)

	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}

	prev := getDaemonClient
	getDaemonClient = func(*cobra.Command) (waverpc.DaemonServiceClient,
		*grpc.ClientConn, error) {

		conn, err := grpc.NewClient(
			"passthrough:///bufconn",
			grpc.WithContextDialer(dialer),
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		)
		if err != nil {
			return nil, nil, err
		}

		return waverpc.NewDaemonServiceClient(conn), conn, nil
	}
	t.Cleanup(func() {
		getDaemonClient = prev
	})
}

// TestVTXOsRefreshWiringNonTTYNeverDispatches drives the full command
// path: a non-interactive real refresh without --yes must refuse
// before ANY RPC reaches the daemon — the gate is wired ahead of
// dispatch, not beside it.
func TestVTXOsRefreshWiringNonTTYNeverDispatches(t *testing.T) {
	// NOT t.Parallel() — overrides getDaemonClient and stdinIsTTY.

	fake := &fakeRefreshDaemon{}
	withFakeRefreshDaemon(t, fake)

	prev := stdinIsTTY
	stdinIsTTY = func(*cobra.Command) bool { return false }
	defer func() {
		stdinIsTTY = prev
	}()

	cmd := newVTXOsRefreshCmd()
	cmd.SetContext(t.Context())
	require.NoError(t, cmd.Flags().Set("outpoint", "aa:0"))

	err := vtxosRefresh(cmd, nil)
	require.Error(t, err)
	require.True(t, ErrorWasPrinted(err))
	require.Empty(
		t, fake.refreshReqs, "the refusal must fire before any RPC",
	)
	require.Zero(t, fake.joinCalls)
}

// TestVTXOsRefreshWiringYesDispatchesOnce verifies --yes skips the
// gate and performs exactly one real dispatch plus the auto-join —
// no preview round-trip is spent on scripted consent.
func TestVTXOsRefreshWiringYesDispatchesOnce(t *testing.T) {
	// NOT t.Parallel() — overrides getDaemonClient.

	fake := &fakeRefreshDaemon{}
	withFakeRefreshDaemon(t, fake)

	cmd, _ := newRefreshTestCmd(t, "" /* no stdin */)
	require.NoError(t, cmd.Flags().Set("outpoint", "aa:0"))
	require.NoError(t, cmd.Flags().Set("yes", "true"))

	require.NoError(t, vtxosRefresh(cmd, nil))
	require.Len(t, fake.refreshReqs, 1)
	require.False(t, fake.refreshReqs[0].DryRun)
	require.Equal(t, 1, fake.joinCalls)
}

// TestVTXOsRefreshWiringDeclineNeverDispatches verifies an
// interactive "n" leaves the daemon untouched beyond the preview:
// one dry-run fetch, zero real dispatches, zero joins.
func TestVTXOsRefreshWiringDeclineNeverDispatches(t *testing.T) {
	// NOT t.Parallel() — overrides getDaemonClient.
	requireInteractiveStdin(t)

	fake := &fakeRefreshDaemon{}
	withFakeRefreshDaemon(t, fake)

	cmd, _ := newRefreshTestCmd(t, "n\n")
	require.NoError(t, cmd.Flags().Set("outpoint", "aa:0"))

	err := vtxosRefresh(cmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "aborted by user")
	require.Len(t, fake.refreshReqs, 1,
		"exactly the preview fetch")
	require.True(t, fake.refreshReqs[0].DryRun)
	require.Zero(t, fake.realRefreshCalls())
	require.Zero(t, fake.joinCalls)
}

// TestVTXOsRefreshWiringDryRunPrintsSummary verifies the --dry_run
// flag path end to end: one dry-run RPC, no join, no prompt, and the
// stderr summary carries the estimate headline.
func TestVTXOsRefreshWiringDryRunPrintsSummary(t *testing.T) {
	// NOT t.Parallel() — overrides getDaemonClient.

	fake := &fakeRefreshDaemon{
		previewResp: &waverpc.RefreshVTXOsResponse{
			QueuedOutpoints: []string{
				"aa:0",
			},
			Status: "preview",
			FeeEstimate: &waverpc.RefreshFeeEstimate{
				EstimatedTotalFeeSat: proto.Int64(432),
				Outpoints: []*waverpc.OutpointFeeEstimate{
					{
						Outpoint: "aa:0",
					},
				},
			},
		},
	}
	withFakeRefreshDaemon(t, fake)

	cmd, out := newRefreshTestCmd(t, "" /* no stdin */)
	require.NoError(t, cmd.Flags().Set("outpoint", "aa:0"))
	require.NoError(t, cmd.Flags().Set("dry_run", "true"))

	require.NoError(t, vtxosRefresh(cmd, nil))
	require.Len(t, fake.refreshReqs, 1)
	require.True(t, fake.refreshReqs[0].DryRun)
	require.Zero(t, fake.joinCalls, "dry run must not auto-join")
	require.Contains(t, out.String(), "432 sat")
	require.NotContains(t, out.String(), "Proceed with refresh?")
}

// TestVTXOsRefreshWiringOldDaemonWarns verifies the version-skew
// warning: a dry-run preview with outpoints but no fee estimate can
// only come from a pre-feature daemon, and the CLI must say so
// instead of silently dropping the promised preview.
func TestVTXOsRefreshWiringOldDaemonWarns(t *testing.T) {
	// NOT t.Parallel() — overrides getDaemonClient.

	fake := &fakeRefreshDaemon{
		previewResp: &waverpc.RefreshVTXOsResponse{
			QueuedOutpoints: []string{
				"aa:0",
			},
			Status: "preview",
		},
	}
	withFakeRefreshDaemon(t, fake)

	cmd, out := newRefreshTestCmd(t, "" /* no stdin */)
	require.NoError(t, cmd.Flags().Set("outpoint", "aa:0"))
	require.NoError(t, cmd.Flags().Set("dry_run", "true"))

	require.NoError(t, vtxosRefresh(cmd, nil))
	require.Contains(t, out.String(), "no fee estimate")
	require.Contains(t, out.String(), "still charged")
}

// TestVTXOsRefreshWiringEmptyAllSkipsPrompt verifies a TTY refresh
// --all over an empty wallet does not warn about a charge for a
// selection that queues nothing: the gate skips the prompt and says
// so.
func TestVTXOsRefreshWiringEmptyAllSkipsPrompt(t *testing.T) {
	// NOT t.Parallel() — overrides getDaemonClient.
	requireInteractiveStdin(t)

	fake := &fakeRefreshDaemon{
		previewResp: &waverpc.RefreshVTXOsResponse{
			Status: "preview",
		},
	}
	withFakeRefreshDaemon(t, fake)

	// No stdin: an unexpected prompt would fail loudly on EOF.
	cmd, out := newRefreshTestCmd(t, "")
	require.NoError(t, cmd.Flags().Set("all", "true"))

	require.NoError(t, vtxosRefresh(cmd, nil))
	require.Contains(t, out.String(), "no live VTXOs to refresh")
	require.NotContains(t, out.String(), "Proceed with refresh?")
	require.Equal(
		t, 1, fake.realRefreshCalls(),
		"the harmless empty dispatch still proceeds",
	)
}
