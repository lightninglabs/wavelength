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
		string(encoded) + `}}`
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
			"details": "` + details + `"
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
			"details": "send"
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
				`"WALLET_LOCKED","message":%q}}`
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
			"message": "receive intent already used"
		}
	}`
	require.JSONEq(t, expected, buf.String())
}

func TestPrintCommandErrorLeavesPlainErrorsGeneric(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printCommandError(&buf, errors.New("boom")))

	expected := `{
		"error": {
			"code": "EXECUTION_FAILED",
			"message": "boom"
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
			"message": "unknown error"
		}
	}`
	require.JSONEq(t, expected, buf.String())
}
