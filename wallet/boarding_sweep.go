package wallet

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightningnetwork/lnd/input"
)

// Boarding-sweep tunables. These mirror the values that previously lived in
// darepod/boarding_sweep.go; the values are unchanged so operator experience
// stays identical across the move.
const (
	// DefaultBoardingSweepFallbackFeeRateSatPerVByte is used when no
	// caller-supplied fee rate is given and the chain backend cannot
	// produce a fresh estimate.
	DefaultBoardingSweepFallbackFeeRateSatPerVByte int64 = 2

	// DefaultBoardingSweepConfTarget is the confirmation target used when
	// the caller asks the daemon to estimate the sweep fee rate without
	// specifying a target.
	DefaultBoardingSweepConfTarget uint32 = 6

	// BoardingSweepHighFeeRateWarningSatPerVByte is the threshold above
	// which the daemon emits a warning. High estimates are still used —
	// the absolute fee is bounded by the value-percent guard below.
	BoardingSweepHighFeeRateWarningSatPerVByte int64 = 100

	// DefaultBoardingSweepMaxFeePercent refuses sweeps whose absolute fee
	// would burn more than this percent of the selected boarding value.
	DefaultBoardingSweepMaxFeePercent int64 = 25

	// DefaultBoardingSweepMaxInputs caps one aggregate sweep below the
	// standard transaction weight limit.
	DefaultBoardingSweepMaxInputs = 100

	// boardingSweepPolicyVersion is the spend-info policy version used by
	// existing boarding scripts.
	boardingSweepPolicyVersion = 1

	// boardingSweepAnchorValue is the sat amount paid into the P2A anchor
	// output appended to the sweep tx. The parent pays its own miner fee,
	// so the anchor cannot be zero-value — BIP-433 (Ephemeral Dust)
	// requires that a tx carrying a sub-dust output pay zero fee. The
	// P2A dust threshold is 240 sats; 330 is a conventional safe value
	// above that threshold. The anchor stays anyone-can-spend, so a
	// fee-bump tool that supports above-dust anchors can still attach a
	// CPFP child if needed.
	boardingSweepAnchorValue = int64(330)
)

// SweepSigner provides the cryptographic and address operations needed when
// assembling a boarding-timeout sweep transaction. It deliberately mirrors
// unroll.SweepWallet's shape so the existing per-backend adapters
// (lndUnrollWallet, lwUnrollWallet, btcwUnrollWallet) satisfy this
// interface without modification, allowing the boarding-sweep helpers to
// live in the wallet package without taking an unroll/ dependency.
type SweepSigner interface {
	input.Signer

	// NewWalletPkScript returns a fresh wallet-managed destination script
	// for the sweep output.
	NewWalletPkScript(ctx context.Context) ([]byte, error)
}

// The boarding-sweep flow consumes the shared txconfirm.TxBroadcasterActor
// directly via its actor ref. The cycle that previously required an
// adapter interface here was broken by moving LockID / Utxo / OutputLeaser
// to walletcore, so wallet/ can now import txconfirm/ without re-introducing
// the cycle.

// BoardingChainBackend is the chain subset needed to scan maturity, estimate
// fees, and broadcast sweep transactions. Concrete chain backends (such as
// chainsource.ChainBackend) satisfy this interface structurally without
// importing the wallet package.
type BoardingChainBackend interface {
	BestBlock(ctx context.Context) (int32, chainhash.Hash, error)

	EstimateFee(ctx context.Context,
		targetConf uint32) (btcutil.Amount, error)

	BroadcastTx(ctx context.Context, tx *wire.MsgTx, label string) error
}

// IsIgnorableBroadcastError reports whether a broadcast error should be
// treated as a benign duplicate. Concrete chain backends provide this through
// their package-level helper (chainsource.IsIgnorableBroadcastError); the
// move-only commit forwards via this hook to keep callers wallet-domain.
type IsIgnorableBroadcastError func(error) bool

// BoardingSweepTx describes one signed boarding timeout-path sweep
// transaction and the fee paid by that transaction.
type BoardingSweepTx struct {
	Tx     *wire.MsgTx
	Fee    btcutil.Amount
	VBytes int64
}

// boardingSweepInput holds a validated boarding input and its previous output.
type boardingSweepInput struct {
	intent       BoardingIntent
	targetOutput *wire.TxOut
}

