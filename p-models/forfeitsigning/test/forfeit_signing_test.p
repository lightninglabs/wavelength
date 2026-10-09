// forfeit_signing_test.p - Model-checking profiles for signing authority.

fun ExactTestBinding(): FundingBinding {
    return (
        payment_hash = 11,
        outpoint = 22,
        amount = 33,
        script = 44,
        policy = 55,
        policy_hash = 11
    );
}

fun TestRequest(reply_to: machine, request_id: int,
    signature: int, signature_valid: bool,
    ack_succeeds: bool): SigningRequest {

    var binding: FundingBinding;

    binding = ExactTestBinding();

    return (
        reply_to = reply_to,
        request_id = request_id,
        payment_hash = binding.payment_hash,
        outpoint = binding.outpoint,
        amount = binding.amount,
        script = binding.script,
        policy = binding.policy,
        policy_hash = binding.policy_hash,
        signature = signature,
        signature_valid = signature_valid,
        ack_succeeds = ack_succeeds
    );
}

enum IdentityMismatch {
    PaymentHashMismatch,
    OutpointMismatch,
    AmountMismatch,
    ScriptMismatch,
    PolicyMismatch,
    RequestPolicyHashMismatch,
    BindingPolicyHashMismatch
}

// TestForfeitSigningRejectsIdentityMismatch independently drives every field
// in the complete binding. These cases keep the authority monitor independent
// from the lifecycle's admission predicate: omitting any comparison from that
// predicate reaches the signer and fails the monitor.
machine TestForfeitSigningRejectsIdentityMismatch {
    var lifecycle: ForfeitSigningLifecycle;
    var mismatch: IdentityMismatch;

    start state Init {
        entry (selected_mismatch: IdentityMismatch) {
            var binding: FundingBinding;
            var req: SigningRequest;
            var response: (
                request_id: int,
                result: SigningResult,
                acked: bool
            );

            mismatch = selected_mismatch;
            lifecycle = new ForfeitSigningLifecycle(BoundAuthority);
            binding = ExactTestBinding();
            req = TestRequest(this, 401, 1, true, true);

            if (mismatch == PaymentHashMismatch) {
                req.payment_hash = req.payment_hash + 1;
            } else if (mismatch == OutpointMismatch) {
                req.outpoint = req.outpoint + 1;
            } else if (mismatch == AmountMismatch) {
                req.amount = req.amount + 1;
            } else if (mismatch == ScriptMismatch) {
                req.script = req.script + 1;
            } else if (mismatch == PolicyMismatch) {
                req.policy = req.policy + 1;
            } else if (mismatch == RequestPolicyHashMismatch) {
                req.policy_hash = req.policy_hash + 1;
            } else if (mismatch == BindingPolicyHashMismatch) {
                binding.policy_hash = binding.policy_hash + 1;
            }

            send lifecycle, ePublishFunding, (
                binding = binding, durable = true
            );
            send lifecycle, eSigningRequest, req;
            receive {
                case eSigningResult: (rejected: (
                    request_id: int,
                    result: SigningResult,
                    acked: bool
                )) {
                    response = rejected;
                }
            }
            assert response.result == SigningRejected && !response.acked,
                "identity mismatch must remain unsigned";

            goto Done;
        }
    }

    state Done {}
}

