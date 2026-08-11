package arkchannel

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// ID is the stable identifier shared by both sides of an Ark channel.
type ID [32]byte

// Kind identifies how the VTXO backing a channel is obtained.
type Kind uint8

const (
	// KindPromotion upgrades an existing client-owned VTXO.
	KindPromotion Kind = iota + 1

	// KindReceiveIntent creates a hub-funded VTXO in an Ark round.
	KindReceiveIntent
)

// String returns the durable name of a channel kind.
func (k Kind) String() string {
	switch k {
	case KindPromotion:
		return "promotion"

	case KindReceiveIntent:
		return "receive_intent"

	default:
		return "unknown"
	}
}

// Party identifies one endpoint of the private Lightning channel.
type Party uint8

const (
	// PartyClient is the Wavelength endpoint.
	PartyClient Party = iota + 1

	// PartyHub is the swapdk-server endpoint.
	PartyHub
)

// String returns the durable name of a channel party.
func (p Party) String() string {
	switch p {
	case PartyClient:
		return "client"

	case PartyHub:
		return "hub"

	default:
		return "unknown"
	}
}

// Phase is the Ark coordination phase, not the Lightning channel state.
type Phase uint8

const (
	// PhaseRequested means immutable channel terms are registered.
	PhaseRequested Phase = iota + 1

	// PhaseNegotiating means the exact VTXO is bound and lnd funding is in
	// progress.
	PhaseNegotiating

	// PhaseBackingReady means the signed backing and both lnd finalization
	// acknowledgements are durable.
	PhaseBackingReady

	// PhaseAwaitingConfirmation means a receive round was committed and
	// must confirm before virtual activation.
	PhaseAwaitingConfirmation

	// PhaseActivating means lnd should observe the virtual channel point as
	// confirmed.
	PhaseActivating

	// PhaseActive means lnd reports the ordinary channel active.
	PhaseActive

	// PhaseMaterializing means the Ark ancestry and backing transaction are
	// being published.
	PhaseMaterializing

	// PhaseOnChain means the backing transaction was published.
	PhaseOnChain

	// PhaseClosed means lnd completed the channel lifecycle.
	PhaseClosed

	// PhaseFailed means negotiation failed before the no-failure boundary.
	PhaseFailed
)

// String returns the durable name of a coordination phase.
func (p Phase) String() string {
	switch p {
	case PhaseRequested:
		return "requested"

	case PhaseNegotiating:
		return "negotiating"

	case PhaseBackingReady:
		return "backing_ready"

	case PhaseAwaitingConfirmation:
		return "awaiting_confirmation"

	case PhaseActivating:
		return "activating"

	case PhaseActive:
		return "active"

	case PhaseMaterializing:
		return "materializing"

	case PhaseOnChain:
		return "on_chain"

	case PhaseClosed:
		return "closed"

	case PhaseFailed:
		return "failed"

	default:
		return "unknown"
	}
}

// IsTerminal reports whether Ark coordination has no further work.
func (p Phase) IsTerminal() bool {
	return p == PhaseClosed || p == PhaseFailed
}

// Terms are immutable facts shared by client and hub.
type Terms struct {
	ID               ID
	Kind             Kind
	Funder           Party
	PendingChannelID [32]byte
	ReservedSCID     uint64
	Capacity         btcutil.Amount
	ClientNodeKey    [33]byte
	HubNodeKey       [33]byte
	PaymentHash      [32]byte
	PolicyTemplate   []byte
	PkScript         []byte
}

// Clone returns terms without aliases to mutable byte slices.
func (t Terms) Clone() Terms {
	t.PolicyTemplate = slices.Clone(t.PolicyTemplate)
	t.PkScript = slices.Clone(t.PkScript)

	return t
}

// Validate checks the balance and identity invariants shared by both sides.
func (t Terms) Validate() error {
	if t.ID == (ID{}) {
		return fmt.Errorf("channel ID is required")
	}
	if t.PendingChannelID == ([32]byte{}) {
		return fmt.Errorf("pending channel ID is required")
	}
	if t.Capacity <= 0 {
		return fmt.Errorf("channel capacity must be positive")
	}
	if len(t.PolicyTemplate) == 0 {
		return fmt.Errorf("channel VTXO policy is required")
	}
	if len(t.PkScript) == 0 {
		return fmt.Errorf("channel VTXO script is required")
	}
	if err := validateNodeKey("client", t.ClientNodeKey); err != nil {
		return err
	}
	if err := validateNodeKey("hub", t.HubNodeKey); err != nil {
		return err
	}
	if bytes.Equal(t.ClientNodeKey[:], t.HubNodeKey[:]) {
		return fmt.Errorf("client and hub node keys must differ")
	}

	switch t.Kind {
	case KindPromotion:
		if t.Funder != PartyClient {
			return fmt.Errorf("promotion must be client funded")
		}
		if t.PaymentHash != ([32]byte{}) {
			return fmt.Errorf("promotion cannot bind a payment " +
				"hash")
		}

	case KindReceiveIntent:
		if t.Funder != PartyHub {
			return fmt.Errorf("receive intent must be hub funded")
		}
		if t.ReservedSCID == 0 {
			return fmt.Errorf("receive intent requires a " +
				"reserved SCID")
		}
		if t.PaymentHash == ([32]byte{}) {
			return fmt.Errorf("receive intent requires a payment " +
				"hash")
		}

	default:
		return fmt.Errorf("unknown channel kind %d", t.Kind)
	}

	return nil
}

