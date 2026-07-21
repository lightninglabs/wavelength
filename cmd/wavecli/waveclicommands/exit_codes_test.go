package waveclicommands

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestExitCodeForCLIError verifies the explicit cliError exit codes
// take precedence and survive errors.As-style wrapping.
func TestExitCodeForCLIError(t *testing.T) {
	t.Parallel()

	err := newCLIError(ExitDryRunOK, errors.New("preview"))

	require.Equal(t, ExitDryRunOK, ExitCodeFor(err))
	require.Equal(t, "preview", err.Error())
}

// TestExitCodeForPrintedError verifies the printedError returned from
// PrintError participates in the same exit-code mapping so callers
// that `return PrintError(...)` get the right semantic code.
func TestExitCodeForPrintedError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code     string
		expected int
	}{
		{
			"INVALID_STATUS",
			ExitInvalidArgs,
		},
		{
			"INVALID_OUTPOINT",
			ExitInvalidArgs,
		},
		{
			"AUTH_FAILURE",
			ExitAuthFailure,
		},
		{
			"METHOD_NOT_FOUND",
			ExitNotFound,
		},
		{
			"CONFIRMATION_REQUIRED",
			ExitConfirmationRequired,
		},
		{
			"DRY_RUN_OK",
			ExitDryRunOK,
		},
		{
			"SOMETHING_ELSE",
			ExitGenericError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			err := &printedError{code: tc.code, msg: "x"}
			require.Equal(t, tc.expected, ExitCodeFor(err))
			require.True(t, ErrorWasPrinted(err))
		})
	}
}

// TestExitCodeForGRPCStatus verifies daemon-returned gRPC status
// codes map onto the matching semantic exit codes so agents can
// branch on the failure category without parsing message prose.
func TestExitCodeForGRPCStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code     codes.Code
		expected int
	}{
		{
			codes.InvalidArgument,
			ExitInvalidArgs,
		},
		{
			codes.OutOfRange,
			ExitInvalidArgs,
		},
		{
			codes.FailedPrecondition,
			ExitInvalidArgs,
		},
		{
			codes.Unauthenticated,
			ExitAuthFailure,
		},
		{
			codes.PermissionDenied,
			ExitAuthFailure,
		},
		{
			codes.NotFound,
			ExitNotFound,
		},
		{
			codes.Unavailable,
			ExitGenericError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			err := status.Error(tc.code, "x")
			require.Equal(t, tc.expected, ExitCodeFor(err))
		})
	}
}

// TestExitCodeForNil confirms the zero-error path returns 0 so a
// successful run from main.go doesn't accidentally exit non-zero.
func TestExitCodeForNil(t *testing.T) {
	t.Parallel()

	require.Equal(t, 0, ExitCodeFor(nil))
	require.False(t, ErrorWasPrinted(nil))
}

// TestExitCodeForCobraArgError covers the cobra/pflag pre-RunE
// validation classifier. Each prefix the upstream libraries emit
// must map onto ExitInvalidArgs so an agent that passes the wrong
// flags sees exit code 2 instead of the generic 1.
func TestExitCodeForCobraArgError(t *testing.T) {
	t.Parallel()

	cases := []string{
		"required flag(s) \"outpoint\" not set",
		"unknown flag: --bogus",
		"unknown shorthand flag: 'q' in -q",
		"invalid argument \"foo\" for \"--amt\"",
		"unknown command \"shrug\" for \"wavecli\"",
		"accepts 1 arg(s), received 0",
		"requires at least 1 arg(s), received 0",
		"requires between 1 and 3 arg(s), received 5",
		"subcommand is required",
	}

	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			err := errors.New(msg)
			require.True(t, IsCobraArgError(err))
			require.Equal(t, ExitInvalidArgs, ExitCodeFor(err))
		})
	}
}

// TestIsCobraArgErrorSkipsClassifiedWrappers confirms that errors
// already carrying a cliError or printedError exit code are NOT
// re-classified as cobra errors. The wrapper's code wins.
func TestIsCobraArgErrorSkipsClassifiedWrappers(t *testing.T) {
	t.Parallel()

	// A printedError whose message happens to start with a cobra
	// prefix should still keep its own ExitCode.
	pe := &printedError{
		code: "DRY_RUN_OK",
		msg:  "required flag(s) \"x\" not set",
	}
	require.False(t, IsCobraArgError(pe))
	require.Equal(t, ExitDryRunOK, ExitCodeFor(pe))

	// A cliError wrapping a cobra-shaped message: the wrapper's
	// explicit code wins.
	ce := newCLIError(
		ExitAuthFailure, errors.New("unknown flag: --x"),
	)
	require.False(t, IsCobraArgError(ce))
	require.Equal(t, ExitAuthFailure, ExitCodeFor(ce))
}

// TestIsCobraArgErrorIgnoresUnrelated guards against the prefix
// classifier accidentally swallowing a plain error whose message
// happens to share a leading word with cobra's vocabulary but is
// actually unrelated (an RPC failure, a network error, etc.).
func TestIsCobraArgErrorIgnoresUnrelated(t *testing.T) {
	t.Parallel()

	cases := []string{
		"daemon unreachable: connection refused",
		"unrelated error",
		"a flag set incorrectly somewhere",
	}

	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			err := errors.New(msg)
			require.False(t, IsCobraArgError(err))
		})
	}

	require.False(t, IsCobraArgError(nil))
}
