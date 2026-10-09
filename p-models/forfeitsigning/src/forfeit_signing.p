// forfeit_signing.p - Receive-side forfeit signing authority model.
//
// A forfeit signature authorizes a spend of one exact funded vHTLC. The
// mailbox request is transport, not authority: every identity field must
// match an immutable binding published by the receive session after its
// configured persistence step succeeds. A failed mailbox acknowledgement can
// redeliver the same request and produce different, independently valid
// Schnorr bytes. The broker accepts that replay for the retained transcript
// while preserving the first accepted signature set.

enum SigningPolicy {
    BoundAuthority,
    RequestDerived
}

enum SigningResult {
    SigningRejected,
    SigningAccepted
}

type FundingBinding = (
    payment_hash: int,
    outpoint: int,
    amount: int,
    script: int,
    policy: int,
    policy_hash: int
);

type SigningRequest = (
    reply_to: machine,
    request_id: int,
    payment_hash: int,
    outpoint: int,
    amount: int,
    script: int,
    policy: int,
    policy_hash: int,
    signature: int,
    signature_valid: bool,
    ack_succeeds: bool
);

event ePublishFunding: (binding: FundingBinding, durable: bool);
event eCrashRestart;
event eSigningRequest: SigningRequest;
event eSigningResult: (
    request_id: int,
    result: SigningResult,
    acked: bool
);

// The monitors observe the actual authority and broker decisions rather than
// inferring them from the driver's expected response.
event eSignerInvoked: (
    lifecycle: machine,
    request_id: int,
    authority_published: bool,
    binding: FundingBinding,
    request: SigningRequest
);
event eBrokerAccepted: (
    lifecycle: machine,
    request_id: int,
    submitted_signature: int,
    retained_signature: int,
    replay: bool
);

fun ExactBinding(binding: FundingBinding, req: SigningRequest): bool {
    return req.payment_hash == binding.payment_hash &&
        req.outpoint == binding.outpoint &&
        req.amount == binding.amount &&
        req.script == binding.script &&
        req.policy == binding.policy &&
        req.policy_hash == binding.policy_hash &&
        req.policy_hash == req.payment_hash &&
        binding.policy_hash == binding.payment_hash;
}

machine ForfeitSigningLifecycle {
    var policy: SigningPolicy;
    var authority_published: bool;
    var authority_durable: bool;
    var binding: FundingBinding;
    var answered: map[int, int];

    start state Running {
        entry (selected_policy: SigningPolicy) {
            policy = selected_policy;
        }

        on ePublishFunding do (published: (
            binding: FundingBinding, durable: bool
        )) {
            binding = published.binding;
            authority_durable = published.durable;
            authority_published = true;
        }

        on eCrashRestart do {
            if (!authority_durable) {
                authority_published = false;
            }
        }

        on eSigningRequest do HandleSigningRequest;
    }

    fun HandleSigningRequest(req: SigningRequest) {
        var exact_binding: bool;
        var retained_signature: int;
        var replay: bool;

        exact_binding = authority_published &&
            ExactBinding(binding, req);

        if (policy == BoundAuthority && !exact_binding) {
            send req.reply_to, eSigningResult, (
                request_id = req.request_id,
                result = SigningRejected,
                acked = false
            );

            return;
        }

        announce eSignerInvoked, (
            lifecycle = this,
            request_id = req.request_id,
            authority_published = authority_published,
            binding = binding,
            request = req
        );

        if (!req.signature_valid) {
            send req.reply_to, eSigningResult, (
                request_id = req.request_id,
                result = SigningRejected,
                acked = false
            );

            return;
        }

        replay = req.request_id in answered;
        if (replay) {
            retained_signature = answered[req.request_id];
        } else {
            answered[req.request_id] = req.signature;
            retained_signature = req.signature;
        }

        announce eBrokerAccepted, (
            lifecycle = this,
            request_id = req.request_id,
            submitted_signature = req.signature,
            retained_signature = retained_signature,
            replay = replay
        );
        send req.reply_to, eSigningResult, (
            request_id = req.request_id,
            result = SigningAccepted,
            acked = req.ack_succeeds
        );
    }
}

// SigningRequiresPublishedExactAuthority rejects every execution that reaches
// the signing oracle before authority is published or with any identity-field
// mismatch.
spec SigningRequiresPublishedExactAuthority observes eSignerInvoked {
    start state Monitoring {
        on eSignerInvoked do (attempt: (
            lifecycle: machine,
            request_id: int,
            authority_published: bool,
            binding: FundingBinding,
            request: SigningRequest
        )) {
            assert attempt.authority_published,
                "signer invoked before funded authority was published";
            assert attempt.request.payment_hash ==
                attempt.binding.payment_hash,
                "signer invoked for a different payment hash";
            assert attempt.request.outpoint == attempt.binding.outpoint,
                "signer invoked for a different outpoint";
            assert attempt.request.amount == attempt.binding.amount,
                "signer invoked for a different amount";
            assert attempt.request.script == attempt.binding.script,
                "signer invoked for a different script";
            assert attempt.request.policy == attempt.binding.policy,
                "signer invoked for a different policy";
            assert attempt.request.policy_hash ==
                attempt.binding.policy_hash,
                "signer invoked for a different policy payment hash";
            assert attempt.request.policy_hash ==
                attempt.request.payment_hash,
                "request policy does not commit to its payment hash";
            assert attempt.binding.policy_hash ==
                attempt.binding.payment_hash,
                "bound policy does not commit to its payment hash";
        }
    }
}

// BrokerReplayPreservesFirstSignature captures the idempotency boundary. A
// valid replay can carry different signature bytes, but acceptance must keep
// the first set that was delivered to the original waiter.
spec BrokerReplayPreservesFirstSignature observes eBrokerAccepted {
    var retained: map[(machine, int), int];

    start state Monitoring {
        on eBrokerAccepted do (accepted: (
            lifecycle: machine,
            request_id: int,
            submitted_signature: int,
            retained_signature: int,
            replay: bool
        )) {
            var key: (machine, int);

            key = (accepted.lifecycle, accepted.request_id);
            if (key in retained) {
                assert accepted.replay,
                    "an answered request was treated as new";
                assert accepted.retained_signature == retained[key],
                    "valid replay replaced the first signature set";
            } else {
                assert !accepted.replay,
                    "first accepted submission was treated as replay";
                retained[key] = accepted.retained_signature;
            }
        }
    }
}
