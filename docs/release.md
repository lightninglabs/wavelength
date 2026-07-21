# `wavelength`'s Reproducible Build System

This package contains the build script that the `wavelength` project uses
to build binaries for each new release. With modern Go build flags
(`-trimpath`, a stripped `-buildid`), binaries are reproducible: developers
can build the binary on distinct machines and end up with a byte-for-byte
identical result.

Ported from `lightningnetwork/lnd`'s `docs/release.md` and
`scripts/release.sh`/`scripts/verify-install.sh`.

## Building a New Release

### macOS

The first requirement is to have [`docker`](https://www.docker.com/)
installed locally and running. The second requirement is to have `make`
installed. Everything else (including `golang`) is included in the release
helper image.

To build a release, run the following commands:

```shell
$  git clone https://github.com/lightninglabs/wavelength.git
$  cd wavelength
$  git checkout <TAG> # <TAG> is the name of the next release/tag
$  make docker-release tag=<TAG>
```

Where `<TAG>` is the name of the next release of `wavelength`, e.g.
`v0.1.0-rc2`.

### Linux/Windows (WSL)

No prior set up beyond a pinned `go` toolchain (see `GO_VERSION` in the
`Makefile`) is required on Linux. On Windows, the only way to build the
release binaries at the moment is by using the Windows Subsystem for Linux.
One can build the release binaries following these steps:

```shell
$  git clone https://github.com/lightninglabs/wavelength.git
$  cd wavelength
$  git checkout <TAG> # <TAG> is the name of the next release/tag
$  make release tag=<TAG>
```

This will then create a directory of the form `wavelength-<TAG>` containing
archives of the release binaries (`waved` and `wavecli`) for each
supported operating system and architecture, and a manifest file containing
the hash of each archive.

## Publishing a Release

Pushing a `v*` tag starts the Release Build workflow
(`.github/workflows/release.yaml`). It runs `make docker-release` with the
builder image pinned by this repository, then uploads the archives, source
bundle, vendored dependencies, and manifest to a draft GitHub release. Release
candidate tags are marked as prereleases.

The release stays a draft until maintainers have signed the manifest and
uploaded the detached signatures described below. This keeps unsigned assets
from being presented as a finished release.

## Publishing the wasm and mobile binding assets

The reproducible manifest covers `waved` and `wavecli` only. Two auxiliary
asset sets that downstream consumers (notably `wavelength-sdk`) depend on are
published out of band, because they need host toolchains the reproducible
build deliberately avoids. Neither is part of the signed manifest.

### Browser wasm runtime → hosted bucket

The wasm wallet runtime (`wavewalletdk.wasm`, its gzip, `wasm_exec.js`, and
the go-wasmsqlite worker assets) is pure Go with no CGO. The SDK's web app
serves it from a hosted bucket and fetches `<base>/<version>/<file>` at deploy
time, where `<version>` is the SDK's `RUNTIME_MANIFEST_VERSION` and must equal
the release tag. Publish it at tag time with the release manager's own
`gcloud` credentials (no CI service account is wired for the bucket):

```shell
$  gcloud auth login            # once, if not already authenticated
$  make wasm-publish tag=<TAG> bucket=gs://<runtime-assets-bucket>
```

This builds the runtime (`make wasm-wallet`) and uploads the asset set to
`gs://<runtime-assets-bucket>/<TAG>/`, which the SDK reads through its HTTPS
front (`https://staging.lightning.engineering/walletdk/<TAG>/<file>`). The
file list lives in `scripts/publish-wasm-assets.sh` and must stay in sync with
the SDK's `RUNTIME_ASSET_FILES`.

The Mobile Bindings workflow also runs `make wasm-wallet` for release tags,
packages the complete runtime asset set as `Wavewalletdk.wasm.tar.gz`, and
attaches that archive to the GitHub release. The archive makes the exact CI
build available with the Android and iOS bindings, but it does not replace the
hosted-bucket step required by the web app.

### Mobile bindings → GitHub release assets

The gomobile bindings (`Wavewalletdk.aar`, `Wavewalletdk.xcframework`) depend
on the Android NDK and Xcode, so they are built in CI and attached to the
draft GitHub release automatically. The Mobile Bindings workflow
(`.github/workflows/mobile-bindings.yml`) runs on every `v*` tag: it
cross-compiles both mobile bindings, builds the browser WASM runtime, and its
`publish` job creates a draft release for the tag if one does not exist yet.
It then uploads `Wavewalletdk.aar`, `Wavewalletdk.xcframework.tar.gz`, and
`Wavewalletdk.wasm.tar.gz` as release assets. `wavelength-sdk`'s
`packages/react-native` consumes the mobile bindings instead of building from
a sibling checkout. No manual step is required beyond pushing the tag to
publish the GitHub release assets.

The Release Build and Mobile Bindings workflows can finish in either order.
They both target the same draft and create it only when absent, so re-runs do
not create competing releases. Once the manifest is signed, finalize the title
and notes, attach the signatures, and publish that draft.

## Verifying a Release

Third parties can independently run the release process and verify that all
the hashes of the release binaries match exactly those produced by the
official release manager, rather than having to trust them.

To verify a release, one must obtain the following tools (many of these come
installed by default on most Unix systems): `gpg`/`gpg2`, `shasum`, and
`tar`/`unzip`.

Once done, verifiers can proceed with the following steps:

1. Acquire the archive containing the release binaries for one's specific
   operating system and architecture, and the manifest file along with its
   signature(s).
2. Verify the signature of the manifest file with `gpg --verify
   manifest-<TAG>.txt.sig`. This will require obtaining the PGP keys which
   signed the manifest file — see `scripts/keys/`.
3. Recompute the `SHA256` hash of the archive with `shasum -a 256 <filename>`,
   locate the corresponding one in the manifest file, and ensure they match
   __exactly__.

At this point, verifiers can use the release binaries acquired if they trust
the integrity of the release manager(s). Otherwise, one can proceed with the
guide to verify the release binaries were built properly by obtaining
`shasum` and `go` (matching the same version used in the release):

4. Extract the release binaries contained within the archive, compute their
   hashes as done above, and note them down.
5. Ensure `go` is installed, matching the same version noted in the release
   (`GO_VERSION` in the `Makefile`).
6. Obtain a copy of `wavelength`'s source code with `git clone
   https://github.com/lightninglabs/wavelength` and checkout the source
   code of the release with `git checkout <TAG>`.
7. Proceed to verify the tag with `git verify-tag <TAG>` and compile the
   binaries from source for the intended operating system and architecture
   with `make release sys=OS-ARCH tag=<TAG>`.
8. Extract the archive found in the `wavelength-<TAG>` directory created
   by the release script and recompute the `SHA256` hash of the release
   binaries (`waved` and `wavecli`) with `shasum -a 256 <filename>`.
   These should match __exactly__ as the ones noted above.

`scripts/verify-install.sh` automates steps 1-3 against a published GitHub
Release: `./scripts/verify-install.sh <TAG> [path-to-waved
path-to-wavecli]`.

# Signing an Existing Manifest File

If you're a maintainer of `wavelength` and are interested in attaching
your signature to the final release archive, the manifest MUST be signed in
a manner that allows your signature to be verified by
`scripts/verify-install.sh`.

Assuming you've done a local build for _all_ release targets, then you should
have a file called `manifest-TAG.txt` where `TAG` is the actual release tag
description being signed. The verification script expects a particular file
name for each included signature, so we'll need to modify the name of our
output signature during signing.

Assuming `USERNAME` is your current nick as a maintainer, then the following
command will generate a proper signature:
```shell
$  gpg --detach-sig --output manifest-USERNAME-TAG.sig manifest-TAG.txt
```

Add your public key to `scripts/keys/USERNAME.asc` and reference it in the
`KEYS` array in `scripts/verify-install.sh`, then upload both the manifest
and your `.sig` file as assets on the GitHub Release.
