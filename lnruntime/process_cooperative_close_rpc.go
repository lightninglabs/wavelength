package lnruntime

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
)

// ArkChannelRecordToRPC converts the compact durable channel summary exposed
// by local and peer process RPCs.
func ArkChannelRecordToRPC(record arkchannel.Record) *arkchannelrpc.ArkChannel {
	snapshot := record.Snapshot
	channel := &arkchannelrpc.ArkChannel{
		ChannelId:    snapshot.Terms.ID[:],
		Kind:         snapshot.Terms.Kind.String(),
		Phase:        arkChannelPublicPhase(snapshot),
		Funder:       snapshot.Terms.Funder.String(),
		CapacitySat:  int64(snapshot.Terms.Capacity),
		Revision:     record.Revision,
		ReservedScid: snapshot.Terms.ReservedSCID,
	}
	if snapshot.Source != nil {
		channel.SourceOutpoint = snapshot.Source.OutPoint.String()
	}
	if snapshot.Backing != nil {
		channel.ChannelPoint = snapshot.Backing.ChannelPoint.String()
	}
	if snapshot.CooperativeClose != nil {
		channel.RefreshOorTxid =
			snapshot.CooperativeClose.TxID[:]
	}

	return channel
}

// arkChannelPublicPhase keeps internal durable phase names out of the local
// API where the 3-of-3 OOR operation is presented as an in-Ark refresh.
func arkChannelPublicPhase(snapshot arkchannel.Snapshot) string {
	switch snapshot.Phase {
	case arkchannel.PhaseCoopClosing:
		return "refreshing"

	case arkchannel.PhaseCoopCloseSigned:
		return "refresh_authorized"

	case arkchannel.PhaseCoopClosePublished:
		return "refresh_oor_finalized"

	case arkchannel.PhaseClosed:
		if snapshot.CooperativeClose != nil {
			return "refreshed"
		}

		return snapshot.Phase.String()

	default:
		return snapshot.Phase.String()
	}
}

// rpcAmount rejects negative wire amounts before converting them.
func rpcAmount(name string, value int64) (btcutil.Amount, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}

	return btcutil.Amount(value), nil
}

// rpcHash parses one fixed-width transaction hash.
func rpcHash(name string, raw []byte) (chainhash.Hash, error) {
	var hash chainhash.Hash
	if len(raw) != len(hash) {
		return hash, fmt.Errorf("%s must be %d bytes", name, len(hash))
	}
	copy(hash[:], raw)

	return hash, nil
}
