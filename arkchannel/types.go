package arkchannel

import (
	"bytes"
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

// Kind identifies how the VTXO backing a channel is obtained.
type Kind uint8

const (
	// KindPromotion moves an existing client-owned VTXO into a
	// channel-policy VTXO through an OOR transfer.
	KindPromotion Kind = 1
)

// String returns the durable name of a channel kind.
func (k Kind) String() string {
	switch k {
	case KindPromotion:
		return "promotion"

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

	VTXO VTXOTerms
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
	if reservedSCID.TxPosition != 0 {
		return fmt.Errorf("channel funding output position must be " +
			"zero")
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
