//go:build systest

package systest

import (
	"context"
	"sync"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// VTXOObserverMsg is a sealed interface for VTXO observer messages.
// Uses actor.Message as the constraint to receive VTXOCreatedNotification.
type VTXOObserverMsg interface {
	actor.Message
}

// VTXOObserverResp is the response type for VTXO observer messages.
type VTXOObserverResp interface{}

// VTXOObserver is an actor that observes VTXO creation notifications from the
// round client actor. This is used in e2e tests to detect when a round has
// completed and VTXOs have been created.
//
// The observer provides an event-based interface via channels, allowing tests
// to wait for notifications without polling.
type VTXOObserver struct {
	mu sync.Mutex

	// vtxos collects all VTXOs received via VTXOCreatedNotification.
	vtxos []*round.ClientVTXO

	// notificationCount tracks how many VTXOCreatedNotification messages
	// have been received.
	notificationCount int

	// notifyChan is signaled each time a VTXOCreatedNotification is
	// received. Buffered to avoid blocking the actor.
	notifyChan chan *round.VTXOCreatedNotification
}

// NewVTXOObserver creates a new VTXO observer actor.
func NewVTXOObserver() *VTXOObserver {
	return &VTXOObserver{
		vtxos: make([]*round.ClientVTXO, 0),
		// Buffer a few notifications to avoid blocking.
		notifyChan: make(chan *round.VTXOCreatedNotification, 10),
	}
}

// Receive processes incoming messages for the VTXO observer actor.
func (o *VTXOObserver) Receive(ctx context.Context,
	msg VTXOObserverMsg) fn.Result[VTXOObserverResp] {

	switch m := msg.(type) {
	case *round.VTXOCreatedNotification:
		o.mu.Lock()
		o.vtxos = append(o.vtxos, m.VTXOs...)
		o.notificationCount++
		o.mu.Unlock()

		// Signal via channel (non-blocking).
		select {
		case o.notifyChan <- m:
		default:
			// Channel full, notification is still recorded above.
		}

		return fn.Ok[VTXOObserverResp](nil)

	default:
		// Ignore unknown message types.
		return fn.Ok[VTXOObserverResp](nil)
	}
}

// NotifyChan returns a channel that receives VTXOCreatedNotification events.
// Use this to wait for round completion in tests without polling.
func (o *VTXOObserver) NotifyChan() <-chan *round.VTXOCreatedNotification {
	return o.notifyChan
}

// VTXOCount returns the number of VTXOs observed.
func (o *VTXOObserver) VTXOCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return len(o.vtxos)
}

// NotificationCount returns the number of VTXOCreatedNotification messages
// received.
func (o *VTXOObserver) NotificationCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.notificationCount
}

// GetVTXOs returns a copy of all observed VTXOs.
func (o *VTXOObserver) GetVTXOs() []*round.ClientVTXO {
	o.mu.Lock()
	defer o.mu.Unlock()

	result := make([]*round.ClientVTXO, len(o.vtxos))
	copy(result, o.vtxos)

	return result
}

// HasReceivedNotification returns true if at least one VTXOCreatedNotification
// has been received.
func (o *VTXOObserver) HasReceivedNotification() bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.notificationCount > 0
}
