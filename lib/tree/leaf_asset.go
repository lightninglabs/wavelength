package tree

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
)

// NewAssetLeafDescriptor constructs a LeafDescriptor with populated asset
// metadata. InputProof and labels are defensively copied to avoid external
// mutation after construction.
func NewAssetLeafDescriptor(pkScript []byte, amount btcutil.Amount,
	coSigner *btcec.PublicKey, inputProof []byte,
	assetAmount uint64, funding LeafFunding, changePkScript []byte,
	exitRebalance btcutil.Amount, labels map[string]string) LeafDescriptor {

	var (
		proofCopy  []byte
		changeCopy []byte
		labelCopy  map[string]string
	)

	if len(inputProof) > 0 {
		proofCopy = append([]byte(nil), inputProof...)
	}

	if len(changePkScript) > 0 {
		changeCopy = append([]byte(nil), changePkScript...)
	}

	if len(labels) > 0 {
		labelCopy = make(map[string]string, len(labels))
		for k, v := range labels {
			labelCopy[k] = v
		}
	}

	return LeafDescriptor{
		PkScript:    pkScript,
		Amount:      amount,
		CoSignerKey: coSigner,
		Asset: &AssetMetadata{
			InputProof:     proofCopy,
			AssetAmount:    assetAmount,
			Funding:        funding,
			ChangePkScript: changeCopy,
			ExitRebalance:  exitRebalance,
			Labels:         labelCopy,
		},
	}
}

// AnchorPlanToLeafDescriptor wraps NewAssetLeafDescriptor while validating the
// required fields that come from an anchor PSBT. It is the intended adapter
// between AssetTxBuilder outputs (anchor txout) and the tree builder.
func AnchorPlanToLeafDescriptor(output *wire.TxOut, coSigner *btcec.PublicKey,
	inputProof []byte, assetAmount uint64, funding LeafFunding,
	changePkScript []byte, exitRebalance btcutil.Amount,
	labels map[string]string) (LeafDescriptor, error) {

	if output == nil {
		return LeafDescriptor{}, fmt.Errorf("output required")
	}

	if coSigner == nil {
		return LeafDescriptor{}, fmt.Errorf("cosigner key required")
	}

	return NewAssetLeafDescriptor(
		output.PkScript, btcutil.Amount(output.Value), coSigner,
		inputProof, assetAmount, funding, changePkScript, exitRebalance,
		labels,
	), nil
}
