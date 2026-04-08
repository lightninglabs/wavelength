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
	Version       uint8            `json:"version"`
	Height        int32            `json:"height"`
	Started       bool             `json:"started"`
	Trigger       StartTrigger     `json:"trigger"`
	State         unrollplan.State `json:"state"`
	SweepTx       *wire.MsgTx      `json:"sweep_tx"`
	Fail          string           `json:"fail"`
	SweepAttempts int              `json:"sweep_attempts"`
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
