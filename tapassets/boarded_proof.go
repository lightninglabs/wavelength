package tapassets

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
)

// ExportBoardedProof exports the confirmed proof file of an onboarded
// composed output from the owner's own tapd. The output is not part of
// tapd's wallet inventory (its script key is the round-scoped OP_TRUE
// spec), so the proof is located through the transfer that created it.
// A transfer that has not confirmed yet returns ErrBoardedProofPending.
func ExportBoardedProof(ctx context.Context, wallet *tapsdk.Wallet,
	outpoint wire.OutPoint) ([]byte, error) {

	if wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}

	client := wallet.Client()
	transfers, err := client.ListTransfers(
		ctx, &tapsdk.ListTransfersRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("list Taproot Asset transfers: %w", err)
	}

	target := sdkOutpoint(outpoint)
	anchorTxid := outpoint.Hash.String()
	for _, transfer := range transfers {
		if transfer == nil || transfer.AnchorTxid != anchorTxid {
			continue
		}
		if transfer.AnchorTxBlockHash == ([32]byte{}) {
			return nil, ErrBoardedProofPending
		}

		for i := range transfer.Outputs {
			out := &transfer.Outputs[i]
			if out.AnchorOutpoint != target {
				continue
			}

			exported, err := client.ExportProof(
				ctx, tapsdk.AssetRefFromAssetID(out.IssuanceID),
				out.ScriptKey, &out.AnchorOutpoint,
			)
			if err != nil {
				return nil, fmt.Errorf("export boarded "+
					"proof: %w", err)
			}
			if exported == nil ||
				len(exported.RawProofFile) == 0 {
				return nil, fmt.Errorf("exported boarded " +
					"proof is empty")
			}

			return exported.RawProofFile, nil
		}
	}

	return nil, fmt.Errorf("no transfer creates outpoint %v", outpoint)
}

// ErrBoardedProofPending reports an onboarding transfer that tapd has not
// seen confirm yet, so its proof file cannot be exported.
var ErrBoardedProofPending = fmt.Errorf("boarded asset proof is not " +
	"confirmed yet")
