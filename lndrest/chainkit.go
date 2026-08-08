package lndrest

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/url"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
)

// ChainKit REST paths, taken from lnd's chainrpc grpc-gateway pattern vars.
// These are GET endpoints whose request fields bind from the query string.
const (
	pathGetBlock       = "/v2/chainkit/block"
	pathGetBlockHeader = "/v2/chainkit/blockheader"
	pathGetBestBlock   = "/v2/chainkit/bestblock"
	pathGetBlockHash   = "/v2/chainkit/blockhash"
)

// chainKitClient implements lndclient.ChainKitClient over lnd's REST gateway.
type chainKitClient struct {
	conn *conn
}

// A compile-time check that chainKitClient satisfies the lndclient interface.
var _ lndclient.ChainKitClient = (*chainKitClient)(nil)

// RawClientWithMacAuth is required by the ServiceClient interface but returns a
// nil raw client: the REST backend has no gRPC client to expose.
func (s *chainKitClient) RawClientWithMacAuth(parentCtx context.Context) (
	context.Context, time.Duration, chainrpc.ChainKitClient) {

	return parentCtx, s.conn.timeout, nil
}

// queryPath appends url-encoded query parameters to a base path.
func queryPath(base string, params url.Values) string {
	if len(params) == 0 {
		return base
	}

	return base + "?" + params.Encode()
}

// hashQuery builds the block_hash query parameter. grpc-gateway decodes bytes
// query parameters as base64, so the hash is standard-base64 encoded.
func hashQuery(hash chainhash.Hash) url.Values {
	params := url.Values{}
	params.Set("block_hash", base64.StdEncoding.EncodeToString(hash[:]))

	return params
}

// GetBlock returns the full block for the given hash.
func (s *chainKitClient) GetBlock(ctx context.Context, hash chainhash.Hash) (
	*wire.MsgBlock, error) {

	resp := &chainrpc.GetBlockResponse{}
	path := queryPath(pathGetBlock, hashQuery(hash))
	if err := s.conn.get(ctx, path, resp); err != nil {
		return nil, err
	}

	return decodeBlock(resp.RawBlock)
}

// GetBlockHeader returns the header for the given block hash.
func (s *chainKitClient) GetBlockHeader(ctx context.Context,
	hash chainhash.Hash) (*wire.BlockHeader, error) {

	resp := &chainrpc.GetBlockHeaderResponse{}
	path := queryPath(pathGetBlockHeader, hashQuery(hash))
	if err := s.conn.get(ctx, path, resp); err != nil {
		return nil, err
	}

	header := &wire.BlockHeader{}
	if err := header.Deserialize(
		bytes.NewReader(resp.RawBlockHeader),
	); err != nil {
		return nil, err
	}

	return header, nil
}

// GetBestBlock returns the best block hash and its height.
func (s *chainKitClient) GetBestBlock(ctx context.Context) (chainhash.Hash,
	int32, error) {

	resp := &chainrpc.GetBestBlockResponse{}
	if err := s.conn.get(ctx, pathGetBestBlock, resp); err != nil {
		return chainhash.Hash{}, 0, err
	}

	var blockHash chainhash.Hash
	copy(blockHash[:], resp.BlockHash)

	return blockHash, resp.BlockHeight, nil
}

// GetBlockHash returns the hash of the block at the given height.
func (s *chainKitClient) GetBlockHash(ctx context.Context, blockHeight int64) (
	chainhash.Hash, error) {

	params := url.Values{}
	params.Set("block_height", strconv.FormatInt(blockHeight, 10))

	resp := &chainrpc.GetBlockHashResponse{}
	path := queryPath(pathGetBlockHash, params)
	if err := s.conn.get(ctx, path, resp); err != nil {
		return chainhash.Hash{}, err
	}

	var blockHash chainhash.Hash
	copy(blockHash[:], resp.BlockHash)

	return blockHash, nil
}
