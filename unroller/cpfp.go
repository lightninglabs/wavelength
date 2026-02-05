package unroller

import (
	"context"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// childVSizeEstimate is the estimated virtual size of a CPFP
	// child transaction with 2 inputs (P2A anchor + P2TR wallet
	// UTXO) and 1 P2TR output (change). Used to calculate the
	// total package fee.
	childVSizeEstimate = 155

	// maxConfsForUTXO is the maximum confirmation count for
	// ListUnspent queries when selecting fee UTXOs.
	maxConfsForUTXO = int32(999999)

	// dustLimit is the minimum value for a change output. If the
	// change would be below this, it is donated as additional fee.
	dustLimit = btcutil.Amount(330)
)

// WalletKit defines wallet operations needed for CPFP child
// construction during package broadcasting. This narrow interface
// avoids coupling the unroller to the full lndclient.WalletKitClient
// (20+ methods), making test mocking trivial.
// lndclient.WalletKitClient satisfies this interface implicitly.
type WalletKit interface {
	// ListUnspent returns confirmed wallet UTXOs within the given
	// confirmation range.
	ListUnspent(ctx context.Context, minConfs, maxConfs int32,
		opts ...lndclient.ListUnspentOption,
	) ([]*lnwallet.Utxo, error)

	// NextAddr generates a new address from the wallet.
	NextAddr(ctx context.Context, accountName string,
		addressType walletrpc.AddressType,
		change bool) (btcutil.Address, error)

	// FinalizePsbt signs all wallet-owned inputs and finalizes
	// the PSBT into a complete transaction.
	FinalizePsbt(ctx context.Context, packet *psbt.Packet,
		account string) (*psbt.Packet, *wire.MsgTx, error)
}

// feeUTXO represents a confirmed wallet UTXO selected for fee
// payment in a CPFP child transaction.
type feeUTXO struct {
	outpoint wire.OutPoint
	output   *wire.TxOut
}

// selectFeeUTXO finds a confirmed wallet UTXO with sufficient value
// to cover the required fee. Selects the smallest sufficient UTXO to
// minimize change output size and waste.
func selectFeeUTXO(ctx context.Context, wk WalletKit,
	minValue btcutil.Amount) (*feeUTXO, error) {

	utxos, err := wk.ListUnspent(ctx, 1, maxConfsForUTXO)
	if err != nil {
		return nil, fmt.Errorf("list unspent: %w", err)
	}

	if len(utxos) == 0 {
		return nil, fmt.Errorf("no confirmed UTXOs available")
	}

	// Sort by value ascending so we pick the smallest sufficient
	// UTXO, minimizing change output size and waste.
	sort.Slice(utxos, func(i, j int) bool {
		return utxos[i].Value < utxos[j].Value
	})

	for _, utxo := range utxos {
		if utxo.Value >= minValue {
			return &feeUTXO{
				outpoint: utxo.OutPoint,
				output: &wire.TxOut{
					Value:    int64(utxo.Value),
					PkScript: utxo.PkScript,
				},
			}, nil
		}
	}

	return nil, fmt.Errorf(
		"no UTXO with sufficient value (need %d sat, "+
			"best has %d sat)",
		int64(minValue), int64(utxos[len(utxos)-1].Value),
	)
}

// buildCPFPChild constructs an unsigned V3 CPFP child transaction
// that spends the parent's P2A anchor output and a confirmed wallet
// UTXO, sending change back to the wallet.
func buildCPFPChild(parentTxid chainhash.Hash, anchorIdx uint32,
	fee *feeUTXO, changePkScript []byte,
	totalFee btcutil.Amount) (*wire.MsgTx, error) {

	// V3 for 1P1C package relay compatibility.
	childTx := wire.NewMsgTx(3)

	// Input 0: P2A anchor output (anyone-can-spend, empty
	// witness).
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  parentTxid,
			Index: anchorIdx,
		},
		Sequence: wire.MaxTxInSequenceNum,
	})

	// Input 1: confirmed wallet UTXO for fee payment.
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: fee.outpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	// Output 0: change back to wallet.
	changeValue := btcutil.Amount(fee.output.Value) - totalFee
	if changeValue < 0 {
		return nil, fmt.Errorf(
			"fee UTXO value %d insufficient for fee %d",
			fee.output.Value, int64(totalFee),
		)
	}

	// If change is below dust, donate it as extra fee.
	if changeValue >= dustLimit {
		childTx.AddTxOut(&wire.TxOut{
			Value:    int64(changeValue),
			PkScript: changePkScript,
		})
	}

	return childTx, nil
}

// signCPFPChild signs the CPFP child transaction using LND's PSBT
// signing flow. The P2A anchor input is pre-finalized with an empty
// witness. LND signs the wallet UTXO input.
func signCPFPChild(ctx context.Context, wk WalletKit,
	childTx *wire.MsgTx, anchorOut *wire.TxOut,
	fee *feeUTXO) (*wire.MsgTx, error) {

	// Build input outpoints and sequences for PSBT creation.
	inputs := make([]*wire.OutPoint, len(childTx.TxIn))
	sequences := make([]uint32, len(childTx.TxIn))
	for i, txIn := range childTx.TxIn {
		op := txIn.PreviousOutPoint
		inputs[i] = &op
		sequences[i] = txIn.Sequence
	}

	// Create the PSBT packet.
	packet, err := psbt.New(
		inputs, childTx.TxOut, childTx.Version,
		childTx.LockTime, sequences,
	)
	if err != nil {
		return nil, fmt.Errorf("create PSBT: %w", err)
	}

	// Input 0 (P2A anchor): set WitnessUtxo and finalize with
	// an empty witness. P2A (segwit v1 with 2-byte program) is
	// anyone-can-spend and requires no witness elements.
	packet.Inputs[0].WitnessUtxo = anchorOut
	packet.Inputs[0].FinalScriptWitness = []byte{0x00}

	// Input 1 (wallet UTXO): set WitnessUtxo so LND can sign.
	packet.Inputs[1].WitnessUtxo = fee.output

	// FinalizePsbt signs all wallet-owned inputs and finalizes.
	// It skips input 0 because FinalScriptWitness is already set.
	_, signedTx, err := wk.FinalizePsbt(ctx, packet, "")
	if err != nil {
		return nil, fmt.Errorf("finalize PSBT: %w", err)
	}

	return signedTx, nil
}

// estimateWeight computes the transaction weight including witness
// data. This is used to calculate the parent's virtual size for
// package fee computation.
func estimateWeight(tx *wire.MsgTx) int64 {
	// Base size (non-witness data).
	baseSize := int64(tx.SerializeSizeStripped())

	// Total size including witness.
	totalSize := int64(tx.SerializeSize())

	// Weight = base_size * 3 + total_size (BIP 141).
	return baseSize*3 + totalSize
}
