// mailbox_fifo_test.p - Durable mailbox specification tests.

machine TestMailboxFIFO_LegacyAvailableAtCanReorder {
    start state Init {
        entry {
            var rows: seq[MailboxRow];
            var claim_id: int;

            rows += (sizeof(rows), NewMailboxRow(
                1, 10, 100, 0, 5, NoLease(), NoLeaseToken(), 1, 3, 0
            ));
            rows += (sizeof(rows), NewMailboxRow(
                2, 10, 100, 0, 1, NoLease(), NoLeaseToken(), 0, 3, 1
            ));

            claim_id = ClaimNextMailboxRow(
                rows, 10, 1, LegacyAvailableAtOrder
            );

            assert claim_id == 2,
                "legacy available_at ordering permits same-key overtake";

            goto Done;
        }
    }

    state Done {}
}

machine TestMailboxFIFO_BlocksSameKeyBackoffOvertake {
    start state Init {
        entry {
            var rows: seq[MailboxRow];
            var claim_id: int;

            rows += (sizeof(rows), NewMailboxRow(
                1, 10, 100, 0, 5, NoLease(), NoLeaseToken(), 1, 3, 0
            ));
            rows += (sizeof(rows), NewMailboxRow(
                2, 10, 100, 0, 1, NoLease(), NoLeaseToken(), 0, 3, 1
            ));

            claim_id = ClaimNextMailboxRow(
                rows, 10, 1, PerCorrelationKeyFIFO
            );
            assert claim_id == NoMailboxRow(),
                "same-key successor must wait behind backoff predecessor";

            claim_id = ClaimNextMailboxRow(
                rows, 10, 5, PerCorrelationKeyFIFO
            );
            assert claim_id == 1,
                "backoff predecessor must claim before same-key successor";

            rows = RemoveMailboxRow(rows, 1);
            claim_id = ClaimNextMailboxRow(
                rows, 10, 5, PerCorrelationKeyFIFO
            );
            assert claim_id == 2,
                "successor becomes claimable after predecessor ack/removal";

            goto Done;
        }
    }

    state Done {}
}

machine TestMailboxFIFO_BlocksSameKeyActiveLeaseOvertake {
    start state Init {
        entry {
            var rows: seq[MailboxRow];
            var claim_id: int;

            rows += (sizeof(rows), NewMailboxRow(
                1, 10, 100, 0, 0, 10, 55, 1, 3, 0
            ));
            rows += (sizeof(rows), NewMailboxRow(
                2, 10, 100, 0, 1, NoLease(), NoLeaseToken(), 0, 3, 1
            ));

            claim_id = ClaimNextMailboxRow(
                rows, 10, 1, PerCorrelationKeyFIFO
            );

            assert claim_id == NoMailboxRow(),
                "same-key successor must wait while predecessor is leased";

            goto Done;
        }
    }

    state Done {}
}

machine TestMailboxFIFO_CrossKeyIndependence {
    start state Init {
        entry {
            var rows: seq[MailboxRow];
            var claim_id: int;

            rows += (sizeof(rows), NewMailboxRow(
                1, 10, 100, 0, 10, NoLease(), NoLeaseToken(), 1, 3, 0
            ));
            rows += (sizeof(rows), NewMailboxRow(
                2, 10, 200, 0, 1, NoLease(), NoLeaseToken(), 0, 3, 1
            ));

            claim_id = ClaimNextMailboxRow(
                rows, 10, 1, PerCorrelationKeyFIFO
            );

            assert claim_id == 2,
                "different keys must not block each other";

            goto Done;
        }
    }

    state Done {}
}

machine TestMailboxFIFO_UnkeyedLaneUnaffected {
    start state Init {
        entry {
            var rows: seq[MailboxRow];
            var claim_id: int;

            rows += (sizeof(rows), NewMailboxRow(
                1, 10, 100, 0, 10, NoLease(), NoLeaseToken(), 1, 3, 0
            ));
            rows += (sizeof(rows), NewMailboxRow(
                2, 10, NullCorrelationKey(), 0, 1, NoLease(),
                NoLeaseToken(), 0, 3, 1
            ));

            claim_id = ClaimNextMailboxRow(
                rows, 10, 1, PerCorrelationKeyFIFO
            );

            assert claim_id == 2,
                "unkeyed rows must remain outside keyed head-of-line lanes";

            goto Done;
        }
    }

    state Done {}
}

