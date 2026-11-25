package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/vtxo"
)

// OORCarrierLease is a reservation of one operator carrier-float VTXO. The
// transfer spends the float whole: the new asset-leaf carriers come out of it
// and the residual returns to the float pkScript as the operator's change.
type OORCarrierLease struct {
	// Outpoint identifies the leased float VTXO.
	Outpoint wire.OutPoint

	// Value is the float VTXO's full value.
	Value btcutil.Amount

	// PolicyTemplate is the float VTXO's serialized arkscript policy.
	PolicyTemplate []byte

	// PkScript is the float VTXO's output script, which the operator's
	// change output must pay verbatim.
	PkScript []byte

	// ExpiresAtUnix is when the lease lapses if unconsumed, in Unix
	// seconds.
	ExpiresAtUnix int64
}

// Validate checks that the lease names a spendable float VTXO with valid
// standard VTXO policy metadata and distinct float-owner / operator keys.
func (l *OORCarrierLease) Validate() error {
	switch {
	case l == nil:
		return fmt.Errorf("carrier lease must be provided")

	case l.Outpoint.Hash == (chainhash.Hash{}):
		return fmt.Errorf("carrier lease outpoint txid must be " +
			"non-zero")

	case l.Value <= 0 || l.Value > btcutil.MaxSatoshi:
		return fmt.Errorf("carrier lease value is invalid")

	case len(l.PolicyTemplate) == 0:
		return fmt.Errorf("carrier lease policy template is required")

	case len(l.PkScript) == 0:
		return fmt.Errorf("carrier lease pkscript is required")

	case l.ExpiresAtUnix <= 0:
		return fmt.Errorf("carrier lease expiry must be positive")
	}

	template, err := arkscript.DecodePolicyTemplate(l.PolicyTemplate)
	if err != nil {
		return fmt.Errorf("decode carrier lease policy: %w", err)
	}
	if !template.MatchesPkScript(l.PkScript) {
		return fmt.Errorf("carrier lease policy does not match " +
			"pkscript")
	}

	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err != nil {
		return fmt.Errorf("carrier lease must use standard VTXO "+
			"policy: %w", err)
	}
	if err := oortx.ValidateCarrierFloatKeys(
		params.OwnerKey, params.OperatorKey,
	); err != nil {
		return err
	}

	return nil
}

// FundingEquals reports whether two leases name the same float funding.
// Expiry is excluded: a re-grant of the same float differs only in its
// expiry, and the journaled funding is what the transfer graph committed to.
func (l *OORCarrierLease) FundingEquals(other *OORCarrierLease) bool {
	if l == nil || other == nil {
		return l == other
	}

	return l.Outpoint == other.Outpoint &&
		l.Value == other.Value &&
		bytes.Equal(l.PolicyTemplate, other.PolicyTemplate) &&
		bytes.Equal(l.PkScript, other.PkScript)
}

// Clone deep-copies the lease.
func (l *OORCarrierLease) Clone() *OORCarrierLease {
	if l == nil {
		return nil
	}

	return &OORCarrierLease{
		Outpoint:       l.Outpoint,
		Value:          l.Value,
		PolicyTemplate: bytes.Clone(l.PolicyTemplate),
		PkScript:       bytes.Clone(l.PkScript),
		ExpiresAtUnix:  l.ExpiresAtUnix,
	}
}

// ToTransferInput converts a validated carrier lease into an operator-funded
// TransferInput whose checkpoint owner leaf binds the float owner key and the
// session operatorKey. expectedCarrierKey must be the operator's advertised
// GetInfo.oor_carrier_pubkey; the lease's policy owner key is verified against
// it so the client never accepts an unrelated VTXO as a carrier float.
func (l *OORCarrierLease) ToTransferInput(
	sessionOperatorKey, expectedCarrierKey *btcec.PublicKey) (TransferInput,
	error) {

	if err := l.Validate(); err != nil {
		return TransferInput{}, err
	}
	if sessionOperatorKey == nil {
		return TransferInput{}, fmt.Errorf("session operator key " +
			"must be provided")
	}
	if expectedCarrierKey == nil {
		return TransferInput{}, fmt.Errorf("expected carrier pubkey " +
			"must be provided")
	}

	template, err := arkscript.DecodePolicyTemplate(l.PolicyTemplate)
	if err != nil {
		return TransferInput{}, fmt.Errorf("decode carrier lease "+
			"policy: %w", err)
	}
	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err != nil {
		return TransferInput{}, fmt.Errorf("decode carrier lease "+
			"params: %w", err)
	}
	if !bytes.Equal(
		schnorr.SerializePubKey(params.OwnerKey),
		schnorr.SerializePubKey(expectedCarrierKey),
	) {
		return TransferInput{}, fmt.Errorf("carrier lease owner key " +
			"does not match advertised oor_carrier_pubkey")
	}
	if err := oortx.ValidateCarrierFloatKeys(
		params.OwnerKey, sessionOperatorKey,
	); err != nil {
		return TransferInput{}, err
	}

	ownerLeafScript, ownerLeafPolicy, err := defaultOwnerLeaf(
		params.OwnerKey, sessionOperatorKey,
	)
	if err != nil {
		return TransferInput{}, err
	}

	in := TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint:       l.Outpoint,
			Amount:         l.Value,
			PkScript:       bytes.Clone(l.PkScript),
			PolicyTemplate: bytes.Clone(l.PolicyTemplate),
			OperatorKey:    params.OperatorKey,
			RelativeExpiry: params.ExitDelay,
		},
		VTXOPolicyTemplate: bytes.Clone(l.PolicyTemplate),
		OwnerLeafScript:    ownerLeafScript,
		OwnerLeafPolicy:    ownerLeafPolicy,
		OperatorFunded:     true,
	}
	if err := in.Validate(); err != nil {
		return TransferInput{}, err
	}

	return in, nil
}
