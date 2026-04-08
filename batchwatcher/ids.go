package batchwatcher

import (
	"fmt"

	"github.com/google/uuid"
)

// BatchIDForRoundOutput derives the deterministic batch ID used for a
// confirmed round output registered with the BatchWatcher.
func BatchIDForRoundOutput(roundID uuid.UUID, outputIdx int) BatchID {
	batchIDName := fmt.Sprintf("%s-%d", roundID, outputIdx)

	return BatchID(uuid.NewSHA1(roundID, []byte(batchIDName)))
}
