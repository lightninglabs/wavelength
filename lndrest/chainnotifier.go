package lndrest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/rpc/restclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
)

// ChainNotifier REST paths, taken from lnd's chainrpc grpc-gateway pattern
// vars. These are POST server-streaming endpoints.
const (
	pathRegisterConfs  = "/v2/chainnotifier/register/confirmations"
	pathRegisterSpends = "/v2/chainnotifier/register/spends"
	pathRegisterBlocks = "/v2/chainnotifier/register/blocks"
)

// chainNotifierClient implements lndclient.ChainNotifierClient over lnd's REST
// gateway. Each registration opens a grpc-gateway server stream and forwards
// decoded events onto the channel-returning shape lndclient exposes; a
// background goroutine reads the stream until the caller cancels the context
// it registered with, which tears the underlying HTTP stream down.
type chainNotifierClient struct {
	conn *conn
}

// A compile-time check that chainNotifierClient satisfies the interface.
var _ lndclient.ChainNotifierClient = (*chainNotifierClient)(nil)

// RawClientWithMacAuth is required by the ServiceClient interface but returns a
// nil raw client: the REST backend has no gRPC client to expose.
func (s *chainNotifierClient) RawClientWithMacAuth(parentCtx context.Context) (
	context.Context, time.Duration, chainrpc.ChainNotifierClient) {

	return parentCtx, s.conn.timeout, nil
}

// RegisterBlockEpochNtfn streams block heights as they are connected.
func (s *chainNotifierClient) RegisterBlockEpochNtfn(ctx context.Context) (
	chan int32, chan error, error) {

	resp, err := s.conn.stream( //nolint:bodyclose // stream owns body
		ctx, pathRegisterBlocks, &chainrpc.BlockEpoch{},
	)
	if err != nil {
		return nil, nil, err
	}

	stream := restclient.NewStreamClient(
		resp, "RegisterBlockEpochNtfn",
		func() *chainrpc.BlockEpoch { return &chainrpc.BlockEpoch{} },
	)

	blockEpochChan := make(chan int32)
	blockErrorChan := make(chan error, 1)

	go func() {
		for {
			epoch, err := stream.Recv()
			if err != nil {
				blockErrorChan <- err

				return
			}

			select {
			case blockEpochChan <- int32(epoch.Height):
			case <-ctx.Done():
				return
			}
		}
	}()

	return blockEpochChan, blockErrorChan, nil
}

// RegisterConfirmationsNtfn streams confirmation events for a script/txid.
func (s *chainNotifierClient) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint int32,
	optFuncs ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	opts := lndclient.DefaultNotifierOptions()
	for _, optFunc := range optFuncs {
		optFunc(opts)
	}

	var txidSlice []byte
	if txid != nil {
		txidSlice = txid[:]
	}

	req := &chainrpc.ConfRequest{
		Script:       pkScript,
		NumConfs:     uint32(numConfs),
		HeightHint:   uint32(heightHint),
		Txid:         txidSlice,
		IncludeBlock: opts.IncludeBlock,
	}
	resp, err := s.conn.stream( //nolint:bodyclose // stream owns body
		ctx, pathRegisterConfs, req,
	)
	if err != nil {
		return nil, nil, err
	}

	stream := restclient.NewStreamClient(
		resp, "RegisterConfirmationsNtfn",
		func() *chainrpc.ConfEvent { return &chainrpc.ConfEvent{} },
	)

	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	errChan := make(chan error, 1)

	go s.forwardConfEvents(ctx, stream, opts, confChan, errChan)

	return confChan, errChan, nil
}

