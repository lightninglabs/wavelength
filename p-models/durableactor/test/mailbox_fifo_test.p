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

// TestDurableMailboxSpec_LeaselessPeekMasksStaleLease exercises the
// SingleWorkerLeaseless mailbox edge: a row that was previously leased can
// become peek-eligible after the lease deadline passes, even if no maintenance
// worker has cleared the stale lease metadata. Peek must surface an empty token
// and leave attempts untouched; the by-ID nack then increments attempts and
// clears the stale metadata.
machine TestDurableMailboxSpec_LeaselessPeekMasksStaleLease {
    var mailbox: DurableMailboxSpec;

    start state Init {
        entry {
            var op_resp: (int, DurableMailboxOpResult);
            var lease_resp: (int, DurableMailboxOpResult);
            var peek_resp: (int, int, int, DurableMailboxOpResult);

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
            assert op_resp.1 == MailboxOpOk, "enqueue succeeds";

            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 11,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (resp1: (int, DurableMailboxOpResult)) {
                    lease_resp = resp1;
                }
            }
            assert lease_resp.0 == 1 && lease_resp.1 == MailboxOpOk,
                "leased path sets stale metadata and increments attempts";

            // No eDurableMailboxExpireLeasesAt here: production PeekNextMessage
            // treats lease_until < now as eligible even when the persisted row
            // still carries the old token/deadline.
            send mailbox, eDurableMailboxPeekNext, (
                reply_to = this, mailbox_id = 10, now = 6
            );
            receive { case eDurableMailboxPeekResp:
                (resp2: (int, int, int, DurableMailboxOpResult)) {
                    peek_resp = resp2;
                }
            }
            assert peek_resp.0 == 1 && peek_resp.3 == MailboxOpOk,
                "peek selects the expired-lease row";
            assert peek_resp.1 == NoLeaseToken(),
                "peek surfaces an empty actor-layer token";
            assert peek_resp.2 == 1,
                "peek is read-only and does not increment attempts";

            send mailbox, eDurableMailboxNackByID, (
                reply_to = this, id = 1, available_at = 7
            );
            receive { case eDurableMailboxOpResp:
                (resp3: (int, DurableMailboxOpResult)) { op_resp = resp3; }
            }
            assert op_resp.1 == MailboxOpOk,
                "by-ID nack succeeds without validating the stale token";

            send mailbox, eDurableMailboxPeekNext, (
                reply_to = this, mailbox_id = 10, now = 7
            );
            receive { case eDurableMailboxPeekResp:
                (resp4: (int, int, int, DurableMailboxOpResult)) {
                    peek_resp = resp4;
                }
            }
            assert peek_resp.0 == 1 && peek_resp.1 == NoLeaseToken(),
                "row remains on the empty-token leaseless path";
            assert peek_resp.2 == 2,
                "by-ID nack increments attempts for retry accounting";

            send mailbox, eDurableMailboxAckByID, (
                reply_to = this, id = 1
            );
            receive { case eDurableMailboxOpResp:
                (resp5: (int, DurableMailboxOpResult)) { op_resp = resp5; }
            }
            assert op_resp.1 == MailboxOpOk,
                "by-ID ack consumes the peeked row";

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

// TestDurableMailboxSpec_StageThenFencedCommitExactlyOnce drives the
// early-durable-write path under a crash between the Stage and the Commit.
// Consumer A leases the row, Stages its checkpoint and broadcasts a sweep, then
// crashes before committing (its lease expires mid-IO). Consumer B reclaims the
// same durable row, replays the Stage -- which must reuse the persisted sweep id
// (a monotone no-op on the checkpoint) -- and commits under the fence. A's stale
// commit is an ErrLeaseLost no-op. The StagedEffectAppliedAtMostOnceUnderReplay
// and CheckpointAdvancesMonotonically monitors confirm the replay neither
// double-broadcasts nor regresses the checkpoint, while
// LeaseFencedCommitAppliesEffectAtMostOnce confirms the consume is exactly once.
machine TestDurableMailboxSpec_StageThenFencedCommitExactlyOnce {
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

            // Consumer A leases the row (token 11) and begins its work.
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 11,
                now = 0, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (r1: (int, DurableMailboxOpResult)) { lease_resp = r1; }
            }
            assert lease_resp.0 == 1 && lease_resp.1 == MailboxOpOk,
                "consumer A leases the row";

            // A stages its checkpoint (level 1) and the sweep id (7) it is
            // about to broadcast, then would do its IO. stable = true: the sweep
            // id is persisted with the checkpoint. fenced = true: the write is
            // gated on A's lease token (which it holds here).
            send mailbox, eDurableMailboxStage, (
                reply_to = this, id = 1, lease_token = 11, effect_seq = 1,
                sweep_seq = 7, stable = true, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r2: (int, DurableMailboxOpResult)) { op_resp = r2; }
            }
            assert op_resp.1 == MailboxOpOk, "consumer A stages its checkpoint";

            // A crashes before committing: its lease expires and consumer B
            // reclaims the same durable row (the staged checkpoint survives).
            send mailbox, eDurableMailboxExpireLeasesAt, 6;
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 22,
                now = 6, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (r3: (int, DurableMailboxOpResult)) { lease_resp = r3; }
            }
            assert lease_resp.0 == 1 && lease_resp.1 == MailboxOpOk,
                "consumer B reclaims the row after lease expiry";

            // B replays the same event under its own token. It re-stages level
            // 1 and, because the design is stable, reuses the persisted sweep id
            // 7 even though it offers a freshly-derived 9 -- so the re-broadcast
            // is the same id and the receiver dedups it.
            send mailbox, eDurableMailboxStage, (
                reply_to = this, id = 1, lease_token = 22, effect_seq = 1,
                sweep_seq = 9, stable = true, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r4: (int, DurableMailboxOpResult)) { op_resp = r4; }
            }
            assert op_resp.1 == MailboxOpOk, "consumer B replays the stage";

            // A wakes up stale and tries to stage its older view with its
            // now-reclaimed token. The Stage fence rejects it, so it cannot
            // regress B's checkpoint -- the multi-consumer lost-update guard.
            send mailbox, eDurableMailboxStage, (
                reply_to = this, id = 1, lease_token = 11, effect_seq = 1,
                sweep_seq = 7, stable = true, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (rStale: (int, DurableMailboxOpResult)) { op_resp = rStale; }
            }
            assert op_resp.1 == MailboxOpTokenMismatch,
                "stale consumer A stage is fenced out (no regression)";

            // A finishes its IO and commits with its now-stale token: fenced
            // out, no effect.
            send mailbox, eDurableMailboxCommit, (
                reply_to = this, id = 1, lease_token = 11, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r5: (int, DurableMailboxOpResult)) { op_resp = r5; }
            }
            assert op_resp.1 == MailboxOpTokenMismatch,
                "stale consumer A commit is an ErrLeaseLost no-op";

            // B commits under the current token: the message is consumed once.
            send mailbox, eDurableMailboxCommit, (
                reply_to = this, id = 1, lease_token = 22, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r6: (int, DurableMailboxOpResult)) { op_resp = r6; }
            }
            assert op_resp.1 == MailboxOpOk,
                "current owner B commits and consumes the row";

            goto Done;
        }
    }

    state Done {}
}

