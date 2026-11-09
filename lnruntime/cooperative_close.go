package lnruntime

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
)

// mapCleanBalances converts a runtime-relative view into stable Ark roles.
func mapCleanBalances(party arkchannel.Party,
	state CleanChannelState) (btcutil.Amount, btcutil.Amount) {

	if party == arkchannel.PartyClient {
		return state.LocalBalance, state.RemoteBalance
	}

	return state.RemoteBalance, state.LocalBalance
}

// validateCooperativeCloseState rejects a signed settlement whose role-stable
// balance split no longer matches the endpoint's clean lnd commitment.
func validateCooperativeCloseState(party arkchannel.Party,
	state CleanChannelState,
	proposal arkchannel.CooperativeCloseProposal) error {

	clientBalance, hubBalance := mapCleanBalances(party, state)
	if proposal.CommitmentHeight != state.CommitmentHeight ||
		proposal.ClientBalance != clientBalance ||
		proposal.HubBalance != hubBalance {
		return fmt.Errorf("cooperative close proposal does not match " +
			"local clean lnd state")
	}

	return nil
}

// otherChannelParty returns the only counterparty role.
func otherChannelParty(party arkchannel.Party) arkchannel.Party {
	if party == arkchannel.PartyClient {
		return arkchannel.PartyHub
	}

	return arkchannel.PartyClient
}
