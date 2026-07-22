package lndrest

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// Lightning REST paths, taken from lnd's grpc-gateway pattern vars. Both are
// GET endpoints with no request body.
const (
	pathGetInfo       = "/v1/getinfo"
	pathWalletBalance = "/v1/balance/blockchain"
)

// lightningClient implements the small slice of lndclient.LightningClient the
// wallet backend uses (GetInfo for node metadata at connect time, and
// WalletBalance for the on-chain balance surface) over lnd's REST gateway.
//
// The full LightningClient interface has ~45 methods, none of the rest of
// which the daemon calls in lnd mode. Rather than hand-stub every one with its
// exotic parameter types, the interface is embedded (nil): the two used methods
// are overridden below, and any unused method is simply never invoked. This
// mirrors the way lndclient's own clients embed their generated service
// interface.
type lightningClient struct {
	lndclient.LightningClient

	conn *conn
}

// A compile-time check that lightningClient satisfies the lndclient interface.
var _ lndclient.LightningClient = (*lightningClient)(nil)

// RawClientWithMacAuth is required by the ServiceClient interface but returns a
// nil raw client: the REST backend has no gRPC client to expose.
func (s *lightningClient) RawClientWithMacAuth(parentCtx context.Context) (
	context.Context, time.Duration, lnrpc.LightningClient) {

	return parentCtx, s.conn.timeout, nil
}

// WalletBalance returns the node's on-chain wallet balance.
func (s *lightningClient) WalletBalance(ctx context.Context) (
	*lndclient.WalletBalance, error) {

	resp := &lnrpc.WalletBalanceResponse{}
	if err := s.conn.get(ctx, pathWalletBalance, resp); err != nil {
		return nil, err
	}

	return &lndclient.WalletBalance{
		Confirmed:   btcAmount(resp.ConfirmedBalance),
		Unconfirmed: btcAmount(resp.UnconfirmedBalance),
	}, nil
}

// GetInfo returns basic information about the connected lnd node. Only the
// fields the daemon reads (alias, identity pubkey, network, sync/version
// metadata) are populated; fields requiring extra parsing (color, best block
// hash) are left zero-valued since no caller consumes them over this path.
func (s *lightningClient) GetInfo(ctx context.Context) (*lndclient.Info,
	error) {

	resp := &lnrpc.GetInfoResponse{}
	if err := s.conn.get(ctx, pathGetInfo, resp); err != nil {
		return nil, err
	}

	pubKey, err := hex.DecodeString(resp.IdentityPubkey)
	if err != nil {
		return nil, err
	}
	var pubKeyArray [33]byte
	copy(pubKeyArray[:], pubKey)

	var network string
	if len(resp.Chains) > 0 {
		network = resp.Chains[0].Network
	}

	return &lndclient.Info{
		Version:             resp.Version,
		CommitHash:          resp.CommitHash,
		BlockHeight:         resp.BlockHeight,
		BestHeaderTimeStamp: time.Unix(resp.BestHeaderTimestamp, 0),
		IdentityPubkey:      pubKeyArray,
		Alias:               resp.Alias,
		Network:             network,
		Uris:                resp.Uris,
		SyncedToChain:       resp.SyncedToChain,
		SyncedToGraph:       resp.SyncedToGraph,
		ActiveChannels:      resp.NumActiveChannels,
		InactiveChannels:    resp.NumInactiveChannels,
		PendingChannels:     resp.NumPendingChannels,
		NumPeers:            resp.NumPeers,
	}, nil
}
