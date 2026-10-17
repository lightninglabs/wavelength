--------------------------- MODULE DurableHandoff ---------------------------
EXTENDS TLC

\* These switches keep the safe and intentionally weakened profiles on the
\* same transition system. A counterexample therefore demonstrates the exact
\* guarantee supplied by the corresponding production choice.
CONSTANTS AtomicHandoff,
          DiscoverDelayed,
          DiscoverLeased,
          KeepTerminalConsumer

VARIABLES source,
          target,
          runtime,
          consumer,
          terminal,
          failedOnce,
          crashedOnce

vars == <<source, target, runtime, consumer, terminal, failedOnce, crashedOnce>>

SourceStates == {"Pending", "Acked"}
TargetStates == {"Absent", "Delayed", "Leased", "Ready", "Consumed"}
PendingTargetStates == {"Delayed", "Leased", "Ready"}
RuntimeStates == {"Up", "Down"}

TargetPending == target \in PendingTargetStates

\* Discoverable records the rows found by restart enumeration. Delayed rows
\* are not claimable yet and leased rows are owned until their lease expires,
\* but both remain durable work that needs a future consumer.
Discoverable ==
    \/ target = "Ready"
    \/ /\ target = "Delayed"
       /\ DiscoverDelayed
    \/ /\ target = "Leased"
       /\ DiscoverLeased

Init ==
    /\ source = "Pending"
    /\ target = "Absent"
    /\ runtime = "Up"
    /\ consumer = TRUE
    /\ terminal = FALSE
    /\ failedOnce = FALSE
    /\ crashedOnce = FALSE

\* Rollback models the first transactional fold failing. Neither the source
\* acknowledgement nor the target enqueue may escape the aborted commit.
Rollback ==
    /\ source = "Pending"
    /\ target = "Absent"
    /\ ~failedOnce
    /\ failedOnce' = TRUE
    /\ UNCHANGED <<source, target, runtime, consumer, terminal, crashedOnce>>

\* AtomicCommit is the production fold: acknowledging the source and
\* persisting its target handoff are one state transition.
AtomicCommit ==
    /\ AtomicHandoff
    /\ failedOnce
    /\ source = "Pending"
    /\ target = "Absent"
    /\ source' = "Acked"
    /\ target' \in {"Delayed", "Leased"}
    /\ UNCHANGED <<runtime, consumer, terminal, failedOnce, crashedOnce>>

\* SplitAck and SplitEnqueue form the deliberately unsafe profile. A crash or
\* even observation between them exposes an acknowledged source with no
\* durable successor.
SplitAck ==
    /\ ~AtomicHandoff
    /\ failedOnce
    /\ source = "Pending"
    /\ target = "Absent"
    /\ source' = "Acked"
    /\ UNCHANGED <<target, runtime, consumer, terminal, failedOnce,
                    crashedOnce>>

SplitEnqueue ==
    /\ ~AtomicHandoff
    /\ source = "Acked"
    /\ target = "Absent"
    /\ target' \in {"Delayed", "Leased"}
    /\ UNCHANGED <<source, runtime, consumer, terminal, failedOnce,
                    crashedOnce>>

\* Crash occurs at most once. The durable row remains while all in-memory
\* consumers disappear.
Crash ==
    /\ runtime = "Up"
    /\ TargetPending
    /\ ~crashedOnce
    /\ runtime' = "Down"
    /\ consumer' = FALSE
    /\ crashedOnce' = TRUE
    /\ UNCHANGED <<source, target, terminal, failedOnce>>

\* Restart reconstructs a consumer from durable mailbox inventory. The
\* weakened profiles omit rows that are delayed, leased, or terminal-owned.
Restart ==
    /\ runtime = "Down"
    /\ runtime' = "Up"
    /\ consumer' = Discoverable /\
        (KeepTerminalConsumer \/ ~terminal)
    /\ UNCHANGED <<source, target, terminal, failedOnce, crashedOnce>>

\* EnterTerminal captures a domain FSM reaching its final state before all
\* durable retries have drained. Production keeps that consumer reachable.
EnterTerminal ==
    /\ runtime = "Up"
    /\ TargetPending
    /\ ~terminal
    /\ terminal' = TRUE
    /\ consumer' = IF KeepTerminalConsumer THEN consumer ELSE FALSE
    /\ UNCHANGED <<source, target, runtime, failedOnce, crashedOnce>>

DelayElapses ==
    /\ target = "Delayed"
    /\ target' = "Ready"
    /\ UNCHANGED <<source, runtime, consumer, terminal, failedOnce,
                    crashedOnce>>

LeaseExpires ==
    /\ target = "Leased"
    /\ target' = "Ready"
    /\ UNCHANGED <<source, runtime, consumer, terminal, failedOnce,
                    crashedOnce>>

Consume ==
    /\ runtime = "Up"
    /\ consumer
    /\ target = "Ready"
    /\ target' = "Consumed"
    /\ UNCHANGED <<source, runtime, consumer, terminal, failedOnce,
                    crashedOnce>>

Next ==
    \/ Rollback
    \/ AtomicCommit
    \/ SplitAck
    \/ SplitEnqueue
    \/ Crash
    \/ Restart
    \/ EnterTerminal
    \/ DelayElapses
    \/ LeaseExpires
    \/ Consume

\* Fairness turns persistence into eventual progress: an enabled retry,
\* restart, timer, lease expiry, or consumer step cannot be ignored forever.
Spec ==
    /\ Init
    /\ [][Next]_vars
    /\ WF_vars(Rollback)
    /\ WF_vars(AtomicCommit)
    /\ WF_vars(SplitAck)
    /\ WF_vars(SplitEnqueue)
    /\ WF_vars(Restart)
    /\ WF_vars(DelayElapses)
    /\ WF_vars(LeaseExpires)
    /\ WF_vars(Consume)

TypeOK ==
    /\ source \in SourceStates
    /\ target \in TargetStates
    /\ runtime \in RuntimeStates
    /\ consumer \in BOOLEAN
    /\ terminal \in BOOLEAN
    /\ failedOnce \in BOOLEAN
    /\ crashedOnce \in BOOLEAN

\* AckHasDurableSuccessor is the atomic handoff contract.
AckHasDurableSuccessor ==
    source = "Pending" \/ target # "Absent"

\* RollbackLeavesRetryEnabled makes the forced failed attempt observable and
\* independently proves that the atomic retry remains enabled from the exact
\* post-rollback state.
RollbackLeavesRetryEnabled ==
    ~(failedOnce /\ source = "Pending" /\ target = "Absent")
    \/ ENABLED AtomicCommit

\* PendingTargetHasConsumer covers immediate, delayed, leased, and terminal
\* work whenever the process is running.
PendingTargetHasConsumer ==
    runtime = "Down" \/ ~TargetPending \/ consumer

\* RetryEventuallyCommits and PendingEventuallyDrains check that the safe
\* profile provides progress as well as state preservation.
RetryEventuallyCommits == <>(source = "Acked")
PendingEventuallyDrains == <>(target = "Consumed")

=============================================================================
