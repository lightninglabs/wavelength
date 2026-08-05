package tapassets

import (
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
)

// InventoryVerifierClient is the slice of the tap-sdk client the inventory
// verifier needs.
type InventoryVerifierClient = proofInventoryClient

// NewInventoryVerifier binds a confirmed proof to the operator's own tapd
// inventory: the proof tip must match the expected asset, amount, and
// anchor outpoint, and that anchor must appear in tapd's managed-UTXO
// inventory with the expected Taproot Asset commitment root. Round
// integrations use it to verify tranche proofs at plan and commit time.
func NewInventoryVerifier(client InventoryVerifierClient,
	assetRef tapsdk.AssetRef, amount uint64, anchor wire.OutPoint,
	assetRoot tapsdk.Hash) tapsdk.ConfirmedProofVerifier {

	return &proofInventoryVerifier{
		client:    client,
		assetRef:  assetRef,
		amount:    amount,
		anchor:    sdkOutpoint(anchor),
		assetRoot: assetRoot,
	}
}
