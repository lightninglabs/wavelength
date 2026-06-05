package darepod

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/input"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultExitPlanConfTarget uint32 = 6
	walletSweepTxVersion      int32  = 2

	walletSweepDustFailureReason = "sweep amount is dust after fees"
)

// ExitPlanRequest asks the daemon to preview unilateral-exit funding
// readiness for a wallet-facing caller.
type ExitPlanRequest struct {
	Outpoint   string
	ConfTarget uint32
}

// ExitPlanResponse describes the current unroll CPFP fee-input readiness.
type ExitPlanResponse struct {
	FundingAddress             string
	RequiredConfirmations      uint32
	FeeRateSatPerVByte         int64
	RequiredFeeUTXOCount       uint32
	UsableFeeUTXOCount         uint32
	RecommendedUTXOAmountSat   int64
	RecommendedTotalFundingSat int64
	FundingShortfallSat        int64
	CanStart                   bool
	ExitJobFound               bool
	ExitStatus                 daemonrpc.UnrollJobStatus
	SweepTxid                  string
	LastError                  string
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
	Txid               string
	FailureReason      string
}

// GetExitPlan previews the wallet-side resources needed before admitting a
// unilateral exit.
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

	outpoint, err := parseOutpointString(req.Outpoint)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"outpoint: %v", err)
	}
	if r.server.vtxoStore == nil {
		return nil, status.Error(
			codes.Unavailable, "VTXO store not initialized",
		)
	}

	desc, err := r.server.vtxoStore.GetVTXO(ctx, outpoint)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "VTXO %s "+
				"not found", outpoint)
		}

		return nil, status.Errorf(codes.Internal, "get VTXO: %v", err)
	}

	feeRate, err := r.estimateWalletFeeRate(ctx, req.ConfTarget)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "estimate fee: %v",
			err)
	}

	recommended, err := recommendedUnrollUTXOAmount(
		btcutil.Amount(feeRate),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "estimate unroll "+
			"package fee: %v", err)
	}

	required, err := exitPlanRequiredUTXOCount(outpoint, desc)
	if err != nil {
		return nil, err
	}
	usable, err := r.usableUnrollFeeUTXOCount(ctx, recommended)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list wallet "+
			"unspent: %v", err)
	}

	shortCount := required - minUint32(required, usable)
	shortfall := btcutil.Amount(shortCount) * recommended
	totalRecommended := btcutil.Amount(required) * recommended

	fundingAddress, err := r.server.exitPlanFundingAddress(
		ctx, outpoint.String(), usable < required,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "exit plan funding "+
			"address: %v", err)
	}

	plan := &ExitPlanResponse{
		FundingAddress:             fundingAddress,
		RequiredConfirmations:      1,
		FeeRateSatPerVByte:         feeRate,
		RequiredFeeUTXOCount:       required,
		UsableFeeUTXOCount:         usable,
		RecommendedUTXOAmountSat:   int64(recommended),
		RecommendedTotalFundingSat: int64(totalRecommended),
		FundingShortfallSat:        int64(shortfall),
		CanStart:                   usable >= required,
	}

	statusResp, err := r.GetUnrollStatus(
		ctx, &daemonrpc.GetUnrollStatusRequest{
			Outpoint: outpoint.String(),
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "exit status: %v",
			err)
	}
	plan.ExitJobFound = statusResp.GetFound()
	plan.ExitStatus = statusResp.GetStatus()
	plan.SweepTxid = statusResp.GetSweepTxid()
	plan.LastError = statusResp.GetLastError()

	return plan, nil
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

	destScript, err := txscript.PayToAddrScript(addr)
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
		if resp.FailureReason == "" {
			resp.FailureReason = walletSweepDustFailureReason
		}

		return resp, nil
	}

	tx, err := buildWalletSweepTx(utxos, destScript, resp.NetAmountSat)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build sweep tx: %v",
			err)
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

	resp.Txid = finalTx.TxHash().String()

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

	s.exitPlanFundingAddressesMu.Lock()
	defer s.exitPlanFundingAddressesMu.Unlock()

	if s.exitPlanFundingAddresses == nil {
		s.exitPlanFundingAddresses = make(map[string]string)
	}

	if address := s.exitPlanFundingAddresses[outpoint]; address != "" {
		return address, nil
	}
	if !create {
		return "", nil
	}

	address, err := s.NewWalletAddress(ctx)
	if err != nil {
		return "", err
	}

	s.exitPlanFundingAddresses[outpoint] = address

	return address, nil
}

func exitPlanRequiredUTXOCount(outpoint wire.OutPoint,
	desc *vtxo.Descriptor) (uint32, error) {

	required := requiredUnrollFeeUTXOCount(desc)
	if required == 0 {
		return 0, status.Errorf(codes.FailedPrecondition, "VTXO %s "+
			"has no unilateral-exit ancestry", outpoint)
	}

	return required, nil
}

func requiredUnrollFeeUTXOCount(desc *vtxo.Descriptor) uint32 {
	if desc == nil {
		return 0
	}

	return uint32(len(desc.Ancestry))
}

func recommendedUnrollUTXOAmount(feeRate btcutil.Amount) (btcutil.Amount,
	error) {

	fee, err := txconfirm.EstimatePackageFee(
		estimatedUnrollParentTx(), feeRate,
	)
	if err != nil {
		return 0, err
	}

	recommended := fee + txconfirm.DustLimit
	if recommended < preflightUnrollMinUTXOSat {
		return preflightUnrollMinUTXOSat, nil
	}

	return recommended, nil
}

func estimatedUnrollParentTx() *wire.MsgTx {
	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{},
		Sequence:         wire.MaxTxInSequenceNum - 2,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(txconfirm.DustLimit),
		PkScript: make([]byte, 34),
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_TRUE},
	})

	return tx
}

func (r *RPCServer) usableUnrollFeeUTXOCount(ctx context.Context,
	minAmount btcutil.Amount) (uint32, error) {

	utxos, err := r.server.ListWalletUnspent(
		ctx, 1, wallet.MaxConfsForListUnspent,
	)
	if err != nil {
		return 0, err
	}

	var usable uint32
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}
		if utxo.Amount >= minAmount {
			usable++
		}
	}

	return usable, nil
}

func walletSweepPreview(utxos []*wallet.Utxo, destScript []byte,
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
		resp.FailureReason = "no confirmed backing-wallet UTXOs"

		return resp
	}

	fee := int64(estimateWalletSweepVSize(utxos, destScript)) * feeRate
	resp.EstimatedFeeSat = fee
	resp.NetAmountSat = resp.TotalInputSat - fee
	if resp.NetAmountSat <= int64(txconfirm.DustLimit) {
		resp.FailureReason = walletSweepDustFailureReason

		return resp
	}

	resp.CanBroadcast = true

	return resp
}

func estimateWalletSweepVSize(utxos []*wallet.Utxo, destScript []byte) int {
	var est input.TxWeightEstimator
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		addSweepInputForScript(&est, utxo.PkScript)
	}
	est.AddOutput(destScript)

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

func buildWalletSweepTx(utxos []*wallet.Utxo, destScript []byte,
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
		PkScript: destScript,
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

func minUint32(a, b uint32) uint32 {
	if a < b {
		return a
	}

	return b
}