// TestDurableMailboxSpec_StagedDoubleBroadcastCounterexample drives the same
// crash-and-replay scenario with the UNSTABLE design: the behavior re-derives a
// fresh broadcast id (a sweep rebuilt with a new wallet address) on the replay
// instead of reusing the one persisted with the checkpoint. The replay therefore
// emits a second, distinct broadcast for the same row -- a double-broadcast.
// There is no in-machine assertion for it: it is raised solely by
// StagedEffectAppliedAtMostOnceUnderReplay, proving the monitor catches the
// exact failure the persist-before-broadcast / sweep-reuse rule prevents.
machine TestDurableMailboxSpec_StagedDoubleBroadcastCounterexample {
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

            // A stages and broadcasts sweep id 7 (unstable design). A holds the
            // lease here, so the fence passes; the bug is the broadcast id.
            send mailbox, eDurableMailboxStage, (
                reply_to = this, id = 1, lease_token = 11, effect_seq = 1,
                sweep_seq = 7, stable = false, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r2: (int, DurableMailboxOpResult)) { op_resp = r2; }
            }

            // A crashes; B reclaims the same row.
            send mailbox, eDurableMailboxExpireLeasesAt, 6;
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 22,
                now = 6, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (r3: (int, DurableMailboxOpResult)) { lease_resp = r3; }
            }

            // B replays under its own token, but the unstable design derives a
            // FRESH sweep id 9 and broadcasts it -- a second, distinct broadcast
            // for the same row.
            send mailbox, eDurableMailboxStage, (
                reply_to = this, id = 1, lease_token = 22, effect_seq = 1,
                sweep_seq = 9, stable = false, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r4: (int, DurableMailboxOpResult)) { op_resp = r4; }
            }

            goto Done;
        }
    }

    state Done {}
}

