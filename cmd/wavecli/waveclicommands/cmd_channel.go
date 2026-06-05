package waveclicommands

import (
	"fmt"
	"strconv"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

const virtualChannelBackingMarginSat = 1000

// newChannelCmd creates the virtual channel command group.
func newChannelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channel",
		Short: "Manage virtual Lightning channels",
		Long: "Commands for requesting and inspecting virtual " +
			"Lightning channels backed by Ark rounds.",
	}

	cmd.AddCommand(newChannelRequestCmd())

	return cmd
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

// channelRequest executes RequestVirtualChannelIntent with CLI defaults.
func channelRequest(cmd *cobra.Command, args []string) error {
	amount, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return PrintError(
			"INVALID_ARGS", "amount-sat must be a positive integer",
		)
	}
	if amount <= 0 {
		return PrintError(
			"INVALID_ARGS", "amount-sat must be greater than zero",
		)
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &waverpc.RequestVirtualChannelIntentRequest{
		CapacitySat:      amount,
		BackingAmountSat: amount + virtualChannelBackingMarginSat,
		Private:          true,
		ZeroConf:         true,
		RoundFunded:      true,
	}
	resp, err := client.RequestVirtualChannelIntent(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("request virtual channel: %w", err)
	}

	return printJSON(resp)
}
