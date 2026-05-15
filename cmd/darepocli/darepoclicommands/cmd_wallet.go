package darepoclicommands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// newWalletCmd creates the wallet parent command with subcommands for
// create, unlock, balance, and newaddress.
func newWalletCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wallet",
		Short: "Wallet operations",
		Long: "Manage the daemon wallet: create, " +
			"unlock, check balance, " +
			"generate addresses.",
	}

	cmd.AddCommand(
		newWalletCreateCmd(), newWalletUnlockCmd(),
		newWalletBalanceCmd(), newWalletNewAddressCmd(),
	)

	// The walletrpc build adds the simplified high-level wallet
	// subcommands (send/recv/list/deposit/status) under the same parent
	// so users get one wallet vocabulary. The hook is a no-op in builds
	// without the walletrpc tag.
	addWalletRPCSubcommands(cmd)

	return cmd
}

// newWalletCreateCmd creates the wallet create subcommand.
func newWalletCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new wallet from a fresh seed",
		Long: "Generates a new aezeed mnemonic, then " +
			"creates and encrypts the wallet. The " +
			"wallet password is read from stdin, " +
			"DAREPOD_WALLET_PASSWORD env var, or " +
			"--wallet_password_file flag (never " +
			"from CLI args).",
		RunE: walletCreate,
	}

	cmd.Flags().String("wallet_password_file", "",
		"path to file containing wallet password")

	cmd.Flags().String("seed_passphrase", "",
		"optional aezeed passphrase (empty for none)")

	return cmd
}

// walletCreate implements the wallet create flow: GenSeed + InitWallet.
func walletCreate(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx := context.Background()

	// When --json is provided, the agent supplies the full
	// InitWalletRequest directly (mnemonic + password), skipping
	// the interactive GenSeed flow.
	initReq := &daemonrpc.InitWalletRequest{}
	if err := parseRequest(cmd, initReq, nil); err != nil {
		return err
	}

	if len(initReq.Mnemonic) > 0 {
		resp, err := client.InitWallet(ctx, initReq)
		if err != nil {
			return fmt.Errorf("InitWallet RPC failed: %w", err)
		}

		return printJSON(resp)
	}

	// Bespoke-flag path: GenSeed first, then InitWallet.
	seedPassphrase, _ := cmd.Flags().GetString(
		"seed_passphrase",
	)

	genResp, err := client.GenSeed(
		ctx, &daemonrpc.GenSeedRequest{
			SeedPassphrase: []byte(seedPassphrase),
		},
	)
	if err != nil {
		return fmt.Errorf("GenSeed RPC failed: %w", err)
	}

	// Display the mnemonic to stderr so it doesn't mix with
	// structured JSON output on stdout.
	fmt.Fprintln(os.Stderr, "=== WRITE DOWN YOUR SEED ===")
	for i, word := range genResp.Mnemonic {
		fmt.Fprintf(os.Stderr, "%2d. %s\n", i+1, word)
	}
	fmt.Fprintln(os.Stderr, "============================")
	fmt.Fprintln(os.Stderr)

	password, err := readPassword(cmd)
	if err != nil {
		return err
	}

	initResp, err := client.InitWallet(
		ctx, &daemonrpc.InitWalletRequest{
			Mnemonic:       genResp.Mnemonic,
			WalletPassword: password,
			SeedPassphrase: []byte(seedPassphrase),
		},
	)
	if err != nil {
		return fmt.Errorf("InitWallet RPC failed: %w", err)
	}

	return printJSON(initResp)
}

// newWalletUnlockCmd creates the wallet unlock subcommand.
func newWalletUnlockCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unlock",
		Short: "Unlock an existing wallet",
		Long: "Decrypts the wallet seed and starts the " +
			"wallet subsystem. Password is read from " +
			"stdin, DAREPOD_WALLET_PASSWORD env var, " +
			"or --wallet_password_file flag.",
		RunE: walletUnlock,
	}

	cmd.Flags().String("wallet_password_file", "",
		"path to file containing wallet password")

	return cmd
}

// walletUnlock implements the wallet unlock flow.
func walletUnlock(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.UnlockWalletRequest{}
	if err := parseRequest(cmd, req, func() error {
		password, err := readPassword(cmd)
		if err != nil {
			return err
		}

		req.WalletPassword = password

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.UnlockWallet(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("UnlockWallet RPC failed: %w", err)
	}

	return printJSON(resp)
}

// newWalletBalanceCmd creates the wallet balance subcommand.
func newWalletBalanceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "balance",
		Short: "Display wallet balance",
		Long: "Shows boarding (on-chain), VTXO (off-chain), " +
			"and total balance in satoshis.",
		RunE: walletBalance,
	}
}

// walletBalance executes the GetBalance RPC and prints the result.
func walletBalance(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.GetBalanceRequest{}
	if err := parseRequest(cmd, req, nil); err != nil {
		return err
	}

	resp, err := client.GetBalance(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("GetBalance RPC failed: %w", err)
	}

	return printJSON(resp)
}

// newWalletNewAddressCmd creates the wallet newaddress subcommand.
func newWalletNewAddressCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "newaddress",
		Short: "Generate a new boarding address",
		Long: "Generates a new taproot boarding address " +
			"that can receive on-chain funds for " +
			"use in the Ark protocol.",
		RunE: walletNewAddress,
	}
}

// walletNewAddress executes the NewAddress RPC and prints the result.
func walletNewAddress(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.NewAddressRequest{}
	if err := parseRequest(cmd, req, nil); err != nil {
		return err
	}

	resp, err := client.NewAddress(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("NewAddress RPC failed: %w", err)
	}

	return printJSON(resp)
}

// readPassword reads the wallet password from one of these sources,
// in priority order: DAREPOD_WALLET_PASSWORD env >
// --wallet_password_file flag > stdin pipe > interactive prompt.
// The env var is checked first so that automated environments
// (such as the darepotest REPL) can set it without stdin races.
func readPassword(cmd *cobra.Command) ([]byte, error) {
	// Check environment variable first — takes priority so
	// that callers with piped stdin (e.g. REPL, CI) can
	// override without fighting over stdin.
	if envPass := os.Getenv(
		"DAREPOD_WALLET_PASSWORD"); envPass != "" {
		return []byte(envPass), nil
	}

	// Check --wallet_password_file flag.
	passFile, _ := cmd.Flags().GetString("wallet_password_file")
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

	// Check if stdin has data (piped input).
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			return []byte(scanner.Text()), nil
		}

		return nil, fmt.Errorf("unable to read password from stdin")
	}

	// Interactive prompt (TTY).
	fmt.Fprint(os.Stderr, "Enter wallet password: ")

	fd := int(os.Stdin.Fd())
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

	password, err := readMaskedPassword(os.Stdin, os.Stderr)
	restoreTerminal()
	_, printErr := fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, err
	}
	if printErr != nil {
		return nil, fmt.Errorf("unable to finalize password prompt: %w",
			printErr)
	}

	return password, nil
}

// readMaskedPassword reads a single password line while echoing one asterisk
// per entered byte. Backspace removes the last UTF-8 rune worth of bytes. It
// expects the terminal to already be in raw mode.
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
