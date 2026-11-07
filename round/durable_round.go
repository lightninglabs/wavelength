package round

import (
	"context"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/google/uuid"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableRound records ownership together with the full protocol checkpoint.
type durableRound struct {
	Key         []byte
	RoundID     []byte
	TxID        [32]byte
	Commitment  []byte
	State       []byte
	Environment []byte
	Boarding    []byte
	Flags       uint8
}

// records defines the round checkpoint envelope.
func (v *durableRound) records() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(1, &v.Key),
		tlv.MakePrimitiveRecord(3, &v.RoundID),
		tlv.MakePrimitiveRecord(5, &v.TxID),
		tlv.MakePrimitiveRecord(7, &v.Commitment),
		tlv.MakePrimitiveRecord(9, &v.State),
		tlv.MakePrimitiveRecord(11, &v.Environment),
		tlv.MakePrimitiveRecord(13, &v.Boarding),
		tlv.MakePrimitiveRecord(17, &v.Flags),
	}
}

// encodeDurableRound captures actor metadata and FSM state on the same turn.
func encodeDurableRound(ctx context.Context, round *RoundFSM) ([]byte, error) {
	if round == nil || round.Key == nil || round.FSM == nil {
		return nil, fmt.Errorf("incomplete round checkpoint")
	}
	state, err := round.FSM.CurrentStateWithContext(ctx)
	if err != nil {
		return nil, err
	}
	typed, ok := state.(ClientState)
	if !ok {
		return nil, fmt.Errorf("unexpected client state %T", state)
	}
	v := durableRound{
		Key: []byte(round.Key.KeyString()), RoundID: round.RoundID[:],
		TxID: round.TxID,
	}
	v.State, err = encodeClientState(typed)
	if err != nil {
		return nil, err
	}
	v.Boarding, err = encodeDurableList(
		round.BoardingOutpoints, encodeDurablePoint,
	)
	if err != nil {
		return nil, err
	}
	v.Environment, err = encodeDurableEnvironment(round.env)
	if err != nil {
		return nil, err
	}
	v.Commitment, err = encodeDurablePSBT(round.CommitmentTx.UnwrapOr(nil))
	if err != nil {
		return nil, err
	}
	if round.CommitmentTx.IsSome() {
		v.Flags |= 1
	}

	if round.coldSigning {
		v.Flags |= 8
	}

	return encodeDurableFields(v.records()...)
}

// restoredClientRound requires an explicit cold-session decision before
// startup.
type restoredClientRound struct {
	round              *RoundFSM
	state              ClientState
	lostSignerSessions bool
}

// decodeDurableRound restores metadata without starting a signing machine.
func decodeDurableRound(raw []byte,
	base *ClientEnvironment) (*restoredClientRound, error) {

	var v durableRound
	if err := decodeDurableFields(raw, v.records()...); err != nil {
		return nil, err
	}
	if v.Flags & ^uint8(9) != 0 {
		return nil, fmt.Errorf("invalid round flags")
	}
	key, err := decodeDurableRoundKey(v.Key)
	if err != nil {
		return nil, err
	}
	id, err := parseDurableRoundID(v.RoundID)
	if err != nil {
		return nil, err
	}
	if !key.IsTemp() && key.KeyString() != id.KeyString() {
		return nil, fmt.Errorf("round key and ID differ")
	}
	env, err := decodeDurableEnvironment(v.Environment, base)
	if err != nil {
		return nil, err
	}
	state, lost, err := decodeClientState(v.State, env.ChainParams)
	if err != nil {
		return nil, err
	}
	commitment, err := decodeDurablePSBT(v.Commitment)
	if err != nil {
		return nil, err
	}
	boarding, err := decodeDurableList(v.Boarding, parseDurableOutpoint)
	if err != nil {
		return nil, err
	}
	round := &RoundFSM{
		BoardingOutpoints: boarding,
		env:               env, Key: key, RoundID: id,
		TxID: chainhash.Hash(v.TxID),
	}
	if v.Flags&1 != 0 {
		round.CommitmentTx = fn.Some[*psbt.Packet](commitment)
	}

	return &restoredClientRound{
		round: round, state: state,
		lostSignerSessions: lost || v.Flags&8 != 0,
	}, nil
}

// decodeDurableRoundKey retains temporary identities instead of minting new
// IDs.
func decodeDurableRoundKey(raw []byte) (RoundKey, error) {
	value := string(raw)
	temporary := strings.HasPrefix(value, "temp:")
	id, err := uuid.Parse(strings.TrimPrefix(value, "temp:"))
	if err != nil {
		return nil, err
	}
	var key RoundKey = RoundID(id)
	if temporary {
		key = TempRoundKey(id)
	}
	if key.KeyString() != value {
		return nil, fmt.Errorf("noncanonical round key")
	}

	return key, nil
}
