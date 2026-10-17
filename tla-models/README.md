# TLA+ Models

This directory contains exhaustive TLA+ models for small distributed-system
state spaces. The models complement the randomized P models under
[`p-models`](../p-models/): TLC explores every reachable state within the
declared finite bounds, while each negative configuration proves that the
corresponding invariant detects a concrete unsafe transition.

Run every model and expected counterexample from the repository root:

```shell
./tla-models/scripts/check.sh
```

The script downloads the pinned TLA+ tools release into a user cache and
checks its SHA-256 digest. To use an existing copy instead:

```shell
TLA2TOOLS_JAR=/path/to/tla2tools.jar ./tla-models/scripts/check.sh
```

Java 17 or newer is required.