// TestDurableMailboxSpec_StaleStageRegressesCheckpointCounterexample drives the
// multi-consumer lost-update scenario with an UNFENCED stage. Consumer A stages
// level 2, its lease expires, consumer B reclaims and advances the checkpoint to
// level 3, then stale A wakes up and stages its older level 2 with the fence
// disabled. Because the unfenced write overwrites regardless of the token, it
// regresses the checkpoint from 3 back to 2. There is no in-machine assertion
// for it: the regression is raised solely by CheckpointAdvancesMonotonically,
// proving the monitor catches the exact lost-update the Stage fence prevents.
machine TestDurableMailboxSpec_StaleStageRegressesCheckpointCounterexample {
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

            // A stages level 2 under its lease.
            send mailbox, eDurableMailboxStage, (
                reply_to = this, id = 1, lease_token = 11, effect_seq = 2,
                sweep_seq = 7, stable = true, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r2: (int, DurableMailboxOpResult)) { op_resp = r2; }
            }

            // A's lease expires; B reclaims and advances the checkpoint to 3.
            send mailbox, eDurableMailboxExpireLeasesAt, 6;
            send mailbox, eDurableMailboxLeaseNext, (
                reply_to = this, mailbox_id = 10, lease_token = 22,
                now = 6, lease_duration = 5
            );
            receive { case eDurableMailboxLeaseResp:
                (r3: (int, DurableMailboxOpResult)) { lease_resp = r3; }
            }
            send mailbox, eDurableMailboxStage, (
                reply_to = this, id = 1, lease_token = 22, effect_seq = 3,
                sweep_seq = 7, stable = true, fenced = true
            );
            receive { case eDurableMailboxOpResp:
                (r4: (int, DurableMailboxOpResult)) { op_resp = r4; }
            }

            // Stale A wakes up and stages its older level 2 UNFENCED, so the
            // overwrite regresses the checkpoint from 3 back to 2.
            send mailbox, eDurableMailboxStage, (
                reply_to = this, id = 1, lease_token = 11, effect_seq = 2,
                sweep_seq = 7, stable = true, fenced = false
            );
            receive { case eDurableMailboxOpResp:
                (r5: (int, DurableMailboxOpResult)) { op_resp = r5; }
            }

            goto Done;
        }
    }

    state Done {}
}

// TestOutboxFold_AtomicRollbackThenRedeliver drives the production AtomicFold
// path of the CDC delivery step. A folded delivery fails after the target
// enqueue, so the whole transaction rolls back: no delivery is announced and
// the outbox row stays pending. The publisher reclaims the row under a new
// token and redelivers successfully -- the enqueue and the completion land
// together -- after which the completed row is terminal and no longer
// claimable. The OutboxCompletionImpliesDelivery and
// OutboxTargetDeliveredAtMostOnce monitors confirm no lost message and exactly
// one delivery.
machine TestOutboxFold_AtomicRollbackThenRedeliver {
    var outbox: OutboxFoldSpec;

    start state Init {
        entry {
            var resp: (int, DurableMailboxOpResult);

            outbox = new OutboxFoldSpec(AtomicFold);

            send outbox, eOutboxFoldEnqueue, (reply_to = this, id = 1);
            receive { case eOutboxFoldResp:
                (r0: (int, DurableMailboxOpResult)) { resp = r0; }
            }
            assert resp.1 == MailboxOpOk, "outbox enqueue succeeds";

            send outbox, eOutboxFoldClaim, (
                reply_to = this, id = 1, token = 11
            );
            receive { case eOutboxFoldResp:
                (r1: (int, DurableMailboxOpResult)) { resp = r1; }
            }
            assert resp.1 == MailboxOpOk, "first claim sets the owner token";

            // The fold fails after the enqueue step: AtomicFold rolls the whole
            // transaction back, so nothing is delivered and the row is pending.
            send outbox, eOutboxFoldDeliver, (
                reply_to = this, id = 1, token = 11, enqueue_ok = false
            );
            receive { case eOutboxFoldResp:
                (r2: (int, DurableMailboxOpResult)) { resp = r2; }
            }
            assert resp.1 == MailboxOpOk, "failed fold rolls back cleanly";

            // Reclaim after the prior claim expires (new owner token), then
            // redeliver successfully: the enqueue and the completion commit
            // together.
            send outbox, eOutboxFoldClaim, (
                reply_to = this, id = 1, token = 22
            );
            receive { case eOutboxFoldResp:
                (r3: (int, DurableMailboxOpResult)) { resp = r3; }
            }
            assert resp.1 == MailboxOpOk, "reclaim assigns a new owner token";

            send outbox, eOutboxFoldDeliver, (
                reply_to = this, id = 1, token = 22, enqueue_ok = true
            );
            receive { case eOutboxFoldResp:
                (r4: (int, DurableMailboxOpResult)) { resp = r4; }
            }
            assert resp.1 == MailboxOpOk, "redelivery commits the fold";

            // A completed row is terminal: no further claim returns it.
            send outbox, eOutboxFoldClaim, (
                reply_to = this, id = 1, token = 33
            );
            receive { case eOutboxFoldResp:
                (r5: (int, DurableMailboxOpResult)) { resp = r5; }
            }
            assert resp.1 == MailboxOpNotFound,
                "completed outbox row is terminal";

            goto Done;
        }
    }

    state Done {}
}

