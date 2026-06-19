package darepod

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo-client/walletcore"
	"github.com/lightningnetwork/lnd/input"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultExitPlanConfTarget uint32 = 6
	walletSweepTxVersion      int32  = 2

	walletSweepDustFailureReason = "sweep amount is dust after fees"
)

var walletSweepLockID = func() walletcore.LockID {
	var id walletcore.LockID
	copy(id[:], "darepo-client:sweepwallet")

	return id
}()

// ExitPlanRequest asks the daemon to preview unilateral-exit funding
// readiness for a slice of VTXO outpoints.
type ExitPlanRequest struct {
	Outpoints  []string
	ConfTarget uint32
}

// ExitPlanEntry describes the unroll CPFP fee-input readiness for a single
// previewed VTXO outpoint.
type ExitPlanEntry struct {
	Outpoint                   string
	FundingAddress             string
	RequiredConfirmations      uint32
	RequiredFeeUTXOCount       uint32
	UsableFeeUTXOCount         uint32
	RecommendedUTXOAmountSat   int64
	RecommendedTotalFundingSat int64
	FundingShortfallSat        int64
	CanStart                   bool
	ExitJobFound               bool
	ExitStatus                 daemonrpc.UnrollJobStatus
	SweepTxid                  *chainhash.Hash
	LastError                  error

	// Err is a per-outpoint failure (e.g. VTXO not found) so one bad
	// outpoint does not fail the whole batch. Nil on success.
	Err error
}

// ExitPlanResponse describes the combined unroll CPFP fee-input readiness for
// every previewed outpoint plus aggregate totals.
type ExitPlanResponse struct {
	Plans                      []ExitPlanEntry
	FeeRateSatPerVByte         int64
	CanStart                   bool
	TotalFundingShortfallSat   int64
	TotalRecommendedFundingSat int64
}

// WalletSweepInput describes one backing-wallet UTXO selected by SweepWallet.
type WalletSweepInput struct {
	Outpoint  string
	AmountSat int64
}

// SweepWalletRequest asks the daemon to preview or publish a backing-wallet
// sweep.
type SweepWalletRequest struct {
	DestinationAddress string
	Broadcast          bool
	FeeRateSatPerVByte int64
	ConfTarget         uint32
}

// SweepWalletResponse describes the selected inputs and optional broadcast tx.
type SweepWalletResponse struct {
	Inputs             []WalletSweepInput
	TotalInputSat      int64
	EstimatedFeeSat    int64
	NetAmountSat       int64
	FeeRateSatPerVByte int64
	CanBroadcast       bool
	Txid               *chainhash.Hash
	FailureReason      error
}

// GetExitPlan previews the wallet-side resources needed before admitting a
// unilateral exit for a slice of VTXO outpoints. Per-outpoint failures are
// recorded on the corresponding entry rather than failing the whole batch;
// only request-wide failures return a top-level error.
func (r *RPCServer) GetExitPlan(ctx context.Context, req *ExitPlanRequest) (
	*ExitPlanResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(
			codes.InvalidArgument, "request is nil",
		)
	}
	if len(req.Outpoints) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "outpoints is required",
		)
	}
	if r.server.vtxoStore == nil {
		return nil, status.Error(
			codes.Unavailable, "VTXO store not initialized",
		)
	}

	// The fee estimate and wallet snapshot are wallet-wide, so compute
	// them once and reuse them for every previewed outpoint.
	feeRate, err := r.estimateWalletFeeRate(ctx, req.ConfTarget)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "estimate fee: %v",
			err)
	}

	walletSnapshot, err := r.walletExitFundingSnapshot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list wallet "+
			"unspent: %v", err)
	}

	resp := &ExitPlanResponse{
		FeeRateSatPerVByte: feeRate,
	}
	resp.Plans = make([]ExitPlanEntry, 0, len(req.Outpoints))

	// canStart is the AND over every entry that has no per-outpoint
	// error. It is only meaningful once at least one entry succeeds.
	canStart := true
	sawEntry := false
	for _, raw := range req.Outpoints {
		entry := r.exitPlanEntry(
			ctx, raw, feeRate, walletSnapshot,
		)
		resp.Plans = append(resp.Plans, entry)
		if entry.Err != nil {
			continue
		}

		sawEntry = true
		canStart = canStart && entry.CanStart
		resp.TotalFundingShortfallSat += entry.FundingShortfallSat

		recommended := entry.RecommendedTotalFundingSat
		resp.TotalRecommendedFundingSat += recommended
	}

	resp.CanStart = sawEntry && canStart

	return resp, nil
}