// BoardingSweepFeeRate resolves the caller's requested fee rate or asks the
// chain backend for an estimate at the requested confirmation target.
func BoardingSweepFeeRate(ctx context.Context,
	chainBackend BoardingChainBackend, feeRateSatPerVByte int64,
	confTarget uint32) (int64, uint32, error) {

	if feeRateSatPerVByte > 0 {
		return feeRateSatPerVByte, confTarget, nil
	}
	if confTarget == 0 {
		confTarget = DefaultBoardingSweepConfTarget
	}

	feeRate, err := chainBackend.EstimateFee(ctx, confTarget)
	if err != nil {
		return DefaultBoardingSweepFallbackFeeRateSatPerVByte,
			confTarget, err
	}

	satPerVByte := int64(feeRate)
	switch {
	case satPerVByte <= 0:
		return DefaultBoardingSweepFallbackFeeRateSatPerVByte,
			confTarget, nil

	default:
		return satPerVByte, confTarget, nil
	}
}

// BoardingSweepMaturityHeight returns the first block height at which a
// confirmed boarding output's CSV timeout path can be spent.
func BoardingSweepMaturityHeight(intent BoardingIntent) int32 {
	return intent.ChainInfo.ConfHeight + int32(intent.Address.ExitDelay)
}

// BoardingSweepTargetOutput returns the actual txout being swept.
func BoardingSweepTargetOutput(intent BoardingIntent) (*wire.TxOut, error) {
	tx := intent.ChainInfo.ConfTx
	if tx == nil {
		return nil, fmt.Errorf("boarding intent missing confirmation " +
			"tx")
	}
	if tx.TxHash() != intent.Outpoint.Hash {
		return nil, fmt.Errorf("boarding confirmation tx mismatch")
	}

	index := intent.Outpoint.Index
	if index >= uint32(len(tx.TxOut)) {
		return nil, fmt.Errorf("boarding outpoint index %d out "+
			"of range", index)
	}

	targetOutput := tx.TxOut[index]
	wantScript, err := txscript.PayToAddrScript(intent.Address.Address)
	if err != nil {
		return nil, fmt.Errorf("boarding address pkscript: %w", err)
	}
	if !bytes.Equal(targetOutput.PkScript, wantScript) {
		return nil, fmt.Errorf("boarding target pkscript mismatch")
	}

	return targetOutput, nil
}

// BuildBoardingSweepTx constructs and signs one timeout-path sweep transaction
// that spends all mature boarding UTXOs into one wallet output. The transaction
// is v3/TRUC and always carries an above-dust P2A anchor output appended after
// the wallet output. The parent pays its own miner fee — the anchor is sized
// above the BIP-433 P2A dust threshold (240 sats) rather than zero-value so the
// parent does not trip the ephemeral-dust rule that "tx with dust output must
// be 0-fee", and remains anyone-can-spend so a future fee-bump tool can still
// attach a CPFP child if the initial fee rate becomes uncompetitive.
func BuildBoardingSweepTx(signer SweepSigner, intents []BoardingIntent,
	sweepPkScript []byte,
	feeRateSatPerVByte int64) (*BoardingSweepTx, error) {

	if signer == nil {
		return nil, fmt.Errorf("sweep signer must be provided")
	}
	if len(intents) == 0 {
		return nil, fmt.Errorf("no sweep inputs")
	}
	if len(intents) > DefaultBoardingSweepMaxInputs {
		return nil, fmt.Errorf("too many sweep inputs: %d "+
			"exceeds max %d", len(intents),
			DefaultBoardingSweepMaxInputs)
	}
	if feeRateSatPerVByte <= 0 {
		return nil, fmt.Errorf("fee rate must be positive")
	}

	inputs, totalInput, err := boardingSweepInputs(intents)
	if err != nil {
		return nil, err
	}

	if len(sweepPkScript) == 0 {
		return nil, fmt.Errorf("sweep pkscript must be provided")
	}

	vbytes := estimateBoardingSweepVBytes(len(inputs))
	var signedSweep *BoardingSweepTx
	for range 3 {
		// The first pass starts from a conservative estimate. Schnorr
		// sigs are fixed width, but iterating lets output value and
		// witness weight settle before we return fee and txid.
		signedSweep, err = signBoardingSweepTx(
			signer, inputs, totalInput, sweepPkScript,
			feeRateSatPerVByte, vbytes,
		)
		if err != nil {
			return nil, err
		}
		if signedSweep.VBytes == vbytes {
			return signedSweep, nil
		}

		vbytes = signedSweep.VBytes
	}

	return nil, fmt.Errorf("sweep vsize estimate did not converge")
}

