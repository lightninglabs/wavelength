package virtualchannel

import (
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
)

// BuildBackingTx builds the deterministic VTXO-to-channel-point transaction.
// The returned transaction is unsigned; negotiation must add the collaborative
// VTXO spend witnesses before the registration is persisted.
func BuildBackingTx(backingVTXOs []BackingVTXO, fundingOutput *wire.TxOut) (
	*wire.MsgTx, btcutil.Amount, error) {

	if len(backingVTXOs) == 0 {
		return nil, 0, fmt.Errorf("no backing VTXOs")
	}
	if fundingOutput == nil {
		return nil, 0, fmt.Errorf("funding output is nil")
	}
	if fundingOutput.Value <= 0 {
		return nil, 0, fmt.Errorf("funding output value must be " +
			"positive")
	}
	if len(fundingOutput.PkScript) == 0 {
		return nil, 0, fmt.Errorf("funding output script is empty")
	}

	backing := append([]BackingVTXO(nil), backingVTXOs...)
	sort.Slice(backing, func(i, j int) bool {
		left := backing[i].OutPoint.String()
		right := backing[j].OutPoint.String()

		return left < right
	})

	tx := wire.NewMsgTx(2)
	var total btcutil.Amount
	for _, input := range backing {
		if input.Amount <= 0 {
			return nil, 0, fmt.Errorf("backing VTXO %s amount "+
				"must be positive", input.OutPoint)
		}

		total += input.Amount
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: input.OutPoint,
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}

	outputValue := btcutil.Amount(fundingOutput.Value)
	if total < outputValue {
		return nil, 0, fmt.Errorf("backing total %d sats below "+
			"funding output %d sats", total, outputValue)
	}

	tx.AddTxOut(&wire.TxOut{
		Value:    fundingOutput.Value,
		PkScript: append([]byte(nil), fundingOutput.PkScript...),
	})

	return tx, total - outputValue, nil
}
