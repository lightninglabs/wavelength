package darepoclicommands

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newRoundsCmd creates the rounds parent command with list and watch
// subcommands for observing round FSM state.
func newRoundsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rounds",
		Short: "Inspect round participation state",
		Long: "Commands for listing and watching the " +
			"client's round FSM instances. Use " +
			"'rounds list' for a snapshot and " +
			"'rounds watch' for a live stream.",
	}

	cmd.AddCommand(
		newRoundsListCmd(),
		newRoundsWatchCmd(),
	)

	return cmd
}

// newRoundsListCmd creates the 'rounds list' subcommand.
func newRoundsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List current round FSM states",
		RunE:  roundsList,
	}

	cmd.Flags().Bool("persisted-only", false,
		"only show persisted rounds (skip in-memory pending)")
	cmd.Flags().Int32("page-size", 0,
		"maximum number of persisted rounds to return")
	cmd.Flags().String("page-token", "",
		"cursor from a previous response for pagination")

	return cmd
}

// newRoundsWatchCmd creates the 'rounds watch' subcommand.
func newRoundsWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch",
		Short: "Stream round state updates",
		Long: "Opens a server-streaming connection " +
			"that prints round state transitions " +
			"as they occur. Press Ctrl-C to stop.",
		RunE: roundsWatch,
	}
}

// roundsList executes the ListRounds RPC and prints the result.
func roundsList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.ListRoundsRequest{}
	fromFlags := func() error {
		persistedOnly, _ := cmd.Flags().GetBool("persisted-only")
		req.PersistedOnly = persistedOnly

		pageSize, _ := cmd.Flags().GetInt32("page-size")
		req.PageSize = pageSize

		pageToken, _ := cmd.Flags().GetString("page-token")
		req.PageToken = pageToken

		return nil
	}

	if err := parseRequest(cmd, req, fromFlags); err != nil {
		return err
	}

	resp, err := client.ListRounds(context.Background(), req)
	if err != nil {
		return fmt.Errorf("ListRounds RPC failed: %w", err)
	}

	return printJSON(resp)
}

// roundsWatch executes the WatchRounds streaming RPC and prints
// each state update as it arrives.
func roundsWatch(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := client.WatchRounds(
		context.Background(),
		&daemonrpc.WatchRoundsRequest{},
	)
	if err != nil {
		return fmt.Errorf("WatchRounds RPC failed: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stream error: %w", err)
		}

		if err := printJSON(resp); err != nil {
			return err
		}
	}
}
