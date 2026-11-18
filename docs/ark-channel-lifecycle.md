# Ark channels: open first, pay on an active channel

Opening a channel and receiving a payment are separate operations. The client
can promote wallet VTXOs through the prepared out-of-round (OOR) transfer.
The funding wallet must not release that transfer until both lnd endpoints
have enforceable commitments and the signed VTXO-to-channel backing. Neither
the funding script nor its delays change.

Only the client opens the channel. It uses the authenticated hub mailbox to
register the promotion, coordinate signatures, and install the finalized
source recovery package. The separate Lightning peer transport carries lnd's
opening messages, commitment updates, and reestablishment in both directions.
After installation, either endpoint can materialize and force close using its
own durable state without a request to the other.

An invoice can use an existing channel with inbound capacity. Selecting that
channel does not reserve its balance until the payer arrives. If capacity
changes, the payment can fail; it cannot fund a new channel or switch to a
vHTLC under the same invoice. A wallet without a suitable channel can receive
through the ordinary vHTLC flow and later promote its wallet funds.

## Cooperative settlement

1. The client asks for the hub's payout destination and saves both destinations.
   This first query does not stop either link.
2. The client quiesces its lnd link and proposes the payout from its clean
   commitment. The hub checks its own clean lnd balances, commitment height,
   source, and destination. It saves the exact signed authorization before
   returning it. A lost signing reply is not permission to reopen the link.
3. The client saves the authorization and returns a digest of the two hub
   signatures. The unsigned transaction ID would not prove receipt. Only
   then may the client submit the 3-of-3 spend through the usual OOR flow.
4. The client records the completed OOR and reports its ID. A hub with a
   payout checks the wallet's completed OOR session for its exact output,
   not today's list of spendable VTXOs. A hub without a payout needs the
   authenticated client report. Each endpoint archives its own lnd record.
   No subsequent acknowledgment of local archival is needed.

This close unrolls nothing. The wallets retain their replacement VTXOs and
ancestry watches. On a force close, Ark instead materializes the pre-agreed
funding outpoint. lnd's chain arbitrator and sweeper resolve the channel.

## Two boundaries to keep

**Transport.** Ark commands still need authenticated, durable delivery. A
retry of an exact close proposal, signature receipt, or OOR result goes to
the channel FSM, not a second execution journal. An unsigned abort is
different: a delayed duplicate must not abort a later attempt.

A live connection for Lightning frames is a separate transport change.
The current sender promises durable admission; a WebSocket write cannot
satisfy that promise. A replacement could carry ordered binary frames over
an outbound authenticated connection, bind it to one peer, fence its old
connection on resume, and start with lnd's channel reestablishment. Only
then could it replace the per-frame mailbox and ingress receipts. It would
not replace durable Ark commands, lnd's circuit database, or chain watches
while a device sleeps.

**Forwarding.** SwapDK's public lnd is reached through RPC; its private Ark
links share a different in-process lnd switch. The payment bridge therefore
still owns the relationship between two HTLCs, including their hash, nested
deadlines, and settlement after restart. To remove the bridge, the operator
would need to host its public and Ark links in the same switch and circuit
database, with consistent peer identities, channel aliases, fees, and
onion forwarding. That experiment belongs on the operator. It needs no
second switch, public graph, or pathfinder in the client.
