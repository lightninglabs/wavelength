package serverconn

import (
	"testing"

	"github.com/lightninglabs/wavelength/arkrpc"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	"github.com/stretchr/testify/require"
)

// activeArkPolicy builds an ACTIVE policy for the given version, used to
// populate the operator's advertised policy list in fake GetInfo responses.
func activeArkPolicy(version uint32) *arkrpc.ArkVersionPolicy {
	return &arkrpc.ArkVersionPolicy{
		Version: version,
		State:   arkrpc.ArkVersionPolicy_STATE_ACTIVE,
	}
}

// disabledArkPolicy builds a DISABLED policy for the given version.
func disabledArkPolicy(version uint32) *arkrpc.ArkVersionPolicy {
	return &arkrpc.ArkVersionPolicy{
		Version: version,
		State:   arkrpc.ArkVersionPolicy_STATE_DISABLED,
	}
}

// TestResolveArkVersionSelection covers the pure selection decision: normal
// preference selection, an operator selecting an unsupported version, a zero
// selection (no overlap or pre-versioning server) which is always fatal, and
// a no-overlap response with a non-empty list.
func TestResolveArkVersionSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		resp         *arkrpc.GetInfoResponse
		client       []uint32
		wantSelected uint32
		wantErr      bool
	}{
		{
			name: "normal v1 selection",
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 1,
			},
			client: []uint32{
				1,
			},
			wantSelected: 1,
		},
		{
			name: "operator selects v2 client supports it",
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 2,
			},
			client: []uint32{
				1,
				2,
			},
			wantSelected: 2,
		},
		{
			name: "operator selects unsupported version",
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 2,
			},
			client: []uint32{
				1,
			},
			wantErr: true,
		},
		{
			name: "zero selection is fatal",
			resp: &arkrpc.GetInfoResponse{},
			client: []uint32{
				1,
			},
			wantErr: true,
		},
		{
			name: "no overlap is fatal",
			resp: &arkrpc.GetInfoResponse{
				ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
					activeArkPolicy(2),
					activeArkPolicy(3),
				},
			},
			client: []uint32{
				1,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			selected, err := resolveArkVersionSelection(
				tc.resp, tc.client,
			)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantSelected, selected)
		})
	}
}

// TestValidateRefreshSelection proves a refresh cannot renegotiate: it accepts
// only a matching selection and treats any zero or different selection as a
// terminal ARK_VERSION_MISMATCH. There is no legacy fallback.
func TestValidateRefreshSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		boundArk uint32
		resp     *arkrpc.GetInfoResponse
		wantErr  bool
	}{
		{
			name:     "matching v1",
			boundArk: 1,
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 1,
			},
		},
		{
			name:     "matching v2",
			boundArk: 2,
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 2,
			},
		},
		{
			name:     "v1 rejects zero selection",
			boundArk: 1,
			resp:     &arkrpc.GetInfoResponse{},
			wantErr:  true,
		},
		{
			name:     "v1 rejects nil response",
			boundArk: 1,
			resp:     nil,
			wantErr:  true,
		},
		{
			name:     "v1 rejects different selection",
			boundArk: 1,
			resp: &arkrpc.GetInfoResponse{
				SelectedArkVersion: 2,
			},
			wantErr: true,
		},
		{
			name:     "v2 rejects zero selection",
			boundArk: 2,
			resp:     &arkrpc.GetInfoResponse{},
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ValidateRefreshSelection returns a typed pointer;
			// compare the pointer directly to avoid the
			// nil-interface pitfall.
			statusErr := ValidateRefreshSelection(
				tc.resp, tc.boundArk,
			)
			if tc.wantErr {
				require.NotNil(t, statusErr)
				require.Equal(
					t, mailboxconn.StatusArkVersionMismatch,
					statusErr.Code(),
				)

				return
			}

			require.Nil(t, statusErr)
		})
	}
}

// TestValidateRefreshSelectionSelectedButDisabled proves a refresh that
// re-selects the bound version but advertises it as DISABLED is a terminal,
// mandatory-upgrade failure: a permanent UPGRADE_REQUIRED status error,
// distinct from the renegotiation mismatch path.
func TestValidateRefreshSelectionSelectedButDisabled(t *testing.T) {
	t.Parallel()

	resp := &arkrpc.GetInfoResponse{
		SelectedArkVersion: 1,
		ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
			disabledArkPolicy(1),
		},
	}

	statusErr := ValidateRefreshSelection(resp, 1)
	require.NotNil(t, statusErr)
	require.Equal(
		t, mailboxconn.StatusUpgradeRequired, statusErr.Code(),
	)
}

// TestValidateRefreshSelectionActivePolicyOk proves a matching selection with a
// non-disabled (here ACTIVE) policy for the bound version is a normal, accepted
// refresh — an advertised policy alone does not make a refresh fail.
func TestValidateRefreshSelectionActivePolicyOk(t *testing.T) {
	t.Parallel()

	resp := &arkrpc.GetInfoResponse{
		SelectedArkVersion: 1,
		ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
			activeArkPolicy(1),
		},
	}

	require.Nil(t, ValidateRefreshSelection(resp, 1))
}
