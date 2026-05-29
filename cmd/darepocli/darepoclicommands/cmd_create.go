package darepoclicommands

import (
	"fmt"
	"os"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

// newCreateCmd builds the top-level `create` verb. It is one of the
// seven core wallet verbs (create, unlock, send, recv, list, balance,
// exit) and dials walletdkrpc.WalletService.Create which proxies
// daemonrpc.GenSeed + daemonrpc.InitWallet under the hood. Both the
// wallet password and the optional seed passphrase are read from
// stdin / env var / file (never CLI args) so secrets never enter argv.
func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new wallet from a fresh seed",
		Long: "Initializes a new wallet. The daemon generates a " +
			"24-word aezeed mnemonic, prints it to stderr (so " +
			"the caller can record it offline), and encrypts " +
			"the seed with the supplied password.\n\n" +
			"The wallet password is read from stdin / " +
			"DAREPOD_WALLET_PASSWORD env / " +
			"--wallet_password_file (never CLI args). The " +
			"interactive password prompt asks for confirmation. " +
			"The optional seed passphrase is read from " +
			"DAREPOD_SEED_PASSPHRASE env / " +
			"--seed_passphrase_file. The mnemonic is shown " +
			"on stderr ONCE and is NOT included in the JSON " +
			"response on stdout (the caller must capture it " +
			"from stderr or pass --print-mnemonic-json to " +
			"opt in to the machine-consumable form).\n\n" +
			"Example:\n" +
			"  echo -n 'hunter2hunter2' | darepocli create",
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

	return cmd
}

// walletCreate implements the top-level `create` verb.
func walletCreate(cmd *cobra.Command, _ []string) error {
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

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.Create(
				cmd.Context(), &walletdkrpc.CreateRequest{
					WalletPassword: password,
					SeedPassphrase: seedPassphrase,
				},
			)
			if err != nil {
				return fmt.Errorf("create wallet: %w", err)
			}

			// Print the mnemonic to stderr so it does not mix
			// with structured JSON output on stdout. By default
			// the mnemonic is REDACTED from the JSON response
			// on stdout so a log aggregator or shell redirect
			// cannot persist the master secret; callers that
			// explicitly need the machine-readable form pass
			// --print-mnemonic-json.
			fmt.Fprintln(os.Stderr, "=== WRITE DOWN YOUR SEED ===")
			for i, word := range resp.GetMnemonic() {
				fmt.Fprintf(os.Stderr, "%2d. %s\n", i+1, word)
			}
			fmt.Fprintln(os.Stderr, "============================")
			fmt.Fprintln(os.Stderr)

			if !printJSON {
				resp.Mnemonic = nil
			}

			return printWalletProto(resp)
		},
	)
}
