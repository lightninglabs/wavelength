package darepoclicommands

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/sdk/swaps"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
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

	result, err := swapClient.ReceiveViaLightning(
		ctx, btcutil.Amount(amount),
	)
	if err != nil {
		return fmt.Errorf("receive failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"Invoice: %s\n"+
			"Payment hash: %s\n"+
			"Preimage: %x\n"+
			"VTXO outpoint: %s\n"+
			"Amount: %d sat\n",
		result.Invoice,
		hex.EncodeToString(result.PaymentHash[:]),
		result.Preimage[:],
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
	daemonConn *grpc.ClientConn) (*swaps.SwapClient,
	func(), error) {

	swapAddr, _ := cmd.Flags().GetString("swapserver")

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

	// Connect to the ark server for operator pubkey. Reuse
	// the daemon's connection info — the ark server is
	// typically on the same host as the daemon's server.
	arkClient := arkrpc.NewArkServiceClient(daemonConn)

	serverConn := swaps.NewGRPCSwapServerConn(swapConn)
	daemonWrapper := swaps.NewRPCDaemonConn(
		daemonClient, arkClient,
	)

	// For MVP, create swap client without InvoiceGenerator.
	// ReceiveViaLightning will fail if called without one.
	// TODO(swap): wire InvoiceGenerator from wallet signer.
	client := swaps.NewSwapClient(
		serverConn, daemonWrapper, nil, nil,
	)

	cleanup := func() {
		_ = serverConn.Close()
		swapConn.Close()
	}

	return client, cleanup, nil
}