// exitPlanEntry previews a single VTXO outpoint against the shared fee rate
// and wallet snapshot. Per-outpoint failures are recorded on entry.Err so the
// caller can continue previewing the rest of the batch.
func (r *RPCServer) exitPlanEntry(ctx context.Context, raw string,
	feeRate int64,
	walletSnapshot unroll.ExitFundingSnapshot) ExitPlanEntry {

	entry := ExitPlanEntry{Outpoint: raw}

	outpoint, err := parseOutpointString(raw)
	if err != nil {
		entry.Err = fmt.Errorf("invalid outpoint: %w", err)

		return entry
	}
	entry.Outpoint = outpoint.String()

	desc, err := r.server.vtxoStore.GetVTXO(ctx, outpoint)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			entry.Err = fmt.Errorf("VTXO %s not found", outpoint)

			return entry
		}

		entry.Err = fmt.Errorf("get VTXO: %w", err)

		return entry
	}

	plan := unroll.PlanExitFunding(
		desc, btcutil.Amount(feeRate), walletSnapshot,
	)
	verdict := plan.Feasibility
	if verdict.RequiredWalletInputs == 0 {
		entry.Err = fmt.Errorf("VTXO %s has no unilateral-exit "+
			"ancestry", outpoint)

		return entry
	}

	entry.RequiredConfirmations = plan.RequiredConfirmations
	entry.RequiredFeeUTXOCount = uint32(verdict.RequiredWalletInputs)
	entry.UsableFeeUTXOCount = uint32(verdict.WalletUsableInputs)
	entry.RecommendedUTXOAmountSat = int64(plan.RecommendedUTXOAmountSat)
	entry.RecommendedTotalFundingSat = int64(
		plan.RecommendedTotalFundingSat,
	)
	entry.FundingShortfallSat = int64(plan.FundingShortfallSat)
	entry.CanStart = verdict.Feasible

	// Only allocate a funding address when the outpoint is NOT feasible
	// (there is a shortfall). When feasible, no address is needed so the
	// preview must not persist one.
	if !verdict.Feasible {
		fundingAddress, err := r.server.exitPlanFundingAddress(
			ctx, outpoint.String(), true,
		)
		if err != nil {
			entry.Err = fmt.Errorf("exit plan funding address: %w",
				err)

			return entry
		}

		entry.FundingAddress = fundingAddress
	}

	statusResp, err := r.GetUnrollStatus(
		ctx, &daemonrpc.GetUnrollStatusRequest{
			Outpoint: outpoint.String(),
		},
	)
	if err != nil {
		entry.Err = fmt.Errorf("exit status: %w", err)

		return entry
	}
	entry.ExitJobFound = statusResp.GetFound()
	entry.ExitStatus = statusResp.GetStatus()
	if sweepTxid := statusResp.GetSweepTxid(); sweepTxid != "" {
		txid, err := chainhash.NewHashFromStr(sweepTxid)
		if err != nil {
			entry.Err = fmt.Errorf("exit status sweep txid: %w",
				err)

			return entry
		}

		entry.SweepTxid = txid
	}
	if lastErr := statusResp.GetLastError(); lastErr != "" {
		entry.LastError = errors.New(lastErr)
	}

	return entry
}

