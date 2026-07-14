package waveclicommands

import (
	"fmt"
	"os"
	"strings"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

// newCreateCmd builds the top-level `create` verb. It is one of the
// seven core wallet verbs (create, unlock, send, recv, activity, balance,
// exit) and dials walletdkrpc.WalletService.Create which proxies
// waverpc.GenSeed + waverpc.InitWallet under the hood. Both the
// wallet password and the optional seed passphrase are read from
// stdin / env var / file (never CLI args) so secrets never enter argv.
func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new wallet from a fresh seed",
		Long: "Initializes a new wallet. The daemon generates a " +
			"24-word aezeed mnemonic, prints it to stderr (so " +
			"the caller can record it offline), and creates " +
			"the wallet database encrypted under the " +
			"supplied password.\n\n" +
			"The wallet password is read from stdin / " +
			"WAVED_WALLET_PASSWORD env / " +
			"--wallet_password_file (never CLI args). The " +
			"interactive password prompt asks for confirmation. " +
			"The optional seed passphrase is read from " +
			"WAVED_SEED_PASSPHRASE env / " +
			"--seed_passphrase_file. The mnemonic is shown " +
			"on stderr ONCE and is NOT included in the JSON " +
			"response on stdout (the caller must capture it " +
			"from stderr or pass --print-mnemonic-json to " +
			"opt in to the machine-consumable form).\n\n" +
			"Example:\n" +
			"  echo -n 'hunter2hunter2' | wavecli create",
		Args: cobra.NoArgs,
		RunE: walletCreate,
	}

	cmd.Flags().String("wallet_password_file", "",
		"path to file containing wallet password")
	cmd.Flags().String("seed_passphrase_file", "",
		"path to file containing optional aezeed passphrase")
	cmd.Flags().Bool("print-mnemonic-json", false,
		"include the mnemonic in the JSON response on stdout "+
			"(default: stderr only)")
	cmd.Flags().Bool("recover", false,
		"import an existing mnemonic and recover Ark wallet state")
	cmd.Flags().String("mnemonic-file", "",
		"path to file containing an existing 24-word aezeed mnemonic")
	cmd.Flags().String("mnemonic_file", "",
		"path to file containing an existing 24-word aezeed mnemonic")
	cmd.Flags().Uint32("recovery-window", 0,
		"number of key indexes to scan per recovery family "+
			"(default: daemon wallet.recoverywindow)")
	cmd.Flags().Uint32("recovery_window", 0,
		"number of key indexes to scan per recovery family "+
			"(default: daemon wallet.recoverywindow)")
	_ = cmd.Flags().MarkHidden("mnemonic_file")
	_ = cmd.Flags().MarkHidden("recovery_window")

	return cmd
}

// walletCreate implements the top-level `create` verb.
func walletCreate(cmd *cobra.Command, _ []string) error {
	recoverWallet, _ := cmd.Flags().GetBool("recover")
	mnemonicFile, err := aliasedFlag(
		cmd, "mnemonic-file", "mnemonic_file", cmd.Flags().GetString,
	)
	if err != nil {
		return err
	}

	recoveryWindow, err := aliasedFlag(
		cmd, "recovery-window", "recovery_window",
		cmd.Flags().GetUint32,
	)
	if err != nil {
		return err
	}

	switch {
	case recoverWallet && mnemonicFile == "":
		return fmt.Errorf("--recover requires --mnemonic-file")

	case !recoverWallet && mnemonicFile != "":
		return fmt.Errorf("--mnemonic-file requires --recover")

	case !recoverWallet && recoveryWindow != 0:
		return fmt.Errorf("--recovery-window requires --recover")
	}

	seedPassphrase, err := readSeedPassphrase(cmd)
	if err != nil {
		return err
	}
	defer zeroBytes(seedPassphrase)

	password, err := readPasswordConfirmed(cmd)
	if err != nil {
		return err
	}
	defer zeroBytes(password)

	printJSON, _ := cmd.Flags().GetBool("print-mnemonic-json")

	var mnemonic []string
	if recoverWallet {
		mnemonic, err = readMnemonicFile(mnemonicFile)
		if err != nil {
			return err
		}
	}

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.Create(
				cmd.Context(), &walletdkrpc.CreateRequest{
					WalletPassword: password,
					SeedPassphrase: seedPassphrase,
					Mnemonic:       mnemonic,
					RecoverState:   recoverWallet,
					RecoveryWindow: recoveryWindow,
				},
			)
			if err != nil {
				return fmt.Errorf("create wallet: %w", err)
			}

			// Fresh-create prints the generated mnemonic to stderr
			// so it does not mix with structured JSON output. A
			// recovery import already read the mnemonic from disk,
			// so do not re-print it by default.
			if !recoverWallet {
				fmt.Fprintln(
					os.Stderr,
					"=== WRITE DOWN YOUR SEED ===",
				)
				for i, word := range resp.GetMnemonic() {
					fmt.Fprintf(
						os.Stderr, "%2d. %s\n", i+1,
						word,
					)
				}
				fmt.Fprintln(
					os.Stderr,
					"============================",
				)
				fmt.Fprintln(os.Stderr)
			}

			if !printJSON {
				resp.Mnemonic = nil
			}

			return printWalletProto(resp)
		},
	)
}

// readMnemonicFile reads mnemonic words from path. The file may contain words
// separated by any whitespace, including one word per line.
func readMnemonicFile(path string) ([]string, error) {
	// The file path is explicitly provided by the CLI user.
	data, err := os.ReadFile(path) //nolint:gosec // G304
	if err != nil {
		return nil, fmt.Errorf("unable to read mnemonic file: %w", err)
	}

	words := strings.Fields(string(data))
	if len(words) == 0 {
		return nil, fmt.Errorf("mnemonic file is empty")
	}

	return words, nil
}

// aliasedFlag reads a flag with a hidden compatibility alias.
func aliasedFlag[T any](cmd *cobra.Command, primary, alias string,
	get func(string) (T, error)) (T, error) {

	primaryChanged := cmd.Flags().Changed(primary)
	aliasChanged := cmd.Flags().Changed(alias)
	var zero T
	switch {
	case primaryChanged && aliasChanged:
		return zero, fmt.Errorf("--%s and --%s cannot both be set",
			primary, alias)

	case aliasChanged:
		return get(alias)

	default:
		return get(primary)
	}
}
