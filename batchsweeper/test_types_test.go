package batchsweeper

import (
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo/batchwatcher"
)

// batchWatcherFuture is a type alias used to shorten generic return types in
// tests to satisfy the 80 column limit.
type batchWatcherFuture = actor.Future[batchwatcher.BatchWatcherResp]

// chainSourceFuture is a type alias used to shorten generic return types in
// tests to satisfy the 80 column limit.
type chainSourceFuture = actor.Future[chainsource.ChainSourceResp]
