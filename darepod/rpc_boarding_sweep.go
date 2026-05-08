package darepod

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/wallet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// boardingSweepStatusPreview means the aggregate sweep transaction was
	// previewed but the caller did not opt in to broadcasting it, or no
	// output is currently sweepable.
	boardingSweepStatusPreview = "preview"

	// defaultListBoardingSweepsPageSize is used when no page size is
	// requested.
	defaultListBoardingSweepsPageSize = uint32(100)

	// maxListBoardingSweepsPageSize prevents one RPC from loading an
	// unbounded sweep history.
	maxListBoardingSweepsPageSize = uint32(500)
)

// defaultBoardingSweepStatuses are the persisted lifecycle states that may
// still correspond to a raw boarding UTXO recoverable by the CSV timeout path.
var defaultBoardingSweepStatuses = [...]wallet.BoardingStatus{
	wallet.BoardingStatusConfirmed,
	wallet.BoardingStatusFailed,
	wallet.BoardingStatusExpired,
}

// SweepBoardingUTXOs sweeps CSV-mature boarding UTXOs back to the wallet.
func (r *RPCServer) SweepBoardingUTXOs(ctx context.Context,
	req *daemonrpc.SweepBoardingUTXOsRequest) (
	*daemonrpc.SweepBoardingUTXOsResponse, error) {

	if req == nil {
		req = &daemonrpc.SweepBoardingUTXOsRequest{}
	}

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if r.server.chainBackend == nil {
		return nil, status.Errorf(
			codes.Internal, "chain backend not initialized",
		)
	}
	if req.GetFeeRateSatPerVbyte() < 0 {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"fee_rate_sat_per_vbyte must be non-negative",
		)
	}

	store := r.server.newBoardingStore()
	sweepWallet, err := r.server.newSweepWallet()
	if err != nil {
		return nil, status.Errorf(
			codes.Internal, "sweep wallet unavailable: %v", err,
		)
	}

	height, _, err := r.server.chainBackend.BestBlock(ctx)
	if err != nil {
		return nil, status.Errorf(
			codes.Internal, "best block lookup failed: %v", err,
		)
	}

	intents, err := boardingSweepCandidates(
		ctx, store, req.GetOutpoints(),
	)
	if err != nil {
		return nil, err
	}

	feeRate, confTarget, feeErr := boardingSweepFeeRate(
		ctx, r.server.chainBackend, req.GetFeeRateSatPerVbyte(),
		req.GetConfTarget(),
	)
	if feeErr != nil {
		r.server.log.DebugS(
			ctx, "Falling back to default boarding sweep fee rate",
			feeErr,
			slog.Uint64("conf_target", uint64(confTarget)),
			slog.Int64("fee_rate_sat_per_vbyte", feeRate),
		)
	}
	if feeRate > boardingSweepHighFeeRateWarningSatPerVByte {
		r.server.log.WarnS(
			ctx, "High boarding sweep fee rate",
			nil,
			slog.Int64("fee_rate_sat_per_vbyte", feeRate),
			slog.Int64("warning_threshold_sat_per_vbyte",
				boardingSweepHighFeeRateWarningSatPerVByte),
		)
	}
	sweepable := make([]wallet.BoardingIntent, 0, len(intents))
	outputs := make([]*daemonrpc.BoardingSweepOutput, 0, len(intents))
	var totalAmount btcutil.Amount
	for _, intent := range intents {
		maturityHeight := boardingSweepMaturityHeight(intent)
		if height < maturityHeight {
			continue
		}

		outputs = append(outputs, newBoardingSweepOutput(intent))
		sweepable = append(sweepable, intent)
		totalAmount += intent.ChainInfo.Amount
	}

	resp := &daemonrpc.SweepBoardingUTXOsResponse{
		CurrentHeight:      height,
		SweepableOutputs:   outputs,
		TotalAmountSat:     int64(totalAmount),
		FeeRateSatPerVbyte: feeRate,
		ConfTarget:         confTarget,
	}
	if len(sweepable) == 0 {
		resp.Status = boardingSweepStatusPreview

		return resp, nil
	}

	sweepPkScript, scriptErr := boardingSweepPkScript(
		ctx, sweepWallet, r.server.chainParams, req.GetSweepAddress(),
		req.GetBroadcast(),
	)
	if scriptErr != nil {
		return failedBoardingSweepResponse(resp, scriptErr)
	}

	sweep, buildErr := buildBoardingSweepTx(sweepWallet, sweepable,
		sweepPkScript,
		feeRate)
	if buildErr != nil {
		return failedBoardingSweepResponse(resp, buildErr)
	}

	resp.EstimatedFeeSat = int64(sweep.fee)
	resp.NetAmountSat = int64(totalAmount - sweep.fee)
	resp.TxVbytes = sweep.vbytes
	txid := sweep.tx.TxHash()
	resp.Txid = txid.String()

	if !req.GetBroadcast() {
		resp.Status = boardingSweepStatusPreview

		return resp, nil
	}

	persistErr := store.CreatePendingBoardingSweep(
		ctx, db.NewBoardingSweep{
			Tx:                 sweep.tx,
			DestinationAddress: req.GetSweepAddress(),
			TotalAmount:        totalAmount,
			FeeAmount:          sweep.fee,
			FeeRateSatPerVByte: feeRate,
			VBytes:             sweep.vbytes,
			CreatedHeight:      height,
			Inputs:             newBoardingSweepInputs(sweepable),
		},
	)
	if persistErr != nil {
		return failedBoardingSweepResponse(resp, persistErr)
	}

	broadcastErr := broadcastBoardingSweep(
		ctx, r.server.chainBackend, sweep, "ark boarding timeout sweep",
	)
	if broadcastErr != nil {
		markErr := store.MarkBoardingSweepFailed(
			ctx, txid, broadcastErr,
		)
		if markErr != nil {
			r.server.log.WarnS(
				ctx, "Unable to mark boarding sweep failed",
				markErr,
				slog.String("txid", txid.String()),
			)

			return failedBoardingSweepResponse(
				resp, fmt.Errorf("%w; mark failed: %w",
					broadcastErr, markErr),
			)
		}

		return failedBoardingSweepResponse(resp, broadcastErr)
	}

	resp.Status = db.BoardingSweepStatusPublished
	resp.FeePaidSat = int64(sweep.fee)

	if err := store.MarkBoardingSweepPublished(ctx, txid); err != nil {
		r.server.log.WarnS(
			ctx, "Unable to mark boarding sweep published",
			err, slog.String("txid", txid.String()),
		)
	}

	if watcher := r.server.getBoardingSweepWatcher(); watcher != nil {
		refreshCtx := context.WithoutCancel(ctx)
		if err := watcher.Refresh(refreshCtx); err != nil {
			r.server.log.WarnS(
				ctx, "Unable to refresh boarding sweep watcher",
				err, slog.String("txid", txid.String()),
			)
		}
	}

	return resp, nil
}