machine TestForfeitSigningAuthorityAndReplay {
    var lifecycle: ForfeitSigningLifecycle;
    var response: (
        request_id: int,
        result: SigningResult,
        acked: bool
    );

    start state Init {
        entry {
            lifecycle = new ForfeitSigningLifecycle(BoundAuthority);

            // Transport delivery before publication must remain unsigned and
            // unacknowledged so it can be redelivered later.
            send lifecycle, eSigningRequest,
                TestRequest(this, 101, 1, true, false);
            receive {
                case eSigningResult: (pre_authority: (
                    request_id: int,
                    result: SigningResult,
                    acked: bool
                )) {
                    response = pre_authority;
                }
            }
            assert response.result == SigningRejected && !response.acked,
                "pre-authority delivery must fail closed";

            send lifecycle, ePublishFunding, (
                binding = ExactTestBinding(), durable = true
            );

            // The first accepted submission can outlive a failed mailbox ACK.
            send lifecycle, eSigningRequest,
                TestRequest(this, 101, 1, true, false);
            receive {
                case eSigningResult: (first: (
                    request_id: int,
                    result: SigningResult,
                    acked: bool
                )) {
                    response = first;
                }
            }
            assert response.result == SigningAccepted && !response.acked,
                "accepted submission must remain replayable after ACK failure";

            // A fresh valid Schnorr encoding is accepted as the same answer,
            // while the broker retains the original signature set.
            send lifecycle, eSigningRequest,
                TestRequest(this, 101, 2, true, true);
            receive {
                case eSigningResult: (replay: (
                    request_id: int,
                    result: SigningResult,
                    acked: bool
                )) {
                    response = replay;
                }
            }
            assert response.result == SigningAccepted && response.acked,
                "valid alternate replay must allow the ACK to complete";

            // A process restart with durable authority reconstructs the same
            // binding. The exact request remains admissible afterward.
            send lifecycle, eCrashRestart;
            send lifecycle, eSigningRequest,
                TestRequest(this, 102, 3, true, true);
            receive {
                case eSigningResult: (after_restart: (
                    request_id: int,
                    result: SigningResult,
                    acked: bool
                )) {
                    response = after_restart;
                }
            }
            assert response.result == SigningAccepted,
                "durable authority must survive restart";

            // Parseable but invalid alternate bytes do not satisfy replay.
            send lifecycle, eSigningRequest,
                TestRequest(this, 101, 5, false, true);
            receive {
                case eSigningResult: (invalid_replay: (
                    request_id: int,
                    result: SigningResult,
                    acked: bool
                )) {
                    response = invalid_replay;
                }
            }
            assert response.result == SigningRejected && !response.acked,
                "invalid replay signature must be rejected";

            goto Done;
        }
    }

    state Done {}
}

machine TestEphemeralAuthorityIsLostOnRestart {
    var lifecycle: ForfeitSigningLifecycle;
    var response: (
        request_id: int,
        result: SigningResult,
        acked: bool
    );

    start state Init {
        entry {
            lifecycle = new ForfeitSigningLifecycle(BoundAuthority);
            send lifecycle, ePublishFunding, (
                binding = ExactTestBinding(), durable = false
            );
            send lifecycle, eCrashRestart;
            send lifecycle, eSigningRequest,
                TestRequest(this, 201, 1, true, true);
            receive {
                case eSigningResult: (after_restart: (
                    request_id: int,
                    result: SigningResult,
                    acked: bool
                )) {
                    response = after_restart;
                }
            }
            assert response.result == SigningRejected && !response.acked,
                "ephemeral authority must be republished after restart";

            goto Done;
        }
    }

    state Done {}
}

machine ForfeitSigningGreenDriver {
    start state Init {
        entry {
            new TestForfeitSigningAuthorityAndReplay();
            new TestEphemeralAuthorityIsLostOnRestart();
            new TestForfeitSigningRejectsIdentityMismatch(
                PaymentHashMismatch
            );
            new TestForfeitSigningRejectsIdentityMismatch(
                OutpointMismatch
            );
            new TestForfeitSigningRejectsIdentityMismatch(
                AmountMismatch
            );
            new TestForfeitSigningRejectsIdentityMismatch(
                ScriptMismatch
            );
            new TestForfeitSigningRejectsIdentityMismatch(
                PolicyMismatch
            );
            new TestForfeitSigningRejectsIdentityMismatch(
                RequestPolicyHashMismatch
            );
            new TestForfeitSigningRejectsIdentityMismatch(
                BindingPolicyHashMismatch
            );

            goto Done;
        }
    }

    state Done {}
}

// TestRequestDerivedSigningCounterexample restores the unsafe rule: the
// request itself supplies signing authority. The authority monitor must reject
// the resulting pre-publication signer invocation.
machine TestRequestDerivedSigningCounterexample {
    var lifecycle: ForfeitSigningLifecycle;

    start state Init {
        entry {
            lifecycle = new ForfeitSigningLifecycle(RequestDerived);
            send lifecycle, eSigningRequest,
                TestRequest(this, 301, 1, true, true);

            goto Done;
        }
    }

    state Done {}
}

test tcForfeitSigningAuthorityAndReplay [main=ForfeitSigningGreenDriver]:
    assert SigningRequiresPublishedExactAuthority,
        BrokerReplayPreservesFirstSignature in
    { ForfeitSigningLifecycle,
      TestForfeitSigningRejectsIdentityMismatch,
      TestForfeitSigningAuthorityAndReplay,
      TestEphemeralAuthorityIsLostOnRestart,
      ForfeitSigningGreenDriver };

test tcForfeitRequestDerivedCounterexample
    [main=TestRequestDerivedSigningCounterexample]:
    assert SigningRequiresPublishedExactAuthority in
    { ForfeitSigningLifecycle, TestRequestDerivedSigningCounterexample };
