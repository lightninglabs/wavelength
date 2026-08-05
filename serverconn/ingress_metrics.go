package serverconn

import (
	"github.com/lightninglabs/wavelength/metrics"
)

// markIngressPoll stamps the ingress liveness gauge. It is called after every
// Pull that returns, including the empty long-poll, because the question the
// gauge answers is whether the single goroutine that consumes the server
// mailbox is still going round its loop — not whether there happened to be
// traffic.
//
// Nothing else in the process can answer that question. The connector's only
// pre-existing timestamp is outbound-only, GetInfo's server_connected is set
// once at startup and never cleared on a stall, and the ingress loop reacts to
// errors but not to a wedge, which produces none.
func markIngressPoll() {
	metrics.ServerConnLastIngressPollTimestamp.SetToCurrentTime()
}

// markIngressEvent stamps the inbound-traffic gauge after a pulled batch has
// been delivered and its cursor committed. Read against the poll gauge it
// separates a client with nothing to do from one whose dispatch is not getting
// through.
//
// A deferral that committed a delivered prefix stamps it too. Partial progress
// IS events reaching their actors, and reporting a draining backlog as
// "dispatch is not getting through" would dilute the one reading that means it.
func markIngressEvent() {
	metrics.ServerConnLastIngressEventTimestamp.SetToCurrentTime()
}
