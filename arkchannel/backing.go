package arkchannel

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
)

// BackingTemplate is the unsigned VTXO-to-channel transaction returned by
// lnd's external-funding flow, plus the exact Ark spend path that authorizes
// it. The transaction ID is stable because witness signatures are segregated.
type BackingTemplate struct {
	packet       *psbt.Packet
	previousOut  *wire.TxOut
	spendPath    *arkscript.SpendPath
	channelPoint wire.OutPoint
}

// NewBackingTemplate binds lnd's negotiated funding output to the one
// channel-policy VTXO created by a prepared OOR transfer.
func NewBackingTemplate(packet *psbt.Packet, terms Terms,
	source VTXOBinding) (*BackingTemplate, error) {

	if packet == nil || packet.UnsignedTx == nil {
		return nil, fmt.Errorf("lnd funding PSBT is required")
	}
	if err := terms.Validate(); err != nil {
		return nil, err
	}
	if err := source.Validate(terms); err != nil {
		return nil, err
	}
	if len(packet.UnsignedTx.TxIn) != 0 || len(packet.Inputs) != 0 {
		return nil, fmt.Errorf("lnd funding PSBT must not contain " +
			"inputs")
	}
	if len(packet.UnsignedTx.TxOut) != 1 || len(packet.Outputs) != 1 {
		return nil, fmt.Errorf("lnd funding PSBT must contain " +
			"exactly one channel output")
	}
	if packet.UnsignedTx.TxOut[0].Value != int64(terms.Capacity) {
		return nil, fmt.Errorf("lnd funding output value %d does not "+
			"match capacity %d", packet.UnsignedTx.TxOut[0].Value,
			terms.Capacity)
	}
	reservedSCID := lnwire.NewShortChanIDFromInt(terms.ReservedSCID)
	if reservedSCID.TxPosition != 0 {
		return nil, fmt.Errorf("reserved SCID output %d does not "+
			"match the single lnd funding output",
			reservedSCID.TxPosition)
	}
	if source.Amount <= terms.Capacity {
		return nil, fmt.Errorf("channel VTXO must reserve a positive " +
			"backing transaction fee")
	}

	policy, err := channelPolicy(terms.VTXO)
	if err != nil {
		return nil, err
	}
	spendPath, err := policy.ChannelSpendPath()
	if err != nil {
		return nil, fmt.Errorf("derive channel materialization "+
			"path: %w", err)
	}
	if err := spendPath.VerifyBindsToPkScript(source.PkScript); err != nil {
		return nil, fmt.Errorf("validate channel materialization "+
			"path: %w", err)
	}

	tx := packet.UnsignedTx
	tx.Version = 2
	tx.LockTime = spendPath.RequiredLockTime
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: source.OutPoint,
		Sequence:         spendPath.RequiredSequence,
	})
	previousOut := &wire.TxOut{
		Value:    int64(source.Amount),
		PkScript: bytes.Clone(source.PkScript),
	}
	packet.Inputs = append(packet.Inputs, psbt.PInput{
		WitnessUtxo: previousOut,
	})
	if err := spendPath.AttachTapLeafScript(&packet.Inputs[0]); err != nil {
		return nil, fmt.Errorf("attach channel spend path: %w", err)
	}
	if err := packet.SanityCheck(); err != nil {
		return nil, fmt.Errorf("validate channel backing PSBT: %w", err)
	}

	return &BackingTemplate{
		packet:      packet,
		previousOut: previousOut,
		spendPath:   spendPath,
		channelPoint: wire.OutPoint{
			Hash: tx.TxHash(),
		},
	}, nil
}

// Packet returns the populated PSBT that lnd must verify before its funding
// intent is finalized. The caller must not mutate it after signatures begin.
func (t *BackingTemplate) Packet() *psbt.Packet {
	return t.packet
}

// Transaction returns a copy of the unsigned backing transaction.
func (t *BackingTemplate) Transaction() *wire.MsgTx {
	return t.packet.UnsignedTx.Copy()
}

// ChannelPoint returns the stable channel point before witness signatures are
// attached.
func (t *BackingTemplate) ChannelPoint() wire.OutPoint {
	return t.channelPoint
}

// ValidateFundingOutput lets either endpoint compare the template with the
// exact output derived from its own lnd funding reservation before signing.
func (t *BackingTemplate) ValidateFundingOutput(expected *wire.TxOut) error {
	if expected == nil {
		return fmt.Errorf("expected lnd funding output is required")
	}
	actual := t.packet.UnsignedTx.TxOut[t.channelPoint.Index]
	if actual.Value != expected.Value ||
		!bytes.Equal(actual.PkScript, expected.PkScript) {
		return fmt.Errorf("backing transaction does not fund the " +
			"local lnd reservation")
	}

	return nil
}

