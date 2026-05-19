//go:build itest

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// defaultStopTimeout caps the graceful-shutdown wait before the
// caller has to opt in to a force-kill. 30s comfortably covers
// container teardown on a healthy laptop without making the command
// feel like it has frozen.
const defaultStopTimeout = 30 * time.Second

// stopPollInterval is how often we re-check whether the state file
// has been removed by the start process's shutdown deferred chain.
// 250ms is brisk enough to feel immediate in the common case but
// avoids hammering os.Stat once teardown is well underway.
const stopPollInterval = 250 * time.Millisecond

type stopConfig struct {
	timeout time.Duration
	force   bool
}

var stopCfg stopConfig

func newStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Signal a running arktest harness to shut down",
		Long: "Reads the PID stamped into the state file by " +
			"`arktest start`, sends SIGINT to let the harness " +
			"run its deferred shutdown chain (container " +
			"teardown, state-file cleanup), and waits until the " +
			"state file disappears. With --force, escalates to " +
			"SIGKILL if the harness does not exit within " +
			"--timeout.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStop()
		},
	}

	cmd.Flags().DurationVar(
		&stopCfg.timeout, "timeout", defaultStopTimeout,
		"how long to wait for graceful shutdown",
	)
	cmd.Flags().BoolVar(
		&stopCfg.force, "force", false,
		"escalate to SIGKILL if the harness does not exit within "+
			"--timeout",
	)

	return cmd
}

func runStop() error {
	state, err := loadState()
	if err != nil {
		return err
	}

	if state.Pid <= 0 {
		return fmt.Errorf("state file %s has no recorded pid; the "+
			"harness may have been started by an older build that "+
			"did not stamp one", state.StateFile)
	}

	proc, err := os.FindProcess(state.Pid)
	if err != nil {
		return fmt.Errorf("find arktest process pid=%d: %w", state.Pid,
			err)
	}

	// On Unix os.FindProcess always succeeds; verify the PID is
	// actually live by sending signal 0, which only delivers the
	// permission check.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		_ = deleteState()

		return fmt.Errorf("arktest pid=%d is not running (stale state "+
			"file removed): %w", state.Pid, err)
	}

	fmt.Fprintf(
		os.Stderr, "sending SIGINT to arktest pid=%d...\n", state.Pid,
	)
	if err := proc.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("signal SIGINT to pid=%d: %w", state.Pid, err)
	}

	if waitForExit(proc, stopCfg.timeout) {
		fmt.Fprintln(os.Stderr, "arktest stopped.")

		return nil
	}

	if !stopCfg.force {
		return fmt.Errorf("arktest pid=%d did not exit within %s; "+
			"re-run with --force to escalate to SIGKILL", state.Pid,
			stopCfg.timeout)
	}

	fmt.Fprintf(
		os.Stderr, "escalating to SIGKILL on pid=%d...\n", state.Pid,
	)
	if err := proc.Kill(); err != nil &&
		!errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill pid=%d: %w", state.Pid, err)
	}

	// After SIGKILL there is no shutdown handler to delete the state
	// file. Remove it ourselves so subsequent subcommands fail clean
	// with "is arktest start running?" instead of pointing at a stale
	// topology.
	if err := deleteState(); err != nil {
		return fmt.Errorf("remove stale state file: %w", err)
	}

	fmt.Fprintln(os.Stderr, "arktest killed.")

	return nil
}

// waitForExit polls the target process with signal 0 until it stops
// responding (i.e., has fully exited and the deferred container
// teardown chain has run), or the timeout elapses. The state file
// alone is not a reliable signal because start's deferred cleanup
// removes the file before docker teardown completes; the only honest
// "everything is gone" indicator is the process itself dying.
func waitForExit(proc *os.Process, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for {
		if err := proc.Signal(syscall.Signal(0)); err != nil {

			// Either ESRCH (gone) or "process already finished"
			// — both mean we're done.
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(stopPollInterval)
	}
}