// walletExitFundingSnapshot returns the confirmed backing-wallet balance and
// the count of confirmed UTXOs that satisfy the unroll preflight floor.
func (r *RPCServer) walletExitFundingSnapshot(ctx context.Context) (
	unroll.ExitFundingSnapshot, error) {

	utxos, err := r.server.ListWalletUnspent(
		ctx, int32(unroll.RequiredFeeInputConfirmations),
		wallet.MaxConfsForListUnspent,
	)
	if err != nil {
		return unroll.ExitFundingSnapshot{}, err
	}

	var confirmed btcutil.Amount
	usable := 0
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		confirmed += utxo.Amount
		if utxo.Amount >= unroll.DefaultFeeInputMinAmountSat {
			usable++
		}
	}

	return unroll.ExitFundingSnapshot{
		WalletConfirmedSat: confirmed,
		WalletUsableInputs: usable,
	}, nil
}

// SweepWallet previews or broadcasts a normal backing-wallet sweep that
// excludes boarding outputs.
func (r *RPCServer) SweepWallet(ctx context.Context, req *SweepWalletRequest) (
	*SweepWalletResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(
			codes.InvalidArgument, "request is nil",
		)
	}
	if req.DestinationAddress == "" {
		return nil, status.Error(
			codes.InvalidArgument,
			"destination_address is required",
		)
	}

	addr, err := btcutil.DecodeAddress(
		req.DestinationAddress, r.server.chainParams,
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"destination_address: %v", err)
	}
	if !addr.IsForNet(r.server.chainParams) {
		return nil, status.Error(
			codes.InvalidArgument,
			"destination_address is for the wrong network",
		)
	}

	destScriptBytes, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "destination "+
			"script: %v", err)
	}
	destScript, err := txscript.ParsePkScript(destScriptBytes)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "destination "+
			"script: %v", err)
	}

	feeRate := req.FeeRateSatPerVByte
	if feeRate < 0 {
		return nil, status.Error(
			codes.InvalidArgument,
			"fee_rate_sat_per_vbyte must be non-negative",
		)
	}
	if feeRate == 0 {
		feeRate, err = r.estimateWalletFeeRate(ctx, req.ConfTarget)
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"estimate fee: %v", err)
		}
	} else {
		feeRate = r.capWalletFeeRate(feeRate)
	}

	utxos, err := r.server.ListWalletUnspent(
		ctx, 1, wallet.MaxConfsForListUnspent,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list wallet "+
			"unspent: %v", err)
	}

	resp := walletSweepPreview(utxos, destScript, feeRate)
	if !req.Broadcast {
		return resp, nil
	}
	if !resp.CanBroadcast {
		if resp.FailureReason == nil {
			resp.FailureReason = errors.New(
				walletSweepDustFailureReason)
		}

		return resp, nil
	}

	signer, err := r.server.newSweepWallet()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "wallet signer: %v",
			err)
	}
	txWallet, ok := signer.(txconfirm.Wallet)
	if !ok {
		return nil, status.Error(
			codes.Internal,
			"wallet signer cannot finalize backing wallet PSBTs",
		)
	}

	locked, err := lockWalletSweepInputs(ctx, txWallet, utxos)
	if err != nil {
		resp.FailureReason = err

		return resp, nil
	}
	releaseSweepInputs := true
	defer func() {
		if !releaseSweepInputs {
			return
		}

		_ = releaseWalletSweepInputs(ctx, txWallet, locked)
	}()

	tx, err := buildWalletSweepTx(utxos, destScript, resp.NetAmountSat)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build sweep tx: %v",
			err)
	}

	finalTx, err := signWalletSweepTx(ctx, txWallet, tx, utxos)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign sweep tx: %v",
			err)
	}
	if r.server.chainBackend == nil {
		return nil, status.Error(
			codes.Unavailable, "chain backend not initialized",
		)
	}

	err = r.server.chainBackend.BroadcastTx(
		ctx, finalTx, "wallet backing sweep",
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "broadcast "+
			"sweep tx: %v", err)
	}

	txid := finalTx.TxHash()
	resp.Txid = &txid
	releaseSweepInputs = false

	return resp, nil
}

