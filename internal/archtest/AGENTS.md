# internal/archtest

Architecture tests that check properties of the package dependency graph.

## Leaf actors

`leaf_actors_test.go` pins the structural half of what makes an in-turn wait
on `chainsource` or `txconfirm` safe: the transitive imports of those packages
contain no other actor package, so a leaf cannot name a caller's type.
References can still arrive in messages (the `txconfirm` Subscriber, the
`chainsource` NotifyActor), so the real guarantee is that a leaf never parks a
turn unboundedly on a subscriber reference. `txconfirm` waits on a subscriber
Tell from a goroutine bounded by `terminalNotifyTimeout` (1s), and
`chainsource` delivers from monitor goroutines off the actor turn.

The test covers only the `chainsource` and `txconfirm` subset of the actorblock
analyzer baseline, not the vtxo-manager or other non-leaf waits.

`actorPackages` lists every package that defines an actor. A second test scans
the tree for non-test `Receive(ctx context.Context` methods and fails if a
package defines one but is in neither `actorPackages` nor `frameworkPackages`,
so a new actor package forces a deliberate classification. Add a package to
`leafActors` only after checking that none of its actors park a turn on a
caller supplied reference.