// SignDescriptor returns the exact tapscript descriptor for one channel
// endpoint after checking that the supplied key belongs to that role.
func (t *BackingTemplate) SignDescriptor(terms Terms, party Party,
	keyDesc keychain.KeyDescriptor) (*input.SignDescriptor, error) {

	if keyDesc.PubKey == nil {
		return nil, fmt.Errorf("channel backing signing key is " +
			"required")
	}
	var expected [33]byte
	switch party {
	case PartyClient:
		expected = terms.VTXO.ClientChannelKey

	case PartyHub:
		expected = terms.VTXO.HubChannelKey

	default:
		return nil, fmt.Errorf("unknown channel backing party %d",
			party)
	}
	expectedKey, err := parseChannelKey("channel backing", expected)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(
		schnorr.SerializePubKey(keyDesc.PubKey),
		schnorr.SerializePubKey(expectedKey),
	) {
		return nil, fmt.Errorf("signing key does not match %s "+
			"channel key", party)
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		t.previousOut.PkScript, t.previousOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(
		t.packet.UnsignedTx, prevFetcher,
	)

	return t.spendPath.BuildSignDescriptor(
		keyDesc, t.previousOut, sigHashes, prevFetcher, 0,
	), nil
}

// Complete attaches both endpoint signatures, executes the channel VTXO
// tapscript locally, and returns the fully signed durable backing record.
func (t *BackingTemplate) Complete(terms Terms, source VTXOBinding,
	clientSig, hubSig input.Signature) (Backing, error) {

	if clientSig == nil || hubSig == nil {
		return Backing{}, fmt.Errorf("both channel backing " +
			"signatures are required")
	}
	witness, err := t.spendPath.Witness(
		arkscript.MaybeAppendSighash(
			hubSig, txscript.SigHashDefault,
		),
		arkscript.MaybeAppendSighash(
			clientSig, txscript.SigHashDefault,
		),
	)
	if err != nil {
		return Backing{}, err
	}
	tx := t.packet.UnsignedTx.Copy()
	tx.TxIn[0].Witness = witness
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		t.previousOut.PkScript, t.previousOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	engine, err := txscript.NewEngine(
		t.previousOut.PkScript, tx, 0, txscript.StandardVerifyFlags,
		nil, sigHashes, t.previousOut.Value, prevFetcher,
	)
	if err != nil {
		return Backing{}, fmt.Errorf("construct channel backing "+
			"verifier: %w", err)
	}
	if err := engine.Execute(); err != nil {
		return Backing{}, fmt.Errorf("verify channel backing "+
			"signatures: %w", err)
	}

	var raw bytes.Buffer
	if err := tx.Serialize(&raw); err != nil {
		return Backing{}, fmt.Errorf("serialize channel backing: %w",
			err)
	}
	backing := Backing{
		Transaction:  raw.Bytes(),
		ChannelPoint: t.channelPoint,
	}
	if err := backing.Validate(terms, source); err != nil {
		return Backing{}, err
	}

	return backing, nil
}

// channelPolicy reconstructs the channel-policy tree from durable semantic
// terms rather than trusting caller-supplied scripts.
func channelPolicy(terms VTXOTerms) (*arkscript.ChannelVTXOPolicy, error) {
	clientArkKey, err := parseChannelKey("client Ark", terms.ClientArkKey)
	if err != nil {
		return nil, err
	}
	hubArkKey, err := parseChannelKey("hub Ark", terms.HubArkKey)
	if err != nil {
		return nil, err
	}
	operatorKey, err := parseChannelKey(
		"Ark operator", terms.ArkOperatorKey,
	)
	if err != nil {
		return nil, err
	}
	clientChannelKey, err := parseChannelKey(
		"client channel", terms.ClientChannelKey,
	)
	if err != nil {
		return nil, err
	}
	hubChannelKey, err := parseChannelKey(
		"hub channel", terms.HubChannelKey,
	)
	if err != nil {
		return nil, err
	}
	funderKey, err := parseChannelKey("funder", terms.FunderKey)
	if err != nil {
		return nil, err
	}

	return arkscript.NewChannelVTXOPolicy(arkscript.ChannelVTXOParams{
		ClientArkKey:     clientArkKey,
		HubArkKey:        hubArkKey,
		ArkOperatorKey:   operatorKey,
		ClientChannelKey: clientChannelKey,
		HubChannelKey:    hubChannelKey,
		FunderKey:        funderKey,
		ChannelDelay:     terms.ChannelDelay,
		FunderDelay:      terms.FunderDelay,
		MinExitDelay:     terms.MinExitDelay,
	})
}