machine TestMailboxFIFO_ExhaustedPredecessorDoesNotBlock {
    start state Init {
        entry {
            var rows: seq[MailboxRow];
            var claim_id: int;

            rows += (sizeof(rows), NewMailboxRow(
                1, 10, 100, 0, 0, NoLease(), NoLeaseToken(), 3, 3, 0
            ));
            rows += (sizeof(rows), NewMailboxRow(
                2, 10, 100, 0, 1, NoLease(), NoLeaseToken(), 0, 3, 1
            ));

            claim_id = ClaimNextMailboxRow(
                rows, 10, 1, PerCorrelationKeyFIFO
            );

            assert claim_id == 2,
                "exhausted same-key predecessor must not block successor";

            goto Done;
        }
    }

    state Done {}
}

machine TestMailboxFIFO_MailboxIsolation {
    start state Init {
        entry {
            var rows: seq[MailboxRow];
            var claim_id: int;

            rows += (sizeof(rows), NewMailboxRow(
                1, 10, 100, 0, 10, NoLease(), NoLeaseToken(), 1, 3, 0
            ));
            rows += (sizeof(rows), NewMailboxRow(
                2, 20, 100, 0, 1, NoLease(), NoLeaseToken(), 0, 3, 1
            ));

            claim_id = ClaimNextMailboxRow(
                rows, 20, 1, PerCorrelationKeyFIFO
            );

            assert claim_id == 2,
                "same key in another mailbox must not block this mailbox";

            goto Done;
        }
    }

    state Done {}
}

machine TestDurableMailboxSpec_TokenOwnershipAndLeaseExpiry {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);
            var lease_resp: (int, DurableMailboxOpResult);

            mailbox = new DurableMailboxSpec(PerCorrelationKeyFIFO);
            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    10, 10, 100, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 0
                )
            );
            receive {
                case eDurableMailboxOpResp:
                    (resp0: (int, DurableMailboxOpResult)) {
                        op_resp = resp0;
                    }
            }
            assert op_resp.1 == MailboxOpOk,
                "fresh enqueue must succeed";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 101,
                now = 0, lease_duration = 5
            );
            receive {
                case eDurableMailboxLeaseResp:
                    (resp1: (int, DurableMailboxOpResult)) {
                        lease_resp = resp1;
                    }
            }
            assert lease_resp.0 == 10 && lease_resp.1 == MailboxOpOk,
                "lease grants ownership of the row";

            send mailbox, eDurableMailboxAck, (
                reply_to = this, id = 10, lease_token = 202
            );
            receive {
                case eDurableMailboxOpResp:
                    (resp2: (int, DurableMailboxOpResult)) {
                        op_resp = resp2;
                    }
            }
            assert op_resp.1 == MailboxOpTokenMismatch,
                "stale or wrong owner cannot ack";

            send mailbox, eDurableMailboxExpireLeasesAt, 6;
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 303,
                now = 6, lease_duration = 5
            );
            receive {
                case eDurableMailboxLeaseResp:
                    (resp3: (int, DurableMailboxOpResult)) {
                        lease_resp = resp3;
                    }
            }
            assert lease_resp.0 == 10 && lease_resp.1 == MailboxOpOk,
                "expired lease must redeliver the same durable row";

            send mailbox, eDurableMailboxAck, (
                reply_to = this, id = 10, lease_token = 303
            );
            receive {
                case eDurableMailboxOpResp:
                    (resp4: (int, DurableMailboxOpResult)) {
                        op_resp = resp4;
                    }
            }
            assert op_resp.1 == MailboxOpOk,
                "current owner can ack and remove the row";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 404,
                now = 6, lease_duration = 5
            );
            receive {
                case eDurableMailboxLeaseResp:
                    (resp5: (int, DurableMailboxOpResult)) {
                        lease_resp = resp5;
                    }
            }
            assert lease_resp.1 == MailboxOpNotFound,
                "acked rows are terminal and cannot be redelivered";

            goto Done;
        }
    }

    state Done {}
}

