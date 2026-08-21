package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
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

// Validate checks the lease names a spendable float VTXO.
func (l *OORCarrierLease) Validate() error {
	switch {
	case l == nil:
		return fmt.Errorf("carrier lease must be provided")

	case l.Value <= 0 || l.Value > btcutil.MaxSatoshi:
		return fmt.Errorf("carrier lease value is invalid")

	case len(l.PolicyTemplate) == 0:
		return fmt.Errorf("carrier lease policy template is required")

	case len(l.PkScript) == 0:
		return fmt.Errorf("carrier lease pkscript is required")
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
