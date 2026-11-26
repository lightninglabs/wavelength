package waved

import (
	"context"
	"sort"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// assetAddressWallet returns the SDK wallet of an asset backend that creates
// Taproot Asset addresses and receives at them.
func (r *RPCServer) assetAddressWallet() (*tapsdk.Wallet, error) {
	wallet, err := r.assetWallet()
	if err != nil {
		return nil, err
	}
	if !r.server.assetAddresses {
		return nil, status.Error(
			codes.Unimplemented,
			"Taproot Asset addresses are not available here",
		)
	}

	return wallet, nil
}

// NewAssetAddress returns the wallet's Taproot Asset address for an asset,
// which it creates on the first call.
func (r *RPCServer) NewAssetAddress(ctx context.Context,
	req *waverpc.NewAssetAddressRequest) (*waverpc.AssetAddress, error) {

	ref, err := tapsdk.ParseAssetRef(req.GetAssetRef())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"asset_ref: %v", err)
	}

	wallet, err := r.assetAddressWallet()
	if err != nil {
		return nil, err
	}

	courier, err := r.server.cfg.Assets.Courier.addr()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	if courier == nil {
		return nil, status.Error(
			codes.FailedPrecondition, "Taproot Asset addresses "+
				"require a proof courier: set "+
				"assets.courier.addr",
		)
	}

	// The engine holds a mailbox stream open for every address it stores,
	// so an asset gets one address, however often it is asked for.
	r.assetAddressMu.Lock()
	defer r.assetAddressMu.Unlock()

	addrs, err := wallet.Client().QueryAddrs(ctx, &tapsdk.AddressQuery{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list asset "+
			"addresses: %v", err)
	}
	for _, addr := range addrs {
		if addr == nil || addr.Amount != 0 ||
			addr.ProofCourierAddr != courier.String() ||
			!addr.AssetRef.Equivalent(ref) {

			continue
		}

		return assetAddressToProto(addr), nil
	}

	addr, err := wallet.NewReceiveAddress(ctx, ref)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create asset "+
			"address: %v", err)
	}

	return assetAddressToProto(addr), nil
}

// ListAssetAddresses returns the wallet's Taproot Asset addresses, oldest
// first.
func (r *RPCServer) ListAssetAddresses(ctx context.Context,
	_ *waverpc.ListAssetAddressesRequest) (
	*waverpc.ListAssetAddressesResponse, error) {

	resp := &waverpc.ListAssetAddressesResponse{}
	if r.server.assetClient == nil {
		return resp, nil
	}

	wallet, err := r.assetAddressWallet()
	if err != nil {
		return nil, err
	}

	addrs, err := wallet.Client().QueryAddrs(ctx, &tapsdk.AddressQuery{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list asset "+
			"addresses: %v", err)
	}
	for _, addr := range addrs {
		if addr == nil {
			continue
		}
		resp.Addresses = append(
			resp.Addresses, assetAddressToProto(addr),
		)
	}

	return resp, nil
}

// ListAssetReceives returns the transfers sent to the wallet's Taproot
// Asset addresses, oldest first.
func (r *RPCServer) ListAssetReceives(ctx context.Context,
	_ *waverpc.ListAssetReceivesRequest) (
	*waverpc.ListAssetReceivesResponse, error) {

	resp := &waverpc.ListAssetReceivesResponse{}
	if r.server.assetClient == nil {
		return resp, nil
	}

	wallet, err := r.assetAddressWallet()
	if err != nil {
		return nil, err
	}

	events, err := wallet.Client().AddrReceives(
		ctx, &tapsdk.AddressReceivesQuery{
			Direction: tapsdk.SortAscending,
		},
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list asset "+
			"receives: %v", err)
	}
	outputs, err := receivedOutputs(events)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list asset "+
			"receives: %v", err)
	}

	// The engine's events name no asset and no amount, and an output can
	// have several confirmed events. What an output paid is therefore read
	// from the assets the wallet holds or held at it.
	var records []*tapsdk.AssetRecord
	for _, output := range outputs {
		if output.status != assetReceiveConfirmed {
			continue
		}

		records, err = wallet.Client().ListAssetRecords(
			ctx, &tapsdk.ListAssetsRequest{IncludeSpent: true},
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list "+
				"received assets: %v", err)
		}

		break
	}
	resp.Receives = assetReceivesToProto(outputs, records)

	return resp, nil
}

// SyncAssetReceives reads the mailbox of every Taproot Asset address once
// and works once on every unfinished receive.
func (r *RPCServer) SyncAssetReceives(ctx context.Context,
	_ *waverpc.SyncAssetReceivesRequest) (
	*waverpc.SyncAssetReceivesResponse, error) {

	if _, err := r.assetAddressWallet(); err != nil {
		return nil, err
	}

	engine := r.server.embeddedAssets
	if engine == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"the asset wallet requires an asset backend",
		)
	}

	// A pass fails when a courier or the chain backend cannot be reached,
	// which a later call can outlast.
	if err := engine.SyncReceives(ctx); err != nil {
		return nil, status.Errorf(codes.Unavailable, "sync asset "+
			"receives: %v", err)
	}

	return &waverpc.SyncAssetReceivesResponse{}, nil
}

