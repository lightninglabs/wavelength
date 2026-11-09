package lnruntime

import (
	"github.com/lightninglabs/wavelength/arkchannel"
)

// otherChannelParty returns the only counterparty role.
func otherChannelParty(party arkchannel.Party) arkchannel.Party {
	if party == arkchannel.PartyClient {
		return arkchannel.PartyHub
	}

	return arkchannel.PartyClient
}