machine TestDurableMailboxSpec_NackRetryBlocksSameKey {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);
            var lease_resp: (int, DurableMailboxOpResult);

            mailbox = new DurableMailboxSpec(PerCorrelationKeyFIFO);
            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    1, 10, 100, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 0
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp0: (int, DurableMailboxOpResult)) { op_resp = resp0; }
            }
            assert op_resp.1 == MailboxOpOk, "first enqueue succeeds";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 11,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp1: (int, DurableMailboxOpResult)) {
                    lease_resp = resp1;
                }
            }
            assert lease_resp.0 == 1, "first row claims";

            send mailbox, eDurableMailboxNack, (
                reply_to = this, id = 1, lease_token = 11,
                available_at = 5
            );
            receive { case eDurableMailboxOpResp:
                (resp2: (int, DurableMailboxOpResult)) { op_resp = resp2; }
            }
            assert op_resp.1 == MailboxOpOk, "nack schedules retry";

            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    2, 10, 100, 0, 1, NoLease(), NoLeaseToken(),
                    0, 3, 1
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp3: (int, DurableMailboxOpResult)) { op_resp = resp3; }
            }
            assert op_resp.1 == MailboxOpOk, "successor enqueue succeeds";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 22,
                now = 1, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp4: (int, DurableMailboxOpResult)) {
                    lease_resp = resp4;
                }
            }
            assert lease_resp.1 == MailboxOpNotFound,
                "same-key successor cannot overtake nack/backoff row";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 33,
                now = 5, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp5: (int, DurableMailboxOpResult)) {
                    lease_resp = resp5;
                }
            }
            assert lease_resp.0 == 1, "predecessor claims first on retry";

            send mailbox, eDurableMailboxAck, (
                reply_to = this, id = 1, lease_token = 33
            );
            receive { case eDurableMailboxOpResp:
                (resp6: (int, DurableMailboxOpResult)) { op_resp = resp6; }
            }
            assert op_resp.1 == MailboxOpOk, "predecessor ack succeeds";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 44,
                now = 5, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp7: (int, DurableMailboxOpResult)) {
                    lease_resp = resp7;
                }
            }
            assert lease_resp.0 == 2,
                "successor claims once same-key predecessor is gone";

            goto Done;
        }
    }

    state Done {}
}

machine TestDurableMailboxSpec_DuplicateEnqueuePreservesFirstRow {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);
            var lease_resp: (int, DurableMailboxOpResult);

            mailbox = new DurableMailboxSpec(PerCorrelationKeyFIFO);
            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    50, 10, 100, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 0
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp0: (int, DurableMailboxOpResult)) { op_resp = resp0; }
            }
            assert op_resp.1 == MailboxOpOk, "first enqueue succeeds";

            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    50, 20, 200, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 0
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp1: (int, DurableMailboxOpResult)) { op_resp = resp1; }
            }
            assert op_resp.1 == MailboxOpDuplicate,
                "duplicate enqueue is a no-op by durable id";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 20, lease_token = 55,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp2: (int, DurableMailboxOpResult)) {
                    lease_resp = resp2;
                }
            }
            assert lease_resp.1 == MailboxOpNotFound,
                "duplicate row must not rewrite original mailbox scope";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 56,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp3: (int, DurableMailboxOpResult)) {
                    lease_resp = resp3;
                }
            }
            assert lease_resp.0 == 50 && lease_resp.1 == MailboxOpOk,
                "original row remains claimable";

            goto Done;
        }
    }

    state Done {}
}