// boardingSweepInputs validates each boarding intent and returns the total
// spendable input amount.
func boardingSweepInputs(intents []BoardingIntent) ([]boardingSweepInput,
	btcutil.Amount, error) {

	inputs := make([]boardingSweepInput, 0, len(intents))
	var totalInput btcutil.Amount
	for _, intent := range intents {
		targetOutput, err := BoardingSweepTargetOutput(intent)
		if err != nil {
			return nil, 0, err
		}
		if intent.Address.KeyDesc.PubKey == nil {
			return nil, 0, fmt.Errorf("boarding intent %s missing "+
				"client key", intent.Outpoint)
		}
		if intent.Address.OperatorKey == nil {
			return nil, 0, fmt.Errorf("boarding intent %s missing "+
				"operator key", intent.Outpoint)
		}

		inputs = append(inputs, boardingSweepInput{
			intent:       intent,
			targetOutput: targetOutput,
		})
		totalInput += btcutil.Amount(targetOutput.Value)
	}

	return inputs, totalInput, nil
}

// signBoardingSweepTx builds a transaction for the target vsize estimate and
// signs every boarding timeout input.
func signBoardingSweepTx(signer SweepSigner, inputs []boardingSweepInput,
	totalInput btcutil.Amount, sweepPkScript []byte, feeRateSatPerVByte,
	estimatedVBytes int64) (*BoardingSweepTx, error) {

	fee := btcutil.Amount(feeRateSatPerVByte * estimatedVBytes)
	if err := validateBoardingSweepFee(totalInput, fee); err != nil {
		return nil, err
	}

	// The above-dust anchor output (see boardingSweepAnchorValue) is paid
	// from the swept value, not from miner fees. Deduct it before sizing
	// the main wallet output so the input/output sum matches the signed
	// witness commitment.
	anchorValue := btcutil.Amount(boardingSweepAnchorValue)
	sweepValue := totalInput - fee - anchorValue
	if sweepValue <= 0 {
		return nil, fmt.Errorf("sweep value %d not positive after fee "+
			"%d and anchor %d", sweepValue, fee, anchorValue)
	}

	tx := wire.NewMsgTx(arktx.TxVersion)
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(inputs))
	for _, in := range inputs {
		intent := in.intent
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: intent.Outpoint,
			Sequence: blockchain.LockTimeToSequence(
				false, intent.Address.ExitDelay,
			),
		})
		prevOuts[intent.Outpoint] = in.targetOutput
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(sweepValue),
		PkScript: append([]byte(nil), sweepPkScript...),
	})

	// Append the P2A anchor as the last output. The anchor is above the
	// P2A relay-dust threshold (240 sats per BIP-433) because this
	// parent tx pays its own miner fee — BIP-433's ephemeral-dust rule
	// requires that any tx carrying a sub-dust output pay zero fee,
	// which would otherwise make every operator-initiated sweep fail at
	// submission with "dust, tx with dust output must be 0-fee". The
	// anchor remains anyone-can-spend, so a fee-bump tool that supports
	// above-dust anchors can still attach a CPFP child later. TxOut[0]
	// remains the wallet output and stays the canonical
	// ConfirmationPkScript the actor passes to txconfirm.
	tx.AddTxOut(
		arkscript.AnchorOutput(
			arkscript.WithAnchorValue(boardingSweepAnchorValue),
		),
	)

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	for idx, in := range inputs {
		intent := in.intent
		spendInfo, err := arkscript.NewVTXOSpendInfoFromPolicy(
			intent.Address.KeyDesc.PubKey,
			intent.Address.OperatorKey, intent.Address.ExitDelay,
			boardingSweepPolicyVersion,
		)
		if err != nil {
			return nil, fmt.Errorf("timeout spend info %s: %w",
				intent.Outpoint, err)
		}

		signDesc := spendInfo.BuildSignDescriptor(
			intent.Address.KeyDesc, in.targetOutput, sigHashes,
			prevFetcher, idx,
		)

		witness, err := arkscript.VTXOTimeoutSpendWitness(
			signer, signDesc, tx,
		)
		if err != nil {
			return nil, fmt.Errorf("timeout witness %s: %w",
				intent.Outpoint, err)
		}

		tx.TxIn[idx].Witness = witness
	}

	return &BoardingSweepTx{
		Tx:     tx,
		Fee:    fee,
		VBytes: boardingSweepTxVBytes(tx),
	}, nil
}

