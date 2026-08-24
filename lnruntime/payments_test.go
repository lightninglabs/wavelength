package lnruntime

import (
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestFixedRoutePaymentsUsesNativeLifecycle verifies a one-hop payment is
// recorded and settled by lnd's normal control tower.
func TestFixedRoutePaymentsUsesNativeLifecycle(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	preimage := lntypes.Preimage{1, 2, 3}
	payer := newSettlingPayer(preimage)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	payments, err := NewFixedRoutePayments(FixedRoutePaymentsConfig{
		DB:       db,
		Chain:    fixedHeightChain{height: 800_000},
		Payer:    payer,
		SelfNode: route.NewVertex(clientKey.PubKey()),
		GetLink: func(lnwire.ShortChannelID) (htlcswitch.ChannelLink,
			error) {

			return nil, htlcswitch.ErrChannelLinkNotFound
		},
	})
	require.NoError(t, err)
	require.NoError(t, payments.Start())
	t.Cleanup(func() {
		require.NoError(t, payments.Stop())
	})

	const (
		channelID = uint64(9_001)
		amount    = lnwire.MilliSatoshi(25_000)
	)
	paymentRoute := &route.Route{
		TotalTimeLock: 800_040,
		TotalAmount:   amount,
		SourcePubKey:  route.NewVertex(clientKey.PubKey()),
		Hops: []*route.Hop{
			{
				PubKeyBytes: route.NewVertex(
					operatorKey.PubKey(),
				),
				ChannelID:        channelID,
				OutgoingTimeLock: 800_040,
				AmtToForward:     amount,
				LegacyPayload:    true,
			},
		},
	}

	attempt, err := payments.SendToOperator(
		t.Context(), preimage.Hash(), paymentRoute, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, attempt.Settle)
	require.Equal(t, preimage, attempt.Settle.Preimage)
	require.Equal(
		t, lnwire.NewShortChanIDFromInt(channelID), payer.firstHop,
	)

	payment, err := payments.ControlTower().FetchPayment(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, paymentsdb.StatusSucceeded, payment.GetStatus())
}

// TestFixedRoutePaymentsRejectsNonOperatorRoute verifies pathfinding cannot be
// smuggled into the client through a multi-hop route.
func TestFixedRoutePaymentsRejectsNonOperatorRoute(t *testing.T) {
	t.Parallel()

	payments := &FixedRoutePayments{}
	_, err := payments.SendToOperator(
		t.Context(), lntypes.Hash{}, &route.Route{
			Hops: []*route.Hop{{}, {}},
		}, nil,
	)
	require.ErrorIs(t, err, ErrOneHopRouteRequired)
}

// settlingPayer is a deterministic PaymentAttemptDispatcher used to prove the
// lnd lifecycle owns attempt persistence and settlement.
type settlingPayer struct {
	mu       sync.Mutex
	preimage lntypes.Preimage
	results  map[uint64]chan *htlcswitch.PaymentResult
	firstHop lnwire.ShortChannelID
}

// newSettlingPayer creates a dispatcher that settles every sent attempt.
func newSettlingPayer(preimage lntypes.Preimage) *settlingPayer {
	return &settlingPayer{
		preimage: preimage,
		results:  make(map[uint64]chan *htlcswitch.PaymentResult),
	}
}

// SendHTLC records the first hop and makes a settlement result durable enough
// for the lifecycle's subsequent result lookup.
func (p *settlingPayer) SendHTLC(firstHop lnwire.ShortChannelID,
	attemptID uint64, htlc *lnwire.UpdateAddHTLC) error {

	p.mu.Lock()
	defer p.mu.Unlock()

	result := make(chan *htlcswitch.PaymentResult, 1)
	result <- &htlcswitch.PaymentResult{Preimage: p.preimage}
	p.results[attemptID] = result
	p.firstHop = firstHop

	if htlc.PaymentHash != p.preimage.Hash() {
		return errUnexpectedPaymentHash
	}

	return nil
}

// GetAttemptResult returns the result created by SendHTLC.
func (p *settlingPayer) GetAttemptResult(attemptID uint64, _ lntypes.Hash,
	_ htlcswitch.ErrorDecrypter) (<-chan *htlcswitch.PaymentResult, error) {

	p.mu.Lock()
	defer p.mu.Unlock()

	result, ok := p.results[attemptID]
	if !ok {
		return nil, htlcswitch.ErrPaymentIDNotFound
	}

	return result, nil
}

// CleanStore is a no-op because test results live only for one test.
func (*settlingPayer) CleanStore(map[uint64]struct{}) error {
	return nil
}

// HasAttemptResult reports whether this dispatcher has a stored result.
func (p *settlingPayer) HasAttemptResult(attemptID uint64) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, ok := p.results[attemptID]

	return ok, nil
}

// fixedHeightChain supplies the current height needed by resumed lnd payment
// lifecycles. Fixed-route sends do not query any other chain data.
type fixedHeightChain struct {
	height int32
}

// GetBestBlock returns the configured test height.
func (c fixedHeightChain) GetBestBlock() (*chainhash.Hash, int32, error) {
	return &chainhash.Hash{}, c.height, nil
}

// GetUtxo is unused by fixed-route payment execution.
func (fixedHeightChain) GetUtxo(*wire.OutPoint, []byte, uint32,
	<-chan struct{}) (*wire.TxOut, error) {

	return nil, errUnexpectedChainQuery
}

// GetBlockHash is unused by fixed-route payment execution.
func (fixedHeightChain) GetBlockHash(int64) (*chainhash.Hash, error) {
	return nil, errUnexpectedChainQuery
}

// GetBlock is unused by fixed-route payment execution.
func (fixedHeightChain) GetBlock(*chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, errUnexpectedChainQuery
}

// GetBlockHeader is unused by fixed-route payment execution.
func (fixedHeightChain) GetBlockHeader(*chainhash.Hash) (*wire.BlockHeader,
	error) {

	return nil, errUnexpectedChainQuery
}

var (
	errUnexpectedPaymentHash = &runtimeTestError{"unexpected payment hash"}
	errUnexpectedChainQuery  = &runtimeTestError{"unexpected chain query"}
)

// runtimeTestError avoids string matching in test-only adapters.
type runtimeTestError struct {
	message string
}

// Error returns the test adapter failure message.
func (e *runtimeTestError) Error() string {
	return e.message
}

var _ routing.PaymentAttemptDispatcher = (*settlingPayer)(nil)
var _ lnwallet.BlockChainIO = fixedHeightChain{}
