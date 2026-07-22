package waveclicommands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// newLeaveTestCmd returns a vtxos leave cobra command with the same
// flag surface as the real one, plumbed with the supplied stdin so
// the prompt path is testable. Output buffers are returned so tests
// can assert the prompt wording was emitted.
func newLeaveTestCmd(t *testing.T,
	stdin string) (*cobra.Command, *bytes.Buffer) {

	t.Helper()

	cmd := newVTXOsLeaveCmd()
	cmd.SetIn(strings.NewReader(stdin))

	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)

	return cmd, out
}

// TestBuildLeaveVTXOsRequestExplicitOutpoints covers the common
// case: a caller picks specific outpoints and gives a single
// default destination address. The resulting request must carry
// the outpoint selection and the address-typed default, nothing
// else.
func TestBuildLeaveVTXOsRequestExplicitOutpoints(t *testing.T) {
	t.Parallel()

	req, err := buildLeaveVTXOsRequest(
		[]string{"abcd:0", "abcd:1"},
		false, /* all */
		"bcrt1pexample",
		"",   /* pk_script */
		nil,  /* destinations */
		true, /* dry_run */
	)
	require.NoError(t, err)
	require.NotNil(t, req)
	require.True(t, req.DryRun)

	sel, ok := req.Selection.(*waverpc.LeaveVTXOsRequest_Outpoints)
	require.True(t, ok, "expected outpoint selection")
	require.Equal(
		t, []string{
			"abcd:0",
			"abcd:1",
		},
		sel.Outpoints.Outpoints,
	)

	def := req.DefaultDestination
	require.NotNil(t, def)
	addr, ok := def.Target.(*waverpc.LeaveDestination_Address)
	require.True(t, ok, "default destination must be address-typed")
	require.Equal(t, "bcrt1pexample", addr.Address)

	require.Empty(t, req.Destinations)
}

// TestBuildLeaveVTXOsRequestAll verifies the --all path produces
// an LeaveVTXOsRequest_All selection and keeps the per-outpoint
// overrides map empty.
func TestBuildLeaveVTXOsRequestAll(t *testing.T) {
	t.Parallel()

	req, err := buildLeaveVTXOsRequest(
		nil, /* outpoints */
		true,
		"bcrt1pexample",
		"", nil, false,
	)
	require.NoError(t, err)

	sel, ok := req.Selection.(*waverpc.LeaveVTXOsRequest_All)
	require.True(t, ok, "expected All selection")
	require.True(t, sel.All)

	require.Empty(t, req.Destinations)
}

// TestBuildLeaveVTXOsRequestPkScriptDefault verifies the
// --pk-script alternative to --address: the default destination is
// built from raw hex-decoded bytes.
func TestBuildLeaveVTXOsRequestPkScriptDefault(t *testing.T) {
	t.Parallel()

	req, err := buildLeaveVTXOsRequest(
		[]string{"abcd:0"},
		false,
		"",           /* address */
		"5120aabbcc", /* pk_script hex */
		nil, false,
	)
	require.NoError(t, err)

	def := req.DefaultDestination
	require.NotNil(t, def)
	ps, ok := def.Target.(*waverpc.LeaveDestination_PkScript)
	require.True(t, ok)
	require.Equal(t,
		[]byte{0x51, 0x20, 0xaa, 0xbb, 0xcc},
		ps.PkScript,
	)
}

