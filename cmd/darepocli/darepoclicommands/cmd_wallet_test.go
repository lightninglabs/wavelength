package darepoclicommands

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadMaskedPasswordMasksInput(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	password, err := readMaskedPassword(
		strings.NewReader("supersecret\n"), &output,
	)
	require.NoError(t, err)
	require.Equal(t, []byte("supersecret"), password)
	require.Equal(
		t,
		strings.Repeat(
			"*", len("supersecret"),
		),
		output.String(),
	)
	require.NotContains(t, output.String(), "supersecret")
}

func TestReadMaskedPasswordBackspace(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	password, err := readMaskedPassword(
		strings.NewReader("secx\x7fret\r"), &output,
	)
	require.NoError(t, err)
	require.Equal(t, []byte("secret"), password)
	require.Equal(t, "****\b \b***", output.String())
}

func TestReadMaskedPasswordUTF8Backspace(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	password, err := readMaskedPassword(
		strings.NewReader("caf\xc3\xa9\x7feteria\n"), &output,
	)
	require.NoError(t, err)
	require.Equal(t, []byte("cafeteria"), password)
	require.Equal(t, "*****\b \b\b \b******", output.String())
}

func TestReadMaskedPasswordInterrupt(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	password, err := readMaskedPassword(
		strings.NewReader("sec\x03ret\n"), &output,
	)
	require.Nil(t, password)
	require.ErrorContains(t, err, "password entry interrupted")
	require.Equal(t, "***", output.String())
}

func TestReadMaskedPasswordWrapsReadError(t *testing.T) {
	t.Parallel()

	readErr := errors.New("synthetic read failure")
	var output bytes.Buffer
	password, err := readMaskedPassword(
		errReader{err: readErr}, &output,
	)
	require.Nil(t, password)
	require.ErrorIs(t, err, readErr)
	require.ErrorContains(t, err, "unable to read password")
	require.Empty(t, output.String())
}

type errReader struct {
	err error
}

func (r errReader) Read(_ []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}

	return 0, io.EOF
}
