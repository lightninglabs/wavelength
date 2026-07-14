package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type refreshCustomReq = waverpc.RefreshCustomVTXOsRequest

func newCustomRefreshRPCRequest(t *testing.T) *refreshCustomReq {
	t.Helper()

	policy, preimage, _, _, _ := testVHTLCPolicyFixture(t)
	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	claimPath, err := policy.ClaimPath(preimage)
	require.NoError(t, err)

	spendPath, err := claimPath.Encode()
	require.NoError(t, err)

	return &waverpc.RefreshCustomVTXOsRequest{
		Inputs: []*waverpc.CustomRefreshVTXOInput{{
			Outpoint:           testWalletOpsOutpoint(21).String(),
			AmountSat:          42_000,
			PkScript:           pkScript,
			VtxoPolicyTemplate: policyTemplate,
			AuthSpendPath:      spendPath,
			ForfeitSpendPath:   spendPath,
		}},
		Outputs: []*waverpc.CustomRefreshVTXOOutput{{
			AmountSat:          42_000,
			VtxoPolicyTemplate: policyTemplate,
		}},
	}
}

// TestRefreshCustomVTXOsDryRunDoesNotRequireWalletReady verifies that dry-run
// validation is a pure request-shape check. Swap daemons use this as a cheap
// preflight before coordinating counterparties, so malformed custom metadata
// should be reported without requiring an unlocked wallet or initialized wallet
// actor.
func TestRefreshCustomVTXOsDryRunDoesNotRequireWalletReady(t *testing.T) {
	t.Parallel()

	req := newCustomRefreshRPCRequest(t)
	req.DryRun = true

	resp, err := (&RPCServer{
		server: &Server{},
	}).RefreshCustomVTXOs(t.Context(),
		req,
	)
	require.NoError(t, err)
	require.Equal(t, "preview", resp.GetStatus())
	require.Equal(
		t, []string{testWalletOpsOutpoint(21).String()},
		resp.GetQueuedOutpoints(),
	)
}

// TestBuildCustomRefreshRequestPreservesFixedAmount verifies the RPC boundary
// carries the contract-output amount-safety bit into wallet admission.
func TestBuildCustomRefreshRequestPreservesFixedAmount(t *testing.T) {
	t.Parallel()

	req := newCustomRefreshRPCRequest(t)
	req.Outputs[0].FixedAmount = true

	_, outputs, _, err := buildCustomRefreshRequest(req)
	require.NoError(t, err)
	require.Len(t, outputs, 1)
	require.True(t, outputs[0].FixedAmount)
}

// TestRefreshCustomVTXOsRejectsMismatchedMetadata pins the RPC's local
// signature-oracle boundary. A caller may supply custom VTXOs that are not in
// the wallet store, but each semantic policy must still compile to the claimed
// pkScript, and each auth/forfeit spend path must bind to that same script.
func TestRefreshCustomVTXOsRejectsMismatchedMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mutate       func(*testing.T, *refreshCustomReq)
		wantContains string
	}{{
		name: "input policy mismatches pkScript",
		mutate: func(t *testing.T, req *refreshCustomReq) {
			t.Helper()

			req.Inputs[0].PkScript = append(
				[]byte(nil), req.Inputs[0].PkScript...,
			)
			req.Inputs[0].PkScript[len(req.Inputs[0].PkScript)-1] ^=
				0x01
		},
		wantContains: "input 0 policy template does not match",
	}, {
		name: "output policy mismatches pkScript",
		mutate: func(t *testing.T,
			req *waverpc.RefreshCustomVTXOsRequest) {

			t.Helper()

			req.Outputs[0].PkScript = append(
				[]byte(nil), req.Inputs[0].PkScript...,
			)
			last := len(req.Outputs[0].PkScript) - 1
			req.Outputs[0].PkScript[last] ^= 0x01
		},
		wantContains: "output 0 policy template does not match",
	}, {
		name: "auth spend path mismatches pkScript",
		mutate: func(t *testing.T, req *refreshCustomReq) {
			t.Helper()

			otherPolicy, otherPreimage, _, _, _ :=
				testVHTLCPolicyFixture(t)
			otherClaim, err := otherPolicy.ClaimPath(otherPreimage)
			require.NoError(t, err)
			req.Inputs[0].AuthSpendPath, err = otherClaim.Encode()
			require.NoError(t, err)
		},
		wantContains: "auth_spend_path does not bind",
	}, {
		name: "forfeit spend path mismatches pkScript",
		mutate: func(t *testing.T, req *refreshCustomReq) {
			t.Helper()

			otherPolicy, otherPreimage, _, _, _ :=
				testVHTLCPolicyFixture(t)
			otherClaim, err := otherPolicy.ClaimPath(otherPreimage)
			require.NoError(t, err)
			encoded, err := otherClaim.Encode()
			require.NoError(t, err)
			req.Inputs[0].ForfeitSpendPath = encoded
		},
		wantContains: "forfeit_spend_path does not bind",
	}, {
		name: "signing route missing",
		mutate: func(t *testing.T, req *refreshCustomReq) {
			t.Helper()

			req.Inputs[0].ForfeitSigningContext =
				&waverpc.ForfeitSigningContext{
					PaymentHash: make([]byte, 32),
				}
		},
		wantContains: "signing_route is required",
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			req := newCustomRefreshRPCRequest(t)
			req.DryRun = true
			test.mutate(t, req)

			_, err := (&RPCServer{
				server: &Server{},
			}).RefreshCustomVTXOs(t.Context(),
				req,
			)
			require.Equal(
				t, codes.InvalidArgument, status.Code(err),
			)
			require.Contains(
				t, status.Convert(err).Message(),
				test.wantContains,
			)
		})
	}
}
