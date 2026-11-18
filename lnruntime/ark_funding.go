package lnruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/channeldb"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
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

// validateFundingCreated prevents a peer from attaching its pending channel
// to another peer's outpoint before lnd records that point in its peer/global
// pending indexes. Only an installed signed backing paying this reservation
// can advance the native responder.
func (f *FundingRuntime) validateFundingCreated(
	message *lnwire.FundingCreated) error {

	f.notifier.mu.Lock()
	record := f.notifier.virtualFunding[message.FundingPoint.Hash]
	if record == nil || record.canceled ||
		record.funding.OutputIndex != message.FundingPoint.Index {

		f.notifier.mu.Unlock()

		return fmt.Errorf("funding_created has no installed signed " +
			"backing")
	}
	output := record.funding.Transaction.TxOut[record.funding.OutputIndex]
	f.notifier.mu.Unlock()
	expected, err := f.ExpectedFundingOutput(message.PendingChannelID)
	if err != nil {
		return err
	}
	if output.Value != expected.Value ||
		!bytes.Equal(output.PkScript, expected.PkScript) {
		return fmt.Errorf("funding_created does not fund this peer's " +
			"reservation")
	}

	return nil
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
	expectedType := nativeChannelDBType(channel.IsInitiator)
	if channel.ChanType != expectedType {
		return false, fmt.Errorf("finalized lnd channel has "+
			"commitment type %v, expected %v", channel.ChanType,
			expectedType)
	}
	if err := validateNativeChannelConfig(
		"local", channel.LocalChanCfg, terms.Capacity,
	); err != nil {
		return false, err
	}
	if err := validateNativeChannelConfig(
		"remote", channel.RemoteChanCfg, terms.Capacity,
	); err != nil {
		return false, err
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
	// Balances may move after activation. Only the opening commitment
	// proves that the non-funder received no initial allocation.
	if channel.LocalCommitment.CommitHeight == 0 {
		balance := channel.LocalCommitment.LocalBalance
		if expectedInitiator {
			balance = channel.LocalCommitment.RemoteBalance
		}
		if balance != 0 {
			return false, fmt.Errorf("non-funder received " +
				"initial channel liquidity")
		}
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
	link, err := f.switcher.GetLinkByShortID(
		lnwire.NewShortChanIDFromInt(terms.ReservedSCID),
	)
	if errors.Is(err, htlcswitch.ErrChannelLinkNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find active lnd channel link: %w",
			err)
	}
	if link.ChanID() != lnwire.NewChanIDFromOutPoint(backing.ChannelPoint) {
		return false, fmt.Errorf("reserved SCID resolves to another " +
			"channel")
	}
	if !link.EligibleToForward() {
		return false, nil
	}

	return true, nil
}

// validateNativeChannelConfig verifies lnd persisted every security-relevant
// commitment bound negotiated by the native runtime.
func validateNativeChannelConfig(side string, cfg channeldb.ChannelConfig,
	capacity btcutil.Amount) error {

	expectedDust := lnwallet.DustLimitUnknownWitness()
	expectedReserve := defaultRemoteReserve(capacity, expectedDust)
	expectedMaxValue := defaultRemoteMaxValue(capacity)
	switch {
	case cfg.CsvDelay != nativeChannelCSVDelay:
		return fmt.Errorf("finalized lnd %s CSV delay %d does not "+
			"match %d", side, cfg.CsvDelay, nativeChannelCSVDelay)

	case cfg.DustLimit != expectedDust:
		return fmt.Errorf("finalized lnd %s dust limit %d does not "+
			"match %d", side, cfg.DustLimit, expectedDust)

	case cfg.ChanReserve != expectedReserve:
		return fmt.Errorf("finalized lnd %s reserve %d does not "+
			"match %d", side, cfg.ChanReserve, expectedReserve)

	case cfg.MaxPendingAmount != expectedMaxValue:
		return fmt.Errorf("finalized lnd %s in-flight limit %d does "+
			"not match %d", side, cfg.MaxPendingAmount,
			expectedMaxValue)

	case cfg.MinHTLC != nativeChannelMinHTLC:
		return fmt.Errorf("finalized lnd %s minimum HTLC %d does not "+
			"match %d", side, cfg.MinHTLC, nativeChannelMinHTLC)

	case cfg.MaxAcceptedHtlcs != nativeChannelMaxHTLCs:
		return fmt.Errorf("finalized lnd %s HTLC limit %d does not "+
			"match %d", side, cfg.MaxAcceptedHtlcs,
			nativeChannelMaxHTLCs)

	default:
		return nil
	}
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
