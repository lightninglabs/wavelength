package waved

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// channelReservationBackend supplies a fixed tip when source admission is
// enabled, without depending on separate channel-lifetime test fixtures.
type channelReservationBackend struct {
	chainsource.ChainBackend
}

// BestBlock returns a tip safely below the selected source expiries.
func (*channelReservationBackend) BestBlock(context.Context) (int32,
	chainhash.Hash, error) {

	return 500, chainhash.Hash{}, nil
}

// TestPrepareArkChannelOORPreservesReservationEpochs follows selected wallet
// epochs through channel preparation, its durable mailbox and snapshot codec.
func TestPrepareArkChannelOORPreservesReservationEpochs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	terms := testPrePONRTerms(t)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	copy(
		terms.VTXO.ArkOperatorKey[:],
		operator.PubKey().SerializeCompressed(),
	)
	const backingFee btcutil.Amount = 1_000
	coins, deliveries, sessions, packages, reservations :=
		newSendOORTestStores(t)
	first, firstKey := newSendOORTestVTXO(
		t, operator.PubKey(), 0x85, 40_000,
	)
	second, secondKey := newSendOORTestVTXO(
		t, operator.PubKey(), 0x86,
		terms.Capacity+backingFee-first.Amount,
	)
	second.Outpoint.Hash = first.Outpoint.Hash
	second.Outpoint.Index = first.Outpoint.Index + 1
	first.BatchExpiry = 10_000
	second.BatchExpiry = 10_000
	require.NoError(t, coins.SaveVTXO(ctx, first))
	require.NoError(t, coins.SaveVTXO(ctx, second))
	selectedA := selectedVTXOFromDescriptor(first)
	selectedA.ReserveEpoch = 41
	selectedB := selectedVTXOFromDescriptor(second)
	selectedB.ReserveEpoch = 73
	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{
			{
				selectedB,
				selectedA,
			},
		},
	}
	system := actor.NewActorSystem()
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		require.NoError(t, system.Shutdown(stopCtx))
	})
	walletKey := actor.NewServiceKey[wallet.WalletMsg, wallet.WalletResp](
		"channel-reservation-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "channel-reservation-wallet", testWallet,
	)
	registry, err := oor.NewOORRegistryActor(oor.OORRegistryConfig{
		Log: fn.Some[btclog.Logger](btclog.Disabled),
		Signer: input.NewMockSigner(
			[]*btcec.PrivateKey{firstKey, secondKey}, nil,
		),
		IncomingHandler:  noopOORHandler{},
		RegistryStore:    sessions,
		DeliveryStore:    deliveries,
		ServerConn:       &fakeOORServerConn{},
		PackageStore:     packages,
		ReservationStore: reservations,
		ActorSystem:      system,
	})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	server := &Server{
		cfg: &Config{}, log: btclog.Disabled,
		chainParams: &chaincfg.RegressionNetParams,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: operator.
					PubKey().
					SerializeCompressed(),
				VtxoExitDelay: 10, DustLimit: 1,
			},
		}),
		actorSystem: system, vtxoStore: coins,
		walletRef:       fn.Some(walletRef),
		oorSessionStore: sessions,
		chainBackend:    &channelReservationBackend{},
	}
	server.arkProtocolVersion.Store(1)
	binding, err := server.prepareArkChannelOOR(ctx, terms, backingFee)
	require.NoError(t, err)
	response, err := registry.Ref().Ask(ctx, &oor.GetStateRequest{
		SessionID: oor.SessionID(binding.OORSessionID),
	}).Await(ctx).Unpack()
	require.NoError(t, err)
	stateResponse, ok := response.(*oor.GetStateResponse)
	require.True(t, ok)
	prepared, ok := stateResponse.State.(*oor.Prepared)
	require.True(t, ok)
	snapshot, err := oor.NewOutgoingSnapshot(
		oor.SessionID(binding.OORSessionID), prepared,
	)
	require.NoError(t, err)
	var encoded bytes.Buffer
	restore := &oor.RestoreSessionRequest{Snapshot: snapshot}
	require.NoError(t, restore.Encode(&encoded))
	var decoded oor.RestoreSessionRequest
	require.NoError(t, decoded.Decode(&encoded))
	restored, err := oor.OutgoingStateFromSnapshot(decoded.Snapshot)
	require.NoError(t, err)
	prepared, ok = restored.(*oor.Prepared)
	require.True(t, ok)
	epochs := make(map[wire.OutPoint]uint64)
	for _, transferInput := range prepared.TransferInputs {
		epochs[transferInput.VTXO.Outpoint] = transferInput.ReserveEpoch
	}
	require.Equal(t, map[wire.OutPoint]uint64{
		selectedA.Outpoint: selectedA.ReserveEpoch,
		selectedB.Outpoint: selectedB.ReserveEpoch,
	}, epochs)
	require.Equal(t, 1, testWallet.selectCount())
	require.Empty(t, testWallet.unlockBatches())
}
