package round

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightningnetwork/lnd/tlv"
)

// clientActorSnapshot groups rounds with pushes awaiting admission routing.
type clientActorSnapshot struct {
	pendingTimers     map[timeout.ID]*durableClientEffect
	rounds            map[RoundKeyStr]*restoredClientRound
	pendingQuotes     map[RoundID]*JoinRoundQuoteReceived
	commitmentTxIndex map[chainhash.Hash]RoundKeyStr
}

// encodeActorSnapshot runs while the owning actor serializes message delivery.
func (a *RoundClientActor) encodeActorSnapshot(ctx context.Context) ([]byte,
	error) {

	rounds, err := encodeDurableMap(
		a.rounds, encodeDurableRoundMapKey,
		func(round *RoundFSM) ([]byte, error) {
			return encodeDurableRound(ctx, round)
		},
	)
	if err != nil {
		return nil, err
	}
	quotes, err := encodeDurableMap(
		a.pendingQuotes, encodeDurableRoundID,
		func(quote *JoinRoundQuoteReceived) ([]byte, error) {
			if quote == nil || quote.Quote == nil {
				return nil, fmt.Errorf("missing buffered quote")
			}

			return encodeDurableQuote(quote.RoundID, quote.Quote)
		},
	)
	if err != nil {
		return nil, err
	}
	timers, err := encodeDurableTimers(a.pendingTimers)
	if err != nil {
		return nil, err
	}
	version := uint8(1)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &version),
		tlv.MakePrimitiveRecord(3, &rounds),
		tlv.MakePrimitiveRecord(5, &quotes),
		tlv.MakePrimitiveRecord(7, &timers),
	)
}

// decodeActorSnapshot checks ownership before the caller installs any state.
func decodeActorSnapshot(raw []byte,
	base *ClientEnvironment) (*clientActorSnapshot, error) {

	var version uint8
	var rounds, quotes, timers []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &version),
		tlv.MakePrimitiveRecord(3, &rounds),
		tlv.MakePrimitiveRecord(5, &quotes),
		tlv.MakePrimitiveRecord(7, &timers),
	)
	if err != nil {
		return nil, err
	}
	if version != 1 {
		return nil, fmt.Errorf("unknown client checkpoint version")
	}
	restored, err := decodeDurableMap(
		rounds, decodeDurableRoundMapKey,
		func(value []byte) (*restoredClientRound, error) {
			return decodeDurableRound(value, base)
		},
	)
	if err != nil {
		return nil, err
	}
	pending, err := decodeDurableQuoteMap(quotes)
	if err != nil {
		return nil, err
	}
	pendingTimers, err := decodeDurableTimers(timers)
	if err != nil {
		return nil, err
	}
	snapshot := &clientActorSnapshot{
		pendingTimers: pendingTimers,
		rounds:        restored, pendingQuotes: pending,
		commitmentTxIndex: make(map[chainhash.Hash]RoundKeyStr),
	}
	for key, value := range restored {
		if key != RoundKeyStr(value.round.Key.KeyString()) {
			return nil, fmt.Errorf("round checkpoint map key " +
				"differs")
		}
		txid := value.round.TxID
		if txid == (chainhash.Hash{}) {
			continue
		}
		if _, exists := snapshot.commitmentTxIndex[txid]; exists {
			return nil, fmt.Errorf("duplicate commitment ownership")
		}
		snapshot.commitmentTxIndex[txid] = key
	}

	return snapshot, nil
}

// encodeDurableRoundMapKey keeps the actor's existing canonical identity.
func encodeDurableRoundMapKey(key RoundKeyStr) ([]byte, error) {
	return []byte(key), nil
}

// decodeDurableRoundMapKey rejects malformed routing identities.
func decodeDurableRoundMapKey(raw []byte) (RoundKeyStr, error) {
	key, err := decodeDurableRoundKey(raw)
	if err != nil {
		return "", err
	}

	return RoundKeyStr(key.KeyString()), nil
}

// encodeDurableRoundID preserves the complete assigned round identifier.
func encodeDurableRoundID(id RoundID) ([]byte, error) {
	return id[:], nil
}

// decodeDurableQuoteMap checks each quote against its routing key.
func decodeDurableQuoteMap(raw []byte) (map[RoundID]*JoinRoundQuoteReceived,
	error) {

	encoded, err := decodeDurableMap(
		raw, parseDurableRoundID,
		func(value []byte) ([]byte, error) { return value, nil },
	)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxPendingQuotes {
		return nil, fmt.Errorf("too many buffered quotes")
	}
	result := make(map[RoundID]*JoinRoundQuoteReceived, len(encoded))
	for id, value := range encoded {
		quote, err := decodeDurableQuote(id, value)
		if err != nil {
			return nil, err
		}
		if quote == nil {
			return nil, fmt.Errorf("missing buffered quote")
		}
		result[id] = &JoinRoundQuoteReceived{RoundID: id, Quote: quote}
	}

	return result, nil
}
