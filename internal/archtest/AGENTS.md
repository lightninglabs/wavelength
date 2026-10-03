# internal/archtest

Architecture tests that check properties of the package dependency graph.

## Leaf actors

`leaf_actors_test.go` pins the rule that makes an in-turn wait on `chainsource`
or `txconfirm` safe: those packages cannot import the actors that wait on
them, so they cannot hold a reference to a caller and can never send to or wait
on one. A wait toward such a leaf cannot be part of a wait cycle. Add a package
to `leafActors` only after checking that none of its actors ever send to or wait
on a caller, and keep `callerActors` in step with the actors that wait on it.