func (r *RPCServer) estimateWalletFeeRate(ctx context.Context,
	confTarget uint32) (int64, error) {

	if confTarget == 0 {
		confTarget = defaultExitPlanConfTarget
	}

	const minFeeRateSatPerVByte = int64(2)

	if r.server.chainBackend == nil {
		return minFeeRateSatPerVByte, nil
	}

	feeRate, err := r.server.chainBackend.EstimateFee(ctx, confTarget)
	if err != nil {

		//nolint:nilerr // Fresh regtest wallets often lack fee history.
		return minFeeRateSatPerVByte, nil
	}

	rate := int64(feeRate)
	if rate < minFeeRateSatPerVByte {
		return minFeeRateSatPerVByte, nil
	}

	if maxRate := r.server.unrollMaxFeeRate(); maxRate > 0 &&
		rate > maxRate {
		return maxRate, nil
	}

	return rate, nil
}

func (r *RPCServer) capWalletFeeRate(feeRate int64) int64 {
	if maxRate := r.server.unrollMaxFeeRate(); maxRate > 0 &&
		feeRate > maxRate {
		return maxRate
	}

	return feeRate
}

func (s *Server) exitPlanFundingAddress(ctx context.Context, outpoint string,
	create bool) (string, error) {

	if !create {
		return "", nil
	}

	return s.exitFundingAddresses.Address(
		ctx, outpoint, s.NewWalletAddress,
	)
}

func walletSweepPreview(utxos []*wallet.Utxo, destScript txscript.PkScript,
	feeRate int64) *SweepWalletResponse {

	resp := &SweepWalletResponse{
		FeeRateSatPerVByte: feeRate,
		Inputs:             make([]WalletSweepInput, 0, len(utxos)),
	}

	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		resp.Inputs = append(resp.Inputs, WalletSweepInput{
			Outpoint:  utxo.Outpoint.String(),
			AmountSat: int64(utxo.Amount),
		})
		resp.TotalInputSat += int64(utxo.Amount)
	}

	if len(resp.Inputs) == 0 {
		resp.FailureReason = errors.New("no confirmed backing-wallet " +
			"UTXOs")

		return resp
	}

	fee := int64(estimateWalletSweepVSize(utxos, destScript)) * feeRate
	resp.EstimatedFeeSat = fee
	resp.NetAmountSat = resp.TotalInputSat - fee
	if resp.NetAmountSat <= int64(txconfirm.DustLimit) {
		resp.FailureReason = errors.New(walletSweepDustFailureReason)

		return resp
	}

	resp.CanBroadcast = true

	return resp
}

func lockWalletSweepInputs(ctx context.Context, leaser walletcore.OutputLeaser,
	utxos []*wallet.Utxo) ([]wire.OutPoint, error) {

	locked := make([]wire.OutPoint, 0, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		_, err := leaser.LeaseOutput(
			ctx, walletSweepLockID, utxo.Outpoint,
			txconfirm.DefaultFeeInputLeaseExpiry,
		)
		if err != nil {
			_ = releaseWalletSweepInputs(ctx, leaser, locked)

			return nil, fmt.Errorf("lock wallet sweep input %s: %w",
				utxo.Outpoint, err)
		}

		locked = append(locked, utxo.Outpoint)
	}

	return locked, nil
}

func releaseWalletSweepInputs(ctx context.Context,
	leaser walletcore.OutputLeaser, outpoints []wire.OutPoint) error {

	var releaseErr error
	for _, outpoint := range outpoints {
		err := leaser.ReleaseOutput(ctx, walletSweepLockID, outpoint)
		if err != nil {
			releaseErr = errors.Join(releaseErr, err)
		}
	}

	return releaseErr
}