machine TestDurableMailboxSpec_PriorityDoesNotPierceSameKeyFIFO {
    start state Init {
        entry {
            var rows: seq[MailboxRow];
            var claim_id: int;

            rows += (sizeof(rows), NewMailboxRow(
                1, 10, 100, 0, 10, NoLease(), NoLeaseToken(), 1, 3, 0
            ));
            rows += (sizeof(rows), NewMailboxRow(
                2, 10, 100, 99, 1, NoLease(), NoLeaseToken(), 0, 3, 1
            ));
            rows += (sizeof(rows), NewMailboxRow(
                3, 10, 200, 50, 1, NoLease(), NoLeaseToken(), 0, 3, 2
            ));

            claim_id = ClaimNextMailboxRow(
                rows, 10, 1, PerCorrelationKeyFIFO
            );

            assert claim_id == 3,
                "priority can choose among eligible heads, not overtake "+
                "a live same-key predecessor";

            goto Done;
        }
    }

    state Done {}
}

// TestMailboxFIFO_LegacyCounterexampleToIdealFIFO is intentionally excluded
// from the default green test case. Running tcMailboxLegacyReorderCounterexample
// should find the original bug: legacy available_at ordering returns msg-2
// while msg-1 is still a live same-key predecessor in backoff.
machine TestMailboxFIFO_LegacyCounterexampleToIdealFIFO {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);
            var lease_resp: (int, DurableMailboxOpResult);

            mailbox = new DurableMailboxSpec(LegacyAvailableAtOrder);
            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    1, 10, 100, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 0
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp0: (int, DurableMailboxOpResult)) { op_resp = resp0; }
            }
            assert op_resp.1 == MailboxOpOk, "first enqueue succeeds";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 11,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp1: (int, DurableMailboxOpResult)) {
                    lease_resp = resp1;
                }
            }
            assert lease_resp.0 == 1, "first row claims";

            send mailbox, eDurableMailboxNack, (
                reply_to = this, id = 1, lease_token = 11,
                available_at = 5
            );
            receive { case eDurableMailboxOpResp:
                (resp2: (int, DurableMailboxOpResult)) { op_resp = resp2; }
            }
            assert op_resp.1 == MailboxOpOk, "nack schedules retry";

            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    2, 10, 100, 0, 1, NoLease(), NoLeaseToken(),
                    0, 3, 1
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp3: (int, DurableMailboxOpResult)) { op_resp = resp3; }
            }
            assert op_resp.1 == MailboxOpOk, "successor enqueue succeeds";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 22,
                now = 1, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp4: (int, DurableMailboxOpResult)) {
                    lease_resp = resp4;
                }
            }

            assert lease_resp.1 == MailboxOpNotFound,
                "ideal FIFO says successor must not overtake live "+
                "same-key predecessor";

            goto Done;
        }
    }

    state Done {}
}

