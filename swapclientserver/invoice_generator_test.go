//go:build swapruntime

package swapclientserver

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/darepo-client/sdk/swaps"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// stubInvoiceCreator records calls and returns canned responses.
type stubInvoiceCreator struct {
	withKeyCalls     int
	withKeyPathCalls int
	noKeyCalls       int
}

// CreateInvoice records a no-key invocation. The daemonAuthOnlyInvoiceCreator
// wrapper must reject this path before it ever reaches the stub.
func (s *stubInvoiceCreator) CreateInvoice(_ context.Context, _ btcutil.Amount,
	_ string, _ *swaps.RouteHint, _ time.Duration, _ *lntypes.Preimage) (
	*invoices.Invoice, lntypes.Hash, error) {

	s.noKeyCalls++

	return &invoices.Invoice{}, lntypes.Hash{}, nil
}

// CreateInvoiceWithKey records a keyed invocation so the test can confirm
// that the wrapper forwards to the underlying generator.
func (s *stubInvoiceCreator) CreateInvoiceWithKey(_ context.Context,
	_ btcutil.Amount, _ string, _ *swaps.RouteHint, _ time.Duration,
	_ keychain.SingleKeyMessageSigner, _ *lntypes.Preimage) (
	*invoices.Invoice, lntypes.Hash, error) {

	s.withKeyCalls++

	return &invoices.Invoice{}, lntypes.Hash{}, nil
}

// CreateInvoiceWithKeyRouteHintPaths records a keyed multi-path invocation so
// the test can confirm that the wrapper forwards new route-hint-aware calls.
func (s *stubInvoiceCreator) CreateInvoiceWithKeyRouteHintPaths(
	_ context.Context, _ btcutil.Amount, _ string, _ [][]*swaps.RouteHint,
	_ time.Duration, _ keychain.SingleKeyMessageSigner,
	_ *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	s.withKeyPathCalls++

	return &invoices.Invoice{}, lntypes.Hash{}, nil
}

// TestDaemonAuthOnlyInvoiceCreatorRejectsNoKey asserts that the daemon swap
// path can never silently fall back to ephemeral-key signing: a direct
// CreateInvoice call returns an error and the underlying generator is never
// invoked.
func TestDaemonAuthOnlyInvoiceCreatorRejectsNoKey(t *testing.T) {
	t.Parallel()

	stub := &stubInvoiceCreator{}
	wrapper := &daemonAuthOnlyInvoiceCreator{inner: stub}

	_, _, err := wrapper.CreateInvoice(
		t.Context(), btcutil.Amount(1000),
		"memo", nil, time.Minute, nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CreateInvoiceWithKey")
	require.Equal(
		t, 0, stub.noKeyCalls,
		"inner CreateInvoice must not be reached",
	)
	require.Equal(t, 0, stub.withKeyCalls)
	require.Equal(t, 0, stub.withKeyPathCalls)
}

// TestDaemonAuthOnlyInvoiceCreatorForwardsKeyedPath asserts that the wrapper
// passes a keyed invoice creation through to the underlying generator with
// the caller-supplied auth key intact.
func TestDaemonAuthOnlyInvoiceCreatorForwardsKeyedPath(t *testing.T) {
	t.Parallel()

	stub := &stubInvoiceCreator{}
	wrapper := &daemonAuthOnlyInvoiceCreator{inner: stub}

	// Construct an in-memory auth key. The wrapper does not interpret it;
	// it only passes the reference through to the inner generator.
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	authKey := keychain.NewPrivKeyMessageSigner(
		priv, keychain.KeyLocator{},
	)

	_, _, err = wrapper.CreateInvoiceWithKey(
		t.Context(), btcutil.Amount(1000),
		"memo", nil, time.Minute, authKey, nil,
	)
	require.NoError(t, err)
	require.Equal(t, 1, stub.withKeyCalls)
	require.Equal(t, 0, stub.withKeyPathCalls)
	require.Equal(t, 0, stub.noKeyCalls)
}

// TestDaemonAuthOnlyInvoiceCreatorForwardsKeyedRouteHintPaths asserts that
// the daemon wrapper preserves the route-hint paths used for hidden-primary
// payments.
func TestDaemonAuthOnlyInvoiceCreatorForwardsKeyedRouteHintPaths(t *testing.T) {
	t.Parallel()

	stub := &stubInvoiceCreator{}
	wrapper := &daemonAuthOnlyInvoiceCreator{inner: stub}

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	authKey := keychain.NewPrivKeyMessageSigner(
		priv, keychain.KeyLocator{},
	)

	_, _, err = wrapper.CreateInvoiceWithKeyRouteHintPaths(
		t.Context(), btcutil.Amount(1000),
		"memo", nil, time.Minute, authKey, nil,
	)
	require.NoError(t, err)
	require.Equal(t, 0, stub.withKeyCalls)
	require.Equal(t, 1, stub.withKeyPathCalls)
	require.Equal(t, 0, stub.noKeyCalls)
}
