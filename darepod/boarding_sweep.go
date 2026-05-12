package darepod

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/wallet"
)

const (
	// defaultBoardingSweepFallbackFeeRateSatPerVByte is used when a
	// fresh regtest or backend cannot yet provide a fee estimate. Callers
	// can pass an explicit fee rate when recovering in hotter mempools.
	defaultBoardingSweepFallbackFeeRateSatPerVByte int64 = 2

	// defaultBoardingSweepConfTarget is used when the caller asks the
	// daemon to estimate the sweep fee rate without specifying a target.
	defaultBoardingSweepConfTarget uint32 = 6

	// boardingSweepHighFeeRateWarningSatPerVByte is only an operator
	// warning threshold. High fee estimates are still used, then the
	// aggregate fee is bounded by value below.
	boardingSweepHighFeeRateWarningSatPerVByte int64 = 100

	// defaultBoardingSweepMaxFeePercent refuses sweeps whose absolute fee
	// would burn too much of the selected boarding value.
	defaultBoardingSweepMaxFeePercent int64 = 25

	// defaultBoardingSweepMaxInputs caps one aggregate sweep below the
	// standard weight limit and keeps operator mistakes bounded.
	defaultBoardingSweepMaxInputs = 100

	// boardingSweepPolicyVersion is the spend-info policy version used by
	// existing boarding scripts.
	boardingSweepPolicyVersion = 1
)

// boardingSweepTx describes one signed boarding timeout-path sweep
// transaction and the fee paid by that transaction.
type boardingSweepTx struct {
	tx     *wire.MsgTx
	fee    btcutil.Amount
	vbytes int64
}

// boardingSweepInput holds a validated boarding input and its previous output.
type boardingSweepInput struct {
	intent       wallet.BoardingIntent
	targetOutput *wire.TxOut
}

// boardingSweepChainBackend is the chain subset needed to scan maturity,
// estimate fees, and broadcast sweep transactions.
type boardingSweepChainBackend interface {
	BestBlock(ctx context.Context) (int32, chainhash.Hash, error)

	EstimateFee(ctx context.Context,
		targetConf uint32) (btcutil.Amount, error)

	BroadcastTx(ctx context.Context, tx *wire.MsgTx, label string) error
}

// newBoardingStore returns a concrete boarding store for RPC-only direct
// reads and status updates.
func (s *Server) newBoardingStore() *db.BoardingWalletStore {
	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)

	return dbStore.NewBoardingStore(s.chainParams, s.clk)
}

// newSweepWallet returns the wallet adapter used to sign timeout-path sweep
// inputs and derive sweep destination scripts.
func (s *Server) newSweepWallet() (unroll.SweepWallet, error) {
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		if !s.lnd.IsSome() {
			return nil, fmt.Errorf("lnd wallet not initialized")
		}

		lndSvc := s.lnd.UnsafeFromSome()
		clientWallet := lndbackend.NewClientWallet(
			lndSvc.Signer, lndSvc.WalletKit,
		)
		boardingBackend := lndbackend.NewBoardingBackend(
			lndSvc.WalletKit, lndSvc.ChainKit,
		)

		return &lndUnrollWallet{
			ClientWallet:    clientWallet,
			boardingBackend: boardingBackend,
		}, nil

	case WalletTypeLwwallet:
		if !s.lwWallet.IsSome() {
			return nil, fmt.Errorf("lightweight wallet not " +
				"initialized")
		}

		return &lwUnrollWallet{
			Wallet: s.lwWallet.UnsafeFromSome(),
		}, nil

	case WalletTypeBtcwallet:
		if !s.btcwWallet.IsSome() {
			return nil, fmt.Errorf("btcwallet not initialized")
		}

		return &btcwUnrollWallet{
			Wallet: s.btcwWallet.UnsafeFromSome(),
		}, nil

	default:
		return nil, fmt.Errorf("unknown wallet type %q",
			s.cfg.Wallet.Type)
	}
}

// boardingSweepFeeRate resolves the caller's requested fee rate or asks the
// chain backend for an estimate at the requested confirmation target.
func boardingSweepFeeRate(ctx context.Context,
	chainBackend boardingSweepChainBackend, feeRateSatPerVByte int64,
	confTarget uint32) (int64, uint32, error) {

	if feeRateSatPerVByte > 0 {
		return feeRateSatPerVByte, confTarget, nil
	}
	if confTarget == 0 {
		confTarget = defaultBoardingSweepConfTarget
	}

	feeRate, err := chainBackend.EstimateFee(ctx, confTarget)
	if err != nil {
		return defaultBoardingSweepFallbackFeeRateSatPerVByte,
			confTarget, err
	}

	satPerVByte := int64(feeRate)
	switch {
	case satPerVByte <= 0:
		return defaultBoardingSweepFallbackFeeRateSatPerVByte,
			confTarget, nil

	default:
		return satPerVByte, confTarget, nil
	}
}

// boardingSweepMaturityHeight returns the first block height at which a
// confirmed boarding output's CSV timeout path can be spent.
func boardingSweepMaturityHeight(intent wallet.BoardingIntent) int32 {
	return intent.ChainInfo.ConfHeight + int32(intent.Address.ExitDelay)
}

