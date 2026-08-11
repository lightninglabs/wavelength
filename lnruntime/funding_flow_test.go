package lnruntime

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const testFundingCapacity = btcutil.Amount(200_000)

type fundingFinalization struct {
	channelPoint wire.OutPoint
}

// fundingFlowNode contains one composed runtime and the callbacks used to
// observe native lnd funding and channel-link activation.
type fundingFlowNode struct {
	key       *btcec.PrivateKey
	db        *channeldb.DB
	runtime   *Runtime
	notifier  *VirtualFundingNotifier
	peer      *Peer
	finalized chan fundingFinalization
	links     chan *chanstate.OpenChannel
	failures  chan error
	intents   *staticIntentSource
}

// fundingFlowTransport preserves lnd wire ordering over an in-memory version
// of the application transport used between Wavelength and swapdk-server.
type fundingFlowTransport struct {
	remote *fundingFlowNode
}

// SendMessages dispatches funding messages to funding.Manager and commitment
// updates to the native channel link.
func (t *fundingFlowTransport) SendMessages(_ bool,
	messages ...lnwire.Message) error {

	for _, message := range messages {
		switch message := message.(type) {
		case *lnwire.OpenChannel, *lnwire.AcceptChannel,
			*lnwire.FundingCreated, *lnwire.FundingSigned,
			*lnwire.ChannelReady, *lnwire.Warning, *lnwire.Error:

			if err := t.remote.runtime.Funding().ProcessMessage(
				message, t.remote.peer,
			); err != nil {
				return err
			}

		case lnwire.LinkUpdater:
			if err := t.remote.runtime.HandleChannelMessage(
				message,
			); err != nil {
				return err
			}

		case *lnwire.NodeAnnouncement1, *lnwire.ChannelAnnouncement1,
			*lnwire.ChannelUpdate1:

			// Private runtimes have no graph or gossiper.

		default:
			return fmt.Errorf("unexpected lnd message %T", message)
		}
	}

	return nil
}

// TestNativeFundingFlowPaysBothDirections proves the composed funding manager
// negotiates an unpublished channel and hands it to the same native links used
// by lnd's invoice and payment lifecycles.
func TestNativeFundingFlowPaysBothDirections(t *testing.T) {
	t.Parallel()

	alice := newFundingFlowNode(t, arkchannel.PartyHub)
	bob := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, alice, bob)
	require.NoError(t, alice.runtime.Start())
	require.NoError(t, bob.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, alice.runtime.Stop())
		require.NoError(t, bob.runtime.Stop())
	})

	pendingID := lndfunding.PendingChanID{1, 3, 3, 7}
	record, _ := testReceiveIntentRecord(t)
	record.Snapshot.Terms.PendingChannelID = pendingID
	record.Snapshot.Terms.Capacity = testFundingCapacity
	record.Snapshot.Terms.HubNodeKey = compressedIntentKey(alice.key)
	record.Snapshot.Terms.ClientNodeKey = compressedIntentKey(bob.key)
	record.Snapshot.Source.Amount = testFundingCapacity + 1_000
	record.Snapshot.Source.OutPoint = wire.OutPoint{
		Hash: chainhash.Hash{
			4,
			2,
		},
		Index: 1,
	}
	alice.intents.record = record
	bob.intents.record = record

	flow, err := alice.runtime.Funding().OpenChannel(FundingOpenRequest{
		Peer:             alice.peer,
		PendingChannelID: pendingID,
		Capacity:         testFundingCapacity,
	})
	require.NoError(t, err)
	packet := awaitFundingPSBT(t, flow)
	funding := completeVirtualFunding(
		t, packet, lnwire.NewShortChanIDFromInt(
			record.Snapshot.Terms.ReservedSCID,
		),
	)
	require.NoError(t, bob.runtime.Funding().RegisterBacking(funding))
	require.NoError(
		t, alice.runtime.Funding().FinalizeBacking(
			pendingID, packet, funding,
		),
	)

	aliceFinalized := awaitFinalized(t, alice, flow.Errors)
	bobFinalized := awaitFinalized(t, bob, nil)
	require.Equal(
		t, funding.Transaction.TxHash(),
		aliceFinalized.channelPoint.Hash,
	)
	require.Equal(t, aliceFinalized, bobFinalized)
	backing := arkBackingFromFunding(t, funding)
	for _, node := range []*fundingFlowNode{alice, bob} {
		finalized, err := node.runtime.Funding().FundingFinalized(
			t.Context(), record.Snapshot.Terms, backing,
		)
		require.NoError(t, err)
		require.True(t, finalized)
	}
	txid := funding.Transaction.TxHash()
	require.NoError(t, alice.runtime.Funding().ConfirmBacking(txid))
	require.NoError(t, bob.runtime.Funding().ConfirmBacking(txid))
	aliceChannel := awaitLink(t, alice)
	bobChannel := awaitLink(t, bob)
	require.Equal(
		t, aliceChannel.FundingOutpoint, bobChannel.FundingOutpoint,
	)
	require.False(t, aliceChannel.IsPending)
	require.False(t, bobChannel.IsPending)
	require.Positive(t, aliceChannel.LocalCommitment.LocalBalance)
	require.Zero(t, bobChannel.LocalCommitment.LocalBalance)

	const firstAmount = lnwire.MilliSatoshi(30_000_000)
	payRuntimeInvoice(t, alice, bob, firstAmount, lntypes.Preimage{1, 2, 3})

	const returnAmount = lnwire.MilliSatoshi(10_000_000)
	payRuntimeInvoice(
		t, bob, alice, returnAmount, lntypes.Preimage{7, 8, 9},
	)
}

