package waveclicommands

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// inputDisabled reports whether the caller explicitly requested a
// non-interactive invocation. CI=true has the same prompt-suppression
// behavior as --no-input, but does not alter normal command output.
func inputDisabled(cmd *cobra.Command) bool {
	if cmd != nil {
		noInput, err := cmd.Flags().GetBool("no-input")
		if err == nil && noInput {
			return true
		}

		root := cmd.Root()
		if root != nil {
			noInput, err = root.PersistentFlags().GetBool(
				"no-input",
			)
			if err == nil && noInput {
				return true
			}
		}
	}

	return strings.EqualFold(strings.TrimSpace(os.Getenv("CI")), "true")
}

// canPrompt requires both an interactive input stream and permission
// to prompt. Explicit confirmation flags remain the automation path.
func canPrompt(cmd *cobra.Command) bool {
	return !inputDisabled(cmd) && stdinIsTTY(cmd)
}

// writePrompt keeps all interactive diagnostics on stderr so stdout
// remains safe for command results and machine-readable output.
func writePrompt(cmd *cobra.Command, text string) error {
	_, err := fmt.Fprint(cmd.ErrOrStderr(), text)

	return err
}

// promptConfirmation writes one y/N question to stderr and reads one
// answer from the command's configured stdin. Callers must check
// canPrompt before invoking it.
func promptConfirmation(cmd *cobra.Command, prompt string) error {
	if err := writePrompt(cmd, prompt); err != nil {
		return fmt.Errorf("write confirmation prompt: %w", err)
	}

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
