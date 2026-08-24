package arkchannel

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/lnwire"
)

// DefaultBackingFee is the fixed fee reserved above channel capacity for the
// deterministic VTXO-to-channel funding transaction.
const DefaultBackingFee btcutil.Amount = 1_000

// ID is the stable identifier shared by both sides of an Ark channel.
type ID [32]byte

// ReceiveIntentID derives the stable channel identifier for one incoming
// payment. Replays of the same invoice can only address the same intent.
func ReceiveIntentID(paymentHash [32]byte) ID {
	digest := sha256.Sum256(
		append(
			[]byte("wavelength-receive-channel-id:"),
			paymentHash[:]...,
		),
	)

	return ID(digest)
}

// ReceiveIntentPendingID derives lnd's stable pending channel identifier for
// one incoming payment.
func ReceiveIntentPendingID(paymentHash [32]byte) [32]byte {
	return sha256.Sum256(
		append(
			[]byte("wavelength-receive-pending-id:"),
			paymentHash[:]...,
		),
	)
}

// Kind identifies how the VTXO backing a channel is obtained.
type Kind uint8

const (
	// KindPromotion moves an existing client-owned VTXO into a
	// channel-policy VTXO through an OOR transfer.
	KindPromotion Kind = iota + 1

	// KindReceiveIntent moves hub liquidity into a channel-policy VTXO
	// through an OOR transfer before resuming an incoming payment.
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

// FundingInitiator returns the endpoint that starts lnd's channel-opening
// state machine. The Ark funder also pays Lightning commitment fees, which
// lets receive intents allocate the entire opening balance to the hub.
func (t Terms) FundingInitiator() Party {
	return t.Funder
}

// InitialPushAmount returns the amount the lnd initiator pushes to its peer.
// Both channel kinds start with all liquidity on the Ark funder's side.
func (t Terms) InitialPushAmount() btcutil.Amount {
	return 0
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
	// acknowledgements are durable, so the prepared OOR transfer can
	// commit.
	PhaseBackingReady

	// PhaseActivating means lnd should observe the virtual channel point.
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

	// PhaseCancelling means the prepared OOR transfer is being aborted
	// before lnd's pending funding reservation is removed.
	PhaseCancelling

	// PhaseFailed means negotiation failed before the no-failure boundary.
	PhaseFailed

	// PhaseCoopClosing means both endpoints must quiesce lnd and agree on
	// one clean commitment state before authorizing an ordinary OOR close.
	PhaseCoopClosing

	// PhaseCoopCloseSigned means both endpoints durably stored the hub's
	// authorization for the exact 3-of-3 OOR close. The historical name is
	// retained because phase values are persisted.
	PhaseCoopCloseSigned

	// PhaseCoopClosePublished means the ordinary OOR close finalized and
	// both lnd databases must archive the channel. The historical name is
	// retained because phase values are persisted.
	PhaseCoopClosePublished
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

	case PhaseCancelling:
		return "cancelling"

	case PhaseFailed:
		return "failed"

	case PhaseCoopClosing:
		return "coop_closing"

	case PhaseCoopCloseSigned:
		return "coop_close_authorized"

	case PhaseCoopClosePublished:
		return "coop_close_oor_finalized"

	default:
		return "unknown"
	}
}

// IsTerminal reports whether Ark coordination has no further work.
func (p Phase) IsTerminal() bool {
	return p == PhaseClosed || p == PhaseFailed
}

// VTXOTerms identify every key role and relative delay in the channel VTXO.
// Persisting semantics instead of derived script bytes gives both peers one
// canonical policy representation to validate.
type VTXOTerms struct {
	ClientArkKey     [33]byte
	HubArkKey        [33]byte
	ArkOperatorKey   [33]byte
	ClientChannelKey [33]byte
	HubChannelKey    [33]byte
	FunderKey        [33]byte
	ChannelDelay     uint32
	FunderDelay      uint32
	MinExitDelay     uint32
}

