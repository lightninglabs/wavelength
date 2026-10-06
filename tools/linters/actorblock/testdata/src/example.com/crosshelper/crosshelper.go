package crosshelper

import (
	"context"

	"example.com/actor"
)

// Wait awaits a future.
func Wait(ctx context.Context, f actor.Future[int]) int {
	return f.Await(ctx).Val
}

// Pure never blocks.
func Pure() int { return 1 }
