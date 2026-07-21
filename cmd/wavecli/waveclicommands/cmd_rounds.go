package waveclicommands

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
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
	addListOutputFlags(cmd, "round")

	return cmd
}

// newRoundsJoinCmd creates the 'rounds join' subcommand.
func newRoundsJoinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join",
		Short: "Commit queued intents and join the next round",
		Long: "Asks the daemon's round actor to commit currently " +
			"queued round intents (refresh or leave) and emit " +
			"a JoinRoundRequest to the operator.\n\n" +
			"`ark vtxos refresh` and `ark vtxos leave` invoke " +
			"this automatically on the caller's behalf. Use " +
			"`join` directly only when one of those commands " +
			"was passed `--no_join` to batch multiple intents " +
			"into the same round.",
		RunE: roundsJoin,
	}
}

// newRoundsWatchCmd creates the 'rounds watch' subcommand.
func newRoundsWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream round state updates",
		Long: "Opens a server-streaming connection " +
			"that prints round state transitions " +
			"as they occur. Press Ctrl-C to stop.",
		RunE: roundsWatch,
	}

	cmd.Flags().Uint32("max-events", 0,
		"stop after this many updates; 0 has no event-count limit")
	cmd.Flags().Duration("for", 0,
		"watch for this duration instead of the global --timeout")

	return cmd
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

	req := &waverpc.GetRoundRequest{}
	fromFlags := func() error {
		roundID, _ := cmd.Flags().GetString("round-id")
		req.RoundId = roundID

		return nil
	}

	if err := parseRequest(cmd, req, fromFlags); err != nil {
		return err
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.GetRound(ctx, req)
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

	req := &waverpc.ListRoundsRequest{}
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

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.ListRounds(ctx, req)
	if err != nil {
		return fmt.Errorf("ListRounds RPC failed: %w", err)
	}

	items := make([]proto.Message, len(resp.Rounds))
	for i, r := range resp.Rounds {
		items[i] = r
	}

	return renderListOutput(cmd, resp, items)
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

	req := &waverpc.JoinNextRoundRequest{}
	if err := parseRequest(cmd, req, func() error {
		return nil
	}); err != nil {
		return err
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.JoinNextRound(ctx, req)
	if err != nil {
		return fmt.Errorf("JoinNextRound RPC failed: %w", err)
	}

	return printJSON(resp)
}

// roundsWatch executes the WatchRounds streaming RPC and prints
// each state update as it arrives.
func roundsWatch(cmd *cobra.Command, _ []string) error {
	maxEvents, _ := cmd.Flags().GetUint32("max-events")
	watchFor, _ := cmd.Flags().GetDuration("for")
	if watchFor < 0 {
		return invalidArgs(fmt.Errorf("--for must be non-negative"))
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := rpcContext(cmd)
	if watchFor > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(cmd.Context(), watchFor)
	}
	defer cancel()

	stream, err := client.WatchRounds(ctx, &waverpc.WatchRoundsRequest{})
	if err != nil {
		return fmt.Errorf("WatchRounds RPC failed: %w", err)
	}

	var events uint32
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) ||
				(watchFor > 0 && errors.Is(
					ctx.Err(), context.DeadlineExceeded,
				)) {
				return nil
			}

			return fmt.Errorf("stream error: %w", err)
		}

		if err := printJSON(resp); err != nil {
			return err
		}

		events++
		if maxEvents > 0 && events >= maxEvents {
			return nil
		}
	}
}

// parseRoundStateFilter converts a CLI state filter into the proto enum.
func parseRoundStateFilter(state string) (waverpc.RoundState, error) {
	switch state {
	case "", "all":
		return waverpc.RoundState_ROUND_STATE_UNKNOWN, nil

	case "idle":
		return waverpc.RoundState_ROUND_STATE_IDLE, nil

	case "pending_assembly":
		return waverpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY, nil

	case "registration_sent":
		return waverpc.RoundState_ROUND_STATE_REGISTRATION_SENT, nil

	case "quote_received":
		return waverpc.RoundState_ROUND_STATE_QUOTE_RECEIVED, nil

	case "joined":
		return waverpc.RoundState_ROUND_STATE_JOINED, nil

	case "commitment_received":
		return waverpc.RoundState_ROUND_STATE_COMMITMENT_RECEIVED, nil

	case "commitment_validated":
		return waverpc.RoundState_ROUND_STATE_COMMITMENT_VALIDATED,
			nil

	case "forfeit_collecting":
		return waverpc.RoundState_ROUND_STATE_FORFEIT_COLLECTING, nil

	case "nonces_sent":
		return waverpc.RoundState_ROUND_STATE_NONCES_SENT, nil

	case "nonces_aggregated":
		return waverpc.RoundState_ROUND_STATE_NONCES_AGGREGATED, nil

	case "partial_sigs_sent":
		return waverpc.RoundState_ROUND_STATE_PARTIAL_SIGS_SENT, nil

	case "input_sig_sent":
		return waverpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, nil

	case "confirmed":
		return waverpc.RoundState_ROUND_STATE_CONFIRMED, nil

	case "failed":
		return waverpc.RoundState_ROUND_STATE_FAILED, nil

	case "recovery":
		return waverpc.RoundState_ROUND_STATE_RECOVERY, nil

	default:
		return waverpc.RoundState_ROUND_STATE_UNKNOWN,
			fmt.Errorf("unknown round state filter: %s", state)
	}
}