// Artifacts derives and validates the canonical channel policy and P2TR
// script from semantic terms.
func (t VTXOTerms) Artifacts() ([]byte, []byte, error) {
	clientArkKey, err := parseChannelKey("client Ark", t.ClientArkKey)
	if err != nil {
		return nil, nil, err
	}
	hubArkKey, err := parseChannelKey("hub Ark", t.HubArkKey)
	if err != nil {
		return nil, nil, err
	}
	operatorKey, err := parseChannelKey("Ark operator", t.ArkOperatorKey)
	if err != nil {
		return nil, nil, err
	}
	clientChannelKey, err := parseChannelKey(
		"client channel", t.ClientChannelKey,
	)
	if err != nil {
		return nil, nil, err
	}
	hubChannelKey, err := parseChannelKey(
		"hub channel", t.HubChannelKey,
	)
	if err != nil {
		return nil, nil, err
	}
	funderKey, err := parseChannelKey("funder", t.FunderKey)
	if err != nil {
		return nil, nil, err
	}

	policy, pkScript, err := arkscript.EncodeChannelVTXOArtifacts(
		arkscript.ChannelVTXOParams{
			ClientArkKey:     clientArkKey,
			HubArkKey:        hubArkKey,
			ArkOperatorKey:   operatorKey,
			ClientChannelKey: clientChannelKey,
			HubChannelKey:    hubChannelKey,
			FunderKey:        funderKey,
			ChannelDelay:     t.ChannelDelay,
			FunderDelay:      t.FunderDelay,
			MinExitDelay:     t.MinExitDelay,
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("derive channel VTXO: %w", err)
	}

	return policy, pkScript, nil
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
	VTXO             VTXOTerms
}

// Clone returns an isolated copy of immutable terms.
func (t Terms) Clone() Terms {
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
	reservedSCID := lnwire.NewShortChanIDFromInt(t.ReservedSCID)
	if reservedSCID.BlockHeight == 0 {
		return fmt.Errorf("channel requires a reserved virtual SCID")
	}
	if _, _, err := t.VTXO.Artifacts(); err != nil {
		return err
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
	OORSessionID   [32]byte
	OutPoint       wire.OutPoint
	Amount         btcutil.Amount
	ArkTransaction []byte
	PolicyTemplate []byte
	PkScript       []byte
}

// Clone returns a binding without aliases to mutable byte slices.
func (b VTXOBinding) Clone() VTXOBinding {
	b.ArkTransaction = slices.Clone(b.ArkTransaction)
	b.PolicyTemplate = slices.Clone(b.PolicyTemplate)
	b.PkScript = slices.Clone(b.PkScript)

	return b
}

// Validate checks a binding against immutable channel terms.
func (b VTXOBinding) Validate(terms Terms) error {
	if b.OutPoint.Hash == (chainhash.Hash{}) {
		return fmt.Errorf("VTXO outpoint is required")
	}
	if b.OORSessionID == ([32]byte{}) {
		return fmt.Errorf("OOR session ID is required")
	}
	if len(b.ArkTransaction) == 0 {
		return fmt.Errorf("prepared Ark transaction is required")
	}
	tx := wire.NewMsgTx(2)
	reader := bytes.NewReader(b.ArkTransaction)
	if err := tx.Deserialize(reader); err != nil {
		return fmt.Errorf("decode prepared Ark transaction: %w", err)
	}
	if reader.Len() != 0 {
		return fmt.Errorf("prepared Ark transaction has %d "+
			"trailing bytes", reader.Len())
	}
	txid := tx.TxHash()
	if txid != chainhash.Hash(b.OORSessionID) ||
		b.OutPoint.Hash != txid {
		return fmt.Errorf("VTXO outpoint does not belong to prepared " +
			"OOR session")
	}
	if int(b.OutPoint.Index) >= len(tx.TxOut) {
		return fmt.Errorf("VTXO output %d is out of range",
			b.OutPoint.Index)
	}
	output := tx.TxOut[b.OutPoint.Index]
	if output.Value != int64(b.Amount) ||
		!bytes.Equal(output.PkScript, b.PkScript) {
		return fmt.Errorf("prepared Ark transaction output does not " +
			"match VTXO binding")
	}
	if b.Amount < terms.Capacity {
		return fmt.Errorf("VTXO amount %d is below channel capacity %d",
			b.Amount, terms.Capacity)
	}
	policy, pkScript, err := terms.VTXO.Artifacts()
	if err != nil {
		return err
	}
	if !bytes.Equal(b.PolicyTemplate, policy) {
		return fmt.Errorf("VTXO policy does not match channel terms")
	}
	if !bytes.Equal(b.PkScript, pkScript) {
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
	reservedSCID := lnwire.NewShortChanIDFromInt(terms.ReservedSCID)
	if reservedSCID.TxPosition != uint16(b.ChannelPoint.Index) {
		return fmt.Errorf("reserved SCID output %d does not match "+
			"channel point output %d", reservedSCID.TxPosition,
			b.ChannelPoint.Index)
	}

	return nil
}

// Snapshot is the complete Ark-owned durable state for one channel.
type Snapshot struct {
	Terms                   Terms
	Phase                   Phase
	OORPreparationStarted   bool
	Source                  *VTXOBinding
	Backing                 *Backing
	ClientFinalized         bool
	HubFinalized            bool
	OORFinalized            bool
	OORAborted              bool
	RecoveryReady           bool
	SourceConflict          *SourceConflict
	BackingPublished        bool
	CooperativeCloseRequest *CooperativeCloseRequest
	CooperativeClose        *CooperativeClose
	ClientCloseSigned       bool
	HubCloseSigned          bool
	ClientCloseFinalized    bool
	HubCloseFinalized       bool
	Failure                 string
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
	if s.SourceConflict != nil {
		conflict := *s.SourceConflict
		s.SourceConflict = &conflict
	}
	if s.CooperativeCloseRequest != nil {
		request := s.CooperativeCloseRequest.Clone()
		s.CooperativeCloseRequest = &request
	}
	if s.CooperativeClose != nil {
		settlement := s.CooperativeClose.Clone()
		s.CooperativeClose = &settlement
	}

	return s
}

// SourceConflict records the first confirmed spend of any on-path ancestor
// watched for a channel-policy VTXO. The channel FSM retains this evidence so
// materialization and force close resume after a process restart.
type SourceConflict struct {
	OutPoint     wire.OutPoint
	SpendingTxID chainhash.Hash
}

// ReadyToCommitOOR reports whether both lnd endpoints and the signed backing
// are durable, allowing the prepared OOR transfer to cross its commit gate.
func (s Snapshot) ReadyToCommitOOR() bool {
	return s.Source != nil && s.Backing != nil && s.ClientFinalized &&
		s.HubFinalized && s.Phase == PhaseBackingReady &&
		!s.OORFinalized && !s.OORAborted
}

// validateNodeKey checks a compressed secp256k1 node key.
func validateNodeKey(name string, raw [33]byte) error {
	if _, err := btcec.ParsePubKey(raw[:]); err != nil {
		return fmt.Errorf("invalid %s node key: %w", name, err)
	}

	return nil
}

// parseChannelKey validates and parses one compressed policy key.
func parseChannelKey(name string, raw [33]byte) (*btcec.PublicKey, error) {
	key, err := btcec.ParsePubKey(raw[:])
	if err != nil {
		return nil, fmt.Errorf("invalid %s key: %w", name, err)
	}

	return key, nil
}