// TestNativeFundingCancellationAfterFinalization proves a round can reseal
// after both lnd databases persist the channel but before Ark commits.
func TestNativeFundingCancellationAfterFinalization(t *testing.T) {
	t.Parallel()

	alice := newFundingFlowNode(t, arkchannel.PartyHub)
	bob := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, alice, bob)
	require.NoError(t, alice.runtime.Start())
	require.NoError(t, bob.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, alice.runtime.Stop())
		require.NoError(t, bob.runtime.Stop())
	})

	pendingID := lndfunding.PendingChanID{8, 6, 7, 5}
	record, _ := testReceiveIntentRecord(t)
	record.Snapshot.Terms.PendingChannelID = pendingID
	record.Snapshot.Terms.Capacity = testFundingCapacity
	record.Snapshot.Terms.HubNodeKey = compressedIntentKey(alice.key)
	record.Snapshot.Terms.ClientNodeKey = compressedIntentKey(bob.key)
	record.Snapshot.Source.Amount = testFundingCapacity + 1_000
	record.Snapshot.Source.OutPoint = wire.OutPoint{
		Hash: chainhash.Hash{
			4,
			2,
		},
		Index: 1,
	}
	alice.intents.record = record
	bob.intents.record = record

	flow, err := alice.runtime.Funding().OpenChannel(FundingOpenRequest{
		Peer:             alice.peer,
		PendingChannelID: pendingID,
		Capacity:         testFundingCapacity,
	})
	require.NoError(t, err)
	packet := awaitFundingPSBT(t, flow)
	funding := completeVirtualFunding(
		t, packet, lnwire.NewShortChanIDFromInt(
			record.Snapshot.Terms.ReservedSCID,
		),
	)
	require.NoError(t, bob.runtime.Funding().RegisterBacking(funding))
	require.NoError(
		t, alice.runtime.Funding().FinalizeBacking(
			pendingID, packet, funding,
		),
	)
	_ = awaitFinalized(t, alice, flow.Errors)
	_ = awaitFinalized(t, bob, nil)

	channelPoint := wire.OutPoint{
		Hash:  funding.Transaction.TxHash(),
		Index: funding.OutputIndex,
	}
	for _, node := range []*fundingFlowNode{alice, bob} {
		require.NoError(
			t, node.runtime.Funding().CancelBacking(
				pendingID, &channelPoint,
			),
		)
		_, err := node.db.ChannelStateDB().FetchChannel(channelPoint)
		require.ErrorIs(t, err, channeldb.ErrChannelNotFound)
		_, err = node.db.ChannelStateDB().FetchClosedChannel(
			&channelPoint,
		)
		require.NoError(t, err)
		require.NoError(
			t, node.runtime.Funding().CancelBacking(
				pendingID, &channelPoint,
			),
		)
		require.Error(
			t, node.runtime.Funding().ConfirmBacking(
				channelPoint.Hash,
			),
		)
	}
}

