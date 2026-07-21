package waveclicommands

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// errPasswordConfirmationMismatch is returned when the create-wallet
// password confirmation does not match the first interactive entry.
var errPasswordConfirmationMismatch = errors.New("passwords do not match")

// zeroBytes overwrites every byte of b with zero. Best-effort
// scrubbing of cryptographic material before it falls out of scope —
// Go's GC means we cannot guarantee the bytes are unreachable, but
// zeroing the slice closes the dominant exposure window (heap-dump,
// post-call inspection of the same goroutine's stack/heap).
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// readSeedPassphrase reads the optional aezeed seed passphrase from
// the same sources used for the wallet password: WAVED_SEED_PASSPHRASE
// env var, then --seed-passphrase-file flag. Returns an empty (not
// nil) slice when neither source is set so the passphrase is genuinely
// optional. The CLI does NOT accept the seed passphrase via argv —
// `ps aux` and `/proc/<pid>/cmdline` would otherwise leak it.
func readSeedPassphrase(cmd *cobra.Command) ([]byte, error) {
	if env := os.Getenv("WAVED_SEED_PASSPHRASE"); env != "" {
		return []byte(env), nil
	}

	path, _ := cmd.Flags().GetString("seed-passphrase-file")
	if path == "" {
		return []byte{}, nil
	}

	// The file path is explicitly provided by the CLI user.
	data, err := os.ReadFile(path) //nolint:gosec // G304
	if err != nil {
		return nil, fmt.Errorf("unable to read seed passphrase "+
			"file: %w", err)
	}

	return []byte(strings.TrimRight(string(data), "\n\r")), nil
}

// readPassword reads the wallet password from one of these sources, in
// priority order: WAVED_WALLET_PASSWORD env > --wallet-password-file
// flag > explicit --password-stdin > interactive prompt. The CLI never
// consumes stdin implicitly or accepts passwords via argv.
func readPassword(cmd *cobra.Command) ([]byte, error) {
	return readWalletPassword(cmd, false)
}

// readPasswordConfirmed reads the wallet password with interactive
// confirmation. Non-interactive sources (env, file, explicit stdin) stay
// single-read so automated callers can continue to provide exactly one
// secret.
func readPasswordConfirmed(cmd *cobra.Command) ([]byte, error) {
	return readWalletPassword(cmd, true)
}

// readWalletPassword reads the wallet password from one of the supported
// sources, optionally confirming only the interactive prompt path.
func readWalletPassword(cmd *cobra.Command,
	confirmInteractive bool) ([]byte, error) {

	// Check environment variable first — takes priority so that
	// callers with piped stdin (e.g. REPL, CI) can override without
	// fighting over stdin.
	if envPass := os.Getenv(
		"WAVED_WALLET_PASSWORD"); envPass != "" {
		return []byte(envPass), nil
	}

	// Check --wallet-password-file flag.
	passFile, _ := cmd.Flags().GetString("wallet-password-file")
	if passFile != "" {
		// The file path is explicitly provided by the CLI user; a
		// variable path is the intended API.
		data, err := os.ReadFile(passFile) //nolint:gosec // G304
		if err != nil {
			return nil, fmt.Errorf("unable to read password "+
				"file: %w", err)
		}

		// Strip trailing newline.
		return []byte(strings.TrimRight(
			string(data), "\n\r",
		)), nil
	}

	passwordStdin, _ := cmd.Flags().GetBool("password-stdin")
	if passwordStdin {
		scanner := bufio.NewScanner(cmd.InOrStdin())
		if scanner.Scan() {
			return []byte(scanner.Text()), nil
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("unable to read password from "+
				"stdin: %w", err)
		}

		return nil, fmt.Errorf("unable to read password from stdin")
	}

	if !canPrompt(cmd) {
		return nil, fmt.Errorf("wallet password input required: set " +
			"WAVED_WALLET_PASSWORD, use --wallet-password-file, " +
			"or explicitly pass --password-stdin")
	}

	readInteractive := func(prompt string) ([]byte, error) {
		return readInteractivePassword(cmd, prompt)
	}
	if confirmInteractive {
		return readConfirmedPassword(readInteractive)
	}

	return readInteractive("Enter wallet password: ")
}

