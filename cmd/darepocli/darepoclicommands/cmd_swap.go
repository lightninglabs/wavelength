package darepoclicommands

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	sdkark "github.com/lightninglabs/darepo-client/sdk/ark"
	"github.com/lightninglabs/darepo-client/sdk/swaps"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// defaultSwapDBPath is the default isolated SQLite database used by
	// darepocli swap commands.
	defaultSwapDBPath = "~/.darepod/swaps.db"
)

// newSwapCmd creates the swap parent command.
func newSwapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "swap",
		Short: "Lightning swap operations",
		Long: "Swap between Lightning and Ark. Use 'receive' to " +
			"get a Lightning invoice that deposits into Ark, " +
			"or 'pay' to pay a Lightning invoice from Ark " +
			"funds.",
	}

	cmd.PersistentFlags().String("swapserver", "localhost:10030",
		"swap server gRPC address")
	cmd.PersistentFlags().String("swapserver-tlscert", "",
		"swap server TLS certificate path")
	cmd.PersistentFlags().Bool("swapserver-insecure", false,
		"disable TLS when connecting to a remote swap server")
	cmd.PersistentFlags().String("swapdb", defaultSwapDBPath,
		"isolated swap session SQLite database path")

	cmd.AddCommand(
		newSwapListCmd(),
		newSwapReceiveCmd(),
		newSwapPayCmd(),
		newSwapResumeCmd(),
	)

	return cmd
}

// newSwapListCmd creates the swap list subcommand.
func newSwapListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List persisted Lightning swap sessions",
		Long: "Lists persisted swap sessions from the isolated " +
			"swap database. Use --pending to show only " +
			"non-terminal sessions that can still be resumed.",
		RunE: swapList,
	}

	cmd.Flags().Bool("pending", false,
		"show only non-terminal resumable swaps")
	cmd.Flags().Bool("verbose", false,
		"include terminal reason and OOR session identifiers")

	return cmd
}

// newSwapReceiveCmd creates the swap receive subcommand.
func newSwapReceiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "receive",
		Short: "Receive BTC via Lightning into Ark",
		Long: "Creates a Lightning invoice that, when paid, " +
			"deposits the funds as an Ark VTXO. Blocks " +
			"until the payment is received and the VTXO " +
			"is claimed.",
		RunE: swapReceive,
	}

	cmd.Flags().Int64("amount", 0,
		"amount in satoshis to receive (required)")
	_ = cmd.MarkFlagRequired("amount")
	cmd.Flags().Bool("verbose", false,
		"print intermediate vHTLC funding and sweep details")

	return cmd
}

// newSwapPayCmd creates the swap pay subcommand.
func newSwapPayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pay",
		Short: "Pay a Lightning invoice from Ark funds",
		Long: "Pays a Lightning invoice by funding a vHTLC " +
			"that the swap server claims after paying the " +
			"invoice. Blocks until the payment completes.",
		RunE: swapPay,
	}

	cmd.Flags().String("invoice", "",
		"BOLT-11 Lightning invoice to pay (required)")
	_ = cmd.MarkFlagRequired("invoice")

	cmd.Flags().Uint64("maxfee", 0,
		"maximum fee in satoshis (0 = no limit)")

	return cmd
}

// newSwapResumeCmd creates the swap resume subcommand.
func newSwapResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume [payment_hash]",
		Short: "Resume a persisted Lightning swap session",
		Long: "Resumes one persisted pay or receive session by " +
			"payment hash. When --direction is omitted, the " +
			"command looks up the pending swap in the isolated " +
			"swap database and resumes the matching direction. " +
			"The command continues the same blocking lifecycle " +
			"used by swap pay and swap receive.",
		Args: cobra.MaximumNArgs(1),
		RunE: swapResume,
	}

	cmd.Flags().String("direction", "",
		"swap direction to resume: pay or receive")
	cmd.Flags().String("payment_hash", "",
		"deprecated; use the positional payment_hash argument")

	return cmd
}

