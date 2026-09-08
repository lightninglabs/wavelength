package waveclicommands

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	channelCreateKeyPrefix = "wavecli-channel-"
	channelCreateRetryWait = 250 * time.Millisecond
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
		newChannelCreateCmd(), newChannelGetCmd(), newChannelListCmd(),
		newChannelSendCmd(), newChannelReceiveCmd(), newChannelPayCmd(),
		newChannelCloseCmd(), newChannelForceCloseCmd(),
	)

	return cmd
}

// newChannelCreateCmd promotes one wallet VTXO into an OOR channel.
func newChannelCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <amount-sat>",
		Short: "Promote wallet value into a channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			amount, err := parsePositiveChannelAmount(args[0])
			if err != nil {
				return err
			}
			if err := confirmMoneyMovement(
				cmd, fmt.Sprintf("promote %d sat into an "+
					"Ark channel", amount),
			); err != nil {
				return err
			}
			idempotencyKey, _ := cmd.Flags().GetString(
				"idempotency-key",
			)
			if idempotencyKey == "" {
				idempotencyKey, err = newChannelCreateKey()
				if err != nil {
					return err
				}
				fmt.Fprintf(
					cmd.ErrOrStderr(),
					"Channel creation idempotency key: "+
						"%s\n", idempotencyKey,
				)
			}
			client, conn, err := getArkChannelClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := promoteVTXOWithRetry(
				cmd, client, &arkchannelrpc.PromoteVTXORequest{
					AmountSat:      amount,
					IdempotencyKey: idempotencyKey,
				},
			)
			if err != nil {
				return fmt.Errorf("create channel with "+
					"idempotency key %q: %w",
					idempotencyKey, err)
			}

			return printJSON(resp)
		},
	}
	cmd.Flags().String("idempotency-key", "",
		"stable key for safely retrying this creation")
	cmd.Flags().Bool("yes", false,
		"approve promoting wallet funds into a channel")

	return cmd
}

// newChannelCreateKey returns the retry identity used for one CLI invocation.
func newChannelCreateKey() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate channel creation key: %w", err)
	}

	return channelCreateKeyPrefix + hex.EncodeToString(entropy[:]), nil
}

// promoteVTXOWithRetry keeps one logical channel creation alive across finite
// RPC attempt deadlines and transient transport loss. The command context
// remains the overall cancellation boundary.
func promoteVTXOWithRetry(cmd *cobra.Command,
	client arkchannelrpc.ArkChannelServiceClient,
	req *arkchannelrpc.PromoteVTXORequest) (
	*arkchannelrpc.PromoteVTXOResponse, error) {

	ctx := commandContext(cmd)
	for {
		attemptCtx, cancel := rpcContextFrom(cmd, ctx)
		response, err := client.PromoteVTXO(attemptCtx, req)
		cancel()
		if err == nil {
			return response, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryChannelCreateError(err) {
			return nil, err
		}
		if err := waitChannelCreateRetry(ctx); err != nil {
			return nil, err
		}
	}
}

// retryChannelCreateError reports transport outcomes that can be retried with
// the same mandatory idempotency key.
func retryChannelCreateError(err error) bool {
	switch status.Code(err) {
	case codes.Aborted, codes.DeadlineExceeded, codes.ResourceExhausted,
		codes.Unavailable:
		return true

	default:
		return false
	}
}

// waitChannelCreateRetry applies bounded backoff without obscuring command
// cancellation.
func waitChannelCreateRetry(ctx context.Context) error {
	timer := time.NewTimer(channelCreateRetryWait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-timer.C:
		return nil
	}
}

// newChannelGetCmd returns one durable channel snapshot.
func newChannelGetCmd() *cobra.Command {
	return channelIDCommand(
		"get <channel-id>", "Show one channel", "",
		func(cmd *cobra.Command,
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

// newChannelListCmd returns every channel that still needs daemon recovery or
// operator action.
func newChannelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List recoverable channels and live balances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, conn, err := getArkChannelClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			response, err := client.ListChannels(
				ctx, &arkchannelrpc.ListChannelsRequest{},
			)
			if err != nil {
				return err
			}

			return printJSON(response)
		},
	}
}

// newChannelSendCmd sends a local test payment over one channel.
func newChannelSendCmd() *cobra.Command {
	return channelPaymentCommand(
		"send", "Send from the local balance to the hub", func(
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
		},
	)
}

// newChannelReceiveCmd receives a local test payment over one channel.
func newChannelReceiveCmd() *cobra.Command {
	return channelPaymentCommand(
		"receive", "Receive from the hub into the local balance", func(
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
		},
	)
}

// newChannelPayCmd bridges a private source HTLC to a public invoice.
func newChannelPayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pay <bolt11>",
		Short: "Pay a public Lightning invoice through a channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			maxFee, _ := cmd.Flags().GetUint64("max-fee-sat")
			if err := confirmMoneyMovement(
				cmd, fmt.Sprintf("pay a public invoice with "+
					"a maximum %d sat fee", maxFee),
			); err != nil {
				return err
			}
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
	cmd.Flags().Uint64("max-fee-sat", 1_000,
		"maximum public Lightning routing fee")
	cmd.Flags().Bool("yes", false,
		"approve paying the public invoice")

	return cmd
}

// newChannelCloseCmd cooperatively closes one clean channel.
func newChannelCloseCmd() *cobra.Command {
	return channelIDCommand(
		"close <channel-id>", "Cooperatively close one channel",
		"cooperatively close Ark channel", func(cmd *cobra.Command,
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
			"channel", "materialize and force close Ark channel",
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
func channelPaymentCommand(name, short string,
	dispatch channelPaymentDispatch) *cobra.Command {

	cmd := &cobra.Command{
		Use:   name + " <channel-id> <amount-sat>",
		Short: short,
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
			if err := confirmMoneyMovement(
				cmd, fmt.Sprintf("%s %d sat over Ark "+
					"channel %x", name, amount,
					channelID[:4]),
			); err != nil {
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
	cmd.Flags().Bool("yes", false,
		"approve changing the channel balance")

	return cmd
}

// channelIDDispatch executes one channel-ID-bearing RPC.
type channelIDDispatch func(
	*cobra.Command, arkchannelrpc.ArkChannelServiceClient, []byte,
) error

// channelIDCommand builds a command whose sole argument is a channel ID.
func channelIDCommand(use, short, action string,
	dispatch channelIDDispatch) *cobra.Command {

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channelID, err := parseChannelID(args[0])
			if err != nil {
				return err
			}
			if action != "" {
				err := confirmMoneyMovement(
					cmd, fmt.Sprintf("%s %x", action,
						channelID[:4]),
				)
				if err != nil {
					return err
				}
			}
			client, conn, err := getArkChannelClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			return dispatch(cmd, client, channelID)
		},
	}
	if action != "" {
		cmd.Flags().Bool("yes", false, "approve the channel close")
	}

	return cmd
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
