package batchsweeper

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/batchwatcher"
)

// MapBatchWatcherNotification creates a TellOnlyRef that adapts the
// batchwatcher.BatchSweeperMsg notification surface into this package's sealed
// Msg interface. This allows BatchWatcher to be wired directly to a
// BatchSweeperActor without changing the BatchWatcher API.
func MapBatchWatcherNotification(
	self actor.TellOnlyRef[Msg],
) actor.TellOnlyRef[batchwatcher.BatchSweeperMsg] {

	return &batchWatcherNotificationRef{
		self: self,
	}
}

// batchWatcherNotificationRef forwards BatchWatcher notifications to a
// BatchSweeperActor by wrapping them as internal messages.
type batchWatcherNotificationRef struct {
	self actor.TellOnlyRef[Msg]
}

// ID returns the unique identifier for this adapter reference.
func (r *batchWatcherNotificationRef) ID() string {
	if r.self == nil {
		return "batchsweeper-nil-self"
	}

	return fmt.Sprintf("batchwatcher-adapter-%s", r.self.ID())
}

// Tell forwards a BatchWatcher notification to the BatchSweeperActor.
func (r *batchWatcherNotificationRef) Tell(ctx context.Context,
	msg batchwatcher.BatchSweeperMsg) {

	if r.self == nil {
		return
	}

	switch m := msg.(type) {
	case *batchwatcher.BatchExpiredNotification:
		r.self.Tell(ctx, &BatchExpiredEvent{
			Notification: m,
		})

	case *batchwatcher.TreeStateChangedNotification:
		r.self.Tell(ctx, &TreeStateChangedEvent{
			Notification: m,
		})
	}
}
