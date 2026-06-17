package darepoclicommands

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReadMaskedPassword exercises the masked-entry state machine:
// asterisk echoing, ASCII/UTF-8 backspace erase, Ctrl-C interrupt,
// Ctrl-D submit/empty, and underlying read-error wrapping.
func TestReadMaskedPassword(t *testing.T) {
	t.Parallel()

	syntheticErr := errors.New("synthetic read failure")

	cases := []struct {
		name        string
		input       string
		reader      io.Reader
		wantPass    []byte
		wantOutput  string
		checkOutput bool
		errContains string
		errIs       error
	}{
		{
			name:        "masks input",
			input:       "supersecret\n",
			wantPass:    []byte("supersecret"),
			wantOutput:  strings.Repeat("*", len("supersecret")),
			checkOutput: true,
		},
		{
			name:        "ascii backspace",
			input:       "secx\x7fret\r",
			wantPass:    []byte("secret"),
			wantOutput:  "****\b \b***",
			checkOutput: true,
		},
		{
			name:        "utf8 backspace",
			input:       "caf\xc3\xa9\x7feteria\n",
			wantPass:    []byte("cafeteria"),
			wantOutput:  "*****\b \b\b \b******",
			checkOutput: true,
		},
		{
			name:        "ctrl-c interrupt",
			input:       "sec\x03ret\n",
			wantOutput:  "***",
			checkOutput: true,
			errContains: "password entry interrupted",
		},
		{
			name:     "ctrl-d submit",
			input:    "secret\x04",
			wantPass: []byte("secret"),
		},
		{
			name:        "ctrl-d empty",
			input:       "\x04",
			errContains: "unable to read password",
		},
		{
			name: "wraps read error",
			reader: errReader{
				err: syntheticErr,
			},
			errContains: "unable to read password",
			errIs:       syntheticErr,
			wantOutput:  "",
			checkOutput: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reader := tc.reader
			if reader == nil {
				reader = strings.NewReader(tc.input)
			}

			var output bytes.Buffer
			password, err := readMaskedPassword(reader, &output)

			if tc.errContains != "" || tc.errIs != nil {
				require.Nil(t, password)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantPass, password)
			}
			if tc.errContains != "" {
				require.ErrorContains(t, err, tc.errContains)
			}
			if tc.errIs != nil {
				require.ErrorIs(t, err, tc.errIs)
			}
			if tc.checkOutput {
				require.Equal(t, tc.wantOutput, output.String())
			}
		})
	}
}

// TestReadConfirmedPasswordAcceptsMatch confirms the interactive
// confirmation flow returns the first password when both entries match.
func TestReadConfirmedPasswordAcceptsMatch(t *testing.T) {
	t.Parallel()

	var prompts []string
	values := [][]byte{
		[]byte("secret"),
		[]byte("secret"),
	}
	password, err := readConfirmedPassword(
		func(prompt string) ([]byte, error) {
			prompts = append(prompts, prompt)
			value := values[0]
			values = values[1:]

			return value, nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, []byte("secret"), password)
	require.Equal(
		t, []string{
			"Enter wallet password: ",
			"Confirm wallet password: ",
		}, prompts,
	)
}

// TestReadConfirmedPasswordRejectsMismatch confirms a mistyped
// confirmation fails locally and scrubs both entered secrets.
func TestReadConfirmedPasswordRejectsMismatch(t *testing.T) {
	t.Parallel()

	passwordBytes := []byte("secret")
	confirmationBytes := []byte("secrex")
	values := [][]byte{
		passwordBytes,
		confirmationBytes,
	}

	password, err := readConfirmedPassword(
		func(_ string) ([]byte, error) {
			value := values[0]
			values = values[1:]

			return value, nil
		},
	)
	require.Nil(t, password)
	require.ErrorIs(t, err, errPasswordConfirmationMismatch)
	require.Equal(t, []byte{0, 0, 0, 0, 0, 0}, passwordBytes)
	require.Equal(t, []byte{0, 0, 0, 0, 0, 0}, confirmationBytes)
}

// TestReadConfirmedPasswordZerosOnConfirmError confirms the first
// password entry is scrubbed if the confirmation prompt fails.
func TestReadConfirmedPasswordZerosOnConfirmError(t *testing.T) {
	t.Parallel()

	passwordBytes := []byte("secret")
	confirmErr := errors.New("confirmation failed")
	reads := 0

	password, err := readConfirmedPassword(
		func(_ string) ([]byte, error) {
			reads++
			if reads == 1 {
				return passwordBytes, nil
			}

			return nil, confirmErr
		},
	)
	require.Nil(t, password)
	require.ErrorIs(t, err, confirmErr)
	require.Equal(t, []byte{0, 0, 0, 0, 0, 0}, passwordBytes)
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
