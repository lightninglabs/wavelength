package chainbackends

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

// stubLndClientNotifier exposes controllable lndclient notification streams.
type stubLndClientNotifier struct {
	lndclient.ChainNotifierClient

	confChan  chan *chainntnfs.TxConfirmation
	confErr   chan error
	spendChan chan *chainntnfs.SpendDetail
	spendErr  chan error
}

// RegisterConfirmationsNtfn returns the controllable confirmation streams.
func (s *stubLndClientNotifier) RegisterConfirmationsNtfn(_ context.Context,
	_ *chainhash.Hash, _ []byte, _, _ int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	return s.confChan, s.confErr, nil
}

// RegisterSpendNtfn returns the controllable spend streams.
func (s *stubLndClientNotifier) RegisterSpendNtfn(_ context.Context,
	_ *wire.OutPoint, _ []byte, _ int32, _ ...lndclient.NotifierOption) (
	chan *chainntnfs.SpendDetail, chan error, error) {

	return s.spendChan, s.spendErr, nil
}

// TestLndClientConfStreamErrorClosesRegistration verifies that an lndclient
// receive error cancels the forwarding context and closes the downstream
// registration instead of leaving it blocked forever.
func TestLndClientConfStreamErrorClosesRegistration(t *testing.T) {
	t.Parallel()

	stub := &stubLndClientNotifier{
		confChan: make(chan *chainntnfs.TxConfirmation),
		confErr:  make(chan error, 1),
	}
	notifier := NewLndClientChainNotifier(LndClientChainNotifierConfig{
		LND: &lndclient.LndServices{ChainNotifier: stub},
	})

	event, err := notifier.RegisterConfirmationsNtfn(
		&chainhash.Hash{0x01}, []byte{0x51}, 1, 100,
	)
	require.NoError(t, err)
	defer event.Cancel()

	stub.confErr <- errors.New("confirmation stream failed")

	select {
	case _, ok := <-event.Confirmed:
		require.False(t, ok, "confirmation stream remained open")

	case <-time.After(time.Second):
		t.Fatal("confirmation stream error was not propagated")
	}
}

// TestLndClientSpendStreamErrorClosesRegistration is the spend-side
// equivalent of TestLndClientConfStreamErrorClosesRegistration.
func TestLndClientSpendStreamErrorClosesRegistration(t *testing.T) {
	t.Parallel()

	stub := &stubLndClientNotifier{
		spendChan: make(chan *chainntnfs.SpendDetail),
		spendErr:  make(chan error, 1),
	}
	notifier := NewLndClientChainNotifier(LndClientChainNotifierConfig{
		LND: &lndclient.LndServices{ChainNotifier: stub},
	})

	event, err := notifier.RegisterSpendNtfn(
		&wire.OutPoint{
			Hash: chainhash.Hash{0x02},
		},
		[]byte{0x51},
		100,
	)
	require.NoError(t, err)
	defer event.Cancel()

	stub.spendErr <- errors.New("spend stream failed")

	select {
	case _, ok := <-event.Spend:
		require.False(t, ok, "spend stream remained open")

	case <-time.After(time.Second):
		t.Fatal("spend stream error was not propagated")
	}
}