// TestDurableMailboxSpec_DeadLetterByIdRemovesExhaustedRow exercises the
// dead-letter path for a retry-exhausted row whose lease token has already been
// cleared by a nack. The old token-gated model could never reach this: nack
// clears the token, so a TokenMatches gate would reject the very rows
// dead-lettering exists to remove. Dead-letter is by id, matching production
// MoveToDeadLetter(ctx, id, reason).
machine TestDurableMailboxSpec_DeadLetterByIdRemovesExhaustedRow {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);
            var lease_resp: (int, DurableMailboxOpResult);

            mailbox = new DurableMailboxSpec(PerCorrelationKeyFIFO);

            // Row 1: max_attempts = 1, so a single lease exhausts it.
            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    1, 10, 100, 0, 0, NoLease(), NoLeaseToken(),
                    0, 1, 0
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp0: (int, DurableMailboxOpResult)) { op_resp = resp0; }
            }
            assert op_resp.1 == MailboxOpOk, "first enqueue succeeds";

            // Row 2: same-key successor with a normal retry budget.
            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    2, 10, 100, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 1
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp1: (int, DurableMailboxOpResult)) { op_resp = resp1; }
            }
            assert op_resp.1 == MailboxOpOk, "successor enqueue succeeds";

            // Leasing claims row 1 (row 2 is blocked behind the live head) and
            // pushes attempts to max_attempts, exhausting it.
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 11,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp2: (int, DurableMailboxOpResult)) {
                    lease_resp = resp2;
                }
            }
            assert lease_resp.0 == 1,
                "head-of-line row claims before its successor";

            // Nack clears the lease token; the row stays present but exhausted.
            send mailbox, eDurableMailboxNack, (
                reply_to = this, id = 1, lease_token = 11,
                available_at = 0
            );
            receive { case eDurableMailboxOpResp:
                (resp3: (int, DurableMailboxOpResult)) { op_resp = resp3; }
            }
            assert op_resp.1 == MailboxOpOk, "nack schedules retry";

            // Dead-letter by id succeeds with no token, removing the row.
            send mailbox, eDurableMailboxDeadLetter, (
                reply_to = this, id = 1
            );
            receive { case eDurableMailboxOpResp:
                (resp4: (int, DurableMailboxOpResult)) { op_resp = resp4; }
            }
            assert op_resp.1 == MailboxOpOk,
                "dead-letter removes the exhausted row by id";

            // The row is gone: a second dead-letter finds nothing.
            send mailbox, eDurableMailboxDeadLetter, (
                reply_to = this, id = 1
            );
            receive { case eDurableMailboxOpResp:
                (resp5: (int, DurableMailboxOpResult)) { op_resp = resp5; }
            }
            assert op_resp.1 == MailboxOpNotFound,
                "dead-lettered row is terminal";

            // With the predecessor removed, the successor is claimable.
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 22,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp6: (int, DurableMailboxOpResult)) {
                    lease_resp = resp6;
                }
            }
            assert lease_resp.0 == 2,
                "successor claims after dead-letter clears the head";

            goto Done;
        }
    }

    state Done {}
}

// TestMailboxFIFO_LegacyReorderNoInlineAssert replays the production reorder
// against the legacy claim mode but, unlike
// TestMailboxFIFO_LegacyCounterexampleToIdealFIFO, contains NO in-machine
// assertion. It exists to prove that the SameKeyFIFOClaimsRespectLiveHead
// monitor catches the backoff-overtake bug on its own. Run under
// tcMailboxMonitorCatchesLegacyReorder, which is expected to find a bug.
machine TestMailboxFIFO_LegacyReorderNoInlineAssert {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);
            var lease_resp: (int, DurableMailboxOpResult);

            mailbox = new DurableMailboxSpec(LegacyAvailableAtOrder);
            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    1, 10, 100, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 0
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp0: (int, DurableMailboxOpResult)) { op_resp = resp0; }
            }

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 11,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp1: (int, DurableMailboxOpResult)) {
                    lease_resp = resp1;
                }
            }

            send mailbox, eDurableMailboxNack, (
                reply_to = this, id = 1, lease_token = 11,
                available_at = 5
            );
            receive { case eDurableMailboxOpResp:
                (resp2: (int, DurableMailboxOpResult)) { op_resp = resp2; }
            }

            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    2, 10, 100, 0, 1, NoLease(), NoLeaseToken(),
                    0, 3, 1
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp3: (int, DurableMailboxOpResult)) { op_resp = resp3; }
            }

            // Legacy ordering claims row 2 while row 1 is a live same-key head.
            // No in-machine assertion fires here; only the global monitor can
            // catch the violation.
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 22,
                now = 1, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp4: (int, DurableMailboxOpResult)) {
                    lease_resp = resp4;
                }
            }

            goto Done;
        }
    }

    state Done {}
}

