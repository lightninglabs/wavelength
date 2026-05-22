package darepoclicommands

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
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
