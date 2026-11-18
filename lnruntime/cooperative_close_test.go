package lnruntime

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestExternalCloseBalances verifies role symmetry and exact conservation for
// every possible sub-satoshi split, with and without commitment anchors.
func TestExternalCloseBalances(t *testing.T) {
	t.Parallel()

	for _, typ := range []channeldb.ChannelType{
		0,
		channeldb.AnchorOutputsBit,
	} {
		var s channeldb.ChannelSnapshot
		s.CommitFee = 7
		s.Capacity = 100 + s.CommitFee
		if typ.HasAnchors() {
			s.Capacity += 2 * lnwallet.AnchorSize
		}
		for i := range lnwire.NewMSatFromSatoshis(1) {
			s.LocalBalance = lnwire.NewMSatFromSatoshis(60) + i
			s.RemoteBalance = lnwire.NewMSatFromSatoshis(40) - i
			local, remote, err := externalCloseBalances(
				typ, true, &s,
			)
			require.NoError(t, err)
			require.Equal(t, s.RemoteBalance.ToSatoshis(), remote)
			require.Equal(t, s.Capacity-remote, local)

			s.LocalBalance, s.RemoteBalance =
				s.RemoteBalance, s.LocalBalance
			local2, remote2, err := externalCloseBalances(
				typ, false, &s,
			)
			require.NoError(t, err)
			require.Equal(t, remote, local2)
			require.Equal(t, local, remote2)
		}
		// Neither fractional nor whole-satoshi discrepancies are fees.
		s.LocalBalance++
		_, _, err := externalCloseBalances(typ, true, &s)
		require.Error(t, err)
		s.LocalBalance--
		s.Capacity++
		_, _, err = externalCloseBalances(typ, true, &s)
		require.Error(t, err)
	}
}

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