// MailboxLivenessDriver drives the non-starvation liveness property. It
// enqueues a same-key pair and a cross-key row (all available, no backoff),
// announces eMailboxWorkArrived per row, then drains by leasing-and-acking in a
// loop, announcing eMailboxWorkDrained per row. A correct model drains every
// row, returning MailboxKeyedWorkEventuallyDrains to its cold state; a model in
// which a row could never be claimed would leave it hot forever.
machine MailboxLivenessDriver {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);

            mailbox = new DurableMailboxSpec(PerCorrelationKeyFIFO);

            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    1, 10, 100, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 0
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp0: (int, DurableMailboxOpResult)) { op_resp = resp0; }
            }
            announce eMailboxWorkArrived;

            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    2, 10, 100, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 1
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp1: (int, DurableMailboxOpResult)) { op_resp = resp1; }
            }
            announce eMailboxWorkArrived;

            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    3, 10, 200, 0, 0, NoLease(), NoLeaseToken(),
                    0, 3, 2
                )
            );
            receive { case eDurableMailboxOpResp:
                (resp2: (int, DurableMailboxOpResult)) { op_resp = resp2; }
            }
            announce eMailboxWorkArrived;

            goto Draining;
        }
    }

    state Draining {
        entry {
            var lease_resp: (int, DurableMailboxOpResult);
            var op_resp: (int, DurableMailboxOpResult);
            var token: int;

            token = 1000;
            while (true) {
                send mailbox, eDurableMailboxLeaseNext, (
                    reply_to = this, mailbox_id = 10, lease_token = token,
                    now = 0, lease_duration = 5
                );
                receive { case eDurableMailboxLeaseResp:
                    (resp: (int, DurableMailboxOpResult)) {
                        lease_resp = resp;
                    }
                }

                if (lease_resp.1 != MailboxOpOk) {
                    break;
                }

                send mailbox, eDurableMailboxAck, (
                    reply_to = this, id = lease_resp.0, lease_token = token
                );
                receive { case eDurableMailboxOpResp:
                    (ackResp: (int, DurableMailboxOpResult)) {
                        op_resp = ackResp;
                    }
                }
                assert op_resp.1 == MailboxOpOk,
                    "lease owner can ack the row it just claimed";

                announce eMailboxWorkDrained;
                token = token + 1;
            }

            goto Done;
        }
    }

    state Done {}
}

// TestDurableMailboxSpec_LeaseFencedCommitExactlyOnce drives the durable
// actor's Read/Commit consume step under lease expiry mid-IO. Consumer A leases
// the row and begins its side-effect IO; while it is off doing IO the lease
// expires and consumer B reclaims and reprocesses the same row. A's stale
// fenced Commit must be an ErrLeaseLost no-op (token mismatch, no effect), and
// only B's fenced Commit applies the effect and consumes the row. The
// LeaseFencedCommitAppliesEffectAtMostOnce monitor confirms the effect is
// applied exactly once across both consumers.
machine TestDurableMailboxSpec_LeaseFencedCommitExactlyOnce {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);
            var lease_resp: (int, DurableMailboxOpResult);

            mailbox = new DurableMailboxSpec(PerCorrelationKeyFIFO);

            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    1, 10, 100, 0, 0, NoLease(), NoLeaseToken(), 0, 3, 0
                )
            );
            receive { case eDurableMailboxOpResp:
                (r0: (int, DurableMailboxOpResult)) { op_resp = r0; }
            }
            assert op_resp.1 == MailboxOpOk, "enqueue succeeds";

            // Consumer A leases the row (token 11), then begins IO.
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 11,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (r1: (int, DurableMailboxOpResult)) { lease_resp = r1; }
            }
            assert lease_resp.0 == 1 && lease_resp.1 == MailboxOpOk,
                "consumer A leases the row";

            // A's lease expires mid-IO; consumer B reclaims the same durable
            // row and reprocesses it.
            send mailbox, eDurableMailboxExpireLeasesAt, 6;
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 22,
                now = 6, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (r2: (int, DurableMailboxOpResult)) { lease_resp = r2; }
            }
            assert lease_resp.0 == 1 && lease_resp.1 == MailboxOpOk,
                "consumer B reclaims the same row after lease expiry";

            // A finishes its IO and commits with its now-stale token: the lease
            // fence rejects it and no effect is applied.
            send mailbox, eDurableMailboxCommit, (
                reply_to = this, id = 1, lease_token = 11, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r3: (int, DurableMailboxOpResult)) { op_resp = r3; }
            }
            assert op_resp.1 == MailboxOpTokenMismatch,
                "stale consumer A commit is an ErrLeaseLost no-op";

            // B commits with the current token: effect applied, row consumed.
            send mailbox, eDurableMailboxCommit, (
                reply_to = this, id = 1, lease_token = 22, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r4: (int, DurableMailboxOpResult)) { op_resp = r4; }
            }
            assert op_resp.1 == MailboxOpOk,
                "current owner B commits and consumes the row";

            // The committed row is terminal: no further redelivery.
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 33,
                now = 6, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (r5: (int, DurableMailboxOpResult)) { lease_resp = r5; }
            }
            assert lease_resp.1 == MailboxOpNotFound,
                "committed row is terminal";

            // A late commit for the already-consumed row hits the missing-row
            // branch: it must be a no-op (no second effect). This exercises the
            // MailboxOpNotFound path the reclaim-after-consume case takes, and
            // the LeaseFencedCommitAppliesEffectAtMostOnce monitor confirms it
            // applies no effect.
            send mailbox, eDurableMailboxCommit, (
                reply_to = this, id = 1, lease_token = 22, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r6: (int, DurableMailboxOpResult)) { op_resp = r6; }
            }
            assert op_resp.1 == MailboxOpNotFound,
                "commit of an already-consumed row is a not-found no-op";

            goto Done;
        }
    }

    state Done {}
}