// assetAddressToProto converts an address to its RPC form.
func assetAddressToProto(addr *tapsdk.Address) *waverpc.AssetAddress {
	return &waverpc.AssetAddress{
		Address:          addr.Encoded,
		AssetRef:         addr.AssetRef.String(),
		ProofCourierAddr: addr.ProofCourierAddr,
	}
}

// assetReceiveStatus orders the states of a receive by how far it got.
type assetReceiveStatus uint8

const (
	// assetReceiveFailed is a transfer the wallet took no asset from.
	assetReceiveFailed assetReceiveStatus = iota

	// assetReceiveDetected is a transfer a sender announced.
	assetReceiveDetected

	// assetReceiveConfirmed is a transfer whose assets the wallet took.
	assetReceiveConfirmed
)

// rpcStatus converts the state of a receive to its RPC form.
func (s assetReceiveStatus) rpcStatus() waverpc.AssetReceiveStatus {
	switch s {
	case assetReceiveConfirmed:
		return waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_CONFIRMED

	case assetReceiveDetected:
		return waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_DETECTED

	default:
		return waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_FAILED
	}
}

// receivedOutput is what the engine's events say about one anchor output
// paid to one address.
type receivedOutput struct {
	outpoint tapsdk.Outpoint
	address  *tapsdk.Address
	status   assetReceiveStatus
	height   uint32
}

// receivedOutputs folds the engine's address events, one per announcement,
// into one entry per anchor output and address, in the order of their first
// event. Anyone who knows an address can announce an output, so an output
// takes the state of the announcement that got furthest.
func receivedOutputs(events []*tapsdk.AddressEvent) ([]*receivedOutput, error) {
	type outputKey struct {
		outpoint tapsdk.Outpoint
		address  string
	}

	var (
		outputs []*receivedOutput
		byKey   = make(map[outputKey]*receivedOutput)
	)
	for _, event := range events {
		if event == nil || event.Address == nil {
			continue
		}

		outpoint, err := tapsdk.NewOutpointFromStr(event.Outpoint)
		if err != nil {
			return nil, err
		}

		var eventStatus assetReceiveStatus
		switch event.Status {
		case tapsdk.AddressEventStatusCompleted:
			eventStatus = assetReceiveConfirmed

		// The engine reports a failed receive with the unknown status.
		case tapsdk.AddressEventStatusUnknown:
			eventStatus = assetReceiveFailed

		default:
			eventStatus = assetReceiveDetected
		}

		key := outputKey{
			outpoint: outpoint,
			address:  event.Address.Encoded,
		}
		output, ok := byKey[key]
		if !ok {
			output = &receivedOutput{
				outpoint: outpoint,
				address:  event.Address,
				status:   eventStatus,
				height:   event.ConfirmationHeight,
			}
			byKey[key] = output
			outputs = append(outputs, output)

			continue
		}
		if eventStatus > output.status {
			output.status = eventStatus
			output.height = event.ConfirmationHeight
		}
	}

	return outputs, nil
}

// assetReceivesToProto lists what each received output paid. A confirmed
// output has one entry per asset ID the wallet holds or held at it, and any
// other output one entry without an asset ID.
func assetReceivesToProto(outputs []*receivedOutput,
	records []*tapsdk.AssetRecord) []*waverpc.AssetReceive {

	// units sums the wallet's records at each output by asset ID.
	units := make(map[tapsdk.Outpoint]map[tapsdk.AssetID]uint64)
	for _, record := range records {
		if record == nil || record.AnchorOutpoint == nil {
			continue
		}

		held := units[*record.AnchorOutpoint]
		if held == nil {
			held = make(map[tapsdk.AssetID]uint64)
			units[*record.AnchorOutpoint] = held
		}
		held[record.Genesis.IssuanceID] += record.Amount
	}

	var receives []*waverpc.AssetReceive
	for _, output := range outputs {
		addr := output.address
		receive := func() *waverpc.AssetReceive {
			return &waverpc.AssetReceive{
				Outpoint:           output.outpoint.String(),
				Address:            addr.Encoded,
				Status:             output.status.rpcStatus(),
				AssetRef:           addr.AssetRef.String(),
				ConfirmationHeight: output.height,
			}
		}

		held := units[output.outpoint]
		if output.status != assetReceiveConfirmed || len(held) == 0 {
			receives = append(receives, receive())

			continue
		}

		assetIDs := make([]tapsdk.AssetID, 0, len(held))
		for assetID := range held {
			assetIDs = append(assetIDs, assetID)
		}
		sort.Slice(assetIDs, func(i, j int) bool {
			return assetIDs[i].String() < assetIDs[j].String()
		})
		for _, assetID := range assetIDs {
			paid := receive()
			paid.AssetId = assetID.String()
			paid.Amount = held[assetID]
			receives = append(receives, paid)
		}
	}

	return receives
}
