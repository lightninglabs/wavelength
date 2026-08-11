package lnruntime

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwire"
)

// VirtualFunding identifies an unpublished Lightning funding transaction and
// the stable SCID lnd should use after its Ark backing round confirms.
type VirtualFunding struct {
	Transaction *wire.MsgTx
	OutputIndex uint32
	SCID        lnwire.ShortChannelID
}

// virtualFundingRecord stores one immutable backing transaction and all lnd
// confirmation subscriptions waiting for Ark activation.
type virtualFundingRecord struct {
	funding       VirtualFunding
	confirmed     bool
	registrations map[uint64]*chainntnfs.ConfirmationEvent
}

// VirtualFundingNotifier delegates normal chain events while treating a
// fully signed, round-confirmed Ark backing transaction as confirmed for lnd's
// channel lifecycle. RegisterVirtualFunding must run before funding.Manager
// starts watching the channel point.
type VirtualFundingNotifier struct {
	chainntnfs.ChainNotifier

	mu             sync.Mutex
	nextID         uint64
	virtualFunding map[chainhash.Hash]*virtualFundingRecord
}

// NewVirtualFundingNotifier wraps an ordinary notifier with Ark channel
// activation semantics.
func NewVirtualFundingNotifier(chainNotifier chainntnfs.ChainNotifier) (
	*VirtualFundingNotifier, error) {

	if chainNotifier == nil {
		return nil, fmt.Errorf("chain notifier is required")
	}

	return &VirtualFundingNotifier{
		ChainNotifier: chainNotifier,
		virtualFunding: make(
			map[chainhash.Hash]*virtualFundingRecord,
		),
	}, nil
}

// RegisterVirtualFunding installs an immutable backing transaction before lnd
// registers its confirmation subscription. Re-registration is idempotent only
// when every funding detail is identical.
func (n *VirtualFundingNotifier) RegisterVirtualFunding(
	funding VirtualFunding) error {

	if funding.Transaction == nil {
		return fmt.Errorf("virtual funding transaction is required")
	}
	if len(funding.Transaction.TxIn) == 0 {
		return fmt.Errorf("virtual funding transaction has no inputs")
	}
	for index, txIn := range funding.Transaction.TxIn {
		if len(txIn.Witness) == 0 && len(txIn.SignatureScript) == 0 {
			return fmt.Errorf("virtual funding input %d is "+
				"not signed", index)
		}
	}
	if funding.OutputIndex >= uint32(len(funding.Transaction.TxOut)) {
		return fmt.Errorf("virtual funding output %d is out of range",
			funding.OutputIndex)
	}
	if funding.SCID.TxPosition != uint16(funding.OutputIndex) {
		return fmt.Errorf("virtual SCID output %d does not match "+
			"funding output %d", funding.SCID.TxPosition,
			funding.OutputIndex)
	}
	if funding.SCID.BlockHeight == 0 {
		return fmt.Errorf("virtual funding SCID height is required")
	}

	tx := funding.Transaction.Copy()
	txid := tx.TxHash()
	funding.Transaction = tx

	n.mu.Lock()
	defer n.mu.Unlock()

	existing, ok := n.virtualFunding[txid]
	if ok {
		return sameVirtualFunding(existing.funding, funding)
	}

	n.virtualFunding[txid] = &virtualFundingRecord{
		funding:       funding,
		registrations: make(map[uint64]*chainntnfs.ConfirmationEvent),
	}

	return nil
}

// RegisterConfirmationsNtfn intercepts only registered virtual funding txids.
// Every other confirmation remains owned by the underlying chain notifier.
func (n *VirtualFundingNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	if txid == nil {
		return n.ChainNotifier.RegisterConfirmationsNtfn(
			txid, pkScript, numConfs, heightHint, opts...,
		)
	}

	n.mu.Lock()
	record, ok := n.virtualFunding[*txid]
	if !ok {
		n.mu.Unlock()

		return n.ChainNotifier.RegisterConfirmationsNtfn(
			txid, pkScript, numConfs, heightHint, opts...,
		)
	}

	fundingOutput := record.funding.Transaction.TxOut[record.
		funding.
		OutputIndex]
	if !bytes.Equal(fundingOutput.PkScript, pkScript) {
		n.mu.Unlock()

		return nil, fmt.Errorf("virtual funding script does not " +
			"match registered channel output")
	}

	n.nextID++
	registrationID := n.nextID
	event := chainntnfs.NewConfirmationEvent(numConfs, func() {
		n.cancelRegistration(*txid, registrationID)
	})
	record.registrations[registrationID] = event
	confirmed := record.confirmed
	funding := record.funding
	n.mu.Unlock()

	if confirmed {
		n.notifyConfirmed(event, funding)
	}

	return event, nil
}

