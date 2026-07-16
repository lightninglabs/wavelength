# Release verification keys

This directory contains all keys that are currently signing `wavelength`
releases.

The name of the file must match exactly the suffix that user is going to use
when signing a release.
For example, if the key is called `eugene_.asc` then that user must upload a
signature file called `manifest-eugene_-v0.xx.yy.sig`, otherwise the
verification will fail.

In addition to adding the key file here as a `.asc` file the
`scripts/verify-install.sh` file must also be updated with the key ID and the
reference to the key file.

Ported from `lightningnetwork/lnd`'s `scripts/keys/`. This list is currently
bootstrapped with a single signer; `scripts/verify-install.sh`'s
`MIN_REQUIRED_SIGNATURES` must be raised as more maintainers are added here.
