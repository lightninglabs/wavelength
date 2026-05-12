package harness

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrefixedWriterPrefixesCompletedLines(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writer := newPrefixedWriter(&buf, "alice")

	_, err := writer.Write([]byte("first line\nsecond"))
	require.NoError(t, err)

	_, err = writer.Write([]byte(" line\nthird line\n"))
	require.NoError(t, err)

	require.Equal(
		t, "[alice] first line\n[alice] second line\n[alice] third "+
			"line\n", buf.String(),
	)
}
