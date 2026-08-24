package waveclicommands

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

// getArkChannelClient is replaceable by command wiring tests.
var getArkChannelClient = defaultGetArkChannelClient

// defaultGetArkChannelClient connects to the local Ark channel service.
func defaultGetArkChannelClient(cmd *cobra.Command) (
	arkchannelrpc.ArkChannelServiceClient, *grpc.ClientConn, error) {

	conn, err := getDaemonConn(cmd)
	if err != nil {
		return nil, nil, err
	}

	return arkchannelrpc.NewArkChannelServiceClient(conn), conn, nil
}

// newChannelCmd builds the daemon-development Ark channel control surface.
func newChannelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channel",
		Short: "Manage native Ark-backed Lightning channels",
	}
	cmd.AddCommand(
		newChannelCreateCmd(), newChannelGetCmd(), newChannelSendCmd(),
		newChannelReceiveCmd(), newChannelPayCmd(),
		newChannelCloseCmd(), newChannelForceCloseCmd(),
	)

	return cmd
}

// newChannelCreateCmd promotes one wallet VTXO into an OOR channel.
func newChannelCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <amount-sat>",
		Short: "Promote wallet value into a channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			amount, err := parsePositiveChannelAmount(args[0])
			if err != nil {
				return err
			}
			client, conn, err := getArkChannelClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := client.PromoteVTXO(
				ctx, &arkchannelrpc.PromoteVTXORequest{
					AmountSat: amount,
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}
}

// newChannelGetCmd returns one durable channel snapshot.
func newChannelGetCmd() *cobra.Command {
	return channelIDCommand(
		"get <channel-id>", "Show one channel", func(cmd *cobra.Command,
			client arkchannelrpc.ArkChannelServiceClient,
			channelID []byte) error {

			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.GetChannel(
				ctx, &arkchannelrpc.GetChannelRequest{
					ChannelId: channelID,
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	)
}

// newChannelSendCmd sends a local test payment over one channel.
func newChannelSendCmd() *cobra.Command {
	return channelPaymentCommand("send", func(
		ctxClient arkchannelrpc.ArkChannelServiceClient,
		cmd *cobra.Command,
		req *arkchannelrpc.ChannelPaymentRequest) error {

		ctx, cancel := rpcContext(cmd)
		defer cancel()
		resp, err := ctxClient.SendPayment(ctx, req)
		if err != nil {
			return err
		}

		return printJSON(resp)
	})
}

// newChannelReceiveCmd receives a local test payment over one channel.
func newChannelReceiveCmd() *cobra.Command {
	return channelPaymentCommand("receive", func(
		ctxClient arkchannelrpc.ArkChannelServiceClient,
		cmd *cobra.Command,
		req *arkchannelrpc.ChannelPaymentRequest) error {

		ctx, cancel := rpcContext(cmd)
		defer cancel()
		resp, err := ctxClient.ReceivePayment(ctx, req)
		if err != nil {
			return err
		}

		return printJSON(resp)
	})
}

// newChannelPayCmd bridges a private source HTLC to a public invoice.
func newChannelPayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pay <bolt11>",
		Short: "Pay a public Lightning invoice through a channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			maxFee, _ := cmd.Flags().GetUint64("max-fee-sat")
			client, conn, err := getArkChannelClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.PayLightningInvoice(
				ctx, &arkchannelrpc.PayLightningInvoiceRequest{
					PaymentRequest: args[0],
					MaxFeeSat:      maxFee,
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}
	cmd.Flags().Uint64("max-fee-sat", 100_000,
		"maximum public Lightning routing fee")

	return cmd
}

// newChannelCloseCmd cooperatively closes one clean channel.
func newChannelCloseCmd() *cobra.Command {
	return channelIDCommand(
		"close <channel-id>", "Cooperatively close one channel", func(
			cmd *cobra.Command,
			client arkchannelrpc.ArkChannelServiceClient,
			channelID []byte) error {

			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.RequestCooperativeClose(
				ctx,
				&arkchannelrpc.RequestCooperativeCloseRequest{
					ChannelId: channelID,
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	)
}

// newChannelForceCloseCmd materializes and force closes one channel.
func newChannelForceCloseCmd() *cobra.Command {
	return channelIDCommand(
		"force-close <channel-id>", "Materialize and force close a "+
			"channel",
		func(cmd *cobra.Command,
			client arkchannelrpc.ArkChannelServiceClient,
			channelID []byte) error {

			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.MaterializeAndForceClose(
				ctx,
				&arkchannelrpc.MaterializeAndForceCloseRequest{
					ChannelId: channelID,
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	)
}

// channelPaymentDispatch executes one amount-bearing channel RPC.
type channelPaymentDispatch func(
	arkchannelrpc.ArkChannelServiceClient, *cobra.Command,
	*arkchannelrpc.ChannelPaymentRequest,
) error

// channelPaymentCommand builds a two-positional-argument payment command.
func channelPaymentCommand(name string,
	dispatch channelPaymentDispatch) *cobra.Command {

	return &cobra.Command{
		Use:   name + " <channel-id> <amount-sat>",
		Short: name + " over one active channel",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			channelID, err := parseChannelID(args[0])
			if err != nil {
				return err
			}
			amount, err := parsePositiveChannelAmount(args[1])
			if err != nil {
				return err
			}
			client, conn, err := getArkChannelClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			return dispatch(client, cmd,
				&arkchannelrpc.ChannelPaymentRequest{
					ChannelId: channelID, AmountSat: amount,
				})
		},
	}
}

// channelIDDispatch executes one channel-ID-bearing RPC.
type channelIDDispatch func(
	*cobra.Command, arkchannelrpc.ArkChannelServiceClient, []byte,
) error

// channelIDCommand builds a command whose sole argument is a channel ID.
func channelIDCommand(use, short string,
	dispatch channelIDDispatch) *cobra.Command {

	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channelID, err := parseChannelID(args[0])
			if err != nil {
				return err
			}
			client, conn, err := getArkChannelClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			return dispatch(cmd, client, channelID)
		},
	}
}

// parsePositiveChannelAmount parses one positive signed RPC amount.
func parsePositiveChannelAmount(value string) (int64, error) {
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || amount <= 0 {
		return 0, fmt.Errorf("amount-sat must be a positive integer")
	}

	return amount, nil
}

// parseChannelID accepts either canonical hex or protobuf JSON base64.
func parseChannelID(value string) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		decoded, err = base64.StdEncoding.DecodeString(value)
	}
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("channel-id must encode exactly 32 " +
			"bytes")
	}

	return decoded, nil
}
