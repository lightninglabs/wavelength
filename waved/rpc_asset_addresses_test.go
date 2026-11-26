package waved

import (
	"context"
	"fmt"
	"testing"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/tap-sdk/embedded/courier"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeAddressBackend is an asset backend that creates addresses under its
// default proof courier and lists scripted receives and holdings.
type fakeAddressBackend struct {
	tapsdk.Client

	courier string
	addrs   []*tapsdk.Address

	events  []*tapsdk.AddressEvent
	records []*tapsdk.AssetRecord

	// listed are the requests that listed the wallet's records.
	listed []*tapsdk.ListAssetsRequest
}

// NewAddr creates an address for the requested asset.
func (f *fakeAddressBackend) NewAddr(_ context.Context,
	req *tapsdk.NewAddressRequest) (*tapsdk.Address, error) {

	addr := &tapsdk.Address{
		Encoded:          fmt.Sprintf("taprt1addr%d", len(f.addrs)),
		AssetRef:         req.AssetRef,
		Amount:           req.Amount,
		ProofCourierAddr: f.courier,
	}
	f.addrs = append(f.addrs, addr)

	return addr, nil
}

// QueryAddrs returns the created addresses, oldest first.
func (f *fakeAddressBackend) QueryAddrs(context.Context, *tapsdk.AddressQuery) (
	[]*tapsdk.Address, error) {

	return f.addrs, nil
}

// AddrReceives returns the scripted address events.
func (f *fakeAddressBackend) AddrReceives(context.Context,
	*tapsdk.AddressReceivesQuery) ([]*tapsdk.AddressEvent, error) {

	return f.events, nil
}

// ListAssetRecords returns the scripted holdings.
func (f *fakeAddressBackend) ListAssetRecords(_ context.Context,
	req *tapsdk.ListAssetsRequest) ([]*tapsdk.AssetRecord, error) {

	f.listed = append(f.listed, req)

	return f.records, nil
}

// newAssetAddressFixture returns an RPC server whose asset backend creates
// addresses under the configured proof courier.
func newAssetAddressFixture() (*RPCServer, *fakeAddressBackend) {
	backend := &fakeAddressBackend{courier: testCourierAddr}
	server := newAssetChainTestServer(nil)
	server.cfg.Assets.Courier.Addr = testCourierAddr
	server.assetClient = backend
	server.assetAddresses = true

	return &RPCServer{server: server}, backend
}

// assetIDHex returns the hex form of the asset ID that starts with seed.
func assetIDHex(seed byte) string {
	return tapsdk.AssetID{seed}.String()
}

// testAssetOutpoint returns an anchor outpoint that differs per seed.
func testAssetOutpoint(seed byte) tapsdk.Outpoint {
	return tapsdk.Outpoint{Txid: [32]byte{seed}, Index: uint32(seed)}
}

// TestNewAssetAddress creates one address per asset and returns it for every
// later request of that asset, since the engine holds a mailbox stream open
// for each address it stores.
func TestNewAssetAddress(t *testing.T) {
	t.Parallel()

	rpcServer, backend := newAssetAddressFixture()
	ctx := t.Context()

	groupKeys, err := (&AssetsConfig{
		GroupKeys: []string{
			testGroupKeyHex,
		},
	}).groupKeys()
	require.NoError(t, err)
	groupRef := tapsdk.AssetRefFromGroupKey(groupKeys[0])
	assetRef := tapsdk.AssetRefFromAssetID(tapsdk.AssetID{7})

	first, err := rpcServer.NewAssetAddress(
		ctx, &waverpc.NewAssetAddressRequest{
			AssetRef: groupRef.String(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, groupRef.String(), first.AssetRef)
	require.Equal(t, testCourierAddr, first.ProofCourierAddr)

	again, err := rpcServer.NewAssetAddress(
		ctx, &waverpc.NewAssetAddressRequest{
			AssetRef: groupRef.String(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, first.Address, again.Address)
	require.Len(t, backend.addrs, 1)

	// Another asset gets an address of its own.
	other, err := rpcServer.NewAssetAddress(
		ctx, &waverpc.NewAssetAddressRequest{
			AssetRef: assetRef.String(),
		},
	)
	require.NoError(t, err)
	require.NotEqual(t, first.Address, other.Address)
	require.Len(t, backend.addrs, 2)

	listed, err := rpcServer.ListAssetAddresses(
		ctx, &waverpc.ListAssetAddressesRequest{},
	)
	require.NoError(t, err)
	require.Len(t, listed.Addresses, 2)
	require.Equal(t, first.Address, listed.Addresses[0].Address)
	require.Equal(t, other.Address, listed.Addresses[1].Address)

	// An address under another courier than the configured one is not
	// the wallet's address for the asset.
	rpcServer.server.cfg.Assets.Courier.Addr =
		"authmailbox+universerpc://other:10029"
	backend.courier = rpcServer.server.cfg.Assets.Courier.Addr
	moved, err := rpcServer.NewAssetAddress(
		ctx, &waverpc.NewAssetAddressRequest{
			AssetRef: groupRef.String(),
		},
	)
	require.NoError(t, err)
	require.NotEqual(t, first.Address, moved.Address)
	require.Len(t, backend.addrs, 3)

	_, err = rpcServer.NewAssetAddress(
		ctx, &waverpc.NewAssetAddressRequest{
			AssetRef: "not-an-asset",
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestNewAssetAddressRequirements refuses an address without a proof
// courier, without an asset backend, and where the engine receives at no
// address.
func TestNewAssetAddressRequirements(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ref := tapsdk.AssetRefFromAssetID(tapsdk.AssetID{7})
	req := &waverpc.NewAssetAddressRequest{AssetRef: ref.String()}

	rpcServer, backend := newAssetAddressFixture()
	rpcServer.server.cfg.Assets.Courier.Addr = ""
	_, err := rpcServer.NewAssetAddress(ctx, req)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "assets.courier.addr")
	require.Empty(t, backend.addrs)

	rpcServer, backend = newAssetAddressFixture()
	rpcServer.server.assetAddresses = false
	_, err = rpcServer.NewAssetAddress(ctx, req)
	require.Equal(t, codes.Unimplemented, status.Code(err))
	_, err = rpcServer.ListAssetAddresses(
		ctx, &waverpc.ListAssetAddressesRequest{},
	)
	require.Equal(t, codes.Unimplemented, status.Code(err))
	_, err = rpcServer.ListAssetReceives(
		ctx, &waverpc.ListAssetReceivesRequest{},
	)
	require.Equal(t, codes.Unimplemented, status.Code(err))
	_, err = rpcServer.SyncAssetReceives(
		ctx, &waverpc.SyncAssetReceivesRequest{},
	)
	require.Equal(t, codes.Unimplemented, status.Code(err))
	require.Empty(t, backend.addrs)

	// A daemon without assets has no address and no receive.
	rpcServer = &RPCServer{server: newAssetChainTestServer(nil)}
	_, err = rpcServer.NewAssetAddress(ctx, req)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	addrs, err := rpcServer.ListAssetAddresses(
		ctx, &waverpc.ListAssetAddressesRequest{},
	)
	require.NoError(t, err)
	require.Empty(t, addrs.Addresses)
	receives, err := rpcServer.ListAssetReceives(
		ctx, &waverpc.ListAssetReceivesRequest{},
	)
	require.NoError(t, err)
	require.Empty(t, receives.Receives)
}

// TestListAssetReceives lists one entry per anchor output and asset ID from
// the assets the wallet holds at the output. An output can have several
// events, one per announcement, and more than one of them can be confirmed.
func TestListAssetReceives(t *testing.T) {
	t.Parallel()

	rpcServer, backend := newAssetAddressFixture()
	ctx := t.Context()

	addr := &tapsdk.Address{
		Encoded:  "taprt1group",
		AssetRef: tapsdk.AssetRefFromAssetID(tapsdk.AssetID{0xaa}),
	}
	other := &tapsdk.Address{
		Encoded:  "taprt1other",
		AssetRef: tapsdk.AssetRefFromAssetID(tapsdk.AssetID{0xbb}),
	}
	event := func(outpoint tapsdk.Outpoint, to *tapsdk.Address,
		eventStatus tapsdk.AddressEventStatus,
		height uint32) *tapsdk.AddressEvent {

		return &tapsdk.AddressEvent{
			Address:            to,
			Status:             eventStatus,
			Outpoint:           outpoint.String(),
			ConfirmationHeight: height,
		}
	}
	record := func(outpoint tapsdk.Outpoint, assetID byte,
		amount uint64) *tapsdk.AssetRecord {

		return &tapsdk.AssetRecord{
			Genesis: tapsdk.IssuanceGenesis{
				IssuanceID: tapsdk.AssetID{
					assetID,
				},
			},
			Amount:         amount,
			AnchorOutpoint: &outpoint,
		}
	}

	var (
		completed = tapsdk.AddressEventStatusCompleted
		announced = tapsdk.AddressEventStatusTransactionConfirmed
		failed    = tapsdk.AddressEventStatusUnknown

		forged   = testAssetOutpoint(1)
		twoParts = testAssetOutpoint(2)
		waiting  = testAssetOutpoint(3)
		refused  = testAssetOutpoint(4)
	)
	backend.events = []*tapsdk.AddressEvent{
		// A forged announcement fails, and the sender's confirms.
		event(forged, addr, failed, 0),
		event(forged, addr, completed, 101),

		// One announcement names a part of an output and a later one
		// the whole of it, and both confirm.
		event(twoParts, addr, completed, 102),
		event(twoParts, addr, completed, 102),

		event(waiting, other, announced, 103),
		event(refused, other, failed, 0),

		// The same output announced to another address pays that
		// address nothing.
		event(forged, other, failed, 0),
	}
	backend.records = []*tapsdk.AssetRecord{
		record(forged, 0xaa, 100),
		record(twoParts, 0xa2, 30),
		record(twoParts, 0xa1, 20),

		// The wallet's own outputs are no receive.
		record(testAssetOutpoint(9), 0xaa, 999),
		{
			Amount: 5,
		},
	}

	resp, err := rpcServer.ListAssetReceives(
		ctx, &waverpc.ListAssetReceivesRequest{},
	)
	require.NoError(t, err)

	confirmed := waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_CONFIRMED
	detected := waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_DETECTED
	refusedRPC := waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_FAILED
	want := []*waverpc.AssetReceive{{
		Outpoint:           forged.String(),
		Address:            addr.Encoded,
		Status:             confirmed,
		AssetRef:           addr.AssetRef.String(),
		AssetId:            assetIDHex(0xaa),
		Amount:             100,
		ConfirmationHeight: 101,
	}, {
		Outpoint:           twoParts.String(),
		Address:            addr.Encoded,
		Status:             confirmed,
		AssetRef:           addr.AssetRef.String(),
		AssetId:            assetIDHex(0xa1),
		Amount:             20,
		ConfirmationHeight: 102,
	}, {
		Outpoint:           twoParts.String(),
		Address:            addr.Encoded,
		Status:             confirmed,
		AssetRef:           addr.AssetRef.String(),
		AssetId:            assetIDHex(0xa2),
		Amount:             30,
		ConfirmationHeight: 102,
	}, {
		Outpoint:           waiting.String(),
		Address:            other.Encoded,
		Status:             detected,
		AssetRef:           other.AssetRef.String(),
		ConfirmationHeight: 103,
	}, {
		Outpoint: refused.String(),
		Address:  other.Encoded,
		Status:   refusedRPC,
		AssetRef: other.AssetRef.String(),
	}, {
		Outpoint: forged.String(),
		Address:  other.Encoded,
		Status:   refusedRPC,
		AssetRef: other.AssetRef.String(),
	}}
	require.Len(t, resp.Receives, len(want))
	for idx := range want {
		require.Equal(
			t, want[idx].String(), resp.Receives[idx].String(),
		)
	}

	// A received output that the wallet has spent still lists its units.
	require.Len(t, backend.listed, 1)
	require.True(t, backend.listed[0].IncludeSpent)

	// Without a confirmed receive the holdings are not read.
	backend.events = backend.events[4:6]
	backend.listed = nil
	resp, err = rpcServer.ListAssetReceives(
		ctx, &waverpc.ListAssetReceivesRequest{},
	)
	require.NoError(t, err)
	require.Len(t, resp.Receives, 2)
	require.Empty(t, backend.listed)
}

// TestSyncAssetReceives runs a mailbox pass of the asset engine. A wallet
// without an address reads no mailbox.
func TestSyncAssetReceives(t *testing.T) {
	t.Parallel()

	rpcServer, _ := newAssetAddressFixture()
	keys := &assetKeyRing{server: rpcServer.server}
	store := embedded.NewMemoryStore()
	engine, err := embedded.NewClient(&embedded.Config{
		Network: tapsdk.NetworkRegtest,
		Store:   store,
		KeyRing: keys,
		Signer:  keys,
		Chain:   &assetChain{server: rpcServer.server},
		Receive: &embedded.ReceiveConfig{
			Store: store,
			Keys:  keys,
			Dial:  courier.NewDialer(nil),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	rpcServer.server.embeddedAssets = engine

	_, err = rpcServer.SyncAssetReceives(
		t.Context(), &waverpc.SyncAssetReceivesRequest{},
	)
	require.NoError(t, err)

	// A mailbox the engine cannot read is one a later call may reach.
	require.NoError(
		t,
		store.InsertAddress(
			t.Context(), embedded.AddressRecord{
				Encoded: "not-an-address",
			},
		),
	)
	_, err = rpcServer.SyncAssetReceives(
		t.Context(), &waverpc.SyncAssetReceivesRequest{},
	)
	require.Equal(t, codes.Unavailable, status.Code(err))
}