// readInteractivePassword reads one password from the controlling TTY
// using the supplied prompt.
func readInteractivePassword(cmd *cobra.Command,
	prompt string) ([]byte, error) {

	if err := writePrompt(cmd, prompt); err != nil {
		return nil, fmt.Errorf("write password prompt: %w", err)
	}

	input := cmd.InOrStdin()
	output := cmd.ErrOrStderr()
	inputFile, ok := input.(*os.File)
	if !ok {
		password, err := readMaskedPassword(input, output)
		_, printErr := fmt.Fprintln(output)
		if err != nil {
			return nil, err
		}
		if printErr != nil {
			return nil, fmt.Errorf("unable to finalize password "+
				"prompt: %w", printErr)
		}

		return password, nil
	}

	// Interactive prompt (TTY).
	fd := int(inputFile.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("unable to configure terminal for "+
			"password input: %w", err)
	}
	restoreTerminal := func() {
		if oldState == nil {
			return
		}

		_ = term.Restore(fd, oldState)
		oldState = nil
	}
	defer restoreTerminal()

	password, err := readMaskedPassword(input, output)
	restoreTerminal()
	_, printErr := fmt.Fprintln(output)
	if err != nil {
		return nil, err
	}
	if printErr != nil {
		return nil, fmt.Errorf("unable to finalize password prompt: %w",
			printErr)
	}

	return password, nil
}

// readConfirmedPassword reads the password twice and rejects mismatches
// before the daemon sees the secret.
func readConfirmedPassword(read func(string) ([]byte, error)) ([]byte, error) {
	password, err := read("Enter wallet password: ")
	if err != nil {
		return nil, err
	}

	confirmation, err := read("Confirm wallet password: ")
	if err != nil {
		zeroBytes(password)

		return nil, err
	}
	defer zeroBytes(confirmation)

	if !bytes.Equal(password, confirmation) {
		zeroBytes(password)

		return nil, errPasswordConfirmationMismatch
	}

	return password, nil
}

// readMaskedPassword reads a single password line while echoing one
// asterisk per entered byte. Backspace removes the last UTF-8 rune
// worth of bytes. It expects the terminal to already be in raw mode.
func readMaskedPassword(input io.Reader, output io.Writer) ([]byte, error) {
	var password []byte
	var buf [1]byte

	for {
		n, err := input.Read(buf[:])
		if err != nil {
			if errors.Is(err, io.EOF) && len(password) > 0 {
				return password, nil
			}

			return nil, fmt.Errorf("unable to read password: %w",
				err)
		}
		if n == 0 {
			continue
		}

		switch b := buf[0]; b {
		case '\r', '\n':
			return password, nil

		case 3: // Ctrl-C.
			return nil, fmt.Errorf("password entry interrupted")

		case 4: // Ctrl-D.
			if len(password) > 0 {
				return password, nil
			}

			return nil, fmt.Errorf("unable to read password")

		case '\b', 0x7f:
			if len(password) == 0 {
				continue
			}

			_, size := utf8.DecodeLastRune(password)
			password = password[:len(password)-size]

			erase := strings.Repeat("\b \b", size)
			if _, err := fmt.Fprint(output, erase); err != nil {
				return nil, fmt.Errorf("unable to mask "+
					"password input: %w", err)
			}

		default:
			if b < 0x20 {
				continue
			}

			password = append(password, b)
			if _, err := fmt.Fprint(output, "*"); err != nil {
				return nil, fmt.Errorf("unable to mask "+
					"password input: %w", err)
			}
		}
	}
}