// ConfirmVirtualFunding activates lnd's ordinary funding-confirmation path.
// The Ark FSM must call this only after both the backing signatures and the
// containing round confirmation are durable.
func (n *VirtualFundingNotifier) ConfirmVirtualFunding(
	txid chainhash.Hash) error {

	n.mu.Lock()
	record, ok := n.virtualFunding[txid]
	if !ok {
		n.mu.Unlock()

		return fmt.Errorf("virtual funding transaction %s is not "+
			"registered", txid)
	}
	if record.confirmed {
		n.mu.Unlock()

		return nil
	}
	record.confirmed = true
	funding := record.funding
	events := make(
		[]*chainntnfs.ConfirmationEvent, 0, len(record.registrations),
	)
	for _, event := range record.registrations {
		events = append(events, event)
	}
	n.mu.Unlock()

	for _, event := range events {
		n.notifyConfirmed(event, funding)
	}

	return nil
}

// ReorgVirtualFunding retracts a prior virtual confirmation when the Ark
// backing round is reorged. A later ConfirmVirtualFunding call can reactivate
// the same subscriptions.
func (n *VirtualFundingNotifier) ReorgVirtualFunding(txid chainhash.Hash,
	depth int32) error {

	n.mu.Lock()
	record, ok := n.virtualFunding[txid]
	if !ok {
		n.mu.Unlock()

		return fmt.Errorf("virtual funding transaction %s is not "+
			"registered", txid)
	}
	if !record.confirmed {
		n.mu.Unlock()

		return nil
	}
	record.confirmed = false
	events := make(
		[]*chainntnfs.ConfirmationEvent, 0, len(record.registrations),
	)
	for _, event := range record.registrations {
		events = append(events, event)
	}
	n.mu.Unlock()

	for _, event := range events {
		event.NegativeConf <- depth
	}

	return nil
}

// cancelRegistration removes a virtual confirmation subscription.
func (n *VirtualFundingNotifier) cancelRegistration(txid chainhash.Hash,
	registrationID uint64) {

	n.mu.Lock()
	defer n.mu.Unlock()

	record, ok := n.virtualFunding[txid]
	if !ok {
		return
	}
	delete(record.registrations, registrationID)
}

// notifyConfirmed reports the reserved SCID coordinates while attaching the
// actual backing transaction so lnd can validate its channel output.
func (n *VirtualFundingNotifier) notifyConfirmed(
	event *chainntnfs.ConfirmationEvent, funding VirtualFunding) {

	height := funding.SCID.BlockHeight
	if cap(event.Updates) > 0 {
		event.Updates <- chainntnfs.TxUpdateInfo{
			BlockHeight:  height,
			NumConfsLeft: 0,
		}
	}
	event.Confirmed <- &chainntnfs.TxConfirmation{
		BlockHash:   &chainhash.Hash{},
		BlockHeight: height,
		TxIndex:     funding.SCID.TxIndex,
		Tx:          funding.Transaction.Copy(),
	}
}

// sameVirtualFunding checks idempotent registration without retaining a
// caller-owned mutable transaction pointer.
func sameVirtualFunding(a, b VirtualFunding) error {
	if a.OutputIndex != b.OutputIndex || a.SCID != b.SCID {
		return fmt.Errorf("virtual funding transaction already " +
			"registered with different channel coordinates")
	}

	var aBytes, bBytes bytes.Buffer
	if err := a.Transaction.Serialize(&aBytes); err != nil {
		return fmt.Errorf("serialize registered funding "+
			"transaction: %w", err)
	}
	if err := b.Transaction.Serialize(&bBytes); err != nil {
		return fmt.Errorf("serialize repeated funding transaction: %w",
			err)
	}
	if !bytes.Equal(aBytes.Bytes(), bBytes.Bytes()) {
		return fmt.Errorf("virtual funding transaction already " +
			"registered with different bytes")
	}

	return nil
}

var _ chainntnfs.ChainNotifier = (*VirtualFundingNotifier)(nil)
