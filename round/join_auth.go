package round

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// joinRoundAuthIdentifierKeyFamily is the BIP32 key family used
	// for per-request join authorization identifier keys.
	joinRoundAuthIdentifierKeyFamily keychain.KeyFamily = 43
)

// deriveJoinAuthIdentifierKey derives a fresh key descriptor used as the
// join-request BIP-322 identifier and challenge key.
func deriveJoinAuthIdentifierKey(ctx context.Context,
	wallet ClientWallet) (keychain.KeyDescriptor, error) {

	if wallet == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"wallet signer must be provided",
		)
	}

	keyDesc, err := wallet.DeriveNextKey(
		ctx, joinRoundAuthIdentifierKeyFamily,
	)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"derive join auth identifier key: %w", err,
		)
	}

	if keyDesc == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"derive join auth identifier key returned nil " +
				"descriptor",
		)
	}

	if keyDesc.PubKey == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"derive join auth identifier key returned nil " +
				"pubkey",
		)
	}

	return *keyDesc, nil
}

// sortedForfeitRequests returns deterministic forfeit requests for all
// refreshing and leaving VTXOs. Outpoints are sorted by txid bytes then
// output index so the resulting list is stable across map iterations.
func sortedForfeitRequests(refreshing map[wire.OutPoint]*RefreshVTXORequest,
	leaving map[wire.OutPoint]*LeaveVTXORequest) []*ForfeitRequest {

	uniqueOutpoints := make(map[wire.OutPoint]struct{})
	for outpoint := range refreshing {
		uniqueOutpoints[outpoint] = struct{}{}
	}
	for outpoint := range leaving {
		uniqueOutpoints[outpoint] = struct{}{}
	}

	outpoints := make([]wire.OutPoint, 0, len(uniqueOutpoints))
	for outpoint := range uniqueOutpoints {
		outpoints = append(outpoints, outpoint)
	}
	sortOutPoints(outpoints)

	requests := make([]*ForfeitRequest, 0, len(outpoints))
	for i := 0; i < len(outpoints); i++ {
		requests = append(requests, &ForfeitRequest{
			VTXOOutpoint: outpoints[i],
		})
	}

	return requests
}

// sortedLeaveRequests returns deterministic leave requests for leaving
// VTXOs. The iteration order is stabilised by sorting outpoints before
// building the request slice.
func sortedLeaveRequests(
	leaving map[wire.OutPoint]*LeaveVTXORequest) []*LeaveRequest {

	outpoints := make([]wire.OutPoint, 0, len(leaving))
	for outpoint := range leaving {
		outpoints = append(outpoints, outpoint)
	}
	sortOutPoints(outpoints)

	requests := make([]*LeaveRequest, 0, len(outpoints))
	for i := 0; i < len(outpoints); i++ {
		request := leaving[outpoints[i]]
		requests = append(requests, &LeaveRequest{
			Output: request.Output,
		})
	}

	return requests
}

// sortOutPoints orders outpoints by txid bytes then output index.
func sortOutPoints(outpoints []wire.OutPoint) {
	sort.Slice(outpoints, func(i, j int) bool {
		left := outpoints[i]
		right := outpoints[j]

		hashCmp := bytes.Compare(left.Hash[:], right.Hash[:])
		if hashCmp != 0 {
			return hashCmp < 0
		}

		return left.Index < right.Index
	})
}

// computeTotalForfeitAmount computes the total value of VTXOs being
// forfeited via refresh and leave requests. Both contribute input
// value to the batch transaction.
func computeTotalForfeitAmount(
	refreshing map[wire.OutPoint]*RefreshVTXORequest,
	leaving map[wire.OutPoint]*LeaveVTXORequest) btcutil.Amount {

	var total btcutil.Amount
	for _, request := range refreshing {
		total += btcutil.Amount(request.Amount)
	}
	for _, request := range leaving {
		total += btcutil.Amount(request.Amount)
	}

	return total
}
