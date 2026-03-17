package darepoclicommands

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
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
	cmd.PersistentFlags().String("arkserver", "localhost:7070",
		"ark operator gRPC address used for terms and indexer queries")
	cmd.PersistentFlags().Bool("ark-no-tls", false,
		"disable TLS for the ark server connection")
	cmd.PersistentFlags().String("ark-tlscertpath", "",
		"path to ark server TLS certificate")

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

	result, err := session.Wait(ctx)
	if err != nil {
		return fmt.Errorf("receive wait failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"VTXO outpoint: %s\n"+
			"Amount: %d sat\n",
		result.VTXOOutpoint,
		result.AmountSat,
	)

	return nil
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
			"Fee: %d sat\n",
		hex.EncodeToString(result.PaymentHash[:]),
		result.FeeSat,
	)

	return nil
}

// buildSwapClient creates a SwapClient from command flags. It
// connects to the swap server and wraps the daemon client. The
// returned cleanup function closes the swap server connection.
func buildSwapClient(cmd *cobra.Command,
	daemonClient daemonrpc.DaemonServiceClient,
	_ *grpc.ClientConn) (*swaps.SwapClient,
	func(), error) {

	swapAddr, _ := cmd.Flags().GetString("swapserver")
	arkAddr, _ := cmd.Flags().GetString("arkserver")
	arkNoTLS, _ := cmd.Flags().GetBool("ark-no-tls")
	arkTLSCertPath, _ := cmd.Flags().GetString("ark-tlscertpath")

	// Connect to the swap server.
	swapConn, err := grpc.NewClient(
		swapAddr,
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"connect to swap server: %w", err,
		)
	}

	arkConn, err := dialARKSwapConn(arkAddr, arkNoTLS, arkTLSCertPath)
	if err != nil {
		swapConn.Close()

		return nil, nil, err
	}

	info, err := daemonClient.GetInfo(
		context.Background(), &daemonrpc.GetInfoRequest{},
	)
	if err != nil {
		swapConn.Close()
		arkConn.Close()

		return nil, nil, fmt.Errorf("get daemon info: %w", err)
	}

	chainParams, err := chainParamsForNetwork(info.GetNetwork())
	if err != nil {
		swapConn.Close()
		arkConn.Close()

		return nil, nil, err
	}

	invoiceKey, err := btcec.NewPrivateKey()
	if err != nil {
		swapConn.Close()
		arkConn.Close()

		return nil, nil, fmt.Errorf("create invoice key: %w", err)
	}

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

	arkClient := arkrpc.NewArkServiceClient(arkConn)
	indexerClient := arkrpc.NewIndexerServiceClient(arkConn)

	serverConn := swaps.NewGRPCSwapServerConn(swapConn)
	daemonWrapper := swaps.NewRPCDaemonConn(
		daemonClient, arkClient, indexerClient,
	)

	client := swaps.NewSwapClient(
		serverConn, daemonWrapper, nil,
		swaps.NewInvoiceGenerator(
			keychain.NewPrivKeyMessageSigner(
				invoiceKey, keychain.KeyLocator{},
			),
			bestHeight,
			swaps.NewMemoryInvoiceStore(),
			chainParams,
		),
	)

	cleanup := func() {
		_ = serverConn.Close()
		_ = swapConn.Close()
		_ = arkConn.Close()
	}

	return client, cleanup, nil
}

// dialARKSwapConn establishes the optional TLS/insecure Ark gRPC connection
// used by the swap CLI for operator and indexer RPCs.
func dialARKSwapConn(arkAddr string, noTLS bool,
	tlsCertPath string) (*grpc.ClientConn, error) {

	var opts []grpc.DialOption

	switch {
	case noTLS:
		opts = append(opts, grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		))

	case tlsCertPath != "":
		creds, err := credentials.NewClientTLSFromFile(
			tlsCertPath, "",
		)
		if err != nil {
			return nil, fmt.Errorf("load ark TLS cert: %w", err)
		}

		opts = append(opts, grpc.WithTransportCredentials(creds))

	default:
		return nil, fmt.Errorf("ark TLS cert is required unless " +
			"--ark-no-tls is set")
	}

	conn, err := grpc.NewClient(arkAddr, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to ark server: %w", err)
	}

	return conn, nil
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