// TestDurableMailboxSpec_UnfencedCommitDoubleApplyCounterexample drives the same
// reclaim scenario but with UNFENCED commits, modeling a behavior whose domain
// write is not bound to the lease-fenced ack. Consumer A's stale commit applies
// its effect even though its lease expired, then consumer B applies the effect
// again -- a double-apply. There is no in-machine assertion for the
// double-apply: it is raised solely by LeaseFencedCommitAppliesEffectAtMostOnce,
// proving the monitor catches the exact bug the lease fence prevents.
machine TestDurableMailboxSpec_UnfencedCommitDoubleApplyCounterexample {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);
            var lease_resp: (int, DurableMailboxOpResult);

            mailbox = new DurableMailboxSpec(PerCorrelationKeyFIFO);

            send mailbox, eDurableMailboxEnqueue, (
                reply_to = this,
                row = NewMailboxRow(
                    1, 10, 100, 0, 0, NoLease(), NoLeaseToken(), 0, 3, 0
                )
            );
            receive { case eDurableMailboxOpResp:
                (r0: (int, DurableMailboxOpResult)) { op_resp = r0; }
            }

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 11,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (r1: (int, DurableMailboxOpResult)) { lease_resp = r1; }
            }

            send mailbox, eDurableMailboxExpireLeasesAt, 6;
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 22,
                now = 6, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (r2: (int, DurableMailboxOpResult)) { lease_resp = r2; }
            }

            // Stale consumer A commits unfenced: the effect is applied despite
            // the expired-and-reclaimed lease.
            send mailbox, eDurableMailboxCommit, (
                reply_to = this, id = 1, lease_token = 11, fenced = false
            );
            receive { case eDurableMailboxOpResp:
                (r3: (int, DurableMailboxOpResult)) { op_resp = r3; }
            }

            // Consumer B commits unfenced: the effect is applied a second time.
            send mailbox, eDurableMailboxCommit, (
                reply_to = this, id = 1, lease_token = 22, fenced = false
            );
            receive { case eDurableMailboxOpResp:
                (r4: (int, DurableMailboxOpResult)) { op_resp = r4; }
            }

            goto Done;
        }
    }

    state Done {}
}

machine MailboxFIFOTestDriver {
    start state RunTests {
        entry {
            new TestMailboxFIFO_LegacyAvailableAtCanReorder();
            new TestMailboxFIFO_BlocksSameKeyBackoffOvertake();
            new TestMailboxFIFO_BlocksSameKeyActiveLeaseOvertake();
            new TestMailboxFIFO_CrossKeyIndependence();
            new TestMailboxFIFO_UnkeyedLaneUnaffected();
            new TestMailboxFIFO_ExhaustedPredecessorDoesNotBlock();
            new TestMailboxFIFO_MailboxIsolation();
            new TestDurableMailboxSpec_TokenOwnershipAndLeaseExpiry();
            new TestDurableMailboxSpec_NackRetryBlocksSameKey();
            new TestDurableMailboxSpec_DuplicateEnqueuePreservesFirstRow();
            new TestDurableMailboxSpec_PriorityDoesNotPierceSameKeyFIFO();
            new TestDurableMailboxSpec_DeadLetterByIdRemovesExhaustedRow();

            goto Done;
        }
    }

    state Done {}
}

