package waveclicommands

import (
	"bytes"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// withStdin swaps os.Stdin for the duration of the test and restores it
// afterwards so a case can drive defaultStdinIsTTY against a concrete
// descriptor (a real terminal is not available under `go test`).
func withStdin(t *testing.T, f *os.File) {
	t.Helper()

	prev := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = prev
	})
}

// requireInteractiveStdin marks a sequential prompt test as interactive
// without weakening production detection for embedded buffers and pipes.
func requireInteractiveStdin(t *testing.T) {
	t.Helper()

	previous := stdinIsTTY
	stdinIsTTY = func(*cobra.Command) bool { return true }
	t.Cleanup(func() {
		stdinIsTTY = previous
	})
}

// TestDefaultStdinIsTTY pins the consent gate's non-interactive
// detection. The gate refuses to prompt unless stdin is a real
// terminal, so every non-terminal descriptor must report false — a
// character-device check alone was not enough, because /dev/null and a
// closed descriptor are both character devices yet are the most common
// non-interactive agent/CI stdin shapes. Misreading either as a TTY
// prints a y/N prompt that immediately reads EOF, the exact failure the
// gate exists to prevent.
func TestDefaultStdinIsTTY(t *testing.T) {
	// This test mutates the process-global os.Stdin, so it must not run
	// in parallel with other tests that may read it.

	// A custom reader is still a non-terminal stream. Embedded callers must
	// opt into consuming it with the same explicit flags as shell
	// pipelines.
	t.Run("custom reader is not a tty", func(t *testing.T) {
		cmd := &cobra.Command{}
		cmd.SetIn(bytes.NewBufferString("y\n"))

		require.False(t, defaultStdinIsTTY(cmd))
	})

	// /dev/null is a character device but not a terminal: the
	// regression case. It must be refused, not prompted.
	t.Run("dev null is not a tty", func(t *testing.T) {
		devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
		require.NoError(t, err)
		t.Cleanup(func() { _ = devNull.Close() })

		withStdin(t, devNull)

		cmd := &cobra.Command{}
		require.False(t, defaultStdinIsTTY(cmd))
	})

	// A pipe (the shell `echo | wavecli` shape) is not a terminal.
	t.Run("pipe is not a tty", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = r.Close()
			_ = w.Close()
		})

		withStdin(t, r)

		cmd := &cobra.Command{}
		require.False(t, defaultStdinIsTTY(cmd))
	})

	// A closed descriptor (the `wavecli 0<&-` shape) cannot be a
	// terminal and its Fd ioctl fails, so it too must be refused.
	t.Run("closed descriptor is not a tty", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		_ = w.Close()
		require.NoError(t, r.Close())

		withStdin(t, r)

		cmd := &cobra.Command{}
		require.False(t, defaultStdinIsTTY(cmd))
	})
}
