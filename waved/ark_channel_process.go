package waved

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/arkchannel/unrollbridge"
	"github.com/lightninglabs/wavelength/chainbackends"
	"github.com/lightninglabs/wavelength/chainfees"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/lwwallet"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
)

// ArkChannelRecoveryController is the complete endpoint-local source archive,
// watcher, and unroll preparation boundary.
type ArkChannelRecoveryController interface {
	lnruntime.ChannelRecoveryManager
	arkchannel.ChannelEventSinkBinder
	unrollbridge.SourcePreparer

	RestoreWatches(context.Context, []arkchannel.Record) error

	Stop()
}

// ArkChannelOORPreparer reserves wallet VTXOs and prepares the exact OOR
// transfer that creates a channel-policy VTXO.
type ArkChannelOORPreparer func(context.Context, arkchannel.Terms,
	btcutil.Amount) (arkchannel.VTXOBinding, error)

// ArkChannelOORLookup reconciles a deterministic channel OOR key without
// selecting or locking fresh wallet inputs.
type ArkChannelOORLookup func(context.Context, arkchannel.Terms,
	btcutil.Amount) (oorbridge.PreparationLookup, error)

// ArkChannelReceiveCapitalReserver durably reserves global hub capital before
// an armed receive intent is allowed to select funding inputs.
type ArkChannelReceiveCapitalReserver func(context.Context,
	arkchannel.Terms) error

// ArkChannelControllerConfig contains the process-owned dependencies supplied
// after wallet, database, and authenticated swap-server transport startup.
type ArkChannelControllerConfig struct {
	Log            btclog.Logger
	Store          *db.ArkChannelStoreDB
	Peer           lnruntime.ProcessCooperativeClosePeer
	PeerRPC        mailboxrpc.RPCClient
	PeerSender     lnruntime.PeerEventSender
	Wallet         *lwwallet.Wallet
	ChainBackend   chainsource.ChainBackend
	ChainNotifier  *chainbackends.BackendChainNotifier
	FeeEstimator   *chainfees.BackendEstimator
	OOR            *oorbridge.Controller
	FundingOOR     arkchannel.OORTransferController
	Materializer   *unrollbridge.Controller
	Recovery       ArkChannelRecoveryController
	OperatorTerms  *types.OperatorTerms
	IdentityKey    keychain.KeyDescriptor
	OORDestination *btcec.PublicKey
	KeyIndex       uint32
	NetParams      *chaincfg.Params
	ChannelDataDir string

	// OpenChannelDB returns a database owned by this endpoint. Nil uses
	// the application SQL backend, including persistent SQLite in WASM.
	OpenChannelDB         func(string) (*channeldb.DB, error)
	PrepareOOR            ArkChannelOORPreparer
	LookupOOR             ArkChannelOORLookup
	ReserveReceiveCapital ArkChannelReceiveCapitalReserver
	Clock                 clock.Clock
	RecordObserver        arkchannel.RecordObserver
}
