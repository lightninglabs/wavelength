package lnruntime

import (
	"bytes"
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelIntentSource resolves the durable Ark intent named by lnd's pending
// channel ID.
type ChannelIntentSource interface {
	FindByPendingChannelID(context.Context,
		[32]byte) (arkchannel.Record, error)
}

type channelAcceptResponse = chanacceptor.ChannelAcceptResponse

// IntentAcceptor admits only inbound lnd funding messages authorized by a
// source-bound Ark channel intent.
type IntentAcceptor struct {
	localParty arkchannel.Party
	intents    ChannelIntentSource
}

// NewIntentAcceptor constructs an intent-backed native lnd channel acceptor.
func NewIntentAcceptor(localParty arkchannel.Party,
	intents ChannelIntentSource) (*IntentAcceptor, error) {

	if localParty != arkchannel.PartyClient &&
		localParty != arkchannel.PartyHub {
		return nil, fmt.Errorf("local channel party is required")
	}
	if intents == nil {
		return nil, fmt.Errorf("channel intent source is required")
	}

	return &IntentAcceptor{
		localParty: localParty,
		intents:    intents,
	}, nil
}

// Accept validates an incoming single-funder channel against the durable Ark
// intent before lnd allocates any responder state.
func (a *IntentAcceptor) Accept(
	req *chanacceptor.ChannelAcceptRequest) *channelAcceptResponse {

	err := a.validate(req)
	if err != nil {
		return chanacceptor.NewChannelAcceptResponse(
			false, err, nil, 0, 0, 0, 0, 0, 0, false,
		)
	}

	return chanacceptor.NewChannelAcceptResponse(
		true, nil, nil, 0, 0, 1, 0, 0, 0, false,
	)
}

// validate checks the cross-system facts lnd cannot infer by itself.
func (a *IntentAcceptor) validate(
	req *chanacceptor.ChannelAcceptRequest) error {

	_, err := a.terms(req)

	return err
}

// terms validates an inbound request and returns its durable channel terms.
func (a *IntentAcceptor) terms(req *chanacceptor.ChannelAcceptRequest) (
	arkchannel.Terms, error) {

	if req == nil || req.Node == nil || req.OpenChanMsg == nil {
		return arkchannel.Terms{}, fmt.Errorf("complete channel " +
			"request is required")
	}
	open := req.OpenChanMsg
	record, err := a.intents.FindByPendingChannelID(
		context.Background(), open.PendingChannelID,
	)
	if err != nil {
		return arkchannel.Terms{}, fmt.Errorf("find channel intent: %w",
			err)
	}
	snapshot := record.Snapshot
	if snapshot.Source == nil {
		return arkchannel.Terms{}, fmt.Errorf("channel intent has no " +
			"bound VTXO")
	}
	if snapshot.Phase < arkchannel.PhaseNegotiating ||
		snapshot.Phase > arkchannel.PhaseBackingReady {
		return arkchannel.Terms{}, fmt.Errorf("channel intent phase "+
			"%s cannot accept funding", snapshot.Phase)
	}
	terms := snapshot.Terms
	if terms.FundingInitiator() == a.localParty {
		return arkchannel.Terms{}, fmt.Errorf("local opener must " +
			"initiate the channel")
	}

	var expectedPeer [33]byte
	switch terms.FundingInitiator() {
	case arkchannel.PartyClient:
		expectedPeer = terms.ClientNodeKey

	case arkchannel.PartyHub:
		expectedPeer = terms.HubNodeKey

	default:
		return arkchannel.Terms{}, fmt.Errorf("unknown channel "+
			"opener %d", terms.FundingInitiator())
	}
	if !bytes.Equal(
		req.Node.SerializeCompressed(), expectedPeer[:],
	) {
		return arkchannel.Terms{}, fmt.Errorf("channel initiator " +
			"does not match intent")
	}
	if open.FundingAmount != terms.Capacity {
		return arkchannel.Terms{}, fmt.Errorf("channel capacity %d "+
			"does not match intent %d", open.FundingAmount,
			terms.Capacity)
	}
	expectedPush := lnwire.NewMSatFromSatoshis(terms.InitialPushAmount())
	if open.PushAmount != expectedPush {
		return arkchannel.Terms{}, fmt.Errorf("channel push amount %d "+
			"does not match intent %d", open.PushAmount,
			expectedPush)
	}
	if open.ChannelFlags&lnwire.FFAnnounceChannel != 0 {
		return arkchannel.Terms{}, fmt.Errorf("Ark channel must be " +
			"private")
	}

	return terms, nil
}

var _ chanacceptor.ChannelAcceptor = (*IntentAcceptor)(nil)
