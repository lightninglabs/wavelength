package lnruntime

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/stretchr/testify/require"
)

// TestValidateCooperativeCloseState verifies a restart cannot settle a signed
// balance split after lnd's clean commitment state has changed.
func TestValidateCooperativeCloseState(t *testing.T) {
	t.Parallel()

	state := CleanChannelState{
		CommitmentHeight: 7,
		LocalBalance:     btcutil.Amount(40_000),
		RemoteBalance:    btcutil.Amount(60_000),
	}
	proposal := arkchannel.CooperativeCloseProposal{
		CommitmentHeight: 7,
		ClientBalance:    btcutil.Amount(40_000),
		HubBalance:       btcutil.Amount(60_000),
	}
	require.NoError(
		t, validateCooperativeCloseState(
			arkchannel.PartyClient, state, proposal,
		),
	)

	staleHeight := proposal
	staleHeight.CommitmentHeight--
	require.ErrorContains(
		t, validateCooperativeCloseState(
			arkchannel.PartyClient, state, staleHeight,
		),
		"does not match local clean lnd state",
	)

	staleBalance := proposal
	staleBalance.ClientBalance--
	staleBalance.HubBalance++
	require.ErrorContains(
		t, validateCooperativeCloseState(
			arkchannel.PartyClient, state, staleBalance,
		),
		"does not match local clean lnd state",
	)

	hubProposal := proposal
	hubProposal.ClientBalance = state.RemoteBalance
	hubProposal.HubBalance = state.LocalBalance
	require.NoError(
		t, validateCooperativeCloseState(
			arkchannel.PartyHub, state, hubProposal,
		),
	)
}