// swapList executes the swap list command.
func swapList(cmd *cobra.Command, _ []string) error {
	store, cleanup, err := openSwapStoreFromFlags(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	client := swaps.NewSwapClientWithStore(nil, nil, nil, nil, store)

	pendingOnly, _ := cmd.Flags().GetBool("pending")
	verbose, _ := cmd.Flags().GetBool("verbose")

	summaries, err := client.ListSwapSummaries(
		context.Background(), pendingOnly,
	)
	if err != nil {
		return err
	}

	printSwapSummaries(cmd, summaries, verbose)

	return nil
}

// swapReceive executes the swap receive command.
func swapReceive(cmd *cobra.Command, _ []string) error {
	daemonClient, daemonConn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer daemonConn.Close()

	swapClient, cleanup, err := buildSwapClient(
		cmd, daemonClient, daemonConn,
	)
	if err != nil {
		return err
	}
	defer cleanup()

	amount, _ := cmd.Flags().GetInt64("amount")
	verbose, _ := cmd.Flags().GetBool("verbose")

	ctx := context.Background()

	session, err := swapClient.StartReceiveViaLightning(
		ctx, btcutil.Amount(amount),
	)
	if err != nil {
		return fmt.Errorf("receive failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"Invoice: %s\n"+
			"Payment hash: %s\n"+
			"Preimage: %x\n",
		session.Invoice,
		hex.EncodeToString(session.PaymentHash[:]),
		session.Preimage[:],
	)

	if verbose {
		if err := printReceiveVerboseStart(
			cmd, amount, session,
		); err != nil {
			return err
		}
	}

	outpoint, fundedAmount, err := session.WaitForFunding(ctx)
	if err != nil {
		return fmt.Errorf("receive wait failed: %w", err)
	}

	if verbose {
		printReceiveVerboseFunding(
			cmd, outpoint, fundedAmount, session,
		)
	}

	result, err := session.Claim(ctx, outpoint, fundedAmount)
	if err != nil {
		return fmt.Errorf("receive claim failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"VTXO outpoint: %s\n"+
			"Amount: %d sat\n",
		result.VTXOOutpoint,
		result.AmountSat,
	)

	return nil
}

// printReceiveVerboseStart prints the expected incoming vHTLC scripts for one
// receive flow before funding is observed.
func printReceiveVerboseStart(cmd *cobra.Command, amount int64,
	session *swaps.ReceiveSession) error {

	info, err := session.VHTLCInfo()
	if err != nil {
		return fmt.Errorf("describe receive vhtlc: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"\nIncoming vHTLC\n"+
			"  amount:        %d sat\n"+
			"  output script: %x\n"+
			"  claim script:  %x\n"+
			"  waiting for:   funding via paid invoice\n",
		amount,
		info.PkScript,
		info.ClaimScript,
	)

	return nil
}

// printReceiveVerboseFunding prints the funded vHTLC acceptance and the
// upcoming preimage sweep step.
func printReceiveVerboseFunding(cmd *cobra.Command, outpoint string,
	amount int64, session *swaps.ReceiveSession) {

	fmt.Fprintf(cmd.OutOrStdout(),
		"\nAccepted incoming vHTLC\n"+
			"  outpoint: %s\n"+
			"  amount:   %d sat\n"+
			"\nSweeping vHTLC\n"+
			"  preimage: %x\n",
		outpoint,
		amount,
		session.Preimage[:],
	)
}

// swapPay executes the swap pay command.
func swapPay(cmd *cobra.Command, _ []string) error {
	daemonClient, daemonConn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer daemonConn.Close()

	swapClient, cleanup, err := buildSwapClient(
		cmd, daemonClient, daemonConn,
	)
	if err != nil {
		return err
	}
	defer cleanup()

	invoice, _ := cmd.Flags().GetString("invoice")
	maxFee, _ := cmd.Flags().GetUint64("maxfee")

	ctx := context.Background()

	result, err := swapClient.PayViaLightning(
		ctx, invoice, maxFee,
	)
	if err != nil {
		return fmt.Errorf("payment failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"Payment hash: %s\n"+
			"Preimage: %x\n"+
			"Fee: %d sat\n",
		hex.EncodeToString(result.PaymentHash[:]),
		result.Preimage[:],
		result.FeeSat,
	)

	return nil
}

// swapResume executes the swap resume command.
func swapResume(cmd *cobra.Command, args []string) error {
	daemonClient, daemonConn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer daemonConn.Close()

	swapClient, cleanup, err := buildSwapClient(
		cmd, daemonClient, daemonConn,
	)
	if err != nil {
		return err
	}
	defer cleanup()

	hash, err := paymentHashFromArgsOrFlag(cmd, args)
	if err != nil {
		return err
	}

	ctx := context.Background()
	direction, err := resumeDirection(ctx, swapClient, hash, cmd)
	if err != nil {
		return err
	}

	switch direction {
	case swaps.SwapDirectionPay:
		session, err := swapClient.ResumePayViaLightning(ctx, hash)
		if err != nil {
			return err
		}

		result, err := session.Wait(ctx)
		if err != nil {
			return fmt.Errorf("pay resume failed: %w", err)
		}

		fmt.Fprintf(cmd.OutOrStdout(),
			"Payment hash: %s\n"+
				"Preimage: %x\n"+
				"Fee: %d sat\n",
			hex.EncodeToString(result.PaymentHash[:]),
			result.Preimage[:],
			result.FeeSat,
		)

	case swaps.SwapDirectionReceive:
		session, err := swapClient.ResumeReceiveViaLightning(ctx, hash)
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(),
			"Invoice: %s\n"+
				"Payment hash: %s\n",
			session.Invoice,
			hex.EncodeToString(session.PaymentHash[:]),
		)

		result, err := session.Wait(ctx)
		if err != nil {
			return fmt.Errorf("receive resume failed: %w", err)
		}

		fmt.Fprintf(cmd.OutOrStdout(),
			"VTXO outpoint: %s\n"+
				"Amount: %d sat\n",
			result.VTXOOutpoint,
			result.AmountSat,
		)
	}

	return nil
}

// resumeDirection returns the swap direction requested by --direction or
// infers it from pending persisted sessions when the flag is omitted.
func resumeDirection(ctx context.Context, swapClient *swaps.SwapClient,
	hash lntypes.Hash, cmd *cobra.Command) (swaps.SwapDirection, error) {

	requested, _ := cmd.Flags().GetString("direction")
	if requested != "" {
		return parseSwapDirection(requested)
	}

	summaries, err := swapClient.ListSwapSummaries(ctx, true)
	if err != nil {
		return "", fmt.Errorf("list pending swaps: %w", err)
	}

	return inferResumeDirection(hash, summaries)
}

// parseSwapDirection validates one user-provided swap direction.
func parseSwapDirection(direction string) (swaps.SwapDirection, error) {
	switch strings.ToLower(direction) {
	case string(swaps.SwapDirectionPay):
		return swaps.SwapDirectionPay, nil

	case string(swaps.SwapDirectionReceive):
		return swaps.SwapDirectionReceive, nil

	default:
		return "", fmt.Errorf("unknown swap direction %q", direction)
	}
}

// inferResumeDirection finds the unique pending swap direction for a payment
// hash in the persisted swap summaries.
func inferResumeDirection(hash lntypes.Hash,
	summaries []swaps.SwapSummary) (swaps.SwapDirection, error) {

	var (
		found bool
		dir   swaps.SwapDirection
	)
	for _, summary := range summaries {
		if !summary.Pending || summary.PaymentHash != hash {
			continue
		}
		if found && summary.Direction != dir {
			return "", fmt.Errorf(
				"payment hash %s matches multiple pending "+
					"swap directions; pass --direction",
				hex.EncodeToString(hash[:]),
			)
		}

		found = true
		dir = summary.Direction
	}

	if !found {
		return "", fmt.Errorf(
			"no pending swap found for payment hash %s",
			hex.EncodeToString(hash[:]),
		)
	}

	return dir, nil
}

// buildSwapClient creates a SwapClient from command flags. It connects to the
// swap server and wraps the existing darepod daemon client with the Ark SDK
// facade so swap flows reuse the caller's daemon connection. The returned
// cleanup function closes the swap-server helper client and swap store.
func buildSwapClient(cmd *cobra.Command,
	daemonClient daemonrpc.DaemonServiceClient,
	_ *grpc.ClientConn) (*swaps.SwapClient,
	func(), error) {

	store, closeStore, err := openSwapStoreFromFlags(cmd)
	if err != nil {
		return nil, nil, err
	}

	swapAddr, _ := cmd.Flags().GetString("swapserver")
	swapDialOpts, err := swapServerDialOptions(cmd, swapAddr)
	if err != nil {
		closeStore()

		return nil, nil, err
	}

	// Connect to the swap server.
	swapConn, err := grpc.NewClient(swapAddr, swapDialOpts...)
	if err != nil {
		closeStore()

		return nil, nil, fmt.Errorf(
			"connect to swap server: %w", err,
		)
	}

	arkClient := sdkark.WrapDaemonClient(daemonClient, nil)
	closeSwapConn := func(baseErr error) error {
		return errors.Join(baseErr, swapConn.Close(), store.Close())
	}

	info, err := daemonClient.GetInfo(
		context.Background(), &daemonrpc.GetInfoRequest{},
	)
	if err != nil {
		return nil, nil, closeSwapConn(
			fmt.Errorf("get daemon info: %w", err),
		)
	}

	chainParams, err := chainParamsForNetwork(info.GetNetwork())
	if err != nil {
		return nil, nil, closeSwapConn(err)
	}

	// The CLI invoice signer is intentionally process-local. Resumed
	// receive sessions load their already-created invoice from the swap DB,
	// so this key must not be used to recreate historical invoices.
	invoiceKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, nil, closeSwapConn(
			fmt.Errorf("create invoice key: %w", err),
		)
	}

	serverConn := swaps.NewGRPCSwapServerConn(swapConn)

	client := swaps.NewSwapClientWithStore(
		serverConn, arkClient, nil,
		clientInvoiceGenerator(invoiceKey, daemonClient, chainParams),
		store,
	)

	cleanup := func() {
		_ = serverConn.Close()
		_ = swapConn.Close()
		closeStore()
	}

	return client, cleanup, nil
}

func swapServerDialOptions(cmd *cobra.Command,
	addr string) ([]grpc.DialOption, error) {

	tlsCertPath, _ := cmd.Flags().GetString("swapserver-tlscert")
	useInsecure, _ := cmd.Flags().GetBool("swapserver-insecure")

	switch {
	case tlsCertPath != "":
		creds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
		if err != nil {
			return nil, fmt.Errorf(
				"load swap server TLS certificate: %w", err,
			)
		}

		return []grpc.DialOption{
			grpc.WithTransportCredentials(creds),
		}, nil

	case isLocalSwapServerAddr(addr) || useInsecure:
		return []grpc.DialOption{
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		}, nil

	default:
		return []grpc.DialOption{
			grpc.WithTransportCredentials(credentials.NewTLS(
				&tls.Config{MinVersion: tls.VersionTLS12},
			)),
		}, nil
	}
}

func isLocalSwapServerAddr(addr string) bool {
	if strings.HasPrefix(addr, "unix:") {
		return true
	}

	host := addr
	splitHost, _, err := net.SplitHostPort(addr)
	if err == nil {
		host = splitHost
	}

	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// clientInvoiceGenerator builds the invoice creator used by swap receive and
// resume paths.
func clientInvoiceGenerator(invoiceKey *btcec.PrivateKey,
	daemonClient daemonrpc.DaemonServiceClient,
	chainParams *chaincfg.Params) swaps.InvoiceCreator {

	bestHeight := func() (uint32, error) {
		resp, err := daemonClient.GetInfo(
			context.Background(),
			&daemonrpc.GetInfoRequest{},
		)
		if err != nil {
			return 0, err
		}

		return resp.GetBlockHeight(), nil
	}

	return swaps.NewInvoiceGenerator(
		keychain.NewPrivKeyMessageSigner(
			invoiceKey, keychain.KeyLocator{},
		),
		bestHeight,
		swaps.NewMemoryInvoiceStore(),
		chainParams,
	)
}

// openSwapStoreFromFlags opens the isolated swap store configured by CLI flags.
func openSwapStoreFromFlags(cmd *cobra.Command) (*swaps.Store, func(),
	error) {

	swapDB, _ := cmd.Flags().GetString("swapdb")
	expandedPath, err := expandCLIPath(swapDB)
	if err != nil {
		return nil, nil, err
	}

	if err := os.MkdirAll(filepath.Dir(expandedPath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create swap db dir: %w", err)
	}

	store, err := swaps.NewSqliteStore(&swaps.SqliteStoreConfig{
		DatabaseFileName: expandedPath,
	}, btclog.Disabled)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		_ = store.Close()
	}

	return store, cleanup, nil
}

// expandCLIPath expands a leading tilde in one CLI path.
func expandCLIPath(path string) (string, error) {
	if path == "" {
		path = defaultSwapDBPath
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}

		if path == "~" {
			return home, nil
		}

		return filepath.Join(home, path[2:]), nil
	}

	return filepath.Clean(path), nil
}

// paymentHashFromArgsOrFlag parses the resume payment hash from the positional
// argument or the legacy --payment_hash flag.
func paymentHashFromArgsOrFlag(cmd *cobra.Command,
	args []string) (lntypes.Hash, error) {

	encoded, _ := cmd.Flags().GetString("payment_hash")
	if len(args) > 0 {
		if encoded != "" {
			return lntypes.Hash{}, fmt.Errorf(
				"payment hash set as argument and flag",
			)
		}

		encoded = args[0]
	}
	if encoded == "" {
		return lntypes.Hash{}, fmt.Errorf("payment hash is required")
	}

	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return lntypes.Hash{}, fmt.Errorf(
			"decode payment hash: %w", err,
		)
	}
	if len(raw) != lntypes.HashSize {
		return lntypes.Hash{}, fmt.Errorf(
			"payment hash must be %d bytes", lntypes.HashSize,
		)
	}

	var hash lntypes.Hash
	copy(hash[:], raw)

	return hash, nil
}

// printSwapSummaries renders swap summaries in a stable table form.
func printSwapSummaries(cmd *cobra.Command, summaries []swaps.SwapSummary,
	verbose bool) {

	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if verbose {
		fmt.Fprintln(writer,
			"DIRECTION\tPAYMENT_HASH\tSTATE\tPENDING\tAMOUNT_SAT\t"+
				"FEE_SAT\tMAX_FEE_SAT\tVHTLC_OUTPOINT\t"+
				"REFUND_LOCKTIME\tUPDATED_AT\tDETAILS")
	} else {
		fmt.Fprintln(writer,
			"DIRECTION\tPAYMENT_HASH\tSTATE\tPENDING\tAMOUNT_SAT\t"+
				"FEE_SAT\tMAX_FEE_SAT\tVHTLC_OUTPOINT\t"+
				"UPDATED_AT")
	}

	for _, summary := range summaries {
		feeSat := "-"
		if summary.Direction == swaps.SwapDirectionPay {
			feeSat = fmt.Sprintf("%d", summary.FeeSat)
		}

		maxFeeSat := "-"
		if summary.Direction == swaps.SwapDirectionPay {
			maxFeeSat = fmt.Sprintf("%d", summary.MaxFeeSat)
		}

		paymentHash := hex.EncodeToString(summary.PaymentHash[:])
		if verbose {
			fmt.Fprintf(writer,
				"%s\t%s\t%s\t%t\t%d\t%s\t%s\t%s\t%d\t%s\t%s\n",
				summary.Direction,
				paymentHash,
				summary.State,
				summary.Pending,
				summary.AmountSat,
				feeSat,
				maxFeeSat,
				summary.VHTLCOutpoint,
				summary.RefundLocktime,
				summary.UpdatedAt.Format(
					"2006-01-02T15:04:05Z07:00",
				),
				swapSummaryDetails(summary),
			)

			continue
		}

		fmt.Fprintf(writer,
			"%s\t%s\t%s\t%t\t%d\t%s\t%s\t%s\t%s\n",
			summary.Direction,
			paymentHash,
			summary.State,
			summary.Pending,
			summary.AmountSat,
			feeSat,
			maxFeeSat,
			summary.VHTLCOutpoint,
			summary.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		)
	}

	_ = writer.Flush()
}

// swapSummaryDetails returns optional verbose details for one swap row.
func swapSummaryDetails(summary swaps.SwapSummary) string {
	details := make([]string, 0, 4)
	if summary.FundingSessionID != "" {
		details = append(
			details, "funding="+summary.FundingSessionID,
		)
	}
	if summary.ClaimSessionID != "" {
		details = append(details, "claim="+summary.ClaimSessionID)
	}
	if summary.RefundSessionID != "" {
		details = append(details, "refund="+summary.RefundSessionID)
	}
	if summary.TerminalReason != "" {
		details = append(details, "reason="+summary.TerminalReason)
	}

	return strings.Join(details, ",")
}

// chainParamsForNetwork maps the daemon-reported network name to btcutil chain
// parameters for invoice encoding.
func chainParamsForNetwork(network string) (*chaincfg.Params, error) {
	switch network {
	case "bitcoin", "mainnet":
		return &chaincfg.MainNetParams, nil

	case "testnet", "testnet3":
		return &chaincfg.TestNet3Params, nil

	case "regtest":
		return &chaincfg.RegressionNetParams, nil

	case "signet":
		return &chaincfg.SigNetParams, nil

	case "simnet":
		return &chaincfg.SimNetParams, nil

	default:
		return nil, fmt.Errorf("unsupported daemon network: %s",
			network)
	}
}
