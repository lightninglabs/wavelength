package oor

import (
	"context"

	clientoor "github.com/lightninglabs/darepo-client/oor"
)

// RecipientNotifier receives finalized recipient outputs as best-effort
// notifications after durable recipient-event append.
//
// Implementations should not block OOR finalization. Any transport-specific
// failures should be handled internally (for example, via logging/retry).
type RecipientNotifier interface {
	// NotifyRecipientEvent receives a finalized recipient output for the
	// given OOR session.
	NotifyRecipientEvent(ctx context.Context, sessionID SessionID,
		recipient clientoor.ArkRecipientOutput)
}
