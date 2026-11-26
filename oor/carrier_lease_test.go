package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestOORCarrierLeaseValidateAndClone covers lease validation, equality
// ignoring expiry, deep cloning, and conversion to an operator-funded input.
func TestOORCarrierLeaseValidateAndClone(t *testing.T) {
	t.Parallel()

	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherOperatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policyBytes, pkScript, err := arkscript.EncodeStandardVTXOArtifacts(
		floatKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)

	lease := &OORCarrierLease{
		Outpoint: wire.OutPoint{
			Hash: [32]byte{
				0x19,
			},
			Index: 0,
		},
		Value:          25_000,
		PolicyTemplate: policyBytes,
		PkScript:       pkScript,
		ExpiresAtUnix:  1_700_000_100,
	}
	require.NoError(t, lease.Validate())

	cloned := lease.Clone()
	require.True(t, lease.FundingEquals(cloned))
	cloned.ExpiresAtUnix += 300
	require.True(t, lease.FundingEquals(cloned))
	cloned.PkScript[0] ^= 0xff
	require.False(t, lease.FundingEquals(cloned))
	require.Nil(t, (*OORCarrierLease)(nil).Clone())
	require.True(t, (*OORCarrierLease)(nil).FundingEquals(nil))
	require.False(t, lease.FundingEquals(nil))

	// Valid lease converts into a validated OperatorFunded TransferInput.
	input, err := lease.ToTransferInput(
		operatorKey.PubKey(), floatKey.PubKey(),
	)
	require.NoError(t, err)
	require.True(t, input.OperatorFunded)
	require.Nil(t, input.VTXO.ClientKey.PubKey)
	require.Equal(t, lease.Outpoint, input.VTXO.Outpoint)
	require.Equal(t, lease.Value, input.VTXO.Amount)
	require.Equal(t, lease.PkScript, input.VTXO.PkScript)
	require.NotEmpty(t, input.OwnerLeafScript)
	require.NotEmpty(t, input.OwnerLeafPolicy)

	// Operator key rotation: a rotated session operator key builds the
	// checkpoint owner leaf with the new session key while preserving the
	// float VTXO's original operator key for spending the input VTXO.
	rotated, err := lease.ToTransferInput(
		otherOperatorKey.PubKey(), floatKey.PubKey(),
	)
	require.NoError(t, err)
	require.Equal(
		t,
		schnorr.SerializePubKey(
			operatorKey.PubKey(),
		),
		schnorr.SerializePubKey(rotated.VTXO.OperatorKey),
	)
	require.NotEqual(t, input.OwnerLeafScript, rotated.OwnerLeafScript)

	// Passing the float owner key as the session operator key must fail
	// ValidateCarrierFloatKeys key separation.
	_, err = lease.ToTransferInput(floatKey.PubKey(), floatKey.PubKey())
	require.ErrorContains(t, err, "must differ")

	// Mismatched advertised oor_carrier_pubkey is rejected.
	_, err = lease.ToTransferInput(
		operatorKey.PubKey(), otherOperatorKey.PubKey(),
	)
	require.ErrorContains(t, err, "advertised oor_carrier_pubkey")

	// Nil operator key or expected carrier key is rejected.
	_, err = lease.ToTransferInput(nil, floatKey.PubKey())
	require.ErrorContains(t, err, "operator key must be provided")
	_, err = lease.ToTransferInput(operatorKey.PubKey(), nil)
	require.ErrorContains(t, err, "expected carrier pubkey")

	// Policy/script mismatch is rejected.
	badScriptLease := lease.Clone()
	badScriptLease.PkScript[len(badScriptLease.PkScript)-1] ^= 0x01
	_, err = badScriptLease.ToTransferInput(
		operatorKey.PubKey(), floatKey.PubKey(),
	)
	require.ErrorContains(t, err, "does not match pkscript")

	// Identical float owner and operator keys in the policy template are
	// rejected by standard policy decoding.
	sameKeyPolicy, sameKeyScript, err :=
		arkscript.EncodeStandardVTXOArtifacts(
			operatorKey.PubKey(), operatorKey.PubKey(), 144,
		)
	require.NoError(t, err)
	sameKeyLease := &OORCarrierLease{
		Outpoint:       lease.Outpoint,
		Value:          10_000,
		PolicyTemplate: sameKeyPolicy,
		PkScript:       sameKeyScript,
		ExpiresAtUnix:  1_700_000_100,
	}
	_, err = sameKeyLease.ToTransferInput(
		operatorKey.PubKey(), operatorKey.PubKey(),
	)
	require.ErrorContains(t, err, "collab leaf missing operator key")

	// Validation errors on nil, zero outpoint, bad value, missing
	// policy/script, and non-positive expiry.
	require.Error(t, (*OORCarrierLease)(nil).Validate())
	zeroOut := lease.Clone()
	zeroOut.Outpoint = wire.OutPoint{}
	require.ErrorContains(t, zeroOut.Validate(), "outpoint txid")
	badVal := lease.Clone()
	badVal.Value = 0
	require.ErrorContains(t, badVal.Validate(), "value is invalid")
	badVal.Value = btcutil.MaxSatoshi + 1
	require.ErrorContains(t, badVal.Validate(), "value is invalid")
	noPolicy := lease.Clone()
	noPolicy.PolicyTemplate = nil
	require.ErrorContains(t, noPolicy.Validate(), "policy template")
	noScript := lease.Clone()
	noScript.PkScript = nil
	require.ErrorContains(t, noScript.Validate(), "pkscript")
	noExpiry := lease.Clone()
	noExpiry.ExpiresAtUnix = 0
	require.ErrorContains(t, noExpiry.Validate(), "expiry must be positive")
}
