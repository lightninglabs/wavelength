package mailboxrpc

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"
)

// ErrNoHandler is returned when a (service, method) pair has no registered
// handler.
var ErrNoHandler = errors.New("no handler registered")

// ServeMux is an in-process router that maps (service, method) pairs to typed
// handlers.
//
// ServeMux is intended as a small, dependency-free building block for mailbox
// RPC servers. It does not implement transport concerns such as authentication,
// retries, persistence, or acking.
type ServeMux struct {
	mu       sync.RWMutex
	handlers map[routeKey]handlerEntry
}

type routeKey struct {
	service string
	method  string
}

type handlerEntry struct {
	newReq func() proto.Message
	fn     HandlerFunc
}

// NewServeMux creates an empty mux.
func NewServeMux() *ServeMux {
	return &ServeMux{
		handlers: make(map[routeKey]handlerEntry),
	}
}

// Handle registers a typed handler for a single RPC method.
func (m *ServeMux) Handle(service string, method string,
	newReq func() proto.Message, fn HandlerFunc) {

	if service == "" {
		panic("mailboxrpc: empty service name")
	}
	if method == "" {
		panic("mailboxrpc: empty method name")
	}
	if newReq == nil {
		panic("mailboxrpc: nil request constructor")
	}
	if fn == nil {
		panic("mailboxrpc: nil handler function")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.handlers[routeKey{
		service: service,
		method:  method,
	}] = handlerEntry{
		newReq: newReq,
		fn:     fn,
	}
}

// ServeRPC unmarshals reqBytes into the registered request type for
// (service, method) and invokes the handler.
func (m *ServeMux) ServeRPC(ctx context.Context, service string,
	method string, reqBytes []byte) (proto.Message, error) {

	entry, ok := m.lookup(service, method)
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrNoHandler,
			service, method)
	}

	req := entry.newReq()
	if req == nil {
		return nil, fmt.Errorf("nil request prototype for %s/%s",
			service, method)
	}

	if err := (proto.UnmarshalOptions{
		DiscardUnknown: true,
	}).Unmarshal(reqBytes, req); err != nil {
		return nil, err
	}

	return entry.fn(ctx, req)
}

// lookup returns the handler entry for (service, method) if present.
func (m *ServeMux) lookup(service string, method string) (handlerEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.handlers[routeKey{
		service: service,
		method:  method,
	}]

	return entry, ok
}
