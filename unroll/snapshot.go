package unroll

import (
	"encoding/json"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/unrollplan"
)

const (
	checkpointStateType = "unroll.vtxo"
	checkpointVersion   = 1
)

// actorCheckpoint is the durable checkpoint shape for one VTXO unroll actor.
type actorCheckpoint struct {
	Version       uint8
	Height        int32
	Started       bool
	Trigger       StartTrigger
	State         unrollplan.State
	SweepTx       *wire.MsgTx
	Fail          string
	SweepAttempts int
}

// encodeCheckpoint serializes one actor checkpoint.
func encodeCheckpoint(value *actorCheckpoint) ([]byte, error) {
	return json.Marshal(value)
}

// decodeCheckpoint deserializes one actor checkpoint.
func decodeCheckpoint(raw []byte) (*actorCheckpoint, error) {
	var checkpoint actorCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return nil, err
	}

	return &checkpoint, nil
}
