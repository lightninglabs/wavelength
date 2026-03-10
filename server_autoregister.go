package darepo

import (
	"context"
	"fmt"
	"sync"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo/clientconn"
)

// autoRegistrar manages on-demand client registration triggered by
// the first mailbox envelope from an unknown sender. When the mailbox
// edge server receives a Send from a client whose sender ID is not yet
// registered on the bridge, the registrar creates a per-client runtime
// with merged dispatchers from all active subsystems.
type autoRegistrar struct {
	server *Server

	// mu serializes registration attempts to prevent duplicate
	// registrations from concurrent Send calls by the same client.
	mu sync.Mutex
}

// onSend is the SendHook callback. It checks whether the envelope's
// sender is already registered on the bridge; if not, it creates a
// new per-client runtime with merged dispatchers from all active
// subsystems (indexer, rounds, OOR).
func (r *autoRegistrar) onSend(ctx context.Context,
	env *mailboxpb.Envelope) error {

	senderID := env.Sender
	if senderID == "" {
		return nil
	}

	clientID := clientconn.ClientID(senderID)

	// Fast path: already registered (lock-free read).
	if _, ok := r.server.clientBridge.GetClient(clientID); ok {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check under lock to handle concurrent first sends.
	if _, ok := r.server.clientBridge.GetClient(clientID); ok {
		return nil
	}

	// Build an in-process edge client for the new client's
	// runtime. Each runtime gets its own edge instance backed
	// by the shared mailbox store.
	edgeClient, err := newLocalMailboxClient(
		r.server.mailboxStore,
	)
	if err != nil {
		return fmt.Errorf("build edge for %q: %w",
			senderID, err)
	}

	cfg := clientconn.DefaultPerClientConfig()
	cfg.Edge = edgeClient

	// The server pulls from the envelope's recipient mailbox
	// (where the client sends to) and sends responses back to
	// the client's sender mailbox (where the client pulls from).
	cfg.LocalMailboxID = env.Recipient
	cfg.RemoteMailboxID = senderID
	cfg.Store = r.server.deliveryStore
	cfg.ProtocolVersion = 1

	_, err = r.server.RegisterClientWithAllDispatchers(
		ctx, clientID, cfg,
	)
	if err != nil {
		return fmt.Errorf("auto-register client %q: %w",
			senderID, err)
	}

	r.server.log.InfoS(ctx, "Auto-registered external client",
		"client_id", senderID,
		"local_mailbox", env.Recipient,
		"remote_mailbox", senderID)

	return nil
}
