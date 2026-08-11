package lnruntime

import (
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// externalWalletController records accidental ownership transfers while its
// embedded interface supplies methods unused by the lifecycle test.
type externalWalletController struct {
	lnwallet.WalletController

	starts atomic.Int32
	stops  atomic.Int32
}

// Start records an unexpected start by lnd.
func (c *externalWalletController) Start() error {
	c.starts.Add(1)

	return nil
}

// Stop records an unexpected stop by lnd.
func (c *externalWalletController) Stop() error {
	c.stops.Add(1)

	return nil
}

// TestRuntimeStartsNativeFunding verifies Wavelength composes lnd's funding
// manager without transferring ownership of its already-running base wallet.
func TestRuntimeStartsNativeFunding(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	keyRing := &mock.SecretKeyRing{RootKey: nodeKey}
	controller := &externalWalletController{}
	baseNotifier := newRuntimeNotifier(800_000)
	notifier, err := NewVirtualFundingNotifier(baseNotifier)
	require.NoError(t, err)

	runtime, err := NewRuntime(RuntimeConfig{
		DB:       db,
		Chain:    fixedHeightChain{height: 800_000},
		Notifier: notifier,
		OnionKey: &keychain.PrivKeyECDH{PrivKey: nodeKey},
		Signer: input.NewMockSigner(
			[]*btcec.PrivateKey{nodeKey},
			&chaincfg.RegressionNetParams,
		),
		FeeEstimator: chainfee.NewStaticEstimator(1_250, 253),
		WitnessBeacon: &runtimeWitnessBeacon{
			cache: db.NewWitnessCache(),
		},
		SelfNode: route.NewVertex(nodeKey.PubKey()),
		Funding: &FundingConfig{
			WalletController: controller,
			KeyRing:          keyRing,
			NetParams:        &chaincfg.RegressionNetParams,
			IdentityKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamilyNodeKey,
				},
				PubKey: nodeKey.PubKey(),
			},
			ChannelAcceptor: chanacceptor.NewChainedAcceptor(),
			RoutingPolicy: models.ForwardingPolicy{
				MinHTLCOut:    1,
				TimeLockDelta: 18,
			},
			NotifyWhenOnline: func([33]byte, chan<- lnpeer.Peer) {},
			WatchNewChannel: func(*chanstate.OpenChannel,
				*btcec.PublicKey) error {

				return nil
			},
			NotifyFinalized: func([32]byte, bool) error {
				return nil
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, runtime.Funding())
	require.NoError(t, runtime.Start())
	require.NoError(t, runtime.Stop())
	require.Zero(t, controller.starts.Load())
	require.Zero(t, controller.stops.Load())
}

var _ lnwallet.WalletController = (*externalWalletController)(nil)