// VTXOBinding identifies the exact validated Ark output used as backing.
type VTXOBinding struct {
	OutPoint       wire.OutPoint
	Amount         btcutil.Amount
	RoundID        string
	CommitmentTxID chainhash.Hash
	PolicyTemplate []byte
	PkScript       []byte
}

// Clone returns a binding without aliases to mutable byte slices.
func (b VTXOBinding) Clone() VTXOBinding {
	b.PolicyTemplate = slices.Clone(b.PolicyTemplate)
	b.PkScript = slices.Clone(b.PkScript)

	return b
}

// Validate checks a binding against immutable channel terms.
func (b VTXOBinding) Validate(terms Terms) error {
	if b.OutPoint.Hash == (chainhash.Hash{}) {
		return fmt.Errorf("VTXO outpoint is required")
	}
	if b.Amount < terms.Capacity {
		return fmt.Errorf("VTXO amount %d is below channel capacity %d",
			b.Amount, terms.Capacity)
	}
	if b.RoundID == "" {
		return fmt.Errorf("VTXO round ID is required")
	}
	if b.CommitmentTxID == (chainhash.Hash{}) {
		return fmt.Errorf("VTXO commitment transaction is required")
	}
	if !bytes.Equal(b.PolicyTemplate, terms.PolicyTemplate) {
		return fmt.Errorf("VTXO policy does not match channel terms")
	}
	if !bytes.Equal(b.PkScript, terms.PkScript) {
		return fmt.Errorf("VTXO script does not match channel terms")
	}

	return nil
}

// Backing identifies the fully signed VTXO-to-channel transaction.
type Backing struct {
	Transaction  []byte
	ChannelPoint wire.OutPoint
}

// Clone returns a backing record without aliases to transaction bytes.
func (b Backing) Clone() Backing {
	b.Transaction = slices.Clone(b.Transaction)

	return b
}

// Validate proves the signed transaction spends only the bound VTXO and
// creates the exact lnd channel point.
func (b Backing) Validate(terms Terms, source VTXOBinding) error {
	if len(b.Transaction) == 0 {
		return fmt.Errorf("signed backing transaction is required")
	}

	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(b.Transaction)); err != nil {
		return fmt.Errorf("decode backing transaction: %w", err)
	}
	if len(tx.TxIn) != 1 {
		return fmt.Errorf("backing transaction must have one input")
	}
	if tx.TxIn[0].PreviousOutPoint != source.OutPoint {
		return fmt.Errorf("backing transaction does not spend bound " +
			"VTXO")
	}
	if len(tx.TxIn[0].Witness) == 0 {
		return fmt.Errorf("backing transaction is not fully signed")
	}
	if b.ChannelPoint.Hash != tx.TxHash() {
		return fmt.Errorf("channel point transaction ID does not match")
	}
	if int(b.ChannelPoint.Index) >= len(tx.TxOut) {
		return fmt.Errorf("channel point output %d is out of range",
			b.ChannelPoint.Index)
	}
	if tx.TxOut[b.ChannelPoint.Index].Value != int64(terms.Capacity) {
		return fmt.Errorf("channel point value %d does not match "+
			"capacity %d", tx.TxOut[b.ChannelPoint.Index].Value,
			terms.Capacity)
	}

	return nil
}

// Snapshot is the complete Ark-owned durable state for one channel.
type Snapshot struct {
	Terms            Terms
	Phase            Phase
	Source           *VTXOBinding
	Backing          *Backing
	ClientFinalized  bool
	HubFinalized     bool
	RoundCommitted   bool
	RoundConfirmed   bool
	BackingPublished bool
	Failure          string
}

// Clone returns a snapshot without aliases to mutable fields.
func (s Snapshot) Clone() Snapshot {
	s.Terms = s.Terms.Clone()
	if s.Source != nil {
		source := s.Source.Clone()
		s.Source = &source
	}
	if s.Backing != nil {
		backing := s.Backing.Clone()
		s.Backing = &backing
	}

	return s
}

// ReadyForRoundSigning reports whether a receive output crossed the durable
// no-failure boundary required before nonce release.
func (s Snapshot) ReadyForRoundSigning() bool {
	return s.Terms.Kind == KindReceiveIntent &&
		s.Source != nil && s.Backing != nil &&
		s.ClientFinalized && s.HubFinalized &&
		s.Phase >= PhaseBackingReady && s.Phase != PhaseFailed
}

// validateNodeKey checks a compressed secp256k1 node key.
func validateNodeKey(name string, raw [33]byte) error {
	if _, err := btcec.ParsePubKey(raw[:]); err != nil {
		return fmt.Errorf("invalid %s node key: %w", name, err)
	}

	return nil
}
