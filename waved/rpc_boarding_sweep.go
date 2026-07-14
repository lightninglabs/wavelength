package waved

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// defaultListBoardingSweepsPageSize is used when no page size is
	// requested.
	defaultListBoardingSweepsPageSize = uint32(100)

	// maxListBoardingSweepsPageSize prevents one RPC from loading an
	// unbounded sweep history.
	maxListBoardingSweepsPageSize = uint32(500)
)

// boardingSweepStatusFilterValid reports whether a list-status filter is
// recognised, including the empty string ("all").
func boardingSweepStatusFilterValid(s string) bool {
	switch s {
	case "",
		wallet.BoardingSweepStatusPending,
		wallet.BoardingSweepStatusPublished,
		wallet.BoardingSweepStatusConfirmed,
		wallet.BoardingSweepStatusExternalResolved,
		wallet.BoardingSweepStatusFailed:
		return true
	}

	return false
}

// SweepBoardingUTXOs sweeps CSV-mature boarding UTXOs back to the wallet by
// asking the wallet actor.
func (r *RPCServer) SweepBoardingUTXOs(ctx context.Context,
	req *waverpc.SweepBoardingUTXOsRequest) (
	*waverpc.SweepBoardingUTXOsResponse, error) {

	if req == nil {
		req = &waverpc.SweepBoardingUTXOsRequest{}
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Unavailable, "wallet actor "+
			"unavailable")
	}
	if req.GetFeeRateSatPerVbyte() < 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"fee_rate_sat_per_vbyte must be non-negative")
	}

	outpoints, err := parseBoardingSweepOutpoints(req.GetOutpoints())
	if err != nil {
		return nil, err
	}

	walletReq := &wallet.SweepBoardingUTXOsRequest{
		Outpoints:          outpoints,
		FeeRateSatPerVByte: req.GetFeeRateSatPerVbyte(),
		ConfTarget:         req.GetConfTarget(),
		SweepAddress:       req.GetSweepAddress(),
		Broadcast:          req.GetBroadcast(),
	}

	wRef := r.server.walletRef.UnsafeFromSome()
	future := wRef.Ask(ctx, walletReq)
	result := future.Await(ctx)
	if result.IsErr() {
		return nil, status.Errorf(codes.Internal, "sweep boarding "+
			"utxos: %v", result.Err())
	}

	raw := result.UnwrapOr(nil)
	walletResp, ok := raw.(*wallet.SweepBoardingUTXOsResponse)
	if !ok || walletResp == nil {
		return nil, status.Errorf(codes.Internal, "unexpected sweep "+
			"response from wallet actor")
	}

	return walletSweepResponseToProto(walletResp), nil
}

// ListBoardingSweeps returns the daemon's persisted aggregate boarding
// sweeps by reading the sweep store directly. Listing is pure CRUD with no
// in-memory actor state to consult, so taking a wallet-actor mailbox hop
// for it would just add latency.
func (r *RPCServer) ListBoardingSweeps(ctx context.Context,
	req *waverpc.ListBoardingSweepsRequest) (
	*waverpc.ListBoardingSweepsResponse, error) {

	if req == nil {
		req = &waverpc.ListBoardingSweepsRequest{}
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if r.server.boardingSweepStore == nil {
		return nil, status.Errorf(codes.Unavailable, "boarding sweep "+
			"store unavailable")
	}

	statusFilter := req.GetStatus()
	if !boardingSweepStatusFilterValid(statusFilter) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"status filter %q", statusFilter)
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
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"page token: %v", err)
	}

	// Request one extra row so we can emit a next-page token without
	// running a separate count query.
	records, err := r.server.boardingSweepStore.ListBoardingSweeps(
		ctx, statusFilter, int32(pageSize)+1, int32(offset),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list boarding "+
			"sweeps: %v", err)
	}

	nextToken := ""
	if len(records) > int(pageSize) {
		records = records[:pageSize]
		nextToken = strconv.FormatInt(offset+int64(pageSize), 10)
	}

	resp := &waverpc.ListBoardingSweepsResponse{
		Sweeps: make(
			[]*waverpc.BoardingSweep, 0, len(records),
		),
		NextPageToken: nextToken,
	}
	for _, record := range records {
		resp.Sweeps = append(resp.Sweeps, newBoardingSweep(record))
	}

	return resp, nil
}

// parseBoardingSweepOutpoints translates the RPC outpoint strings into
// wire.OutPoint, deduplicating the input set.
func parseBoardingSweepOutpoints(outpointStrings []string) ([]wire.OutPoint,
	error) {

	if len(outpointStrings) == 0 {
		return nil, nil
	}

	out := make([]wire.OutPoint, 0, len(outpointStrings))
	seen := make(map[wire.OutPoint]struct{}, len(outpointStrings))
	for _, s := range outpointStrings {
		op, err := parseOutpointString(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"parse outpoint %q: %v", s, err)
		}
		if _, ok := seen[op]; ok {
			continue
		}
		seen[op] = struct{}{}
		out = append(out, op)
	}

	return out, nil
}

// walletSweepResponseToProto translates the wallet actor's sweep response
// into its proto wire shape. This is the only place in the RPC layer that
// touches sweep proto field names; everything else flows through the
// wallet actor.
func walletSweepResponseToProto(
	resp *wallet.SweepBoardingUTXOsResponse,
) *waverpc.SweepBoardingUTXOsResponse {

	protoResp := &waverpc.SweepBoardingUTXOsResponse{
		Status:             resp.Status,
		CurrentHeight:      resp.CurrentHeight,
		TotalAmountSat:     resp.TotalAmountSat,
		EstimatedFeeSat:    resp.EstimatedFeeSat,
		NetAmountSat:       resp.NetAmountSat,
		FeePaidSat:         resp.FeePaidSat,
		FeeRateSatPerVbyte: resp.FeeRateSatPerVByte,
		ConfTarget:         resp.ConfTarget,
		TxVbytes:           resp.TxVBytes,
		FailureReason:      resp.FailureReason,
	}
	if resp.HasTxid {
		protoResp.Txid = resp.Txid.String()
	}
	for _, out := range resp.SweepableOutputs {
		protoResp.SweepableOutputs = append(
			protoResp.SweepableOutputs,
			&waverpc.BoardingSweepOutput{
				Outpoint:       out.Outpoint.String(),
				AmountSat:      out.AmountSat,
				MaturityHeight: out.MaturityHeight,
			},
		)
	}

	return protoResp
}

// newBoardingSweep converts one persisted aggregate sweep into its RPC shape.
func newBoardingSweep(
	record wallet.BoardingSweepRecord) *waverpc.BoardingSweep {

	sweep := &waverpc.BoardingSweep{
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

// newBoardingSweepInputRecords converts persisted sweep inputs into their
// RPC shape.
func newBoardingSweepInputRecords(
	inputs []wallet.BoardingSweepInputRecord,
) []*waverpc.BoardingSweepInput {

	records := make([]*waverpc.BoardingSweepInput, 0, len(inputs))
	for _, input := range inputs {
		record := &waverpc.BoardingSweepInput{
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
