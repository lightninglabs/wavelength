//go:build itest

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/spf13/cobra"
)

// defaultLogLines is the number of trailing lines shown by arktest logs.
const defaultLogLines = 80

// logTarget names a component log that arktest can locate from the persisted
// harness state.
type logTarget struct {
	Name string
	Path string
}

// newLogsCmd creates the logs subcommand for opening component logs from the
// current arktest artifact directory.
func newLogsCmd() *cobra.Command {
	var follow bool
	var lines int

	cmd := &cobra.Command{
		Use:   "logs [component]",
		Short: "Show a component log from the current arktest run",
		Long: "Shows a component log from the current arktest run. " +
			"Use names like operator, bitcoind, lnd, alice, " +
			"alice-lnd, or client05. With no component, prints " +
			"the known log targets.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := loadState()
			if err != nil {
				return err
			}

			targets := logTargets(state)
			if len(args) == 0 {
				printLogTargets(cmd.OutOrStdout(), targets)

				return nil
			}

			target, ok := findLogTarget(targets, args[0])
			if !ok {
				return fmt.Errorf("unknown log target %q (run "+
					"`arktest logs`)", args[0])
			}

			return tailLog(target.Path, lines, follow)
		},
	}

	cmd.Flags().BoolVarP(
		&follow, "follow", "f", false,
		"follow appended log output",
	)
	cmd.Flags().IntVarP(
		&lines, "lines", "n", defaultLogLines,
		"number of trailing lines to show",
	)

	return cmd
}

// logTargets returns the log targets known for the current persisted topology.
func logTargets(state *harnessState) []logTarget {
	arkdLog := filepath.Join(state.RunDir, "arkd", "arkd.log")
	bitcoindLog := filepath.Join(
		state.RunDir, "bitcoind", "regtest", "debug.log",
	)
	operatorLNDLog := lndLogPath(filepath.Join(state.RunDir, "lnd"))

	targets := []logTarget{
		{
			Name: "events",
			Path: filepath.Join(state.RunDir, defaultEventLogName),
		},
		{
			Name: "harness",
			Path: filepath.Join(state.RunDir, "harness.log"),
		},
		{
			Name: "operator",
			Path: arkdLog,
		},
		{
			Name: "arkd",
			Path: arkdLog,
		},
		{
			Name: "bitcoind",
			Path: bitcoindLog,
		},
		{
			Name: "lnd",
			Path: operatorLNDLog,
		},
		{
			Name: "operator-lnd",
			Path: operatorLNDLog,
		},
	}

	names := make([]string, 0, len(state.Clients))
	for name := range state.Clients {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		client := state.Clients[name]
		targets = append(targets,
			logTarget{
				Name: name,
				Path: clientLogPath(client),
			},
			logTarget{
				Name: name + "-ark",
				Path: clientLogPath(client),
			},
			logTarget{
				Name: name + "-darepod",
				Path: clientLogPath(client),
			},
		)

		lnd, ok := state.ClientLNDs[name]
		if !ok || lnd == nil {
			continue
		}

		targets = append(targets, logTarget{
			Name: name + "-lnd",
			Path: lndLogPath(lnd.DataDir),
		})
	}

	return targets
}

// clientLogPath returns the standard darepod log path for a client state.
func clientLogPath(client *arkClientState) string {
	return filepath.Join(client.DataDir, "darepod.log")
}

// lndLogPath returns the standard regtest lnd log path for a data directory.
func lndLogPath(dataDir string) string {
	return filepath.Join(dataDir, "logs", "bitcoin", "regtest", "lnd.log")
}

// printLogTargets prints the known component names and their log paths.
func printLogTargets(out io.Writer, targets []logTarget) {
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Name < targets[j].Name
	})

	for _, target := range targets {
		fmt.Fprintf(out, "%-18s %s\n", target.Name, target.Path)
	}
}

// findLogTarget returns the target for a component name.
func findLogTarget(targets []logTarget, name string) (logTarget, bool) {
	for _, target := range targets {
		if target.Name == name {
			return target, true
		}
	}

	return logTarget{}, false
}

// tailLog shells out to tail so follow mode behaves like a normal developer
// log tail without reimplementing file polling.
func tailLog(path string, lines int, follow bool) error {
	if lines <= 0 {
		return fmt.Errorf("lines must be positive")
	}

	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("stat log %s: %w", path, err)
	}

	args := []string{"-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, path)

	cmd := exec.Command("tail", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