// newFundingFlowNode constructs one runtime before its transport-backed peer
// is connected.
func newFundingFlowNode(t *testing.T,
	localParty arkchannel.Party) *fundingFlowNode {

	t.Helper()

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	db := channeldb.OpenForTesting(t, t.TempDir())
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	node := &fundingFlowNode{
		key:       nodeKey,
		db:        db,
		finalized: make(chan fundingFinalization, 1),
		links:     make(chan *chanstate.OpenChannel, 1),
		failures:  make(chan error, 2),
		intents:   &staticIntentSource{},
	}
	baseNotifier := newRuntimeNotifier(800_000)
	node.notifier, err = NewVirtualFundingNotifier(baseNotifier)
	require.NoError(t, err)
	keyRing := &mock.SecretKeyRing{RootKey: nodeKey}
	walletController := &mock.WalletController{RootKey: nodeKey}
	signer := mock.NewSingleSigner(nodeKey)
	intentAcceptor, err := NewIntentAcceptor(localParty, node.intents)
	require.NoError(t, err)
	node.runtime, err = NewRuntime(RuntimeConfig{
		DB:           db,
		Chain:        fixedHeightChain{height: 800_000},
		Notifier:     node.notifier,
		OnionKey:     &keychain.PrivKeyECDH{PrivKey: nodeKey},
		Signer:       signer,
		FeeEstimator: chainfee.NewStaticEstimator(1_250, 253),
		WitnessBeacon: &runtimeWitnessBeacon{
			cache: db.NewWitnessCache(),
		},
		SelfNode: route.NewVertex(nodeKey.PubKey()),
		Funding: &FundingConfig{
			WalletController: walletController,
			KeyRing:          keyRing,
			NetParams:        &chaincfg.RegressionNetParams,
			IdentityKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamilyNodeKey,
				},
				PubKey: nodeKey.PubKey(),
			},
			ChannelAcceptor: intentAcceptor,
			RoutingPolicy: models.ForwardingPolicy{
				MinHTLCOut:    1,
				TimeLockDelta: 18,
			},
			NotifyWhenOnline: func(_ [33]byte,
				peerChan chan<- lnpeer.Peer) {

				peerChan <- node.peer
			},
			WatchNewChannel: func(*chanstate.OpenChannel,
				*btcec.PublicKey) error {

				return nil
			},
			NotifyPendingOpen: func(channelPoint wire.OutPoint,
				_ *chanstate.OpenChannel, _ *btcec.PublicKey) {

				node.finalized <- fundingFinalization{
					channelPoint: channelPoint,
				}
			},
		},
	})
	require.NoError(t, err)

	return node
}

// connectFundingFlowNodes creates each endpoint's view of the single logical
// peer and installs newly opened channels into its native switch.
func connectFundingFlowNodes(t *testing.T, alice, bob *fundingFlowNode) {
	t.Helper()

	alice.peer = newFundingFlowPeer(t, alice, bob)
	bob.peer = newFundingFlowPeer(t, bob, alice)
}

// newFundingFlowPeer adapts one side of the direct application transport.
func newFundingFlowPeer(t *testing.T, local, remote *fundingFlowNode) *Peer {
	t.Helper()

	var peer *Peer
	peer, err := NewPeer(PeerConfig{
		RemoteKey: remote.key.PubKey(),
		Transport: &fundingFlowTransport{remote: remote},
		AddChannel: func(channel *lnpeer.NewChannel,
			_ <-chan struct{}) error {

			_, err := local.runtime.AddLink(
				channel.OpenChannel,
				testLinkConfig(peer, local.failures),
			)
			if err != nil {
				return err
			}
			local.links <- channel.OpenChannel

			return nil
		},
	})
	require.NoError(t, err)

	return peer
}

// awaitFundingPSBT waits until lnd has negotiated both multisig keys and asks
// Ark to construct the external backing transaction.
func awaitFundingPSBT(t *testing.T, flow *FundingFlow) *psbt.Packet {
	t.Helper()

	for {
		select {
		case update := <-flow.Updates:
			psbtUpdate := update.GetPsbtFund()
			if psbtUpdate == nil {
				continue
			}
			packet, err := psbt.NewFromRawBytes(
				bytes.NewReader(psbtUpdate.Psbt), false,
			)
			require.NoError(t, err)

			return packet

		case err := <-flow.Errors:
			require.NoError(t, err)

		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for lnd funding PSBT")
		}
	}
}