// forwardConfEvents reads confirmation events off the stream and forwards them
// onto confChan, mirroring lndclient's own event handling.
func (s *chainNotifierClient) forwardConfEvents(ctx context.Context,
	stream *restclient.StreamClient[chainrpc.ConfEvent],
	opts *lndclient.NotifierOptions,
	confChan chan *chainntnfs.TxConfirmation, errChan chan error) {

	for {
		confEvent, err := stream.Recv()
		if err != nil {
			errChan <- err

			return
		}

		switch c := confEvent.Event.(type) {
		case *chainrpc.ConfEvent_Conf:
			tx, err := decodeTx(c.Conf.RawTx)
			if err != nil {
				errChan <- err

				return
			}

			var block *wire.MsgBlock
			if opts.IncludeBlock {
				block, err = decodeBlock(c.Conf.RawBlock)
				if err != nil {
					errChan <- err

					return
				}
			}

			blockHash, err := chainhash.NewHash(c.Conf.BlockHash)
			if err != nil {
				errChan <- err

				return
			}

			conf := &chainntnfs.TxConfirmation{
				BlockHeight: c.Conf.BlockHeight,
				BlockHash:   blockHash,
				Tx:          tx,
				TxIndex:     c.Conf.TxIndex,
				Block:       block,
			}

			select {
			case confChan <- conf:
			case <-ctx.Done():
				return
			}

			// In re-org aware mode we keep listening for the new
			// confirmation after a re-org; otherwise we are done.
			if opts.ReOrgChan == nil {
				return
			}

		case *chainrpc.ConfEvent_Reorg:
			if opts.ReOrgChan != nil {
				select {
				case opts.ReOrgChan <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}

		case nil:
			errChan <- fmt.Errorf("conf event empty")

			return

		default:
			errChan <- fmt.Errorf("conf event has unexpected type")

			return
		}
	}
}

// RegisterSpendNtfn streams spend events for an outpoint/script.
func (s *chainNotifierClient) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint int32,
	optFuncs ...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail,
	chan error, error) {

	opts := lndclient.DefaultNotifierOptions()
	for _, optFunc := range optFuncs {
		optFunc(opts)
	}
	if opts.IncludeBlock {
		return nil, nil, fmt.Errorf("option IncludeBlock is not " +
			"supported by RegisterSpendNtfn")
	}

	var rpcOutpoint *chainrpc.Outpoint
	if outpoint != nil {
		rpcOutpoint = &chainrpc.Outpoint{
			Hash:  outpoint.Hash[:],
			Index: outpoint.Index,
		}
	}

	req := &chainrpc.SpendRequest{
		HeightHint: uint32(heightHint),
		Outpoint:   rpcOutpoint,
		Script:     pkScript,
	}
	resp, err := s.conn.stream( //nolint:bodyclose // stream owns body
		ctx, pathRegisterSpends, req,
	)
	if err != nil {
		return nil, nil, err
	}

	stream := restclient.NewStreamClient(
		resp, "RegisterSpendNtfn",
		func() *chainrpc.SpendEvent { return &chainrpc.SpendEvent{} },
	)

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	errChan := make(chan error, 1)

	go s.forwardSpendEvents(ctx, stream, opts, spendChan, errChan)

	return spendChan, errChan, nil
}

// forwardSpendEvents reads spend events off the stream and forwards them onto
// spendChan, mirroring lndclient's own event handling.
func (s *chainNotifierClient) forwardSpendEvents(ctx context.Context,
	stream *restclient.StreamClient[chainrpc.SpendEvent],
	opts *lndclient.NotifierOptions, spendChan chan *chainntnfs.SpendDetail,
	errChan chan error) {

	for {
		spendEvent, err := stream.Recv()
		if err != nil {
			errChan <- err

			return
		}

		switch c := spendEvent.Event.(type) {
		case *chainrpc.SpendEvent_Spend:
			spend, err := spendDetailFromRPC(
				ctx, c.Spend, spendChan,
			)
			if err != nil {
				errChan <- err

				return
			}
			if spend {
				// Not re-org aware: one spend is terminal.
				if opts.ReOrgChan == nil {
					return
				}
			}

		case *chainrpc.SpendEvent_Reorg:
			if opts.ReOrgChan != nil {
				select {
				case opts.ReOrgChan <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}

		case nil:
			errChan <- fmt.Errorf("spend event empty")

			return

		default:
			errChan <- fmt.Errorf("spend event has unexpected type")

			return
		}
	}
}

// spendDetailFromRPC decodes a spend detail and forwards it onto spendChan,
// returning true once the spend was delivered (or the context was cancelled
// while delivering).
func spendDetailFromRPC(ctx context.Context, d *chainrpc.SpendDetails,
	spendChan chan *chainntnfs.SpendDetail) (bool, error) {

	outpointHash, err := chainhash.NewHash(d.SpendingOutpoint.Hash)
	if err != nil {
		return false, err
	}
	txHash, err := chainhash.NewHash(d.SpendingTxHash)
	if err != nil {
		return false, err
	}
	tx, err := decodeTx(d.RawSpendingTx)
	if err != nil {
		return false, err
	}

	spend := &chainntnfs.SpendDetail{
		SpentOutPoint: &wire.OutPoint{
			Hash:  *outpointHash,
			Index: d.SpendingOutpoint.Index,
		},
		SpenderTxHash:     txHash,
		SpenderInputIndex: d.SpendingInputIndex,
		SpendingTx:        tx,
		SpendingHeight:    int32(d.SpendingHeight),
	}

	select {
	case spendChan <- spend:
		return true, nil

	case <-ctx.Done():
		return true, nil
	}
}
