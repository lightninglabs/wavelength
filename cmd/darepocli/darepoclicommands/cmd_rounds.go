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
		newRoundsGetCmd(), newRoundsJoinCmd(), newRoundsListCmd(),
		newRoundsWatchCmd(),
	)

	return cmd
}

// newRoundsGetCmd creates the 'rounds get' subcommand.
func newRoundsGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Get one round status",
		RunE:  roundsGet,
	}

	cmd.Flags().String("round-id", "",
		"server-assigned round id to fetch")

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
	cmd.Flags().String("state", "",
		"optional state filter, for example confirmed or failed")
	cmd.Flags().Int64("created-after", 0,
		"only show persisted rounds created at or after this Unix time")
	cmd.Flags().Int64("created-before", 0,
		"only show persisted rounds created before this Unix time")

	return cmd
}

// newRoundsJoinCmd creates the 'rounds join' subcommand.
func newRoundsJoinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join",
		Short: "Commit queued intents and join the next round",
		Long: "Asks the daemon's round actor to commit currently " +
			"queued round intents (refresh or leave) and emit a " +
			"JoinRoundRequest to the operator. Use this after " +
			"queueing intents via `ark vtxos refresh` or " +
			"`ark vtxos leave` that intentionally batch in " +
			"PendingRoundAssembly.",
		RunE: roundsJoin,
	}
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

// roundsGet executes the GetRound RPC and prints the result.
func roundsGet(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &daemonrpc.GetRoundRequest{}
	fromFlags := func() error {
		roundID, _ := cmd.Flags().GetString("round-id")
		req.RoundId = roundID

		return nil
	}

	if err := parseRequest(cmd, req, fromFlags); err != nil {
		return err
	}

	resp, err := client.GetRound(context.Background(), req)
	if err != nil {
		return fmt.Errorf("GetRound RPC failed: %w", err)
	}

	return printJSON(resp)
}

// roundsList executes the ListRounds RPC and prints the result.
func roundsList(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &daemonrpc.ListRoundsRequest{}
	fromFlags := func() error {
		persistedOnly, _ := cmd.Flags().GetBool("persisted-only")
		req.PersistedOnly = persistedOnly

		pageSize, _ := cmd.Flags().GetInt32("page-size")
		req.PageSize = pageSize

		pageToken, _ := cmd.Flags().GetString("page-token")
		req.PageToken = pageToken

		state, _ := cmd.Flags().GetString("state")
		filter, err := parseRoundStateFilter(state)
		if err != nil {
			return err
		}
		req.StateFilter = filter

		createdAfter, _ := cmd.Flags().GetInt64("created-after")
		req.CreatedAfter = createdAfter

		createdBefore, _ := cmd.Flags().GetInt64("created-before")
		req.CreatedBefore = createdBefore

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

// roundsJoin executes the JoinNextRound RPC and prints the result.
func roundsJoin(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	req := &daemonrpc.JoinNextRoundRequest{}
	if err := parseRequest(cmd, req, func() error {
		return nil
	}); err != nil {
		return err
	}

	resp, err := client.JoinNextRound(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("JoinNextRound RPC failed: %w", err)
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
		context.Background(), &daemonrpc.WatchRoundsRequest{},
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

// parseRoundStateFilter converts a CLI state filter into the proto enum.
func parseRoundStateFilter(state string) (daemonrpc.RoundState, error) {
	switch state {
	case "", "all":
		return daemonrpc.RoundState_ROUND_STATE_UNKNOWN, nil

	case "idle":
		return daemonrpc.RoundState_ROUND_STATE_IDLE, nil

	case "pending_assembly":
		return daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY, nil

	case "registration_sent":
		return daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT, nil

	case "quote_received":
		return daemonrpc.RoundState_ROUND_STATE_QUOTE_RECEIVED, nil

	case "joined":
		return daemonrpc.RoundState_ROUND_STATE_JOINED, nil

	case "commitment_received":
		return daemonrpc.RoundState_ROUND_STATE_COMMITMENT_RECEIVED, nil

	case "commitment_validated":
		return daemonrpc.RoundState_ROUND_STATE_COMMITMENT_VALIDATED,
			nil

	case "forfeit_collecting":
		return daemonrpc.RoundState_ROUND_STATE_FORFEIT_COLLECTING, nil

	case "nonces_sent":
		return daemonrpc.RoundState_ROUND_STATE_NONCES_SENT, nil

	case "nonces_aggregated":
		return daemonrpc.RoundState_ROUND_STATE_NONCES_AGGREGATED, nil

	case "partial_sigs_sent":
		return daemonrpc.RoundState_ROUND_STATE_PARTIAL_SIGS_SENT, nil

	case "input_sig_sent":
		return daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, nil

	case "confirmed":
		return daemonrpc.RoundState_ROUND_STATE_CONFIRMED, nil

	case "failed":
		return daemonrpc.RoundState_ROUND_STATE_FAILED, nil

	case "recovery":
		return daemonrpc.RoundState_ROUND_STATE_RECOVERY, nil

	default:
		return daemonrpc.RoundState_ROUND_STATE_UNKNOWN,
			fmt.Errorf("unknown round state filter: %s", state)
	}
}
