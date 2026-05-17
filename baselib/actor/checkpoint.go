package actor

import "time"

// CheckpointParams contains parameters for saving an FSM checkpoint.
type CheckpointParams struct {
	// ActorID identifies the actor whose FSM state is checkpointed.
	ActorID string

	// StateType is the name of the current FSM state.
	StateType string

	// StateData contains the encoded state snapshot.
	StateData []byte

	// Version is the checkpoint version.
	Version int64
}

// Checkpoint represents a persisted FSM state checkpoint.
type Checkpoint struct {
	// ActorID identifies the actor.
	ActorID string

	// StateType is the name of the current FSM state.
	StateType string

	// StateData contains the encoded state snapshot.
	StateData []byte

	// Version is the checkpoint version.
	Version int64

	// UpdatedAt is when the checkpoint was last updated.
	UpdatedAt time.Time
}
