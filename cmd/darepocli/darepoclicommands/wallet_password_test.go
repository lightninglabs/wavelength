package darepoclicommands

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReadMaskedPasswordMasksInput confirms each entered byte is
// echoed as a single asterisk and the raw password is never written
// to the masked output stream.
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

// TestReadMaskedPasswordBackspace confirms backspace removes the last
// byte and erases one mask character.
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

// TestReadMaskedPasswordUTF8Backspace confirms backspace removes the
// full last rune (multi-byte sequence) and erases as many masks as
// the rune had bytes.
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

// TestReadMaskedPasswordInterrupt confirms Ctrl-C terminates entry
// with an explicit error and the password bytes already typed are
// discarded.
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

// TestReadMaskedPasswordCtrlD confirms Ctrl-D (EOF byte) submits the
// already-typed password when non-empty.
func TestReadMaskedPasswordCtrlD(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	password, err := readMaskedPassword(
		strings.NewReader("secret\x04"), &output,
	)
	require.NoError(t, err)
	require.Equal(t, []byte("secret"), password)
}

// TestReadMaskedPasswordCtrlDEmpty confirms Ctrl-D on an empty buffer
// surfaces an explicit error rather than returning an empty password.
func TestReadMaskedPasswordCtrlDEmpty(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	password, err := readMaskedPassword(
		strings.NewReader("\x04"), &output,
	)
	require.Nil(t, password)
	require.ErrorContains(t, err, "unable to read password")
}

// TestReadMaskedPasswordWrapsReadError confirms an underlying read
// error is wrapped (not swallowed) and surfaces with the
// "unable to read password" prefix.
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

// TestZeroBytes confirms zeroBytes overwrites every byte of the
// supplied slice with zero.
func TestZeroBytes(t *testing.T) {
	t.Parallel()

	in := []byte("hunter2hunter2")
	zeroBytes(in)
	for i, b := range in {
		require.Zero(t, b, "byte %d not zeroed", i)
	}

	// Nil and empty inputs must not panic.
	zeroBytes(nil)
	zeroBytes([]byte{})
}

// errReader returns a configured error on every Read.
type errReader struct {
	err error
}

// Read implements io.Reader, always returning the configured error
// (or io.EOF if no error is configured).
func (r errReader) Read(_ []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}

	return 0, io.EOF
}