// ListBoardingSweeps returns the daemon's persisted aggregate boarding sweeps.
func (r *RPCServer) ListBoardingSweeps(ctx context.Context,
	req *daemonrpc.ListBoardingSweepsRequest) (
	*daemonrpc.ListBoardingSweepsResponse, error) {

	if req == nil {
		req = &daemonrpc.ListBoardingSweepsRequest{}
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	statusFilter := req.GetStatus()
	if statusFilter != "" && !boardingSweepStatusFilterValid(statusFilter) {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"unknown boarding sweep status %q", statusFilter,
		)
	}

	pageSize := req.GetPageSize()
	if pageSize == 0 {
		pageSize = defaultListBoardingSweepsPageSize
	}
	if pageSize > maxListBoardingSweepsPageSize {
		pageSize = maxListBoardingSweepsPageSize
	}

	offset, err := listBoardingSweepsOffset(req.GetPageToken())
	if err != nil {
		return nil, status.Errorf(
			codes.InvalidArgument, "invalid page token: %v", err,
		)
	}

	store := r.server.newBoardingStore()
	records, err := store.ListBoardingSweeps(
		ctx, statusFilter, int32(pageSize)+1, int32(offset),
	)
	if err != nil {
		return nil, status.Errorf(
			codes.Internal, "list boarding sweeps: %v", err,
		)
	}

	nextToken := ""
	if len(records) > int(pageSize) {
		records = records[:pageSize]
		nextToken = strconv.FormatInt(offset+int64(pageSize), 10)
	}

	resp := &daemonrpc.ListBoardingSweepsResponse{
		Sweeps: make(
			[]*daemonrpc.BoardingSweep, 0, len(records),
		),
		NextPageToken: nextToken,
	}
	for _, record := range records {
		resp.Sweeps = append(resp.Sweeps, newBoardingSweep(record))
	}

	return resp, nil
}

