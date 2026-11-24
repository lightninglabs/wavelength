package ark

import (
	"bytes"
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/waverpc"
)

// SignOORCooperativePackage asks the local wallet to independently rebuild the
// authorized transfer before signing both packets. The caller authenticates
// the recipient against its own swap state, not the signer's proposed PSBTs.
// Returns the checkpoint-input signature followed by the final Ark signature.
func (c *Client) SignOORCooperativePackage(ctx context.Context,
	input CustomOORInput, recipientPubKey []byte, amountSat int64,
	prepared *PreparedOOR) (*TaprootScriptSignature,
	*TaprootScriptSignature, error) {

	if !input.PreserveSourceOwners || prepared == nil ||
		len(prepared.ArkPSBT) == 0 ||
		len(prepared.CheckpointPSBTs) != 1 ||
		len(prepared.CheckpointPSBTs[0]) == 0 {
		return nil, nil, fmt.Errorf("unsigned cooperative package " +
			"required")
	}
	pkg := &waverpc.CooperativeOORPackage{
		CustomInput: customOORInputToRPC(input),
		Recipient: &waverpc.Output{
			Destination: &waverpc.Output_Pubkey{
				Pubkey: bytes.Clone(recipientPubKey),
			},
			AmountSat: amountSat,
		},
		CheckpointPsbt: bytes.Clone(prepared.CheckpointPSBTs[0]),
		ArkPsbt:        bytes.Clone(prepared.ArkPSBT),
	}
	// Leave both legacy fields empty so an older daemon refuses the request
	// rather than silently signing just the checkpoint.
	resp, err := c.daemon.SignOORCustomInput(
		ctx, &waverpc.SignOORCustomInputRequest{
			CooperativePackage: pkg,
		},
	)
	if err != nil {
		return nil, nil, err
	}
	if resp.GetSignature() == nil || resp.GetPayoutSignature() == nil {
		return nil, nil, fmt.Errorf("wallet did not return both " +
			"signatures")
	}

	return signatureFromRPC(resp.Signature), signatureFromRPC(
		resp.PayoutSignature,
	), nil
}

// signatureFromRPC returns an SDK-owned copy of a wallet signature.
func signatureFromRPC(
	sig *waverpc.TaprootScriptSignature) *TaprootScriptSignature {

	return &TaprootScriptSignature{
		PubKey:        bytes.Clone(sig.Pubkey),
		WitnessScript: bytes.Clone(sig.WitnessScript),
		Signature:     bytes.Clone(sig.Signature),
		SigHash:       sig.Sighash,
	}
}