// Spec monitors must be explicitly attached to a test case with `assert ... in`
// for P to check them; they are not activated globally. The safety monitor
// SameKeyFIFOClaimsRespectLiveHead is asserted on every claim-bearing case.
test tcMailboxCorrelationKeyFIFO [main=MailboxFIFOTestDriver]:
  assert SameKeyFIFOClaimsRespectLiveHead in
  { DurableMailboxSpec,
    TestMailboxFIFO_LegacyAvailableAtCanReorder,
    TestMailboxFIFO_BlocksSameKeyBackoffOvertake,
    TestMailboxFIFO_BlocksSameKeyActiveLeaseOvertake,
    TestMailboxFIFO_CrossKeyIndependence,
    TestMailboxFIFO_UnkeyedLaneUnaffected,
    TestMailboxFIFO_ExhaustedPredecessorDoesNotBlock,
    TestMailboxFIFO_MailboxIsolation,
    TestDurableMailboxSpec_TokenOwnershipAndLeaseExpiry,
    TestDurableMailboxSpec_NackRetryBlocksSameKey,
    TestDurableMailboxSpec_DuplicateEnqueuePreservesFirstRow,
    TestDurableMailboxSpec_PriorityDoesNotPierceSameKeyFIFO,
    TestDurableMailboxSpec_DeadLetterByIdRemovesExhaustedRow,
    MailboxFIFOTestDriver };

// tcMailboxLiveness checks the non-starvation liveness property: every enqueued
// row is eventually leased and acked, draining the mailbox.
test tcMailboxLiveness [main=MailboxLivenessDriver]:
  assert MailboxKeyedWorkEventuallyDrains in
  { DurableMailboxSpec, MailboxLivenessDriver };

test tcMailboxLegacyReorderCounterexample
    [main=TestMailboxFIFO_LegacyCounterexampleToIdealFIFO]:
  assert SameKeyFIFOClaimsRespectLiveHead in
  { DurableMailboxSpec, TestMailboxFIFO_LegacyCounterexampleToIdealFIFO };

// tcMailboxMonitorCatchesLegacyReorder runs the legacy reorder with no
// in-machine assertion, so the bug it finds is raised solely by the
// SameKeyFIFOClaimsRespectLiveHead monitor. It is expected to find a bug.
test tcMailboxMonitorCatchesLegacyReorder
    [main=TestMailboxFIFO_LegacyReorderNoInlineAssert]:
  assert SameKeyFIFOClaimsRespectLiveHead in
  { DurableMailboxSpec, TestMailboxFIFO_LegacyReorderNoInlineAssert };

// tcMailboxReadCommitFence checks the durable actor Read/Commit exactly-once
// effect contract: a row whose lease expires mid-IO is reclaimed and
// reprocessed, the stale consumer's lease-fenced commit is an ErrLeaseLost
// no-op, and the behavior effect is applied exactly once.
test tcMailboxReadCommitFence
    [main=TestDurableMailboxSpec_LeaseFencedCommitExactlyOnce]:
  assert LeaseFencedCommitAppliesEffectAtMostOnce in
  { DurableMailboxSpec, TestDurableMailboxSpec_LeaseFencedCommitExactlyOnce };

// tcMailboxUnfencedCommitCounterexample runs the reclaim scenario with unfenced
// commits and no in-machine assertion, so the double-apply is raised solely by
// LeaseFencedCommitAppliesEffectAtMostOnce. It is expected to find a bug.
test tcMailboxUnfencedCommitCounterexample
    [main=TestDurableMailboxSpec_UnfencedCommitDoubleApplyCounterexample]:
  assert LeaseFencedCommitAppliesEffectAtMostOnce in
  { DurableMailboxSpec,
    TestDurableMailboxSpec_UnfencedCommitDoubleApplyCounterexample };