// failedBoardingSweepResponse records an aggregate sweep failure in the RPC
// response body while preserving a successful RPC transport status.
func failedBoardingSweepResponse(
	resp *daemonrpc.SweepBoardingUTXOsResponse, err error) (
	*daemonrpc.SweepBoardingUTXOsResponse, error) {

	resp.Status = db.BoardingSweepStatusFailed
	resp.FailureReason = err.Error()

	return resp, nil
}

// boardingSweepCandidates loads the requested boarding intents, or the default
// set of confirmed/failed/expired intents when no outpoints are specified.
func boardingSweepCandidates(ctx context.Context,
	store *db.BoardingWalletStore, outpointStrings []string) (
	[]wallet.BoardingIntent, error) {

	if len(outpointStrings) == 0 {
		return defaultBoardingSweepCandidates(ctx, store)
	}

	intents := make([]wallet.BoardingIntent, 0, len(outpointStrings))
	seen := make(map[wire.OutPoint]struct{}, len(outpointStrings))
	for _, outpointString := range outpointStrings {
		outpoint, err := parseOutpointString(outpointString)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument, "parse outpoint %q: %v",
				outpointString, err,
			)
		}
		if _, ok := seen[outpoint]; ok {
			continue
		}
		seen[outpoint] = struct{}{}

		intent, err := store.GetIntent(ctx, outpoint)
		if err != nil {
			return nil, status.Errorf(
				codes.NotFound, "load boarding intent %s: %v",
				outpoint, err,
			)
		}

		if !boardingIntentSweepableStatus(intent.Status) {
			continue
		}

		intents = append(intents, *intent)
	}

	return intents, nil
}

// defaultBoardingSweepCandidates loads every persisted boarding intent whose
// lifecycle status may still correspond to an unspent boarding UTXO.
func defaultBoardingSweepCandidates(ctx context.Context,
	store *db.BoardingWalletStore) ([]wallet.BoardingIntent, error) {

	intents, err := store.FetchBoardingIntentsBySweepableStatuses(
		ctx, defaultBoardingSweepStatuses,
	)
	if err != nil {
		return nil, status.Errorf(
			codes.Internal,
			"load boarding intents by status: %v", err,
		)
	}

	return intents, nil
}

// newBoardingSweepOutput builds the static fields for one sweepable output.
func newBoardingSweepOutput(
	intent wallet.BoardingIntent) *daemonrpc.BoardingSweepOutput {

	return &daemonrpc.BoardingSweepOutput{
		Outpoint:       intent.Outpoint.String(),
		AmountSat:      int64(intent.ChainInfo.Amount),
		MaturityHeight: boardingSweepMaturityHeight(intent),
	}
}

// newBoardingSweepInputs converts sweepable boarding intents into the DB input
// records needed to recover and watch the published aggregate sweep.
func newBoardingSweepInputs(
	intents []wallet.BoardingIntent) []db.NewBoardingSweepInput {

	inputs := make([]db.NewBoardingSweepInput, 0, len(intents))
	for _, intent := range intents {
		inputs = append(inputs, db.NewBoardingSweepInput{
			Outpoint:       intent.Outpoint,
			Amount:         intent.ChainInfo.Amount,
			PreviousStatus: intent.Status,
		})
	}

	return inputs
}