// TestOutboxFold_StaleClaimCannotComplete drives the cross-publisher reclaim
// race. A slow publisher's claim expires and a second publisher reclaims the
// row; the stale publisher then folds with its OLD token. Its enqueue commits
// (idempotent by id) but its completion is fenced out (token mismatch), so the
// row stays pending. The current owner then folds: the enqueue is an idempotent
// no-op and the completion lands. The target is delivered exactly once
// (OutboxTargetDeliveredAtMostOnce) and never completed without a delivery
// (OutboxCompletionImpliesDelivery).
machine TestOutboxFold_StaleClaimCannotComplete {
    var outbox: OutboxFoldSpec;

    start state Init {
        entry {
            var resp: (int, DurableMailboxOpResult);

            outbox = new OutboxFoldSpec(AtomicFold);

            send outbox, eOutboxFoldEnqueue, (reply_to = this, id = 1);
            receive { case eOutboxFoldResp:
                (r0: (int, DurableMailboxOpResult)) { resp = r0; }
            }
            assert resp.1 == MailboxOpOk, "outbox enqueue succeeds";

            // Publisher P1 claims (token 11); then P1 stalls and P2 reclaims
            // the same row (token 22) after the claim expires.
            send outbox, eOutboxFoldClaim, (
                reply_to = this, id = 1, token = 11
            );
            receive { case eOutboxFoldResp:
                (r1: (int, DurableMailboxOpResult)) { resp = r1; }
            }
            assert resp.1 == MailboxOpOk, "P1 claims the row";

            send outbox, eOutboxFoldClaim, (
                reply_to = this, id = 1, token = 22
            );
            receive { case eOutboxFoldResp:
                (r2: (int, DurableMailboxOpResult)) { resp = r2; }
            }
            assert resp.1 == MailboxOpOk, "P2 reclaims the row";

            // Stale P1 folds with its now-superseded token: the enqueue
            // commits, but the completion matches no row and is a no-op.
            send outbox, eOutboxFoldDeliver, (
                reply_to = this, id = 1, token = 11, enqueue_ok = true
            );
            receive { case eOutboxFoldResp:
                (r3: (int, DurableMailboxOpResult)) { resp = r3; }
            }
            assert resp.1 == MailboxOpOk, "stale P1 fold commits its enqueue";

            // Current owner P2 folds: the enqueue is an idempotent no-op and
            // the completion lands under the matching token.
            send outbox, eOutboxFoldDeliver, (
                reply_to = this, id = 1, token = 22, enqueue_ok = true
            );
            receive { case eOutboxFoldResp:
                (r4: (int, DurableMailboxOpResult)) { resp = r4; }
            }
            assert resp.1 == MailboxOpOk, "current P2 fold completes the row";

            goto Done;
        }
    }

    state Done {}
}