// validateBoardingSweepFee refuses aggregate sweeps that would spend too much
// of the selected boarding value on miner fees.
func validateBoardingSweepFee(totalInput, fee btcutil.Amount) error {
	maxFee := totalInput * btcutil.Amount(
		DefaultBoardingSweepMaxFeePercent,
	) / 100
	if fee <= maxFee {
		return nil
	}

	return fmt.Errorf("sweep fee %d exceeds max %d (%d%% of total "+
		"input %d)", fee, maxFee, DefaultBoardingSweepMaxFeePercent,
		totalInput)
}

// estimateBoardingSweepVBytes returns the first-pass vsize estimate for an
// aggregate boarding sweep.
func estimateBoardingSweepVBytes(inputCount int) int64 {
	const (
		// These constants are deliberately loose for the first pass.
		// The builder signs the tx and recomputes exact vsize before
		// returning, so this only avoids an initial underpayment.
		boardingSweepBaseVBytes     = int64(40)
		boardingSweepPerInputVBytes = int64(160)
	)

	return boardingSweepBaseVBytes +
		int64(inputCount)*boardingSweepPerInputVBytes
}

// boardingSweepTxVBytes returns the signed transaction virtual size in vbytes.
func boardingSweepTxVBytes(tx *wire.MsgTx) int64 {
	weight := tx.SerializeSizeStripped()*3 + tx.SerializeSize()

	return int64((weight + 3) / 4)
}

// BoardingSweepPkScript returns the caller-provided destination script or
// asks the wallet for a fresh sweep address when no override is set. The
// preview / broadcast distinction matches the original RPC behaviour: a
// preview without a caller-supplied address uses a fixed placeholder so
// fee estimation does not allocate a real wallet output.
func BoardingSweepPkScript(ctx context.Context, signer SweepSigner,
	chainParams *chaincfg.Params, sweepAddress string,
	broadcast bool) ([]byte, error) {

	if sweepAddress == "" {
		if !broadcast {
			return BoardingSweepPreviewPkScript(), nil
		}

		pkScript, err := signer.NewWalletPkScript(ctx)
		if err != nil {
			return nil, fmt.Errorf("sweep pkscript: %w", err)
		}
		if len(pkScript) == 0 {
			return nil, fmt.Errorf("wallet returned empty pkscript")
		}

		return pkScript, nil
	}

	addr, err := btcutil.DecodeAddress(sweepAddress, chainParams)
	if err != nil {
		return nil, fmt.Errorf("decode sweep address: %w", err)
	}
	if !addr.IsForNet(chainParams) {
		return nil, fmt.Errorf("sweep address is for the wrong network")
	}

	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("sweep address pkscript: %w", err)
	}

	return pkScript, nil
}

// BoardingSweepPreviewPkScript returns the fixed-size P2TR placeholder script
// used in preview mode when no caller destination is supplied. This avoids
// allocating a fresh wallet address just to estimate the aggregate sweep fee;
// broadcast sweeps use the real wallet-provided script instead, so their
// estimate matches the actual destination type.
func BoardingSweepPreviewPkScript() []byte {
	const p2trProgramLen = 32

	pkScript := make([]byte, 2+p2trProgramLen)
	pkScript[0] = txscript.OP_1
	pkScript[1] = p2trProgramLen

	return pkScript
}

// BroadcastBoardingSweep broadcasts one signed boarding sweep and treats
// duplicate-broadcast style errors as success when isIgnorable is supplied.
// isIgnorable is provided by the chain-backend caller (it has package-level
// access to the underlying error classification) so the wallet package
// itself does not need to depend on chainsource for this single helper.
func BroadcastBoardingSweep(ctx context.Context,
	chainBackend BoardingChainBackend, sweep *BoardingSweepTx, label string,
	isIgnorable IsIgnorableBroadcastError) error {

	if sweep == nil || sweep.Tx == nil {
		return fmt.Errorf("sweep transaction must be provided")
	}

	err := chainBackend.BroadcastTx(ctx, sweep.Tx, label)
	if err == nil {
		return nil
	}
	if isIgnorable != nil && isIgnorable(err) {
		return nil
	}

	return fmt.Errorf("broadcast sweep: %w", err)
}
