package oor

import (
	"context"
)

// OutboxHandlerFunc adapts a function to the OutboxHandler interface.
type OutboxHandlerFunc func(ctx context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error)

// Handle implements OutboxHandler by calling the underlying function.
func (f OutboxHandlerFunc) Handle(ctx context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	return f(ctx, sessionID, outbox)
}

var _ OutboxHandler = (OutboxHandlerFunc)(nil)