// TestBuildLeaveVTXOsRequestRejectsBothAddressAndPkScript locks in
// the mutually-exclusive check on --address vs --pk-script for the
// default destination.
func TestBuildLeaveVTXOsRequestRejectsBothAddressAndPkScript(t *testing.T) {
	t.Parallel()

	_, err := buildLeaveVTXOsRequest(
		[]string{
			"abcd:0",
		},
		false, "bcrt1pexample", "5120aa",
		nil,
		false,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mutually exclusive")
}

// TestBuildLeaveVTXOsRequestRejectsNoDestination verifies that a
// caller with neither a default nor per-outpoint overrides gets a
// clean CLI-side error rather than hitting the daemon with an
// empty request.
func TestBuildLeaveVTXOsRequestRejectsNoDestination(t *testing.T) {
	t.Parallel()

	_, err := buildLeaveVTXOsRequest(
		[]string{
			"abcd:0",
		},
		false, "", "",
		nil,
		false,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "default destination")
}

// TestBuildLeaveVTXOsRequestPerOutpointOverrides covers the batch
// case: distinct destinations per outpoint via --destination. The
// "script:<hex>" form resolves to a pk_script-typed destination,
// a plain value resolves to an address-typed one.
func TestBuildLeaveVTXOsRequestPerOutpointOverrides(t *testing.T) {
	t.Parallel()

	req, err := buildLeaveVTXOsRequest(
		[]string{"aa:0", "bb:1"}, false,
		"bcrt1pdefault",
		"",
		map[string]string{
			"aa:0": "bcrt1pfirst",
			"bb:1": "script:5120ff",
		},
		false,
	)
	require.NoError(t, err)
	require.Len(t, req.Destinations, 2)

	first := req.Destinations["aa:0"]
	require.NotNil(t, first)
	_, ok := first.Target.(*waverpc.LeaveDestination_Address)
	require.True(t, ok, "plain value must map to address")

	second := req.Destinations["bb:1"]
	require.NotNil(t, second)
	ps, ok := second.Target.(*waverpc.LeaveDestination_PkScript)
	require.True(t, ok, "script: prefix must map to pk_script")
	require.Equal(t, []byte{0x51, 0x20, 0xff}, ps.PkScript)
}

// TestBuildLeaveVTXOsRequestRejectsOverridesWithAll verifies that
// --destination + --all fail fast at the CLI layer, matching the
// daemon's rejection but saving a round trip.
func TestBuildLeaveVTXOsRequestRejectsOverridesWithAll(t *testing.T) {
	t.Parallel()

	_, err := buildLeaveVTXOsRequest(
		nil, true, "bcrt1pdefault", "", map[string]string{
			"aa:0": "bcrt1pfirst",
		},
		false,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--all")
}

// TestBuildLeaveVTXOsRequestRejectsOutpointAndAll verifies that a
// caller can't combine --outpoint and --all on the CLI.
func TestBuildLeaveVTXOsRequestRejectsOutpointAndAll(t *testing.T) {
	t.Parallel()

	_, err := buildLeaveVTXOsRequest(
		[]string{
			"aa:0",
		},
		true, "bcrt1pdefault", "",
		nil,
		false,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mutually exclusive")
}

// TestBuildLeaveVTXOsRequestRejectsInvalidPkScriptHex locks in the
// error path for a typo in --pk-script.
func TestBuildLeaveVTXOsRequestRejectsInvalidPkScriptHex(t *testing.T) {
	t.Parallel()

	_, err := buildLeaveVTXOsRequest(
		[]string{
			"aa:0",
		},
		false, "", "not-hex",
		nil,
		false,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --pk-script hex")
}

// TestParseDestinationValueAddress verifies that a plain string is
// treated as an address.
func TestParseDestinationValueAddress(t *testing.T) {
	t.Parallel()

	dest, err := parseDestinationValue("bcrt1pexample")
	require.NoError(t, err)

	addr, ok := dest.Target.(*waverpc.LeaveDestination_Address)
	require.True(t, ok)
	require.Equal(t, "bcrt1pexample", addr.Address)
}

// TestParseDestinationValueScriptPrefix verifies the "script:<hex>"
// form maps to the pk_script branch.
func TestParseDestinationValueScriptPrefix(t *testing.T) {
	t.Parallel()

	dest, err := parseDestinationValue("script:5120aabbcc")
	require.NoError(t, err)

	ps, ok := dest.Target.(*waverpc.LeaveDestination_PkScript)
	require.True(t, ok)
	require.Equal(t,
		[]byte{0x51, 0x20, 0xaa, 0xbb, 0xcc},
		ps.PkScript,
	)
}

// TestParseDestinationValueInvalidScriptHex locks in the error
// path for a malformed "script:<hex>" value.
func TestParseDestinationValueInvalidScriptHex(t *testing.T) {
	t.Parallel()

	_, err := parseDestinationValue("script:not-hex")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid script hex")
}

// TestParseDestinationValueEmpty verifies that an empty value is
// rejected rather than silently treated as an address.
func TestParseDestinationValueEmpty(t *testing.T) {
	t.Parallel()

	_, err := parseDestinationValue("")
	require.Error(t, err)
}

// TestConfirmLeaveAllIfNeededJSONStillPrompts is the C-1 regression
// guard: a request whose `selection.all` was set via the --request-json input
// path (i.e. without ever entering the flag callback) must still hit
// the y/N prompt. Before the fix the prompt lived inside the flag
// callback and the JSON path silently bypassed it.
func TestConfirmLeaveAllIfNeededJSONStillPrompts(t *testing.T) {
	t.Parallel()

	cmd, _ := newLeaveTestCmd(t, "n\n")

	req := &waverpc.LeaveVTXOsRequest{
		Selection: &waverpc.LeaveVTXOsRequest_All{
			All: true,
		},
		DefaultDestination: &waverpc.LeaveDestination{
			Target: &waverpc.LeaveDestination_Address{
				Address: "bcrt1pexample",
			},
		},
	}

	err := confirmLeaveAllIfNeeded(cmd, req)
	require.Error(
		t, err, "selection=all without --yes/--dry-run must abort "+
			"when stdin says 'n'",
	)
	require.Contains(t, err.Error(), "aborted by user")
}

// TestConfirmLeaveAllIfNeededAcceptsYes verifies the prompt accepts
// "y" and proceeds.
func TestConfirmLeaveAllIfNeededAcceptsYes(t *testing.T) {
	t.Parallel()

	cmd, _ := newLeaveTestCmd(t, "y\n")

	req := &waverpc.LeaveVTXOsRequest{
		Selection: &waverpc.LeaveVTXOsRequest_All{
			All: true,
		},
	}

	require.NoError(t, confirmLeaveAllIfNeeded(cmd, req))
}

// TestConfirmLeaveAllIfNeededNonTTYRefusesPrompt is the agent-cli
// regression guard: when stdin is not a terminal (the production
// agent / pipeline path), --all without --yes or --dry-run must NOT
// hit the y/N prompt. Instead the function fails fast with an
// INVALID_ARGS envelope so an agent gets exit code 2 and a clear
// error directing it to pass --yes or --dry-run, rather than
// hanging on a read of a closed stdin.
func TestConfirmLeaveAllIfNeededNonTTYRefusesPrompt(t *testing.T) {
	// NOT t.Parallel() — we override the package-level
	// stdinIsTTY indirection for this test and a parallel sibling
	// could see the override.

	prev := stdinIsTTY
	stdinIsTTY = func(*cobra.Command) bool { return false }
	defer func() {
		stdinIsTTY = prev
	}()

	cmd := newVTXOsLeaveCmd()

	req := &waverpc.LeaveVTXOsRequest{
		Selection: &waverpc.LeaveVTXOsRequest_All{
			All: true,
		},
	}

	err := confirmLeaveAllIfNeeded(cmd, req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--all requires --yes")
	require.True(
		t, ErrorWasPrinted(err),
		"expected a printedError so main.go can exit with the "+
			"INVALID_ARGS code",
	)
}

// TestConfirmLeaveAllIfNeededYesFlagBypasses verifies the --yes flag
// short-circuits the prompt for scripted use.
func TestConfirmLeaveAllIfNeededYesFlagBypasses(t *testing.T) {
	t.Parallel()

	cmd, _ := newLeaveTestCmd(t, "" /* no stdin */)
	require.NoError(t, cmd.Flags().Set("yes", "true"))

	req := &waverpc.LeaveVTXOsRequest{
		Selection: &waverpc.LeaveVTXOsRequest_All{
			All: true,
		},
	}

	require.NoError(t, confirmLeaveAllIfNeeded(cmd, req))
}

// TestConfirmLeaveAllIfNeededDryRunBypasses verifies that a dry-run
// preview is not gated on the prompt — the user is just previewing,
// not moving funds.
func TestConfirmLeaveAllIfNeededDryRunBypasses(t *testing.T) {
	t.Parallel()

	cmd, _ := newLeaveTestCmd(t, "" /* no stdin */)

	req := &waverpc.LeaveVTXOsRequest{
		Selection: &waverpc.LeaveVTXOsRequest_All{
			All: true,
		},
		DryRun: true,
	}

	require.NoError(t, confirmLeaveAllIfNeeded(cmd, req))
}

// TestConfirmLeaveAllIfNeededOutpointSelectionSkipsPrompt verifies
// that selection=outpoints is unaffected — only --all triggers the
// gate.
func TestConfirmLeaveAllIfNeededOutpointSelectionSkipsPrompt(t *testing.T) {
	t.Parallel()

	cmd, _ := newLeaveTestCmd(t, "" /* no stdin */)

	req := &waverpc.LeaveVTXOsRequest{
		Selection: &waverpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &waverpc.OutpointSelection{
				Outpoints: []string{
					"aa:0",
				},
			},
		},
	}

	require.NoError(t, confirmLeaveAllIfNeeded(cmd, req))
}

// TestConfirmLeaveAllIfNeededRejectsBlankInput verifies that simply
// pressing enter at the prompt aborts (default-N posture).
func TestConfirmLeaveAllIfNeededRejectsBlankInput(t *testing.T) {
	t.Parallel()

	cmd, _ := newLeaveTestCmd(t, "\n")

	req := &waverpc.LeaveVTXOsRequest{
		Selection: &waverpc.LeaveVTXOsRequest_All{
			All: true,
		},
	}

	err := confirmLeaveAllIfNeeded(cmd, req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "aborted by user")
}
