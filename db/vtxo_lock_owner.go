package db

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/lightninglabs/darepo/vtxo"
)

const (
	lockOwnerKindRound = "round"
	lockOwnerKindOOR   = "oor"
)

func lockOwnerFromRoundID(roundID []byte) (string, []byte, error) {
	const roundIDLen = 16

	if len(roundID) != roundIDLen {
		return "", nil, fmt.Errorf("round owner id must be %d bytes",
			roundIDLen)
	}

	ownerID := bytes.Clone(roundID)

	return lockOwnerKindRound, ownerID, nil
}

func parseLockOwner(owner vtxo.LockOwner) (string, []byte, error) {
	if owner == "" {
		return "", nil, fmt.Errorf("owner must be provided")
	}

	ownerStr := string(owner)

	switch {
	case strings.HasPrefix(ownerStr, vtxo.LockOwnerRoundPrefix):
		roundIDStr := strings.TrimPrefix(
			ownerStr, vtxo.LockOwnerRoundPrefix,
		)
		if roundIDStr == "" {
			return "", nil, fmt.Errorf("invalid round owner %q",
				ownerStr)
		}

		roundID, err := uuid.Parse(roundIDStr)
		if err != nil {
			return "", nil, fmt.Errorf("invalid round owner %q: %w",
				ownerStr, err)
		}

		ownerID := make([]byte, len(roundID))
		copy(ownerID, roundID[:])

		return lockOwnerKindRound, ownerID, nil

	case strings.HasPrefix(ownerStr, vtxo.LockOwnerOORPrefix):
		sessionID := strings.TrimPrefix(
			ownerStr, vtxo.LockOwnerOORPrefix,
		)
		if sessionID == "" {
			return "", nil, fmt.Errorf("invalid oor owner %q",
				ownerStr)
		}

		return lockOwnerKindOOR, []byte(sessionID), nil

	default:
		return "", nil, fmt.Errorf("unsupported owner format %q",
			ownerStr)
	}
}

func lockOwnerToValue(kind string, ownerID []byte) vtxo.LockOwner {
	if kind == "" || len(ownerID) == 0 {
		return ""
	}

	switch kind {
	case lockOwnerKindRound:
		if len(ownerID) != 16 {
			return vtxo.LockOwner(
				vtxo.LockOwnerRoundPrefix +
					ownerForDisplay(ownerID),
			)
		}

		var roundUUID uuid.UUID
		copy(roundUUID[:], ownerID)

		return vtxo.RoundLockOwner(roundUUID.String())

	case lockOwnerKindOOR:
		return vtxo.OORLockOwner(string(ownerID))

	default:
		return vtxo.LockOwner(
			fmt.Sprintf(
				"%s:%s", kind, ownerForDisplay(ownerID),
			),
		)
	}
}
