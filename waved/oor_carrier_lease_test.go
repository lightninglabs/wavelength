package waved

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/stretchr/testify/require"
)

// testCarrierFloatLease builds a lease over a real standard float policy
// owned by floatKey with operatorKey as the collab cosigner.
func testCarrierFloatLease(t *testing.T, floatKey, operatorKey *btcec.PublicKey,
	value btcutil.Amount) *oor.OORCarrierLease {

	t.Helper()

	policyTemplate, pkScript, err := arkscript.EncodeStandardVTXOArtifacts(
		floatKey, operatorKey, 10,
	)
	require.NoError(t, err)

	return &oor.OORCarrierLease{
		Outpoint: wire.OutPoint{
			Hash: [32]byte{
				0xf1,
			},
			Index: 1,
		},
		Value:          value,
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		ExpiresAtUnix:  1_700_000_000,
	}
}

// TestOperatorTermsCarryOORCarrierPubKey pins the x-only carrier key parse:
// empty means disabled, garbage fails, and a valid key round-trips.
func TestOperatorTermsCarryOORCarrierPubKey(t *testing.T) {
	t.Parallel()

	carrierKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	resp := &arkrpc.GetInfoResponse{
		Pubkey: testOperatorPubKeyBytes(t),
	}
	terms, err := operatorTermsFromResponse(resp)
	require.NoError(t, err)
	require.Nil(t, terms.OORCarrierPubKey)

	resp.OorCarrierPubkey = schnorr.SerializePubKey(carrierKey.PubKey())
	terms, err = operatorTermsFromResponse(resp)
	require.NoError(t, err)
	require.Equal(
		t,
		schnorr.SerializePubKey(
			carrierKey.PubKey(),
		),
		schnorr.SerializePubKey(terms.OORCarrierPubKey),
	)

	resp.OorCarrierPubkey = []byte{0x01, 0x02}
	_, err = operatorTermsFromResponse(resp)
	require.ErrorContains(t, err, "parse OOR carrier pubkey")
}

// TestLeaseOORCarrier pins the client-side lease binding: the request carries
// the required value and the response is validated before use.
func TestLeaseOORCarrier(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	lease := testCarrierFloatLease(
		t, floatKey.PubKey(), operatorKey.PubKey(), 25_000,
	)
	terms := &types.OperatorTerms{
		PubKey:           operatorKey.PubKey(),
		OORCarrierPubKey: floatKey.PubKey(),
	}

	fake := &fakeArkService{
		leaseResponse: &arkrpc.LeaseOORCarrierResponse{
			Outpoint:           lease.Outpoint.String(),
			ValueSat:           int64(lease.Value),
			VtxoPolicyTemplate: lease.PolicyTemplate,
			PkScript:           lease.PkScript,
			ExpiresAtUnix:      lease.ExpiresAtUnix,
		},
	}
	server := &Server{serverConn: newBufconnClient(t, fake)}

	granted, err := server.leaseOORCarrier(t.Context(), terms, 2_000)
	require.NoError(t, err)
	require.True(t, lease.FundingEquals(granted))
	require.Len(t, fake.leaseRequests, 1)
	require.EqualValues(t, 2_000, fake.leaseRequests[0].GetRequiredSat())

	// A float below the required value must be refused.
	_, err = server.leaseOORCarrier(t.Context(), terms, 30_000)
	require.ErrorContains(t, err, "below required")

	// An operator that advertises no carrier key never gets asked.
	callsBefore := len(fake.leaseRequests)
	_, err = server.leaseOORCarrier(
		t.Context(), &types.OperatorTerms{
			PubKey: operatorKey.PubKey(),
		},
		2_000,
	)
	require.ErrorContains(t, err, "does not fund OOR carriers")
	require.Len(t, fake.leaseRequests, callsBefore)
}

// TestBuildOperatorFundedTransferInput pins the binding checks between the
// lease, the advertised float key, and the current operator key.
func TestBuildOperatorFundedTransferInput(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	lease := testCarrierFloatLease(
		t, floatKey.PubKey(), operatorKey.PubKey(), 25_000,
	)

	in, err := BuildOperatorFundedTransferInput(
		lease, floatKey.PubKey(), operatorKey.PubKey(),
	)
	require.NoError(t, err)
	require.True(t, in.OperatorFunded)
	require.Nil(t, in.VTXO.ClientKey.PubKey)
	require.Equal(t, lease.Outpoint, in.VTXO.Outpoint)
	require.Equal(t, lease.Value, in.VTXO.Amount)
	require.Equal(t, lease.PkScript, in.VTXO.PkScript)
	require.NotEmpty(t, in.OwnerLeafScript)

	// A pkScript the policy does not compile to is a lie about the float.
	tampered := lease.Clone()
	tampered.PkScript[len(tampered.PkScript)-1] ^= 0x01
	_, err = BuildOperatorFundedTransferInput(
		tampered, floatKey.PubKey(), operatorKey.PubKey(),
	)
	require.ErrorContains(t, err, "does not match the leased pkScript")

	// A policy owned by someone other than the advertised carrier key is
	// not the operator's float.
	_, err = BuildOperatorFundedTransferInput(
		lease, otherKey.PubKey(), operatorKey.PubKey(),
	)
	require.ErrorContains(t, err, "not the advertised carrier key")

	// A policy whose collab leaf lacks the current operator key fails
	// policy validation.
	foreign := testCarrierFloatLease(
		t, floatKey.PubKey(), otherKey.PubKey(), 25_000,
	)
	_, err = BuildOperatorFundedTransferInput(
		foreign, floatKey.PubKey(), operatorKey.PubKey(),
	)
	require.ErrorContains(t, err, "invalid float policy")
}
