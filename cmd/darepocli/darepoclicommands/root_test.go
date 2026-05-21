package darepoclicommands

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrintErrorFormatsIndentedJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printError(
		&buf, "EXECUTION_FAILED",
		`recv invoice: rpc error: code = Internal desc = "boom"`,
	)
	require.NoError(t, err)

	require.JSONEq(t, `{
		"error": {
			"code": "EXECUTION_FAILED",
			"message": "recv invoice: rpc error: code = Internal desc = \"boom\""
		}
	}`, buf.String())

	require.Contains(t, buf.String(), "\n  \"error\": {\n")
	require.Contains(t, buf.String(), "\n    \"code\": \"EXECUTION_FAILED\"")
	require.Contains(t, buf.String(), "\n    \"message\": ")
}
