package swaps

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btclog/v2"
)

// HtlcListener manages a streaming connection to the swap server's
// RegisterReceiver RPC and delivers intercepted HTLCs to the swap
// client. It acts as a thin coordination layer between the
// streaming gRPC transport and the local swap processing logic.
type HtlcListener struct {
	server SwapServerConn
	log    btclog.Logger
}

// NewHtlcListener creates a new HTLC listener that reads from the
// given swap server connection.
func NewHtlcListener(server SwapServerConn,
	log btclog.Logger) *HtlcListener {

	if log == nil {
		log = btclog.Disabled
	}

	return &HtlcListener{
		server: server,
		log:    log,
	}
}

// Listen opens the RegisterReceiver stream and returns a channel
// that delivers HTLC intercepts as they arrive. The channel is
// closed when the context is cancelled or the stream terminates.
//
// The caller should select on both the returned channel and the
// context's Done channel to detect shutdown.
func (l *HtlcListener) Listen(
	ctx context.Context) (<-chan HtlcIntercept, error) {

	l.log.InfoS(ctx, "Opening HTLC receiver stream")

	htlcCh, err := l.server.RegisterReceiver(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"register receiver: %w", err,
		)
	}

	// Wrap the raw channel in a logging relay so we can
	// observe each intercept without modifying the transport
	// layer.
	out := make(chan HtlcIntercept, cap(htlcCh))
	go l.relay(ctx, htlcCh, out)

	return out, nil
}

// relay reads from the source channel and forwards each intercept
// to the output channel while logging arrival details.
func (l *HtlcListener) relay(ctx context.Context,
	src <-chan HtlcIntercept, dst chan<- HtlcIntercept) {

	defer close(dst)

	for {
		select {
		case <-ctx.Done():
			l.log.InfoS(
				ctx,
				"HTLC listener shutting down",
			)

			return

		case htlc, ok := <-src:
			if !ok {
				l.log.InfoS(
					ctx,
					"HTLC receiver stream closed",
				)

				return
			}

			l.log.InfoS(ctx,
				"HTLC intercepted",
				btclog.Hex(
					"payment_hash",
					htlc.PaymentHash[:],
				),
				slog.Uint64(
					"amount_msat",
					htlc.IncomingAmountMsat,
				),
				slog.Uint64(
					"expiry",
					uint64(htlc.IncomingExpiry),
				),
			)

			select {
			case dst <- htlc:
			case <-ctx.Done():
				return
			}
		}
	}
}
