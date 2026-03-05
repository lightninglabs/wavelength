package oor

import (
	"context"

	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo/clientconn"
)

// RecipientResolver maps recipient pkScripts to clientconn ClientIDs. The
// server wiring injects a resolver backed by the indexer's receive-script
// registration store. Tests inject a simple map-based resolver.
type RecipientResolver interface {
	// ResolveRecipients groups the provided recipient outputs by the
	// clientconn ClientID that owns the corresponding receive script.
	// Recipients without a registered receive script are silently
	// dropped (the client has not registered for push notifications).
	ResolveRecipients(ctx context.Context,
		recipients []clientoor.ArkRecipientOutput,
	) (map[clientconn.ClientID][]clientoor.ArkRecipientOutput, error)
}