// boardingSweepPkScript returns the caller-provided destination script or asks
// the wallet for a fresh sweep address when no override is set.
func boardingSweepPkScript(ctx context.Context,
	sweepWallet unroll.SweepWallet, chainParams *chaincfg.Params,
	sweepAddress string, broadcast bool) ([]byte, error) {

	if sweepAddress == "" {
		if !broadcast {
			return boardingSweepPreviewPkScript(), nil
		}

		pkScript, err := sweepWallet.NewWalletPkScript(ctx)
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

// boardingSweepPreviewPkScript returns a fixed-size P2TR script for previews
// that do not provide an explicit destination. This avoids allocating a fresh
// wallet address just to estimate the aggregate sweep fee. Broadcast sweeps use
// the wallet-provided script instead, so their estimate matches the actual
// destination type.
func boardingSweepPreviewPkScript() []byte {
	const p2trProgramLen = 32

	pkScript := make([]byte, 2+p2trProgramLen)
	pkScript[0] = txscript.OP_1
	pkScript[1] = p2trProgramLen

	return pkScript
}

// boardingIntentSweepableStatus returns true for persisted status values that
// may still correspond to an unspent raw boarding UTXO.
func boardingIntentSweepableStatus(
	status wallet.BoardingStatus) bool {

	for _, sweepable := range defaultBoardingSweepStatuses {
		if status == sweepable {
			return true
		}
	}

	return false
}

// boardingSweepStatusFilterValid returns true for listable aggregate sweep
// statuses.
func boardingSweepStatusFilterValid(status string) bool {
	switch status {
	case db.BoardingSweepStatusPending,
		db.BoardingSweepStatusPublished,
		db.BoardingSweepStatusConfirmed,
		db.BoardingSweepStatusExternalResolved,
		db.BoardingSweepStatusFailed:

		return true

	default:
		return false
	}
}

// newBoardingSweep converts one persisted aggregate sweep into its RPC shape.
func newBoardingSweep(
	record db.BoardingSweepRecord) *daemonrpc.BoardingSweep {

	sweep := &daemonrpc.BoardingSweep{
		Txid:               record.Txid.String(),
		Status:             record.Status,
		DestinationAddress: record.DestinationAddress,
		TotalAmountSat:     int64(record.TotalAmount),
		FeePaidSat:         int64(record.FeeAmount),
		FeeRateSatPerVbyte: record.FeeRateSatPerVByte,
		TxVbytes:           record.VBytes,
		CreatedHeight:      record.CreatedHeight,
		Inputs:             newBoardingSweepInputRecords(record.Inputs),
	}
	if record.ConfirmedHeight.Valid {
		sweep.ConfirmedHeight = record.ConfirmedHeight.Int32
	}
	if record.LastError.Valid {
		sweep.FailureReason = record.LastError.String
	}

	return sweep
}

// listBoardingSweepsOffset parses the simple offset page token used by the
// sweep list RPC.
func listBoardingSweepsOffset(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}

	offset, err := strconv.ParseInt(token, 10, 64)
	if err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, fmt.Errorf("offset must be non-negative")
	}
	if offset > math.MaxInt32 {
		return 0, fmt.Errorf("offset too large")
	}

	return offset, nil
}

// newBoardingSweepInputRecords converts persisted sweep inputs into their RPC
// shape.
func newBoardingSweepInputRecords(
	inputs []db.BoardingSweepInputRecord) []*daemonrpc.BoardingSweepInput {

	records := make([]*daemonrpc.BoardingSweepInput, 0, len(inputs))
	for _, input := range inputs {
		record := &daemonrpc.BoardingSweepInput{
			Outpoint:  input.Outpoint.String(),
			AmountSat: int64(input.Amount),
			Status:    input.Status,
		}
		if input.SpentByTxid.Valid {
			record.SpentByTxid = input.SpentByTxid.String
		}
		if input.SpentHeight.Valid {
			record.SpentHeight = input.SpentHeight.Int32
		}

		records = append(records, record)
	}

	return records
}
