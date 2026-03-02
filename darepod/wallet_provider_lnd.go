package darepod

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// lndWalletProvider implements WalletProvider using a remote LND node
// via lndclient. It wraps the existing LND wiring that was previously
// inline in server.go: chain backend from ChainNotifier/FeeEstimator,
// boarding backend from WalletKit, and client wallet from Signer +
// WalletKit.
type lndWalletProvider struct {
	lnd *lndclient.GrpcLndServices

	chainBackend   chainsource.ChainBackend
	boardingBacked wallet.BoardingBackend
	clientWallet   round.ClientWallet
}

// newLNDWalletProvider creates and connects an LND wallet provider.
// The call blocks until LND is synced and unlocked.
func newLNDWalletProvider(ctx context.Context,
	cfg *LndConfig, network string) (*lndWalletProvider, error) {

	lndNetwork, err := networkToLndclient(network)
	if err != nil {
		return nil, err
	}

	rpcTimeout := cfg.RPCTimeout
	if rpcTimeout == 0 {
		rpcTimeout = DefaultRPCTimeout
	}

	lndServices, err := lndclient.NewLndServices(
		&lndclient.LndServicesConfig{
			LndAddress:            cfg.Host,
			Network:               lndNetwork,
			CustomMacaroonPath:    cfg.MacaroonPath,
			TLSPath:               cfg.TLSPath,
			BlockUntilChainSynced: true,
			BlockUntilUnlocked:    true,
			CallerCtx:             ctx,
			RPCTimeout:            rpcTimeout,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("connect to lnd: %w", err)
	}

	chainBackend := chainbackends.NewLNDBackendFromLndClient(
		chainbackends.LNDBackendFromLndClientConfig{
			LND: &lndServices.LndServices,
		},
	)

	boardingBackend := lndbackend.NewBoardingBackend(
		lndServices.WalletKit,
	)

	clientWallet := lndbackend.NewClientWallet(
		lndServices.Signer, lndServices.WalletKit,
	)

	return &lndWalletProvider{
		lnd:            lndServices,
		chainBackend:   chainBackend,
		boardingBacked: boardingBackend,
		clientWallet:   clientWallet,
	}, nil
}

// ChainBackend returns the LND chain backend for actor system
// registrations.
func (p *lndWalletProvider) ChainBackend() chainsource.ChainBackend {
	return p.chainBackend
}

// BoardingBackend returns the LND boarding backend for taproot
// script import and UTXO listing.
func (p *lndWalletProvider) BoardingBackend() wallet.BoardingBackend {
	return p.boardingBacked
}

// ClientWallet returns the LND client wallet for MuSig2 signing
// and key derivation.
func (p *lndWalletProvider) ClientWallet() round.ClientWallet {
	return p.clientWallet
}

// ChainParams returns the Bitcoin network parameters from the
// connected LND node.
func (p *lndWalletProvider) ChainParams() *chaincfg.Params {
	return p.lnd.ChainParams
}

// NodePubkey returns the hex-encoded identity public key of the
// connected LND node.
func (p *lndWalletProvider) NodePubkey() string {
	return p.lnd.NodePubkey.String()
}

// OnChainBalance returns the confirmed and unconfirmed on-chain
// balance by querying LND's WalletBalance RPC.
func (p *lndWalletProvider) OnChainBalance(
	ctx context.Context) (btcutil.Amount, btcutil.Amount, error) {

	balance, err := p.lnd.Client.WalletBalance(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf(
			"get wallet balance: %w", err,
		)
	}

	return balance.Confirmed, balance.Unconfirmed, nil
}

// NewAddress generates a new P2TR on-chain receiving address via
// LND's wallet kit.
func (p *lndWalletProvider) NewAddress(
	ctx context.Context) (btcutil.Address, error) {

	addr, err := p.lnd.WalletKit.NextAddr(
		ctx, "",
		walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return nil, fmt.Errorf("next address: %w", err)
	}

	return addr, nil
}

// Close releases the LND connection and all associated resources.
func (p *lndWalletProvider) Close() error {
	p.lnd.Close()

	return nil
}

// Compile-time check that lndWalletProvider satisfies WalletProvider.
var _ WalletProvider = (*lndWalletProvider)(nil)