func estimateWalletSweepVSize(utxos []*wallet.Utxo,
	destScript txscript.PkScript) int {

	var est input.TxWeightEstimator
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		addSweepInputForScript(&est, utxo.PkScript)
	}
	est.AddOutput(destScript.Script())

	return est.VSize()
}

func addSweepInputForScript(est *input.TxWeightEstimator, pkScript []byte) {
	switch txscript.GetScriptClass(pkScript) {
	case txscript.WitnessV0PubKeyHashTy:
		est.AddP2WKHInput()

	case txscript.WitnessV1TaprootTy:
		est.AddTaprootKeySpendInput(txscript.SigHashDefault)

	case txscript.ScriptHashTy:
		est.AddNestedP2WKHInput()

	case txscript.PubKeyHashTy:
		est.AddP2PKHInput()

	default:
		est.AddP2WKHInput()
	}
}

func buildWalletSweepTx(utxos []*wallet.Utxo, destScript txscript.PkScript,
	netAmount int64) (*wire.MsgTx, error) {

	if netAmount <= int64(txconfirm.DustLimit) {
		return nil, fmt.Errorf("net amount %d is dust", netAmount)
	}

	tx := wire.NewMsgTx(walletSweepTxVersion)
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: utxo.Outpoint,
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    netAmount,
		PkScript: destScript.Script(),
	})

	return tx, nil
}

func signWalletSweepTx(ctx context.Context, signer txconfirm.Wallet,
	tx *wire.MsgTx, utxos []*wallet.Utxo) (*wire.MsgTx, error) {

	inputs := make([]*wire.OutPoint, 0, len(tx.TxIn))
	sequences := make([]uint32, 0, len(tx.TxIn))
	witnessByOutpoint := make(map[wire.OutPoint]*wire.TxOut, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		witnessByOutpoint[utxo.Outpoint] = &wire.TxOut{
			Value:    int64(utxo.Amount),
			PkScript: utxo.PkScript,
		}
	}

	for _, txIn := range tx.TxIn {
		inputs = append(inputs, &txIn.PreviousOutPoint)
		sequences = append(sequences, txIn.Sequence)
	}

	packet, err := psbt.New(
		inputs, tx.TxOut, tx.Version, tx.LockTime, sequences,
	)
	if err != nil {
		return nil, fmt.Errorf("create PSBT: %w", err)
	}

	for idx, txIn := range tx.TxIn {
		witness, ok := witnessByOutpoint[txIn.PreviousOutPoint]
		if !ok {
			return nil, fmt.Errorf("missing witness UTXO for %s",
				txIn.PreviousOutPoint)
		}

		packet.Inputs[idx].WitnessUtxo = witness
	}

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize PSBT: %w", err)
	}

	finalTx, err := signer.FinalizePsbt(ctx, buf.Bytes())
	if err != nil {
		return nil, err
	}

	if err := verifySweepOutputsEqual(tx, finalTx); err != nil {
		return nil, err
	}

	return finalTx, nil
}

func verifySweepOutputsEqual(expected, actual *wire.MsgTx) error {
	if expected == nil || actual == nil {
		return fmt.Errorf("transactions must be non-nil")
	}
	if len(expected.TxOut) != len(actual.TxOut) {
		return fmt.Errorf("wallet changed sweep output count from "+
			"%d to %d", len(expected.TxOut), len(actual.TxOut))
	}

	for idx := range expected.TxOut {
		exp := expected.TxOut[idx]
		got := actual.TxOut[idx]
		if exp.Value != got.Value ||
			!bytes.Equal(exp.PkScript, got.PkScript) {
			return fmt.Errorf("wallet changed sweep output %d", idx)
		}
	}

	return nil
}
