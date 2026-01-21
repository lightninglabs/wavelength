**Title:** Implement Client-Side Unrolling Actor

**Assignee:** @sputn1ck

**Labels:** enhancement, priority: high

---

## Description

Implement an unrolling actor (chain resolver) on the client side that handles the on-chain unrolling of VTXO trees when a client needs to force close or go on-chain. This actor is essential for end-to-end testing the fraud detection system and completes the client's ability to respond to timelock expiration.

## Context

The server-side fraud detection architecture is in place (batch watcher actor, fraud detector actor), but it cannot be fully end-to-end tested without the client having the ability to actually perform unrolling.

The client currently has:
- ✅ VTXO FSM with comprehensive state management (merged in PR #69: `vtxo/fsm_environment.go`, `vtxo/transitions.go`)
- ✅ Expiry watcher that counts down the current expiry (`vtxo/expiry.go:83-134`)
  - Similar to LND's approach where it tracks blocks before needing to go on-chain
  - Implements `ExpiryStatusCritical` threshold accounting for tree depth
- ✅ `ExpiringNotification` message (`vtxo/outbox_messages.go:65-78`)
  - Sent when VTXO reaches critical expiry threshold
  - Includes VTXO descriptor, blocks remaining, and reason

**What's missing:**
- ❌ Chain resolver actor to receive `ExpiringNotification` messages
- ❌ Transaction broadcast logic for unrolling
- ❌ Level-by-level tree unroll coordinator accounting for block timing

## Implementation Details

1. **Build off the tip of the client VTXO branch**
   - Foundation already in place from PR #69

2. **Create new chain resolver actor**
   - Follows existing actor pattern (see `round/actor.go` for reference)
   - Receives `ExpiringNotification` from VTXO FSM (`vtxo/outbox_messages.go:65`)

3. **Integrate with existing expiry watcher**
   - Watcher determines when on-chain action required (`vtxo/expiry.go:92-134`)
   - Each branch takes ~3 blocks to confirm
   - Critical threshold accounts for tree depth and CSV delays

4. **Implement unrolling logic**
   - Broadcast transactions for each tree level
   - Use existing tree structures from `lib/tree/tree.go`, `lib/tree/node.go`
   - Account for block timing between levels
   - Handle confirmation monitoring via chainsource actor

5. **Support both scenarios:**
   - Legitimate timelock expiry (cooperative timeout)
   - Fraudulent/"forfeited" VTXO scenarios (for testing)

## Acceptance Criteria

- [ ] Client detects when on-chain action required based on timelock expiry
- [ ] Client broadcasts transactions to go on-chain
- [ ] Client unrolls the VTXO tree level by level with proper block timing
- [ ] Unrolling works for both legitimate (timelock expiry) and "fraudulent" (forfeited VTXO) scenarios
- [ ] Integration tests demonstrating end-to-end unrolling flow
- [ ] Enables end-to-end fraud detection testing

## Dependencies

**Blocked By:**
- Client VTXO PRs (provides the expiry watcher foundation) - ✅ **COMPLETED** (PR #69 merged)

**Unblocks:**
- End-to-end fraud detection testing
- Ability to write elaborate test scenarios (e.g., tricking a client)
- Server-side fraud detector validation (checking if VTXO is out-of-round spent, forfeited, or legitimately on-chain)

## Relationship to Fraud Detection

The unrolling actor ties directly into fraud detection because unrolling can be fraudulent or not. The server-side fraud detector needs to determine:

1. Is this VTXO out-of-round spent?
2. Is this VTXO forfeited?
3. Is this VTXO legitimately allowed to be on-chain?

Without client-side unrolling, these scenarios cannot be tested end-to-end.

## Technical References

- Expiry monitoring: `vtxo/expiry.go:83-134`
- Message flow diagram: `vtxo/interfaces.go:28-40`
- Tree structures: `lib/tree/tree.go`, `lib/tree/node.go`, `lib/tree/batch.go`
- Actor pattern: `round/actor.go`
- Related commits: `2b7d644` (PR #69 - VTXO FSM), `d9dbe7a` (expiry monitoring)
