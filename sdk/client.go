package sdk

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const (
	// bufConnSize is the buffer size for the in-memory gRPC
	// listener used for daemon-to-SDK communication.
	bufConnSize = 1 << 20
)

// Config configures the embedded SDK client. The DaemonConfig is
// passed directly to the daemon; the SDK manages daemon lifecycle
// (start, ready, stop) transparently.
type Config struct {
	// DaemonConfig is the full daemon configuration. The SDK
	// overrides DaemonConfig.RPC.Listener with a bufconn
	// listener for in-process communication.
	DaemonConfig *darepod.Config
}

// Client is a high-level SDK façade that embeds a running daemon and
// communicates with it via gRPC over an in-memory bufconn transport.
// When New() returns, the daemon is fully started and all RPCs are
// available.
type Client struct {
	server *darepod.Server
	cancel context.CancelFunc
	wg     sync.WaitGroup

	conn   *grpc.ClientConn
	daemon daemonrpc.DaemonServiceClient
}

// New creates and starts an embedded daemon, connects via bufconn,
// and returns a ready-to-use SDK client. The call blocks until the
// daemon signals readiness (round actor started, operator terms
// fetched).
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.DaemonConfig == nil {
		return nil, fmt.Errorf("daemon config is required")
	}

	// Create an in-memory listener so the SDK communicates
	// with the daemon without TCP overhead or port allocation.
	bufListener := bufconn.Listen(bufConnSize)

	// Inject the bufconn listener into the daemon config,
	// overriding any existing listener or listen address.
	if cfg.DaemonConfig.RPC == nil {
		cfg.DaemonConfig.RPC = &darepod.RPCConfig{}
	}
	cfg.DaemonConfig.RPC.Listener = bufListener

	// If no listen address is set and validation requires one,
	// provide a placeholder since we use the pre-supplied
	// listener.
	if cfg.DaemonConfig.RPC.ListenAddr == "" {
		cfg.DaemonConfig.RPC.ListenAddr = "bufconn"
	}

	srv, err := darepod.NewServer(cfg.DaemonConfig)
	if err != nil {
		return nil, fmt.Errorf("create daemon: %w", err)
	}

	// Start the daemon in a background goroutine with a
	// cancelable context for clean shutdown.
	daemonCtx, cancel := context.WithCancel(ctx)

	c := &Client{
		server: srv,
		cancel: cancel,
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		if err := srv.RunWithContext(daemonCtx); err != nil {
			// Context cancellation is expected during
			// Stop(); only log unexpected errors.
			if daemonCtx.Err() == nil {
				fmt.Printf("darepod exited with "+
					"error: %v\n", err)
			}
		}
	}()

	// Block until the daemon signals readiness. This ensures
	// all subsystems (wallet, actor system, round actor) are
	// initialized before the SDK returns.
	select {
	case <-srv.Ready():

	case <-ctx.Done():
		cancel()
		c.wg.Wait()

		return nil, fmt.Errorf(
			"context cancelled waiting for daemon: %w",
			ctx.Err(),
		)
	}

	// Dial the daemon via bufconn.
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(
			func(dialCtx context.Context,
				_ string) (net.Conn, error) {

				return bufListener.DialContext(dialCtx)
			},
		),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	if err != nil {
		cancel()
		c.wg.Wait()

		return nil, fmt.Errorf(
			"dial daemon via bufconn: %w", err,
		)
	}

	c.conn = conn
	c.daemon = daemonrpc.NewDaemonServiceClient(conn)

	return c, nil
}

// Stop shuts down the embedded daemon and releases all resources.
func (c *Client) Stop() error {
	c.cancel()
	c.wg.Wait()

	if c.conn != nil {
		return c.conn.Close()
	}

	return nil
}

// GetInfo returns basic information about the running daemon.
func (c *Client) GetInfo(
	ctx context.Context) (*daemonrpc.GetInfoResponse, error) {

	return c.daemon.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
}

// RequestRoundOutputs registers desired output amounts for the
// next round.
func (c *Client) RequestRoundOutputs(ctx context.Context,
	amounts []btcutil.Amount) error {

	protoAmounts := make([]int64, len(amounts))
	for i, a := range amounts {
		protoAmounts[i] = int64(a)
	}

	_, err := c.daemon.RequestRoundOutputs(
		ctx, &daemonrpc.RequestRoundOutputsRequest{
			Amounts: protoAmounts,
		},
	)

	return err
}

// JoinRound tells the daemon to join the next available round.
func (c *Client) JoinRound(ctx context.Context) error {
	_, err := c.daemon.JoinRound(
		ctx, &daemonrpc.JoinRoundRequest{},
	)

	return err
}

// CompletedRoundID returns the most recently completed round ID.
func (c *Client) CompletedRoundID(
	ctx context.Context) (string, error) {

	resp, err := c.daemon.CompletedRoundID(
		ctx, &daemonrpc.CompletedRoundIDRequest{},
	)
	if err != nil {
		return "", err
	}

	return resp.RoundId, nil
}

// ListVTXOs returns all VTXOs known to the daemon.
func (c *Client) ListVTXOs(
	ctx context.Context) ([]*daemonrpc.VTXOInfo, error) {

	resp, err := c.daemon.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{},
	)
	if err != nil {
		return nil, err
	}

	return resp.Vtxos, nil
}

// GetBalance returns the total live VTXO balance.
func (c *Client) GetBalance(
	ctx context.Context) (btcutil.Amount, error) {

	resp, err := c.daemon.GetBalance(
		ctx, &daemonrpc.GetBalanceRequest{},
	)
	if err != nil {
		return 0, err
	}

	return btcutil.Amount(resp.Balance), nil
}

// SendOORPayment performs a single-input out-of-round transfer to
// a recipient address.
func (c *Client) SendOORPayment(ctx context.Context,
	recipientAddress string, amount btcutil.Amount) error {

	_, err := c.daemon.SendOORPayment(
		ctx, &daemonrpc.SendOORPaymentRequest{
			RecipientAddress: recipientAddress,
			Amount:           int64(amount),
		},
	)

	return err
}

// NewReceiveAddress derives a fresh recipient address for incoming
// OOR transfers.
func (c *Client) NewReceiveAddress(
	ctx context.Context) (string, error) {

	resp, err := c.daemon.NewReceiveAddress(
		ctx, &daemonrpc.NewReceiveAddressRequest{},
	)
	if err != nil {
		return "", err
	}

	return resp.Address, nil
}

// SyncIncoming fetches and materializes all unprocessed incoming
// OOR transfers.
func (c *Client) SyncIncoming(
	ctx context.Context) (int, error) {

	resp, err := c.daemon.SyncIncoming(
		ctx, &daemonrpc.SyncIncomingRequest{},
	)
	if err != nil {
		return 0, err
	}

	return int(resp.Processed), nil
}

// GetOnChainBalance returns the on-chain wallet balance.
func (c *Client) GetOnChainBalance(
	ctx context.Context) (confirmed, unconfirmed btcutil.Amount,
	err error) {

	resp, err := c.daemon.GetOnChainBalance(
		ctx, &daemonrpc.GetOnChainBalanceRequest{},
	)
	if err != nil {
		return 0, 0, err
	}

	return btcutil.Amount(resp.Confirmed),
		btcutil.Amount(resp.Unconfirmed), nil
}

// GetNewAddress generates a new on-chain receiving address.
func (c *Client) GetNewAddress(
	ctx context.Context) (string, error) {

	resp, err := c.daemon.GetNewAddress(
		ctx, &daemonrpc.GetNewAddressRequest{},
	)
	if err != nil {
		return "", err
	}

	return resp.Address, nil
}
