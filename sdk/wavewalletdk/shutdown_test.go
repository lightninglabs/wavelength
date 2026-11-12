package wavewalletdk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/lightninglabs/wavelength/db"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
)

// TestCloseContextRetainsCleanup proves that a caller timeout neither cancels
// the cleanup owner nor becomes the cached result of all later Close calls.
func TestCloseContextRetainsCleanup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan context.Context, 1)
		release := make(chan struct{})
		terminal := errors.New("terminal cleanup result")
		var calls atomic.Int32
		client := &Client{closeFn: func(ctx context.Context) error {
			calls.Add(1)
			entered <- ctx
			<-release

			return terminal
		}}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, client.CloseContext(ctx), context.Canceled)
		cleanupCtx := <-entered
		require.NoError(t, cleanupCtx.Err())
		waiting := make(chan error, 1)
		go func() {
			waiting <- client.CloseContext(t.Context())
		}()
		// Wait proves the second caller has reached its cleanup wait
		// before checking that no result is available. No scheduler
		// timing assumption.
		synctest.Wait()
		select {
		case err := <-waiting:
			t.Fatalf("cleanup reported complete while its owner "+
				"is blocked: %v", err)

		default:
		}
		close(release)
		require.ErrorIs(t, <-waiting, terminal)
		require.ErrorIs(t, client.CloseContext(ctx), terminal)
		require.EqualValues(t, 1, calls.Load())
	})
}

// TestCloseJoinsLocalCallbacks runs every direct embedded callback through its
// production call site. Cancellation is not a join: the final resource access
// may still be blocked after observing cancellation.
func TestCloseJoinsLocalCallbacks(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"operator",
		"swap",
		"feed-operator",
		"feed-swap",
		"readiness",
		"recovery",
		"activity-batch",
		"activity-cursor",
		"lightning-receipts",
		"host-mode",
		"receipt-window",
		"maintenance-window",
		"cancel-window",
	} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			entered := make(chan struct{})
			cancelled := make(chan struct{})
			release := make(chan struct{})
			cleaned := make(chan struct{})
			client := &Client{closeFn: func(context.Context) error {
				close(cleaned)

				return nil
			}}
			callback := func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				<-release

				return ctx.Err()
			}
			invoke := shutdownCallbackInvoke(
				t, client, source, callback,
			)
			returned := make(chan error, 1)
			go func() {
				returned <- invoke()
			}()
			<-entered
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(
				t, client.CloseContext(ctx), context.Canceled,
			)
			<-cancelled
			require.ErrorIs(t, invoke(), ErrClientClosed)
			select {
			case <-cleaned:
				t.Fatal(
					"closed resources before the " +
						"callback's final access",
				)

			default:
			}
			close(release)
			require.ErrorIs(t, <-returned, context.Canceled)
			require.NoError(t, client.CloseContext(t.Context()))
			<-cleaned
		})
	}
}

// shutdownCallbackInvoke selects each actual direct-callback call site for the
// shared final-access join regression, without bypassing callback admission.
func shutdownCallbackInvoke(t *testing.T, client *Client, source string,
	callback func(context.Context) error) func() error {

	t.Helper()
	switch source {
	case "operator", "swap":
		register := func(ctx context.Context,
			_ *pb.NotificationSubscriptionMutation) (
			*pb.NotificationSubscriptionResponse, error) {

			return nil, callback(ctx)
		}
		selector := NotificationSourceOperator
		client.setNotificationSubscription = register
		if source == "swap" {
			selector = NotificationSourceSwapServer
			client.setSwapNotificationSubscription = register
		}

		return func() error {
			_, err := client.SetNotificationSubscription(
				t.Context(), NotificationSubscription{
					Source: selector,
				},
			)

			return err
		}

	case "feed-operator", "feed-swap":
		register := func(ctx context.Context, _ *pb.FeedGrantMutation) (
			*pb.FeedGrantResponse, error) {

			return nil, callback(ctx)
		}
		selector := NotificationSourceOperator
		client.setFeedGrant = register
		if source == "feed-swap" {
			selector = NotificationSourceSwapServer
			client.setSwapFeedGrant = register
		}

		return func() error {
			_, err := client.SetNotificationFeedGrant(
				t.Context(), NotificationFeedGrant{
					Source: selector,
				},
			)

			return err
		}

	case "readiness":
		client.waitWalletServicesReady = callback

		return func() error {
			return waitForExternalSeedWalletReady(
				t.Context(), client,
			)
		}

	case "activity-batch":
		client.readActivityBatch = func(ctx context.Context, _, _ int64,
			_ int32) (db.ActivityBatch, error) {

			return db.ActivityBatch{}, callback(ctx)
		}

		return func() error {
			_, err := client.ReadActivityBatch(
				t.Context(), ActivityBatchRequest{
					Limit: 1,
				},
			)

			return err
		}

	case "activity-cursor":
		client.activityCursor = func(ctx context.Context) (int64,
			error) {

			return 0, callback(ctx)
		}

		return func() error {
			_, err := client.ActivityCursor(t.Context())

			return err
		}

	case "lightning-receipts":
		client.readLightningReceipts = func(ctx context.Context,
			_ waved.LightningReceiptRequest) (
			[]waved.LightningReceipt, error) {

			return nil, callback(ctx)
		}

		return func() error {
			_, err := client.ReadLightningReceipts(
				t.Context(), LightningReceiptRequest{},
			)

			return err
		}

	case "host-mode":
		client.setHostForeground = func(ctx context.Context,
			_ bool) error {

			return callback(ctx)
		}

		return func() error {
			return client.SetHostForeground(t.Context(), true)
		}

	case "receipt-window":
		client.runReceiptWindow = func(ctx context.Context,
			_ waved.ReceiptWindowRequest) (
			waved.ReceiptWindowResult, error) {

			return waved.ReceiptWindowResult{}, callback(ctx)
		}

		return func() error {
			_, err := client.RunReceiptWindow(
				t.Context(), ReceiptWindowRequest{},
			)

			return err
		}

	case "maintenance-window":
		client.runMaintenanceWindow = func(ctx context.Context,
			_ string) (waved.ReceiptWindowResult, error) {

			return waved.ReceiptWindowResult{}, callback(ctx)
		}

		return func() error {
			_, err := client.RunMaintenanceWindow(
				t.Context(),
				"window",
			)

			return err
		}

	case "cancel-window":
		client.cancelReceiptWindow = func(ctx context.Context,
			_ string) error {

			return callback(ctx)
		}

		return func() error {
			return client.CancelReceiptWindow(t.Context(), "window")
		}

	case "recovery":
		client.recoverWalletState = func(ctx context.Context,
			_ uint32) (*waved.WalletRecoveryResult, error) {

			return nil, callback(ctx)
		}

		return func() error {
			return client.recoverExternalSeedWalletState(
				t.Context(), 0, &ExternalSeedWalletOpenResult{},
			)
		}

	default:
		t.Fatalf("unknown callback fixture %q", source)

		return nil
	}
}
