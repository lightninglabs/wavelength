package serverconn

import (
	"context"
	"encoding/binary"
	"time"

	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
)

const (
	// HeartbeatService is the well-known protobuf service name used
	// for heartbeat envelopes. The server's ingress dispatcher
	// recognises this service to update the client's liveness
	// status.
	HeartbeatService = "clientconn.v1.HeartbeatService"

	// HeartbeatMethod is the RPC method name for heartbeat
	// envelopes.
	HeartbeatMethod = "Heartbeat"

	// DefaultHeartbeatInterval is the default interval between
	// heartbeat sends. The server's default staleness threshold
	// (60 s) is 2× this interval, giving one missed heartbeat as
	// grace.
	DefaultHeartbeatInterval = 30 * time.Second
)

// startHeartbeat launches a background goroutine that periodically
// sends a lightweight heartbeat envelope to the server's mailbox. The
// heartbeat proves to the server that this client is alive even when
// it has no real traffic to send.
//
// The heartbeat is idle-aware: if real outbound traffic (event or RPC)
// was sent within the current interval, the heartbeat is skipped since
// the server's ingress loop will already observe activity.
//
// The goroutine exits when ctx is cancelled. Errors on individual
// heartbeat sends are silently ignored — the server will transition
// the client to offline after its staleness threshold if heartbeats
// stop arriving.
func (a *ServerConnectionActor) startHeartbeat(ctx context.Context) {
	interval := a.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			// Skip the heartbeat if real traffic was sent
			// recently enough that the server will still
			// consider us active.
			lastSend := a.lastSendNano.Load()
			if lastSend > 0 {
				elapsed := time.Since(
					time.Unix(0, lastSend),
				)
				if elapsed < interval {
					continue
				}
			}

			a.sendHeartbeat(ctx)
		}
	}
}

// sendHeartbeat sends a single heartbeat envelope to the server. The
// envelope has no body — the service/method metadata is sufficient for
// the server to recognise it as a liveness signal.
func (a *ServerConnectionActor) sendHeartbeat(ctx context.Context) {
	// Skip heartbeats once the connector is incompatible: the transition
	// already cancelled this goroutine's context, but guard defensively so
	// we never contact the edge after the terminal state.
	if a.compatibilityError() != nil {
		return
	}

	now := time.Now()

	// Each heartbeat needs a unique MsgId to avoid deduplication
	// by the mailbox transport. We append the current timestamp
	// to the seed before hashing.
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(now.UnixNano()))
	msgID := mailboxconn.StableEventMsgID(
		append(
			[]byte("heartbeat"), buf[:]...,
		),
	)

	envelope := &mailboxpb.Envelope{
		MsgId:           msgID,
		Sender:          a.cfg.LocalMailboxID,
		Recipient:       a.cfg.RemoteMailboxID,
		CreatedAtUnixMs: now.UnixMilli(),
		Headers:         a.cfg.mergeAuthHeaders(nil),
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: HeartbeatService,
			Method:  HeartbeatMethod,
			ReplyTo: a.cfg.LocalMailboxID,
		},
	}

	// Best-effort: log but don't fail. The server will mark us
	// offline after the staleness threshold if heartbeats stop
	// arriving.
	resp, err := a.cfg.Edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: envelope,
	})

	// Best-effort: log on any failure. A permanent version status on a
	// heartbeat is terminal, just like on any other send path, so drive the
	// incompatibility transition (a no-op for a plain transport error).
	if sErr := edgeResponseError("heartbeat", resp, err); sErr != nil {
		a.log.WarnS(ctx, "Heartbeat send failed", sErr)
		a.checkPermanentStatus(ctx, sErr)
	}
}