// completeVirtualFunding adds a signed SegWit VTXO input while preserving the
// exact funding output negotiated by lnd.
func completeVirtualFunding(t *testing.T, packet *psbt.Packet,
	scid lnwire.ShortChannelID) VirtualFunding {

	t.Helper()

	previousOutpoint := wire.OutPoint{Hash: chainhash.Hash{4, 2}, Index: 1}
	packet.UnsignedTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: previousOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	vtxoScript := append([]byte{txscript.OP_1, 32}, make([]byte, 32)...)
	packet.Inputs = append(packet.Inputs, psbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    int64(testFundingCapacity + 1_000),
			PkScript: vtxoScript,
		},
	})
	require.NoError(t, packet.SanityCheck())

	fundingTx := packet.UnsignedTx.Copy()
	fundingTx.TxIn[0].Witness = wire.TxWitness{[]byte{1, 2, 3}}
	fundingOutput := uint32(len(fundingTx.TxOut) - 1)

	return VirtualFunding{
		Transaction: fundingTx,
		OutputIndex: fundingOutput,
		SCID:        scid,
	}
}

// arkBackingFromFunding serializes one fully signed virtual funding record.
func arkBackingFromFunding(t *testing.T,
	funding VirtualFunding) arkchannel.Backing {

	t.Helper()
	var transaction bytes.Buffer
	require.NoError(t, funding.Transaction.Serialize(&transaction))

	return arkchannel.Backing{
		Transaction: transaction.Bytes(),
		ChannelPoint: wire.OutPoint{
			Hash:  funding.Transaction.TxHash(),
			Index: funding.OutputIndex,
		},
	}
}

// awaitFinalized waits for lnd's commitment-signature safety barrier.
func awaitFinalized(t *testing.T, node *fundingFlowNode,
	fundingErrors <-chan error) fundingFinalization {

	t.Helper()

	select {
	case pendingID := <-node.finalized:
		return pendingID

	case err := <-node.failures:
		require.NoError(t, err)

	case err := <-fundingErrors:
		require.NoError(t, err)

	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for lnd funding finalization")
	}

	return fundingFinalization{}
}

// awaitLink waits until funding.Manager hands the open channel to the peer.
func awaitLink(t *testing.T, node *fundingFlowNode) *chanstate.OpenChannel {
	t.Helper()

	select {
	case channel := <-node.links:
		return channel

	case err := <-node.failures:
		require.NoError(t, err)

	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for lnd channel link")
	}

	return nil
}

// payRuntimeInvoice sends a fixed one-hop payment through lnd's native
// control tower, switch, channel links, and invoice registry.
func payRuntimeInvoice(t *testing.T, payer, payee *fundingFlowNode,
	amount lnwire.MilliSatoshi, preimage lntypes.Preimage) {

	t.Helper()

	_, err := payee.runtime.Invoices().AddInvoice(
		t.Context(), &invoices.Invoice{
			CreationDate: time.Now(),
			Terms: invoices.ContractTerm{
				FinalCltvDelta:  18,
				Expiry:          time.Hour,
				PaymentPreimage: &preimage,
				Value:           amount,
				Features:        emptyFeatureVector(),
			},
		}, preimage.Hash(),
	)
	require.NoError(t, err)

	channels, err := payer.db.ChannelStateDB().FetchAllOpenChannels()
	require.NoError(t, err)
	require.Len(t, channels, 1)
	scid := channels[0].ShortChanID().ToUint64()
	paymentRoute := &route.Route{
		TotalTimeLock: 800_040,
		TotalAmount:   amount,
		SourcePubKey:  route.NewVertex(payer.key.PubKey()),
		Hops: []*route.Hop{
			{
				PubKeyBytes: route.NewVertex(
					payee.key.PubKey(),
				),
				ChannelID:        scid,
				OutgoingTimeLock: 800_040,
				AmtToForward:     amount,
				LegacyPayload:    true,
			},
		},
	}

	result := make(chan error, 1)
	go func() {
		attempt, sendErr := payer.runtime.Payments().SendToOperator(
			t.Context(), preimage.Hash(), paymentRoute, nil,
		)
		if sendErr == nil && attempt.Settle == nil {
			sendErr = fmt.Errorf("payment attempt did not settle")
		}
		result <- sendErr
	}()

	select {
	case err := <-result:
		require.NoError(t, err)

	case err := <-payer.failures:
		require.NoError(t, err)

	case err := <-payee.failures:
		require.NoError(t, err)

	case <-time.After(10 * time.Second):
		t.Fatal("native lnd channel payment did not complete")
	}

	invoice, err := payee.runtime.Invoices().LookupInvoice(
		t.Context(), preimage.Hash(),
	)
	require.NoError(t, err)
	require.Equal(t, invoices.ContractSettled, invoice.State)
}
