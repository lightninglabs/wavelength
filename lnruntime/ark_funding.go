package lnruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/channeldb"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
)

// RestoreBacking registers one durable Ark backing transaction before the
// native funding manager restores pending channels from lnd's database.
func (f *FundingRuntime) RestoreBacking(terms arkchannel.Terms,
	backing arkchannel.Backing) error {

	funding, err := virtualFundingFromBacking(terms, backing)
	if err != nil {
		return err
	}

	return f.RegisterBacking(funding)
}

// FundingFinalized reports whether lnd's database contains the exact channel
// described by durable Ark terms and backing.
func (f *FundingRuntime) FundingFinalized(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	select {
	case <-ctx.Done():
		return false, ctx.Err()

	default:
	}

	funding, err := virtualFundingFromBacking(terms, backing)
	if err != nil {
		return false, err
	}
	channel, err := f.stateDB.FetchChannel(backing.ChannelPoint)
	if errors.Is(err, channeldb.ErrChannelNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find finalized lnd channel: %w", err)
	}
	if channel.FundingOutpoint != backing.ChannelPoint ||
		channel.Capacity != terms.Capacity {
		return false, fmt.Errorf("finalized lnd channel does not " +
			"match Ark backing")
	}

	localParty, remoteKey, err := f.channelParties(terms)
	if err != nil {
		return false, err
	}
	if !channel.IdentityPub.IsEqual(remoteKey) {
		return false, fmt.Errorf("finalized lnd channel peer does " +
			"not match Ark terms")
	}
	expectedInitiator := terms.FundingInitiator() == localParty
	if channel.IsInitiator != expectedInitiator {
		return false, fmt.Errorf("finalized lnd channel funder does " +
			"not match Ark terms")
	}
	clientBalance := channel.LocalCommitment.RemoteBalance.ToSatoshis()
	if localParty == arkchannel.PartyClient {
		clientBalance = channel.LocalCommitment.LocalBalance.
			ToSatoshis()
	}
	if terms.Kind == arkchannel.KindReceiveIntent &&
		clientBalance != 0 {
		return false, fmt.Errorf("client received spendable initial " +
			"channel liquidity")
	}

	fundingScript, err := lndfunding.MakeFundingScript(channel)
	if err != nil {
		return false, fmt.Errorf("derive finalized funding script: %w",
			err)
	}
	output := funding.Transaction.TxOut[funding.OutputIndex]
	if !bytes.Equal(output.PkScript, fundingScript) {
		return false, fmt.Errorf("signed backing does not fund lnd's " +
			"negotiated channel script")
	}

	return true, nil
}

// ChannelActive reports whether the exact finalized channel has left lnd's
// pending-open state and its reserved-SCID link can carry payments.
func (f *FundingRuntime) ChannelActive(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	finalized, err := f.FundingFinalized(ctx, terms, backing)
	if err != nil || !finalized {
		return false, err
	}
	channel, err := f.stateDB.FetchChannel(backing.ChannelPoint)
	if errors.Is(err, channeldb.ErrChannelNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find active lnd channel: %w", err)
	}
	if channel.IsPending {
		return false, nil
	}
	_, err = f.switcher.GetLinkByShortID(
		lnwire.NewShortChanIDFromInt(terms.ReservedSCID),
	)
	if errors.Is(err, htlcswitch.ErrChannelLinkNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find active lnd channel link: %w",
			err)
	}

	return true, nil
}

// channelParties resolves this runtime's role and expected remote identity.
func (f *FundingRuntime) channelParties(terms arkchannel.Terms) (
	arkchannel.Party, *btcec.PublicKey, error) {

	clientKey, err := btcec.ParsePubKey(terms.ClientNodeKey[:])
	if err != nil {
		return 0, nil, fmt.Errorf("parse client node key: %w", err)
	}
	hubKey, err := btcec.ParsePubKey(terms.HubNodeKey[:])
	if err != nil {
		return 0, nil, fmt.Errorf("parse hub node key: %w", err)
	}
	switch {
	case bytes.Equal(f.identityKey[:], terms.ClientNodeKey[:]):
		return arkchannel.PartyClient, hubKey, nil

	case bytes.Equal(f.identityKey[:], terms.HubNodeKey[:]):
		return arkchannel.PartyHub, clientKey, nil

	default:
		return 0, nil, fmt.Errorf("runtime identity is not an Ark " +
			"channel party")
	}
}

// virtualFundingFromBacking converts durable Ark facts into the in-memory
// virtual confirmation record expected by lnd.
func virtualFundingFromBacking(terms arkchannel.Terms,
	backing arkchannel.Backing) (VirtualFunding, error) {

	if err := terms.Validate(); err != nil {
		return VirtualFunding{}, err
	}
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(
		bytes.NewReader(backing.Transaction),
	); err != nil {
		return VirtualFunding{}, fmt.Errorf("decode Ark backing: %w",
			err)
	}
	if tx.TxHash() != backing.ChannelPoint.Hash {
		return VirtualFunding{}, fmt.Errorf("Ark backing transaction " +
			"ID does not match channel point")
	}
	if backing.ChannelPoint.Index >= uint32(len(tx.TxOut)) {
		return VirtualFunding{}, fmt.Errorf("Ark backing output is " +
			"out of range")
	}
	if tx.TxOut[backing.ChannelPoint.Index].Value != int64(terms.Capacity) {
		return VirtualFunding{}, fmt.Errorf("Ark backing capacity " +
			"does not match channel terms")
	}
	scid := lnwire.NewShortChanIDFromInt(terms.ReservedSCID)
	if scid.TxPosition != uint16(backing.ChannelPoint.Index) {
		return VirtualFunding{}, fmt.Errorf("reserved SCID does not " +
			"match channel point output")
	}

	return VirtualFunding{
		Transaction: tx,
		OutputIndex: backing.ChannelPoint.Index,
		SCID:        scid,
	}, nil
}

var _ arkchannel.FundingFinalizationSource = (*FundingRuntime)(nil)
