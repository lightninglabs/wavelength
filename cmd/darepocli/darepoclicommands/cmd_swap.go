package darepoclicommands

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	sdkark "github.com/lightninglabs/darepo-client/sdk/ark"
	"github.com/lightninglabs/darepo-client/sdk/swaps"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
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

	cmd.AddCommand(
		newSwapReceiveCmd(),
		newSwapPayCmd(),
	)

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

// buildSwapClient creates a SwapClient from command flags. It connects to the
// swap server and wraps the existing darepod daemon client with the Ark SDK
// facade so swap flows reuse the caller's daemon connection. The returned
// cleanup function closes only the swap-server helper client.
func buildSwapClient(cmd *cobra.Command,
	daemonClient daemonrpc.DaemonServiceClient,
	_ *grpc.ClientConn) (*swaps.SwapClient,
	func(), error) {

	swapAddr, _ := cmd.Flags().GetString("swapserver")
	swapDialOpts, err := swapServerDialOptions(cmd, swapAddr)
	if err != nil {
		return nil, nil, err
	}

	// Connect to the swap server.
	swapConn, err := grpc.NewClient(swapAddr, swapDialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"connect to swap server: %w", err,
		)
	}

	arkClient := sdkark.WrapDaemonClient(daemonClient, nil)
	closeSwapConn := func(baseErr error) error {
		return errors.Join(baseErr, swapConn.Close())
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

	client := swaps.NewSwapClient(
		serverConn, arkClient, nil,
		clientInvoiceGenerator(invoiceKey, daemonClient, chainParams),
	)

	cleanup := func() {
		_ = serverConn.Close()
		_ = swapConn.Close()
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