// boardingSweepTargetOutput returns the actual txout being swept.
func boardingSweepTargetOutput(intent wallet.BoardingIntent) (*wire.TxOut,
	error) {

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

// buildBoardingSweepTx constructs and signs one timeout-path sweep transaction
// that spends all mature boarding UTXOs into one wallet output.
func buildBoardingSweepTx(sweepWallet unroll.SweepWallet,
	intents []wallet.BoardingIntent, sweepPkScript []byte,
	feeRateSatPerVByte int64) (*boardingSweepTx, error) {

	if sweepWallet == nil {
		return nil, fmt.Errorf("sweep wallet must be provided")
	}
	if len(intents) == 0 {
		return nil, fmt.Errorf("no sweep inputs")
	}
	if len(intents) > defaultBoardingSweepMaxInputs {
		return nil, fmt.Errorf("too many sweep inputs: %d "+
			"exceeds max %d", len(intents),
			defaultBoardingSweepMaxInputs)
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
	var signedSweep *boardingSweepTx
	for range 3 {
		// The first pass starts from a conservative estimate. Schnorr
		// sigs are fixed width, but iterating lets output value and
		// witness weight settle before we return fee and txid.
		signedSweep, err = signBoardingSweepTx(
			sweepWallet, inputs, totalInput, sweepPkScript,
			feeRateSatPerVByte, vbytes,
		)
		if err != nil {
			return nil, err
		}
		if signedSweep.vbytes == vbytes {
			return signedSweep, nil
		}

		vbytes = signedSweep.vbytes
	}

	return nil, fmt.Errorf("sweep vsize estimate did not converge")
}

// boardingSweepInputs validates each boarding intent and returns the total
// spendable input amount.
func boardingSweepInputs(intents []wallet.BoardingIntent) ([]boardingSweepInput,
	btcutil.Amount, error) {

	inputs := make([]boardingSweepInput, 0, len(intents))
	var totalInput btcutil.Amount
	for _, intent := range intents {
		targetOutput, err := boardingSweepTargetOutput(intent)
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
func signBoardingSweepTx(sweepWallet unroll.SweepWallet,
	inputs []boardingSweepInput, totalInput btcutil.Amount,
	sweepPkScript []byte, feeRateSatPerVByte,
	estimatedVBytes int64) (*boardingSweepTx, error) {

	fee := btcutil.Amount(feeRateSatPerVByte * estimatedVBytes)
	if err := validateBoardingSweepFee(totalInput, fee); err != nil {
		return nil, err
	}

	sweepValue := totalInput - fee
	if sweepValue <= 0 {
		return nil, fmt.Errorf("sweep value %d not positive "+
			"after fee %d", sweepValue, fee)
	}

	tx := wire.NewMsgTx(arktx.TxVersion)
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(inputs))
	for _, input := range inputs {
		intent := input.intent
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: intent.Outpoint,
			Sequence: blockchain.LockTimeToSequence(
				false, intent.Address.ExitDelay,
			),
		})
		prevOuts[intent.Outpoint] = input.targetOutput
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(sweepValue),
		PkScript: append([]byte(nil), sweepPkScript...),
	})

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	for idx, input := range inputs {
		intent := input.intent
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
			intent.Address.KeyDesc, input.targetOutput, sigHashes,
			prevFetcher, idx,
		)

		witness, err := arkscript.VTXOTimeoutSpendWitness(
			sweepWallet, signDesc, tx,
		)
		if err != nil {
			return nil, fmt.Errorf("timeout witness %s: %w",
				intent.Outpoint, err)
		}

		tx.TxIn[idx].Witness = witness
	}

	return &boardingSweepTx{
		tx:     tx,
		fee:    fee,
		vbytes: boardingSweepTxVBytes(tx),
	}, nil
}

// validateBoardingSweepFee refuses aggregate sweeps that would spend too much
// of the selected boarding value on miner fees.
func validateBoardingSweepFee(totalInput, fee btcutil.Amount) error {
	maxFee := totalInput * btcutil.Amount(
		defaultBoardingSweepMaxFeePercent,
	) / 100
	if fee <= maxFee {
		return nil
	}

	return fmt.Errorf("sweep fee %d exceeds max %d (%d%% of total "+
		"input %d)", fee, maxFee, defaultBoardingSweepMaxFeePercent,
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

// broadcastBoardingSweep broadcasts one signed boarding sweep and treats
// duplicate-broadcast style errors as success.
func broadcastBoardingSweep(ctx context.Context,
	chainBackend boardingSweepChainBackend, sweep *boardingSweepTx,
	label string) error {

	if sweep == nil || sweep.tx == nil {
		return fmt.Errorf("sweep transaction must be provided")
	}

	err := chainBackend.BroadcastTx(ctx, sweep.tx, label)
	if err != nil && !chainsource.IsIgnorableBroadcastError(err) {
		return fmt.Errorf("broadcast sweep: %w", err)
	}

	return nil
}
