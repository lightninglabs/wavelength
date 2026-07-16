package waveclicommands

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

// newChannelCmd creates the virtual channel command group.
func newChannelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channel",
		Short: "Manage virtual Lightning channels",
		Long: "Commands for requesting and inspecting virtual " +
			"Lightning channels backed by Ark rounds.",
	}

	cmd.AddCommand(newChannelPromoteCmd(), newChannelRequestCmd())

	return cmd
}

// newChannelPromoteCmd promotes one existing VTXO to a virtual channel.
func newChannelPromoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "promote <amount-sat>",
		Short: "Promote VTXO liquidity into a channel",
		Long: "Promotes one existing VTXO into a private virtual " +
			"channel with the operator. The amount is the " +
			"desired " +
			"Lightning channel capacity; the daemon selects " +
			"the exact backing VTXO and handles all channel " +
			"construction details.",
		Args: cobra.ExactArgs(1),
		RunE: channelPromote,
	}
}

func channelPromote(cmd *cobra.Command, args []string) error {
	amount, err := positiveChannelAmount(args[0])
	if err != nil {
		return err
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	requestKey, err := newChannelRequestKey()
	if err != nil {
		return fmt.Errorf("generate channel request id: %w", err)
	}
	resp, err := client.OpenVirtualChannel(
		cmd.Context(), &waverpc.OpenVirtualChannelRequest{
			AmountSat:      amount,
			IdempotencyKey: requestKey,
		},
	)
	if err != nil {
		return fmt.Errorf("promote VTXO to virtual channel: %w", err)
	}

	return printJSON(resp)
}

// newChannelRequestCmd requests a round-funded virtual channel.
func newChannelRequestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "request <amount-sat>",
		Short: "Request inbound virtual channel capacity",
		Long: "Requests an operator-liquidity virtual channel " +
			"funded in the next Ark round. The single amount is " +
			"the desired Lightning channel capacity in satoshis; " +
			"the daemon handles the private zero-conf channel " +
			"setup and round-funded backing output internally.",
		Args: cobra.ExactArgs(1),
		RunE: channelRequest,
	}
}

// channelRequest registers a receive-channel intent for the next round.
func channelRequest(cmd *cobra.Command, args []string) error {
	amount, err := positiveChannelAmount(args[0])
	if err != nil {
		return err
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	requestKey, err := newChannelRequestKey()
	if err != nil {
		return fmt.Errorf("generate channel request id: %w", err)
	}
	req := &waverpc.RegisterReceiveChannelIntentRequest{
		AmountSat:      amount,
		IdempotencyKey: requestKey,
	}
	resp, err := client.RegisterReceiveChannelIntent(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("request virtual channel: %w", err)
	}

	return printJSON(resp)
}

func positiveChannelAmount(value string) (int64, error) {
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, PrintError(
			"INVALID_ARGS", "amount-sat must be a positive integer",
		)
	}
	if amount <= 0 {
		return 0, PrintError(
			"INVALID_ARGS", "amount-sat must be greater than zero",
		)
	}

	return amount, nil
}

func newChannelRequestKey() (string, error) {
	var id [32]byte
	if _, err := io.ReadFull(rand.Reader, id[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(id[:]), nil
}
