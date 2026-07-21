package waveclicommands

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

// newRecoveryCmd builds the advanced control-plane commands for daemon-owned
// vHTLC recovery rows. Normal swap clients should let the swap FSM arm and
// cancel recovery automatically; these commands are for manual escalation,
// operator intervention, and debugging.
func newRecoveryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recovery",
		Short: "Manage daemon-owned vHTLC recovery rows",
		Long: "Manage already-armed vHTLC recovery rows. " +
			"Automatic swap execution arms recovery early and " +
			"escalates it only when policy says on-chain " +
			"recovery is needed; this subtree lets an operator " +
			"inspect or override " +
			"that decision for a specific recovery id.",
	}

	cmd.AddCommand(
		newRecoveryListCmd(), newRecoveryStatusCmd(),
		newRecoveryEscalateCmd(), newRecoveryCancelCmd(),
	)

	return cmd
}

// newRecoveryListCmd lists daemon-owned recovery rows and explains which rows
// are still dormant before an operator chooses whether to escalate one.
func newRecoveryListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List vHTLC recovery rows",
		Long: "List vHTLC recovery rows from the daemon. ARMED " +
			"rows are dormant: funds are not being unrolled " +
			"unless an operator runs recovery escalate, or auto " +
			"escalation is enabled by policy.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, conn, err := getDaemonClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			includeTerminal, _ := cmd.Flags().GetBool(
				"include-terminal",
			)
			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := client.ListVHTLCRecoveries(
				ctx,
				&waverpc.ListVHTLCRecoveriesRequest{
					IncludeTerminal: includeTerminal,
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().Bool("include-terminal", false,
		"include completed, cancelled, and failed recovery rows")

	return cmd
}

// newRecoveryStatusCmd returns the daemon's durable recovery row and current
// unroll status, if present.
func newRecoveryStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [recovery_id]",
		Short: "Show one vHTLC recovery row",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := getDaemonClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := client.GetVHTLCRecoveryStatus(
				ctx,
				&waverpc.GetVHTLCRecoveryStatusRequest{
					RecoveryId: args[0],
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}
}

// newRecoveryEscalateCmd manually starts on-chain unroll for one previously
// armed recovery row. Claim recovery can include --claim-preimage-hex when the
// daemon cannot resolve the preimage from the swap store.
func newRecoveryEscalateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "escalate [recovery_id]",
		Short: "Start on-chain recovery for one armed vHTLC",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := getDaemonClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			reason, _ := cmd.Flags().GetString("reason")
			preimageHex, _ := cmd.Flags().GetString(
				"claim-preimage-hex",
			)
			preimage, err := decodeRecoveryPreimage(preimageHex)
			if err != nil {
				return err
			}
			if err := confirmRecoveryEscalation(
				cmd, args[0],
			); err != nil {
				return err
			}

			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := client.EscalateVHTLCRecovery(
				ctx,
				&waverpc.EscalateVHTLCRecoveryRequest{
					RecoveryId:    args[0],
					Reason:        reason,
					ClaimPreimage: preimage,
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().String("reason", "manual client escalation",
		"operator-facing reason stored with the escalation attempt")
	cmd.Flags().String("claim-preimage-hex", "",
		"optional 32-byte claim preimage for claim recoveries")
	cmd.Flags().Bool("yes", false,
		"skip interactive confirmation before starting on-chain "+
			"recovery")

	return cmd
}

// newRecoveryCancelCmd records that cooperative settlement won and the armed
// recovery row is no longer needed.
func newRecoveryCancelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel [recovery_id]",
		Short: "Cancel one armed vHTLC recovery row",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, err := getDaemonClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			reason, _ := cmd.Flags().GetString("reason")
			txid, _ := cmd.Flags().GetString("cooperative-txid")

			ctx, cancel := rpcContext(cmd)
			defer cancel()

			resp, err := client.CancelVHTLCRecovery(
				ctx,
				&waverpc.CancelVHTLCRecoveryRequest{
					RecoveryId:      args[0],
					Reason:          reason,
					CooperativeTxid: txid,
				},
			)
			if err != nil {
				return err
			}

			return printJSON(resp)
		},
	}

	cmd.Flags().String("reason", "manual client cancellation",
		"operator-facing reason stored with the cancellation")
	cmd.Flags().String("cooperative-txid", "",
		"optional cooperative OOR txid/session id that won")

	return cmd
}

// confirmRecoveryEscalation requires explicit operator consent before a manual
// command starts costly on-chain recovery. Non-interactive callers must pass
// --yes so scripts and agents cannot hang on a prompt or escalate by accident.
func confirmRecoveryEscalation(cmd *cobra.Command, recoveryID string) error {
	yes, _ := cmd.Flags().GetBool("yes")
	if yes {
		return nil
	}
	if !stdinIsTTY(cmd) {
		return PrintError(
			confirmationRequiredCode, "recovery escalate "+
				"requires --yes (explicit consent) on "+
				"non-interactive stdin; refusing to prompt "+
				"because an agent cannot respond to y/N",
		)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(
		out, "About to start on-chain vHTLC recovery for %s.\n",
		recoveryID,
	)
	fmt.Fprintln(
		out, "This may unroll Ark state and pay on-chain fees. Use "+
			"recovery status/list first to confirm cooperative "+
			"settlement cannot safely continue.",
	)
	fmt.Fprint(out, "Start recovery? [y/N]: ")

	reader := bufio.NewReader(cmd.InOrStdin())
	answer, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return fmt.Errorf("aborted by user")
	}

	return nil
}

// decodeRecoveryPreimage decodes an optional 32-byte preimage. Empty input maps
// to nil so refund-without-receiver escalation does not send secret material.
func decodeRecoveryPreimage(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}

	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode claim preimage: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("claim preimage must be 32 bytes")
	}

	return raw, nil
}
