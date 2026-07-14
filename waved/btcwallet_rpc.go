package waved

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	btcrpcserver "github.com/btcsuite/btcwallet/rpc/rpcserver"
	btcwalletrpc "github.com/btcsuite/btcwallet/rpc/walletrpc"
	btcwalletbase "github.com/btcsuite/btcwallet/wallet"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const btcwalletRPCBufSize = 1024 * 1024

// registerBtcwalletRPC registers btcsuite btcwallet's native gRPC surface on
// waved's public gRPC server. The wallet service is a proxy because waved's
// gRPC server starts while self-managed wallets may still be locked, but gRPC
// service registration must happen before Serve starts.
func registerBtcwalletRPC(grpcServer *grpc.Server, server *Server) func() {
	btcrpcserver.StartVersionService(grpcServer)

	walletServer := &btcwalletRPCProxy{
		server: server,
	}
	btcwalletrpc.RegisterWalletServiceServer(grpcServer, walletServer)

	return walletServer.stop
}

type btcwalletRPCProxy struct {
	btcwalletrpc.UnimplementedWalletServiceServer

	server *Server

	mu         sync.Mutex
	native     *btcwalletbase.Wallet
	grpcServer *grpc.Server
	listener   *bufconn.Listener
	conn       *grpc.ClientConn
}

func (p *btcwalletRPCProxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.stopLocked()
}

func (p *btcwalletRPCProxy) stopLocked() {
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	if p.grpcServer != nil {
		p.grpcServer.Stop()
		p.grpcServer = nil
	}
	if p.listener != nil {
		_ = p.listener.Close()
		p.listener = nil
	}

	p.native = nil
}

func (p *btcwalletRPCProxy) walletClient(ctx context.Context) (
	btcwalletrpc.WalletServiceClient, error) {

	nativeWallet, err := p.server.nativeBtcwallet()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn != nil && p.native == nativeWallet {
		return btcwalletrpc.NewWalletServiceClient(p.conn), nil
	}

	p.stopLocked()

	listener := bufconn.Listen(btcwalletRPCBufSize)
	grpcServer := grpc.NewServer()
	btcrpcserver.StartWalletService(grpcServer, nativeWallet)

	errChan := make(chan error, 1)
	go func() {
		errChan <- grpcServer.Serve(listener)
	}()

	conn, err := grpc.NewClient(
		"passthrough:///btcwallet-rpc", grpc.WithContextDialer(
			func(dialCtx context.Context, _ string) (net.Conn,
				error) {

				return listener.DialContext(dialCtx)
			},
		),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	if err != nil {
		grpcServer.Stop()
		_ = listener.Close()

		return nil, fmt.Errorf("dial btcwallet RPC proxy: %w", err)
	}

	p.native = nativeWallet
	p.grpcServer = grpcServer
	p.listener = listener
	p.conn = conn

	go func() {
		err := <-errChan
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			p.server.log.WarnS(
				ctx,
				"Native btcwallet RPC proxy stopped",
				err,
			)
		}
	}()

	return btcwalletrpc.NewWalletServiceClient(conn), nil
}

func (s *Server) nativeBtcwallet() (*btcwalletbase.Wallet, error) {
	var internal btcwalletbase.Interface

	switch s.cfg.Wallet.Type {
	case WalletTypeLwwallet:
		if s.lwWallet.IsSome() {
			w := s.lwWallet.UnsafeFromSome()
			internal = w.BtcWallet.InternalWallet()
		}

	case WalletTypeBtcwallet:
		if s.btcwWallet.IsSome() {
			w := s.btcwWallet.UnsafeFromSome()
			internal = w.BtcWallet.InternalWallet()
		}

	default:
		return nil, fmt.Errorf("native btcwallet RPC is only " +
			"available for lwwallet and btcwallet backends")
	}

	if internal == nil {
		return nil, fmt.Errorf("native btcwallet is not initialized")
	}

	wallet, ok := internal.(*btcwalletbase.Wallet)
	if !ok {
		return nil, fmt.Errorf("native btcwallet has "+
			"unexpected type %T", internal)
	}

	return wallet, nil
}

func (p *btcwalletRPCProxy) Ping(ctx context.Context,
	req *btcwalletrpc.PingRequest) (*btcwalletrpc.PingResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.Ping(ctx, req)
}

func (p *btcwalletRPCProxy) Network(ctx context.Context,
	req *btcwalletrpc.NetworkRequest) (*btcwalletrpc.NetworkResponse,
	error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.Network(ctx, req)
}

