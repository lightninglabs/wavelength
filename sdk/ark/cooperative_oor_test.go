package ark

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type cooperativeTestSigner struct {
	waverpc.DaemonServiceClient

	request  *waverpc.SignOORCustomInputRequest
	response *waverpc.SignOORCustomInputResponse
}

// SignOORCustomInput records the RPC variant used by the SDK.
func (s *cooperativeTestSigner) SignOORCustomInput(_ context.Context,
	req *waverpc.SignOORCustomInputRequest, _ ...grpc.CallOption) (
	*waverpc.SignOORCustomInputResponse, error) {

	s.request = req

	return s.response, nil
}

// TestSignOORCooperativePackage locks the remote signer API and prevents a
// partial legacy response from escaping as a valid cooperative authorization.
func TestSignOORCooperativePackage(t *testing.T) {
	t.Parallel()

	signer := &cooperativeTestSigner{}
	signer.response = &waverpc.SignOORCustomInputResponse{
		Signature: &waverpc.TaprootScriptSignature{
			Signature: []byte{
				1,
			},
		},
	}
	client := &Client{daemon: signer}
	in := CustomOORInput{PreserveSourceOwners: true, AmountSat: 5000}
	pkg := &PreparedOOR{ArkPSBT: []byte{1}, CheckpointPSBTs: [][]byte{{2}}}

	cp, payout, err := client.SignOORCooperativePackage(
		t.Context(), in, []byte{3}, 5000, pkg,
	)
	require.Error(t, err)
	require.Nil(t, cp)
	require.Nil(t, payout)
	require.Nil(t, signer.request.CustomInput)
	require.Empty(t, signer.request.CheckpointPsbt)
	wire := signer.request.CooperativePackage
	require.True(t, wire.CustomInput.PreserveSourceOwners)
	require.Equal(t, pkg.ArkPSBT, wire.ArkPsbt)
	require.Equal(t, pkg.CheckpointPSBTs[0], wire.CheckpointPsbt)
	require.EqualValues(t, 5000, wire.Recipient.AmountSat)
	require.Equal(t, []byte{3}, wire.Recipient.GetPubkey())

	signer.response.PayoutSignature = &waverpc.TaprootScriptSignature{
		Signature: []byte{
			2,
		},
	}
	cp, payout, err = client.SignOORCooperativePackage(
		t.Context(), in, []byte{3}, 5000, pkg,
	)
	require.NoError(t, err)
	in.ExternalSignatures = []TaprootScriptSignature{*cp}
	in.PayoutSignatures = []TaprootScriptSignature{*payout}
	send := customOORInputToRPC(in)
	signer.response.Signature.Signature[0]++
	signer.response.PayoutSignature.Signature[0]++
	require.Equal(t, []byte{1}, send.ExternalSignatures[0].Signature)
	require.Equal(t, []byte{2}, send.PayoutSignatures[0].Signature)
}