// TestOutboxFold_SplitWriteLosesMessageCounterexample drives the same delivery
// with the SplitWrite (non-transactional two-step) design and NO in-machine
// assertion. The completion write lands even though the enqueue did not durably
// happen, so the outbox row is completed with the message never delivered. The
// lost message is raised solely by OutboxCompletionImpliesDelivery, proving the
// monitor catches the exact failure the single fold transaction prevents.
machine TestOutboxFold_SplitWriteLosesMessageCounterexample {
    var outbox: OutboxFoldSpec;

    start state Init {
        entry {
            var resp: (int, DurableMailboxOpResult);

            outbox = new OutboxFoldSpec(SplitWrite);

            send outbox, eOutboxFoldEnqueue, (reply_to = this, id = 1);
            receive { case eOutboxFoldResp:
                (r0: (int, DurableMailboxOpResult)) { resp = r0; }
            }

            send outbox, eOutboxFoldClaim, (
                reply_to = this, id = 1, token = 11
            );
            receive { case eOutboxFoldResp:
                (r1: (int, DurableMailboxOpResult)) { resp = r1; }
            }

            // The completion lands but the enqueue did not durably happen: a
            // lost message under the split-write design.
            send outbox, eOutboxFoldDeliver, (
                reply_to = this, id = 1, token = 11, enqueue_ok = false
            );
            receive { case eOutboxFoldResp:
                (r2: (int, DurableMailboxOpResult)) { resp = r2; }
            }

            goto Done;
        }
    }

    state Done {}
}

// OutboxFoldGreenDriver spawns the green outbox-fold scenarios under the
// no-lost-message and exactly-once monitors.
machine OutboxFoldGreenDriver {
    start state RunTests {
        entry {
            new TestOutboxFold_AtomicRollbackThenRedeliver();
            new TestOutboxFold_StaleClaimCannotComplete();

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
            new TestDurableMailboxSpec_LeaselessPeekMasksStaleLease();

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
    TestDurableMailboxSpec_LeaselessPeekMasksStaleLease,
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

// tcMailboxStageCommitExactlyOnce checks the early-durable-write contract: a
// row whose checkpoint is Staged and broadcast, then crashes before the Commit,
// is reclaimed and replayed without double-broadcasting or regressing the
// checkpoint, and is consumed exactly once under the fence.
test tcMailboxStageCommitExactlyOnce
    [main=TestDurableMailboxSpec_StageThenFencedCommitExactlyOnce]:
  assert StagedEffectAppliedAtMostOnceUnderReplay,
         CheckpointAdvancesMonotonically,
         LeaseFencedCommitAppliesEffectAtMostOnce in
  { DurableMailboxSpec,
    TestDurableMailboxSpec_StageThenFencedCommitExactlyOnce };

// tcMailboxStagedDoubleBroadcastCounterexample runs the crash-and-replay
// scenario with the unstable design (a fresh broadcast id on replay) and no
// in-machine assertion, so the double-broadcast is raised solely by
// StagedEffectAppliedAtMostOnceUnderReplay. It is expected to find a bug.
test tcMailboxStagedDoubleBroadcastCounterexample
    [main=TestDurableMailboxSpec_StagedDoubleBroadcastCounterexample]:
  assert StagedEffectAppliedAtMostOnceUnderReplay in
  { DurableMailboxSpec,
    TestDurableMailboxSpec_StagedDoubleBroadcastCounterexample };

// tcMailboxStaleStageRegressesCounterexample runs the multi-consumer lost-update
// scenario with an unfenced stage and no in-machine assertion, so the checkpoint
// regression is raised solely by CheckpointAdvancesMonotonically. It is expected
// to find a bug.
test tcMailboxStaleStageRegressesCounterexample
    [main=TestDurableMailboxSpec_StaleStageRegressesCheckpointCounterexample]:
  assert CheckpointAdvancesMonotonically in
  { DurableMailboxSpec,
    TestDurableMailboxSpec_StaleStageRegressesCheckpointCounterexample };

// tcOutboxFold checks the transactional outbox fold contract: a folded delivery
// commits the target enqueue and the outbox completion together or not at all,
// a failed fold rolls back with no orphan and redelivers after claim expiry,
// completion is token-fenced against stale publishers, and the target is
// delivered exactly once. No outbox row is ever completed without a durable
// delivery.
test tcOutboxFold [main=OutboxFoldGreenDriver]:
  assert OutboxCompletionImpliesDelivery,
         OutboxTargetDeliveredAtMostOnce in
  { OutboxFoldSpec,
    TestOutboxFold_AtomicRollbackThenRedeliver,
    TestOutboxFold_StaleClaimCannotComplete,
    OutboxFoldGreenDriver };

// tcOutboxSplitWriteCounterexample runs the delivery with the non-transactional
// split-write design and no in-machine assertion, so the lost message is raised
// solely by OutboxCompletionImpliesDelivery. It is expected to find a bug.
test tcOutboxSplitWriteCounterexample
    [main=TestOutboxFold_SplitWriteLosesMessageCounterexample]:
  assert OutboxCompletionImpliesDelivery in
  { OutboxFoldSpec,
    TestOutboxFold_SplitWriteLosesMessageCounterexample };
