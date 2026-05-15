//go:build itest

package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRootCommandIncludesFaucet verifies the generic faucet is discoverable
// from the arktest root command.
func TestRootCommandIncludesFaucet(t *testing.T) {
	defer func() {
		stressCfg = stressConfig{}
	}()

	cmd := newRootCmd()

	found, _, err := cmd.Find([]string{"faucet"})
	require.NoError(t, err)
	require.NotNil(t, found)
	require.Equal(t, "faucet", found.Name())
}

// TestFaucetCommandRejectsNonPositiveAmount verifies invalid amounts fail
// before the command tries to contact the persisted arktest topology.
func TestFaucetCommandRejectsNonPositiveAmount(t *testing.T) {
	cmd := newFaucetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"bcrt1qexample", "0"})

	err := cmd.Execute()
	require.ErrorContains(t, err, "amount must be positive")
}

// TestFaucetCommandRejectsInvalidAmount verifies amount parsing errors stay
// attached to the faucet command instead of leaking from bitcoind RPC setup.
func TestFaucetCommandRejectsInvalidAmount(t *testing.T) {
	cmd := newFaucetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"bcrt1qexample", "abc"})

	err := cmd.Execute()
	require.ErrorContains(t, err, "parse amount")
}