func (p *btcwalletRPCProxy) AccountNumber(ctx context.Context,
	req *btcwalletrpc.AccountNumberRequest) (
	*btcwalletrpc.AccountNumberResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.AccountNumber(ctx, req)
}

func (p *btcwalletRPCProxy) Accounts(ctx context.Context,
	req *btcwalletrpc.AccountsRequest) (*btcwalletrpc.AccountsResponse,
	error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.Accounts(ctx, req)
}

func (p *btcwalletRPCProxy) Balance(ctx context.Context,
	req *btcwalletrpc.BalanceRequest) (*btcwalletrpc.BalanceResponse,
	error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.Balance(ctx, req)
}

func (p *btcwalletRPCProxy) GetTransactions(ctx context.Context,
	req *btcwalletrpc.GetTransactionsRequest) (
	*btcwalletrpc.GetTransactionsResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.GetTransactions(ctx, req)
}

func (p *btcwalletRPCProxy) TransactionNotifications(
	req *btcwalletrpc.TransactionNotificationsRequest,
	stream btcwalletrpc.WalletService_TransactionNotificationsServer,
) error {

	client, err := p.walletClient(stream.Context())
	if err != nil {
		return err
	}

	upstream, err := client.TransactionNotifications(stream.Context(), req)
	if err != nil {
		return err
	}

	for {
		resp, err := upstream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			return nil

		case err != nil:
			return err
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (p *btcwalletRPCProxy) SpentnessNotifications(
	req *btcwalletrpc.SpentnessNotificationsRequest,
	stream btcwalletrpc.WalletService_SpentnessNotificationsServer,
) error {

	client, err := p.walletClient(stream.Context())
	if err != nil {
		return err
	}

	upstream, err := client.SpentnessNotifications(stream.Context(), req)
	if err != nil {
		return err
	}

	for {
		resp, err := upstream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			return nil

		case err != nil:
			return err
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (p *btcwalletRPCProxy) AccountNotifications(
	req *btcwalletrpc.AccountNotificationsRequest,
	stream btcwalletrpc.WalletService_AccountNotificationsServer,
) error {

	client, err := p.walletClient(stream.Context())
	if err != nil {
		return err
	}

	upstream, err := client.AccountNotifications(stream.Context(), req)
	if err != nil {
		return err
	}

	for {
		resp, err := upstream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			return nil

		case err != nil:
			return err
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (p *btcwalletRPCProxy) ChangePassphrase(ctx context.Context,
	req *btcwalletrpc.ChangePassphraseRequest) (
	*btcwalletrpc.ChangePassphraseResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.ChangePassphrase(ctx, req)
}

func (p *btcwalletRPCProxy) RenameAccount(ctx context.Context,
	req *btcwalletrpc.RenameAccountRequest) (
	*btcwalletrpc.RenameAccountResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.RenameAccount(ctx, req)
}

func (p *btcwalletRPCProxy) NextAccount(ctx context.Context,
	req *btcwalletrpc.NextAccountRequest) (
	*btcwalletrpc.NextAccountResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.NextAccount(ctx, req)
}

func (p *btcwalletRPCProxy) NextAddress(ctx context.Context,
	req *btcwalletrpc.NextAddressRequest) (
	*btcwalletrpc.NextAddressResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.NextAddress(ctx, req)
}

func (p *btcwalletRPCProxy) ImportPrivateKey(ctx context.Context,
	req *btcwalletrpc.ImportPrivateKeyRequest) (
	*btcwalletrpc.ImportPrivateKeyResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.ImportPrivateKey(ctx, req)
}

func (p *btcwalletRPCProxy) FundTransaction(ctx context.Context,
	req *btcwalletrpc.FundTransactionRequest) (
	*btcwalletrpc.FundTransactionResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.FundTransaction(ctx, req)
}

func (p *btcwalletRPCProxy) SignTransaction(ctx context.Context,
	req *btcwalletrpc.SignTransactionRequest) (
	*btcwalletrpc.SignTransactionResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.SignTransaction(ctx, req)
}

func (p *btcwalletRPCProxy) PublishTransaction(ctx context.Context,
	req *btcwalletrpc.PublishTransactionRequest) (
	*btcwalletrpc.PublishTransactionResponse, error) {

	client, err := p.walletClient(ctx)
	if err != nil {
		return nil, err
	}

	return client.PublishTransaction(ctx, req)
}
