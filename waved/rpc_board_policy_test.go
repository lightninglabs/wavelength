package waved

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// boardPolicyTestKey returns a fresh secp256k1 public key for policy tests.
func boardPolicyTestKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}

// TestValidateBoardPolicyTemplate exercises the Board custom-policy validation
// boundary: the standard (empty-template) path, the pinned-script-without-
// template rejection, and the decode / operator-binding / pk_script-match
// checks on a supplied template.
func TestValidateBoardPolicyTemplate(t *testing.T) {
	t.Parallel()

	const exitDelay = uint32(144)

	operatorKey := boardPolicyTestKey(t)
	ownerKey := boardPolicyTestKey(t)
	otherOperatorKey := boardPolicyTestKey(t)

	terms := &types.OperatorTerms{
		PubKey:        operatorKey,
		VTXOExitDelay: exitDelay,
	}

	// A standard Ark VTXO template owned by ownerKey, co-signed by the
	// operator, with an exit delay meeting the operator's floor. This
	// stands in for a FROST-owned VTXO: ownerKey is opaque to the daemon.
	validTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	decoded, err := arkscript.DecodePolicyTemplate(validTemplate)
	require.NoError(t, err)
	validPkScript, err := decoded.PkScript()
	require.NoError(t, err)

	// A template whose operator key is not the terms' operator: the
	// operator cannot co-sign its collab leaf, so admission must reject it.
	wrongOperatorTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, otherOperatorKey, exitDelay,
	)
	require.NoError(t, err)

	tests := []struct {
		name           string
		policyTemplate []byte
		pkScript       []byte
		wantErr        bool
	}{
		{
			name:           "no template, no script",
			policyTemplate: nil,
			pkScript:       nil,
			wantErr:        false,
		},
		{
			name:           "script without template rejected",
			policyTemplate: nil,
			pkScript:       validPkScript,
			wantErr:        true,
		},
		{
			name: "garbage template rejected",
			policyTemplate: []byte{
				0xff,
				0xff,
				0xff,
			},
			pkScript: nil,
			wantErr:  true,
		},
		{
			name:           "wrong operator rejected",
			policyTemplate: wrongOperatorTemplate,
			pkScript:       nil,
			wantErr:        true,
		},
		{
			name:           "valid template, no script",
			policyTemplate: validTemplate,
			pkScript:       nil,
			wantErr:        false,
		},
		{
			name:           "valid template, matching script",
			policyTemplate: validTemplate,
			pkScript:       validPkScript,
			wantErr:        false,
		},
		{
			name:           "valid template, mismatched script",
			policyTemplate: validTemplate,
			pkScript: append(
				[]byte{0x51, 0x20}, make([]byte, 32)...,
			),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateBoardPolicyTemplate(
				tc.policyTemplate, tc.pkScript, terms,
			)
			if !tc.wantErr {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			require.Equal(
				t, codes.InvalidArgument, status.Code(err),
			)
		})
	}
}
