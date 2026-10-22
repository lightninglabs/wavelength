package waveclicommands

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"golang.org/x/term"
)

// stdoutIsTTY is indirect so output-contract tests can exercise the pipe and
// terminal paths without depending on the test runner's file descriptors.
var stdoutIsTTY = func() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// validateListFormat rejects presentation formats that do not apply to the
// selected list view.
func validateListFormat(format string, view wavewalletrpc.ListView) error {
	switch format {
	case "", "json":
		return nil

	case "table", "expanded", "x":
		if view != wavewalletrpc.ListView_LIST_VIEW_ACTIVITY {
			return newCLIError(
				ExitInvalidArgs,
				fmt.Errorf("--format %s applies only to "+
					"activity output", format),
			)
		}

		return nil

	default:
		return newCLIError(
			ExitInvalidArgs,
			fmt.Errorf("unknown list format %q: expected json, "+
				"table, expanded, or x", format),
		)
	}
}

// printWalletActivityTable writes the compact wallet activity table to stdout.
func printWalletActivityTable(resp *wavewalletrpc.ListResponse) error {
	return renderWalletActivityTable(os.Stdout, resp)
}

// printWalletActivityExpanded writes the expanded wallet activity view to
// stdout.
func printWalletActivityExpanded(resp *wavewalletrpc.ListResponse) error {
	return renderWalletActivityExpanded(os.Stdout, resp)
}

// printWalletActivityNextPage writes a next-page hint to stdout when the
// activity feed has more entries. The human-facing table and expanded views
// omit the raw cursor otherwise, so without this line a caller has no way to
// discover the token needed to reach page two.
func printWalletActivityNextPage(resp *wavewalletrpc.ListResponse) error {
	activity := resp.GetActivity()
	if !activity.GetHasMore() {
		return nil
	}

	_, err := fmt.Fprintf(
		os.Stdout, "\nmore entries available; next page: --cursor %s\n",
		activity.GetNextCursor(),
	)

	return err
}

// renderWalletActivityTable renders activity entries as a tabwriter table.
func renderWalletActivityTable(out io.Writer,
	resp *wavewalletrpc.ListResponse) error {

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	fmt.Fprintln(
		w,
		strings.Join(
			[]string{
				"LAST UPDATE",
				"KIND",
				"STATUS",
				"AMOUNT",
				"FEE",
				"PHASE",
				"ID",
				"NOTE",
			}, "	",
		),
	)

	format := strings.Join([]string{
		"%s",
		"%s",
		"%s",
		"%d",
		"%d",
		"%s",
		"%s",
		"%s\n",
	}, "\t")

	for _, entry := range resp.GetActivity().GetEntries() {
		fmt.Fprintf(
			w, format,
			formatEntryTime(
				entry.GetUpdatedAtUnix(),
			),
			formatEntryKind(
				entry.GetKind(),
			),
			formatEntryStatus(
				entry.GetStatus(),
			),
			entry.GetAmountSat(),
			entry.GetFeeSat(),
			formatEntryPhase(
				entry.GetProgress(),
			),
			entry.GetId(),
			entry.GetNote(),
		)
	}

	if err := w.Flush(); err != nil {
		return err
	}

	// Detail lines terminate tabwriter alignment blocks. Write them only
	// after the complete table, labelling each with its activity ID.
	var details strings.Builder
	for _, entry := range resp.GetActivity().GetEntries() {
		if entry.GetStatus() !=
			wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING {

			continue
		}
		progress := entry.GetProgress()
		if progress.GetWaitingReason() == "" &&
			progress.GetBatchWindow() == nil {

			continue
		}
		fmt.Fprintf(&details, "\nBatch wait for %s:\n", entry.GetId())
		renderActivityBatchWait(&details, entry)
	}

	_, err := io.WriteString(out, details.String())

	return err
}

// renderWalletActivityExpanded renders activity entries as markdown-like
// sections.
func renderWalletActivityExpanded(out io.Writer,
	resp *wavewalletrpc.ListResponse) error {

	entries := resp.GetActivity().GetEntries()
	for i, entry := range entries {
		if i > 0 {
			fmt.Fprintln(out)
		}

		title := "Activity"
		if len(entries) > 1 {
			title = fmt.Sprintf("Activity %d", i+1)
		}
		renderActivitySectionWithTitle(out, title, entry)
	}

	return nil
}

// formatEntryTime returns the CLI timestamp label for a unix timestamp.
func formatEntryTime(ts int64) string {
	if ts == 0 {
		return "-"
	}

	return time.Unix(ts, 0).Format("2006-01-02 15:04:05.000")
}

// formatEntryKind returns the short activity kind label.
func formatEntryKind(kind wavewalletrpc.EntryKind) string {
	return strings.TrimPrefix(kind.String(), "ENTRY_KIND_")
}

// formatEntryStatus returns the short activity status label.
func formatEntryStatus(status wavewalletrpc.EntryStatus) string {
	return strings.TrimPrefix(status.String(), "ENTRY_STATUS_")
}

// formatEntryPhase returns the most display-friendly lifecycle phase label.
func formatEntryPhase(progress *wavewalletrpc.WalletEntryProgress) string {
	if progress == nil {
		return "-"
	}
	if progress.GetPhaseLabel() != "" {
		return progress.GetPhaseLabel()
	}

	return strings.TrimPrefix(
		progress.GetPhase().String(),
		"WALLET_ENTRY_PHASE_",
	)
}

// emptyDash renders an empty string as the CLI placeholder.
func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}

	return value
}

// formatSat renders a signed satoshi amount with its unit.
func formatSat(value int64) string {
	return fmt.Sprintf("%d sat", value)
}

// renderActivityBatchWait keeps scheduled progress next to its pending row
// without widening every activity column. Explicit UTC labels avoid mixing
// operator timestamps with the table's local last-update timestamp.
func renderActivityBatchWait(out io.Writer, entry *wavewalletrpc.WalletEntry) {
	if entry.GetStatus() != wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING {
		return
	}

	progress := entry.GetProgress()
	if reason := progress.GetWaitingReason(); reason != "" {
		fmt.Fprintf(out, "  Waiting reason: %s\n", reason)
	}
	window := progress.GetBatchWindow()
	if window == nil {
		return
	}

	opens := formatBatchTime(window.GetRegistrationOpensUnix() * 1000)
	cutoff := formatBatchTime(window.GetCutoffUnix() * 1000)
	fmt.Fprintf(
		out, "  Registration window (UTC): %s to %s\n", opens, cutoff,
	)
	fmt.Fprintf(
		out, "  Planned join (UTC): %s\n",
		formatBatchTime(
			window.GetPlannedJoinUnixMs(),
		),
	)
}

// formatBatchTime renders an operator-clock timestamp with millisecond
// precision. Zero means unknown, not the Unix epoch.
func formatBatchTime(unixMs int64) string {
	if unixMs == 0 {
		return "-"
	}

	return time.UnixMilli(unixMs).UTC().Format("2006-01-02 15:04:05.000")
}
