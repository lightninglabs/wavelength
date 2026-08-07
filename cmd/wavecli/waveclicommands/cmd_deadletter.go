package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

// newDeadLetterCmd builds the advanced operator commands for dead-lettered
// actor messages: messages a durable actor abandoned after exhausting its
// delivery retries. A parked dead letter never recovers on its own; this
// subtree is how an operator inspects one and decides between requeueing it
// (re-deliver under a fresh message ID) and purging it.
func newDeadLetterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deadletter",
		Short: "Inspect and recover dead-lettered actor messages",
		Long: "Inspect and recover dead-lettered actor messages. A " +
			"dead letter is a message a durable actor abandoned " +
			"after exhausting its delivery retries; it stays " +
			"parked until an operator requeues or deletes it. " +
			"Use list/inspect to triage, requeue to re-deliver " +
			"a message whose failure cause has been fixed, and " +
			"purge to drop entries that are no longer relevant.",
	}

	cmd.AddCommand(
		newDeadLetterListCmd(), newDeadLetterInspectCmd(),
		newDeadLetterRequeueCmd(), newDeadLetterPurgeCmd(),
	)

	return cmd
}

// newDeadLetterListCmd lists parked dead letters, newest first.
func newDeadLetterListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List dead-lettered messages",
		Long: "List parked dead letters, newest first. The " +
			"total_count field reports the global queue depth " +
			"independent of filters, so a bounded listing still " +
			"shows how much is parked overall.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, conn, err := getDaemonClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			actorID, _ := cmd.Flags().GetString("actor-id")
			limit, _ := cmd.Flags().GetInt32("limit")
			offset, _ := cmd.Flags().GetInt32("offset")

			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := client.ListDeadLetters(
				ctx,
				&waverpc.ListDeadLettersRequest{
					ActorId: actorID,
					Limit:   limit,
					Offset:  offset,
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().String("actor-id", "",
		"restrict the listing to one actor's dead letters")
	cmd.Flags().Int32("limit", 0,
		"maximum entries to return (0 uses the daemon default)")
	cmd.Flags().Int32("offset", 0,
		"entries to skip for pagination (global listing only)")

	return cmd
}

// newDeadLetterInspectCmd shows one dead letter including its payload.
func newDeadLetterInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect [id]",
		Short: "Show one dead letter, payload included",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := getDaemonClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := client.GetDeadLetter(
				ctx,
				&waverpc.GetDeadLetterRequest{
					Id: args[0],
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}
}

// newDeadLetterRequeueCmd re-enqueues one dead letter into its original
// actor mailbox.
func newDeadLetterRequeueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "requeue [id]",
		Short: "Re-enqueue one dead letter for delivery",
		Long: "Re-enqueue a dead letter into its original actor " +
			"mailbox under a fresh message ID, with its retry " +
			"budget reset and all routing preserved. Requeue " +
			"only after the failure cause is fixed: a message " +
			"that still fails will burn its retries and land " +
			"back in the dead-letter queue.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := getDaemonClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			if err := confirmDeadLetterRequeue(
				cmd, args[0],
			); err != nil {
				return err
			}

			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := client.RequeueDeadLetter(
				ctx,
				&waverpc.RequeueDeadLetterRequest{
					Id: args[0],
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().Bool("yes", false,
		"skip interactive confirmation before requeueing")

	return cmd
}

// newDeadLetterPurgeCmd permanently deletes dead letters older than a given
// age.
func newDeadLetterPurgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Permanently delete dead letters older than an age",
		Long: "Permanently delete dead letters older than " +
			"--older-than. A purged message cannot be recovered " +
			"or requeued afterwards; inspect the queue first.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, conn, err := getDaemonClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			olderThan, _ := cmd.Flags().GetDuration("older-than")
			if olderThan <= 0 {
				return fmt.Errorf("--older-than must be a " +
					"positive duration")
			}

			if err := confirmDeadLetterPurge(
				cmd, olderThan.String(),
			); err != nil {
				return err
			}

			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := client.PurgeDeadLetters(
				ctx,
				&waverpc.PurgeDeadLettersRequest{
					OlderThanSeconds: int64(
						olderThan.Seconds(),
					),
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().Duration("older-than", 0,
		"delete entries dead-lettered longer ago than this "+
			"duration (required, e.g. 720h)")
	cmd.Flags().Bool("yes", false,
		"skip interactive confirmation before purging")

	return cmd
}

// confirmDeadLetterRequeue requires explicit operator consent before a
// requeue re-injects a message into a live actor. Non-interactive callers
// must pass --yes so scripts and agents cannot hang on a prompt.
func confirmDeadLetterRequeue(cmd *cobra.Command, id string) error {
	yes, _ := cmd.Flags().GetBool("yes")
	if yes {
		return nil
	}
	if !canPrompt(cmd) {
		return PrintError(
			confirmationRequiredCode, "deadletter requeue "+
				"requires --yes (explicit consent) on "+
				"non-interactive stdin or when input is "+
				"disabled; refusing to prompt because an "+
				"agent cannot respond to y/N",
		)
	}

	out := cmd.ErrOrStderr()
	fmt.Fprintf(
		out, "About to requeue dead letter %s into its original "+
			"actor mailbox.\n", id,
	)
	fmt.Fprintln(
		out, "The message will be re-delivered and its side "+
			"effects re-attempted. Inspect it first and "+
			"requeue only after the failure cause is fixed.",
	)

	return promptConfirmation(cmd, "Requeue message? [y/N]: ")
}

// confirmDeadLetterPurge requires explicit operator consent before dead
// letters are irrecoverably deleted.
func confirmDeadLetterPurge(cmd *cobra.Command, olderThan string) error {
	yes, _ := cmd.Flags().GetBool("yes")
	if yes {
		return nil
	}
	if !canPrompt(cmd) {
		return PrintError(
			confirmationRequiredCode, "deadletter purge "+
				"requires --yes (explicit consent) on "+
				"non-interactive stdin or when input is "+
				"disabled; refusing to prompt because an "+
				"agent cannot respond to y/N",
		)
	}

	out := cmd.ErrOrStderr()
	fmt.Fprintf(
		out, "About to permanently delete all dead letters older "+
			"than %s.\n", olderThan,
	)
	fmt.Fprintln(
		out,
		"Purged messages cannot be recovered or requeued afterwards.",
	)

	return promptConfirmation(cmd, "Purge dead letters? [y/N]: ")
}
