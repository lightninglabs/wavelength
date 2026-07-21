package waveclicommands

import (
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Exit codes follow the agent-cli skill's semantic-exit-code table so
// agents can branch on the failure category without parsing prose.
// 0 is success and is set by cobra implicitly when RunE returns nil.
const (
	// ExitGenericError is the catch-all failure code.
	ExitGenericError = 1

	// ExitInvalidArgs indicates the CLI rejected the invocation
	// before reaching the daemon: missing required flag, malformed
	// outpoint, conflicting --offchain/--onchain, etc.
	ExitInvalidArgs = 2

	// ExitAuthFailure indicates a wallet authentication failure
	// (wrong password, locked wallet on a verb that requires it).
	ExitAuthFailure = 3

	// ExitNotFound indicates a queried resource does not exist on the
	// daemon (unknown round id, missing exit job, etc.).
	ExitNotFound = 4

	// ExitConfirmationRequired indicates that a valid fund-moving action
	// needs explicit approval in this non-interactive environment.
	ExitConfirmationRequired = 5

	// ExitDryRunOK indicates a --dry-run invocation passed all local
	// validation; no RPC was dispatched.
	ExitDryRunOK = 10
)

// cliError wraps an underlying error with the exit code that main.go
// should return when the command bubbles the error out. Use New /
// Wrap to attach a code; everything else stays as a normal error.
type cliError struct {
	code int
	err  error
}

// newCLIError attaches an exit code to a fresh error.
func newCLIError(code int, err error) error {
	if err == nil {
		return nil
	}

	return &cliError{code: code, err: err}
}

// Error returns the underlying error message.
func (e *cliError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the underlying error so errors.Is / errors.As keep
// working across the wrapper.
func (e *cliError) Unwrap() error {
	return e.err
}

// ExitCodeFor returns the exit code main.go should use for err. The
// mapping is: explicit cliError code wins, then printedError's own
// code-derived mapping; otherwise gRPC status codes map to the
// closest semantic exit code; otherwise generic.
func ExitCodeFor(err error) int {
	if err == nil {
		return 0
	}

	var ce *cliError
	if errors.As(err, &ce) {
		return ce.code
	}

	var pe *printedError
	if errors.As(err, &pe) {
		return pe.ExitCode()
	}

	// Map common gRPC status codes that the daemon returns onto the
	// semantic exit codes the agent-cli skill prescribes. Anything
	// not in this map falls through to the generic code so agents
	// can still branch on the obvious cases without us needing to
	// enumerate every daemon-side failure.
	if s, ok := status.FromError(err); ok {
		// Only the common cases are mapped; everything else falls
		// through to ExitGenericError on purpose.
		switch s.Code() { //nolint:exhaustive
		case codes.InvalidArgument, codes.OutOfRange,
			codes.FailedPrecondition:
			return ExitInvalidArgs

		case codes.Unauthenticated, codes.PermissionDenied:
			return ExitAuthFailure

		case codes.NotFound:
			return ExitNotFound
		}
	}

	// Cobra and pflag don't expose stable error sentinels for their
	// pre-RunE validation failures (missing required flag, unknown
	// flag, wrong positional count, invalid flag value). Treating
	// them all as ExitGenericError would force agents to parse the
	// prose to learn this was a flag-shape failure, so we classify
	// by message prefix here. The set of prefixes is finite and
	// listed in IsCobraArgError; everything else stays generic.
	if IsCobraArgError(err) {
		return ExitInvalidArgs
	}

	return ExitGenericError
}

// IsCobraArgError reports whether err looks like a cobra or pflag
// pre-RunE validation failure. main.go uses this to emit an
// INVALID_ARGS structured envelope (instead of the generic
// EXECUTION_FAILED) so the stderr surface matches the
// exit-code-2 mapping ExitCodeFor returns for the same error.
func IsCobraArgError(err error) bool {
	if err == nil {
		return false
	}
	// Already-classified errors keep their original mapping; this
	// helper is only for the bare-message cobra/pflag cases.
	var ce *cliError
	if errors.As(err, &ce) {
		return false
	}
	var pe *printedError
	if errors.As(err, &pe) {
		return false
	}

	msg := err.Error()
	for _, prefix := range cobraArgErrorPrefixes {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}

	return false
}

// cobraArgErrorPrefixes lists the message prefixes cobra and pflag
// use for their pre-RunE validation failures. The list mirrors the
// fmt.Errorf calls in github.com/spf13/cobra and
// github.com/spf13/pflag as of v1.10 / v1.0 respectively.
var cobraArgErrorPrefixes = []string{
	"required flag(s)",        // pflag: missing required flag
	"unknown flag:",           // pflag: unrecognized flag
	"unknown shorthand flag:", // pflag: unrecognized -x
	"invalid argument",        // pflag: bad value for typed flag
	"unknown command",         // cobra: unknown subcommand
	"accepts ",                // cobra: ExactArgs/MaximumNArgs et al.
	"requires at least",       // cobra: MinimumNArgs
	"requires between",        // cobra: RangeArgs
	"subcommand is required",  // cobra: NoArgs on a parent
	"if any flags in the group",
}
