package waveclicommands

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPrintErrorFormatsIndentedJSON(t *testing.T) {
	t.Parallel()

	const msg = `recv invoice: rpc error: code = Internal ` +
		`desc = "boom"`

	var buf bytes.Buffer
	err := printError(&buf, "EXECUTION_FAILED", msg)
	require.NoError(t, err)

	encoded, err := json.Marshal(msg)
	require.NoError(t, err)
	expected := `{"error":{"code":"EXECUTION_FAILED","message":` +
		string(encoded) + `,"retryable":false}}`
	require.JSONEq(t, expected, buf.String())

	require.Contains(t, buf.String(), "\n  \"error\": {\n")
	require.Contains(
		t, buf.String(),
		"\n    \"code\": \"EXECUTION_FAILED\"",
	)
	require.Contains(t, buf.String(), "\n    \"message\": ")
}

func TestPrintCommandErrorPromotesNestedGRPCStatus(t *testing.T) {
	t.Parallel()

	const msg = "send: rpc error: code = Internal desc = start pay: " +
		"rpc error: code = Internal desc = start pay swap: " +
		"create in-swap: CreateInSwap RPC: rpc error: code = " +
		"AlreadyExists desc = receive intent already used"
	const details = "send: start pay: start pay swap: create in-swap: " +
		"CreateInSwap RPC"

	var buf bytes.Buffer
	err := printCommandError(&buf, errors.New(msg))
	require.NoError(t, err)

	expected := `{
		"error": {
			"code": "ALREADY_EXISTS",
			"message": "receive intent already used",
			"details": "` + details + `",
			"retryable": false
		}
	}`
	require.JSONEq(t, expected, buf.String())
}

func TestPrintCommandErrorAddsStatusWrapperDetails(t *testing.T) {
	t.Parallel()

	statusErr := status.Error(
		codes.AlreadyExists, "receive intent already used",
	)
	err := fmt.Errorf("send: %w", statusErr)

	var buf bytes.Buffer
	require.NoError(t, printCommandError(&buf, err))

	expected := `{
		"error": {
			"code": "ALREADY_EXISTS",
			"message": "receive intent already used",
			"details": "send",
			"retryable": false
		}
	}`
	require.JSONEq(t, expected, buf.String())
}

func TestPrintCommandErrorMapsWalletNotReadyToStateHint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		state string
		msg   string
	}{
		{
			name:  "none",
			state: waverpc.WalletNotReadyStateNone,
			msg:   "wallet is not created; run `wavecli create`",
		},
		{
			name:  "locked",
			state: waverpc.WalletNotReadyStateLocked,
			msg:   "wallet is locked; run `wavecli unlock`",
		},
		{
			name:  "syncing",
			state: waverpc.WalletNotReadyStateSyncing,
			msg: "wallet is syncing; try again once sync " +
				"completes",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := fmt.Errorf("recv invoice: %w",
				waverpc.WalletNotReadyStateError(
					"wallet is not ready", tc.state,
				))

			var buf bytes.Buffer
			require.NoError(t, printCommandError(&buf, err))

			const expectedTemplate = `{"error":{"code":` +
				`"WALLET_LOCKED","message":%q,` +
				`"retryable":false}}`
			expected := fmt.Sprintf(expectedTemplate, tc.msg)
			require.JSONEq(t, expected, buf.String())
		})
	}
}

func TestPrintCommandErrorHandlesBareGRPCStatus(t *testing.T) {
	t.Parallel()

	err := status.Error(codes.AlreadyExists, "receive intent already used")

	var buf bytes.Buffer
	require.NoError(t, printCommandError(&buf, err))

	expected := `{
		"error": {
			"code": "ALREADY_EXISTS",
			"message": "receive intent already used",
			"retryable": false
		}
	}`
	require.JSONEq(t, expected, buf.String())
}

func TestPrintCommandErrorAddsRetryMetadata(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		code      codes.Code
		retryable bool
	}{
		{
			name:      "unavailable",
			code:      codes.Unavailable,
			retryable: true,
		},
		{
			name:      "resource exhausted",
			code:      codes.ResourceExhausted,
			retryable: true,
		},
		// DEADLINE_EXCEEDED and ABORTED may fire after a fund-moving
		// RPC was already accepted, so they carry remediation but are
		// not retryable — a blind retry could double-spend.
		{
			name:      "deadline",
			code:      codes.DeadlineExceeded,
			retryable: false,
		},
		{
			name:      "aborted",
			code:      codes.Aborted,
			retryable: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			err := status.Error(tc.code, "temporary failure")
			require.NoError(t, printCommandError(&buf, err))

			var envelope errorEnvelope
			require.NoError(
				t,
				json.Unmarshal(
					buf.Bytes(), &envelope,
				),
			)
			require.Equal(t, tc.retryable, envelope.Error.Retryable)
			require.NotEmpty(t, envelope.Error.Remediation)
		})
	}
}

func TestPrintCommandErrorClassifiesCredentialFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		err             error
		remediationPart string
	}{
		{
			name: "TLS certificate",
			err: errors.New(
				"unable to load TLS cert: file does not exist",
			),
			remediationPart: "--tlscertpath",
		},
		{
			name: "macaroon",
			err: errors.New(
				"unable to load macaroon: file does not exist",
			),
			remediationPart: "--macaroonpath",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			require.NoError(t, printCommandError(&buf, tc.err))

			var envelope errorEnvelope
			require.NoError(
				t,
				json.Unmarshal(
					buf.Bytes(), &envelope,
				),
			)
			require.Equal(t, "AUTH_FAILURE", envelope.Error.Code)
			require.False(t, envelope.Error.Retryable)
			require.Contains(
				t, envelope.Error.Remediation,
				tc.remediationPart,
			)
		})
	}
}

func TestPrintCommandErrorPreservesLocalClassification(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := newCLIError(
		ExitInvalidArgs, errors.New("unknown output format \"yaml\""),
	)
	require.NoError(t, printCommandError(&buf, err))

	var envelope errorEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &envelope))
	require.Equal(t, invalidArgsCode, envelope.Error.Code)
	require.False(t, envelope.Error.Retryable)
}

func TestPrintCommandErrorLeavesPlainErrorsGeneric(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printCommandError(&buf, errors.New("boom")))

	expected := `{
		"error": {
			"code": "EXECUTION_FAILED",
			"message": "boom",
			"retryable": false
		}
	}`
	require.JSONEq(t, expected, buf.String())
}

func TestPrintCommandErrorHandlesNilError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printCommandError(&buf, nil))

	expected := `{
		"error": {
			"code": "EXECUTION_FAILED",
			"message": "unknown error",
			"retryable": false
		}
	}`
	require.JSONEq(t, expected, buf.String())
}
