package unroll

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/input"
)

// BuildSignedExitSweep presigns a standard VTXO timeout spend before ancestry
// materialization. The full value pays the destination; a zero-value P2A anchor
// lets an external executor supply all fees through its own CPFP child. The
// signature commits to both outputs. Broadcast must wait for CSV maturity.
func BuildSignedExitSweep(desc *vtxo.Descriptor, proof *recovery.Proof,
	destination []byte, signer input.Signer) (*wire.MsgTx, error) {

	if desc == nil || proof == nil || signer == nil {
		return nil, fmt.Errorf("descriptor, proof, and signer are " +
			"required")
	}
	if desc.TaprootAssetRoot != nil {
		return nil, vtxo.ErrAssetVTXORequiresTransition
	}
	if err := validateProofDescriptorActive(desc); err != nil {
		return nil, err
	}
	if desc.Outpoint != proof.TargetOutpoint() ||
		desc.RelativeExpiry != proof.CSVDelay() {
		return nil, fmt.Errorf("sweep descriptor does not match proof")
	}
	if desc.RelativeExpiry == 0 ||
		desc.RelativeExpiry > recovery.MaxCSVDelay {
		return nil, fmt.Errorf("sweep CSV delay is outside the " +
			"supported range")
	}
	target, err := proof.TargetOutput()
	if err != nil {
		return nil, err
	}
	policy := NewStandardVTXOExitSpendPolicy(desc)
	if err := policy.ValidateTarget(target); err != nil {
		return nil, err
	}
	if target.Value != int64(desc.Amount) || len(desc.PkScript) == 0 {
		return nil, fmt.Errorf("sweep target value or script does " +
			"not match descriptor")
	}
	if len(destination) == 0 || txscript.IsUnspendable(destination) ||
		bytes.Equal(destination, arkscript.AnchorPkScript) {
		return nil, fmt.Errorf("sweep destination must be a " +
			"spendable script")
	}
	output := &wire.TxOut{
		Value: target.Value, PkScript: bytes.Clone(destination),
	}
	if err := txrules.CheckOutput(
		output, txrules.DefaultRelayFeePerKb,
	); err != nil {
		return nil, fmt.Errorf("sweep destination output: %w", err)
	}

	// A zero-value ephemeral anchor must accompany a zero-fee parent.
	// Preserving the full target value also keeps future fee changes out
	// of the owner's signature; the executor reprices its own child.
	sweep := wire.NewMsgTx(arktx.TxVersion)
	sweep.AddTxIn(&wire.TxIn{
		PreviousOutPoint: desc.Outpoint,
		Sequence: blockchain.LockTimeToSequence(
			false, desc.RelativeExpiry,
		),
	})
	sweep.AddTxOut(output)
	sweep.AddTxOut(arkscript.AnchorOutput())
	sweep, err = policy.signSpendTx(ExitSpendRequest{
		TargetOutpoint:      desc.Outpoint,
		TargetOutput:        target,
		DestinationPkScript: destination,
		Signer:              signer,
	}, sweep)
	if err != nil {
		return nil, err
	}
	if err := verifyTxWitnesses(sweep, map[wire.OutPoint]*wire.TxOut{
		desc.Outpoint: target,
	}); err != nil {
		return nil, fmt.Errorf("verify signed exit sweep: %w", err)
	}

	return sweep, nil
}
