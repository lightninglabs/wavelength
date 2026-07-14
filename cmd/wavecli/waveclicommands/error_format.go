package waveclicommands

import (
	"io"
	"strconv"
	"strings"

	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultErrorCode = "EXECUTION_FAILED"
	invalidArgsCode  = "INVALID_ARGS"
	walletLockedCode = "WALLET_LOCKED"
)

type commandError struct {
	code    string
	message string
	details string
}

// PrintCommandError writes err as the CLI's structured JSON error envelope.
func PrintCommandError(err error) error {
	formatted := formatCommandError(err)

	return PrintErrorDetails(
		formatted.code, formatted.message, formatted.details,
	)
}

// printCommandError writes the formatted error envelope to the supplied writer.
func printCommandError(w io.Writer, err error) error {
	formatted := formatCommandError(err)

	return printErrorDetails(
		w, formatted.code, formatted.message, formatted.details,
	)
}

// formatCommandError converts an execution error into the public CLI envelope.
func formatCommandError(err error) commandError {
	if err == nil {
		return commandError{
			code:    defaultErrorCode,
			message: "unknown error",
		}
	}

	if IsCobraArgError(err) {
		return commandError{
			code:    invalidArgsCode,
			message: err.Error(),
		}
	}

	if waverpc.IsWalletNotReadyError(err) {
		return formatWalletNotReadyError(err)
	}

	if rpcErr, ok := parseRPCErrorChain(err.Error()); ok {
		return commandError{
			code:    grpcCodeNameToCLI(rpcErr.code),
			message: rpcErr.message,
			details: rpcErr.details,
		}
	}

	if st, ok := status.FromError(err); ok {
		return commandError{
			code:    grpcCodeToCLI(st.Code()),
			message: st.Message(),
		}
	}

	return commandError{
		code:    defaultErrorCode,
		message: err.Error(),
	}
}

// formatWalletNotReadyError gives wallet lifecycle preconditions the
// actionable CLI wording users need instead of a raw gRPC status string.
func formatWalletNotReadyError(err error) commandError {
	msg := "wallet is not ready; check `wavecli getinfo`"
	state, _ := waverpc.WalletNotReadyState(err)
	switch state {
	case waverpc.WalletNotReadyStateNone:
		msg = "wallet is not created; run `wavecli create`"

	case waverpc.WalletNotReadyStateLocked:
		msg = "wallet is locked; run `wavecli unlock`"

	case waverpc.WalletNotReadyStateSyncing:
		msg = "wallet is syncing; try again once sync completes"
	}

	return commandError{
		code:    walletLockedCode,
		message: msg,
	}
}

type rpcErrorChain struct {
	code    string
	message string
	details string
}

// parseRPCErrorChain extracts the deepest gRPC status from nested error text.
func parseRPCErrorChain(msg string) (rpcErrorChain, bool) {
	const (
		rpcPrefix = "rpc error: code = "
		descSep   = " desc = "
	)

	// This intentionally depends on grpc-go's long-standing status error
	// string format. Keeping the recovery here lets the CLI improve errors
	// that have already crossed process boundaries as text.
	var contexts []string
	var code string
	rest := msg
	for {
		idx := strings.Index(rest, rpcPrefix)
		if idx < 0 {
			break
		}

		if context := cleanRPCContext(rest[:idx]); context != "" {
			contexts = append(contexts, context)
		}

		afterCode := rest[idx+len(rpcPrefix):]
		codeEnd := strings.Index(afterCode, descSep)
		if codeEnd < 0 {
			return rpcErrorChain{}, false
		}

		code = strings.TrimSpace(afterCode[:codeEnd])
		rest = afterCode[codeEnd+len(descSep):]
	}

	if code == "" {
		return rpcErrorChain{}, false
	}

	return rpcErrorChain{
		code:    code,
		message: cleanRPCDescription(rest),
		details: strings.Join(contexts, ": "),
	}, true
}

// cleanRPCContext normalizes one wrapper segment from a nested gRPC error.
func cleanRPCContext(context string) string {
	context = strings.TrimSpace(context)
	context = strings.TrimSuffix(context, ":")

	return strings.TrimSpace(context)
}

// cleanRPCDescription normalizes the deepest gRPC status description.
func cleanRPCDescription(desc string) string {
	desc = strings.TrimSpace(desc)
	if unquoted, err := strconv.Unquote(desc); err == nil {
		return unquoted
	}

	return desc
}

// grpcCodeToCLI maps a gRPC code to the CLI's uppercase code convention.
func grpcCodeToCLI(code codes.Code) string {
	return grpcCodeNameToCLI(code.String())
}

// grpcCodeNameToCLI maps a gRPC code name to the CLI's uppercase convention.
func grpcCodeNameToCLI(name string) string {
	switch name {
	case "OK":
		return "OK"

	case "Canceled":
		return "CANCELED"

	case "Unknown":
		return "UNKNOWN"

	case "InvalidArgument":
		return "INVALID_ARGUMENT"

	case "DeadlineExceeded":
		return "DEADLINE_EXCEEDED"

	case "NotFound":
		return "NOT_FOUND"

	case "AlreadyExists":
		return "ALREADY_EXISTS"

	case "PermissionDenied":
		return "PERMISSION_DENIED"

	case "ResourceExhausted":
		return "RESOURCE_EXHAUSTED"

	case "FailedPrecondition":
		return "FAILED_PRECONDITION"

	case "Aborted":
		return "ABORTED"

	case "OutOfRange":
		return "OUT_OF_RANGE"

	case "Unimplemented":
		return "UNIMPLEMENTED"

	case "Internal":
		return "INTERNAL"

	case "Unavailable":
		return "UNAVAILABLE"

	case "DataLoss":
		return "DATA_LOSS"

	case "Unauthenticated":
		return "UNAUTHENTICATED"

	default:
		return strings.ToUpper(name)
	}
}
