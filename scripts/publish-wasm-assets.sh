#!/usr/bin/env bash
#
# publish-wasm-assets.sh uploads the browser wasm runtime asset set to the
# hosted bucket that the wavelength-sdk web app pulls from. The assets are
# placed under a per-version directory (<bucket>/<version>/<file>) so that
# each daemon build gets a unique, immutable URL and a browser can never
# serve a stale cached runtime. This layout matches the consumer side in
# wavelength-sdk (apps/web-wallet-demo/scripts/fetch-runtime-assets.sh), which
# fetches <RUNTIME_ASSETS_BASE_URL>/<RUNTIME_MANIFEST_VERSION>/<file>.
#
# Auth is intentionally left to the caller's ambient gcloud credentials
# (gcloud auth login / application-default). No CI service account is wired
# for the bucket: releases are cut by a release manager at tag time, who runs
# this from a machine already authenticated to the project that owns the
# bucket.
#
# Usage:
#   scripts/publish-wasm-assets.sh <version> <gcs-bucket-uri> [<asset-dir>]
#
# Example:
#   scripts/publish-wasm-assets.sh v0.1.0 gs://my-walletdk-assets
#
# The public read URL the SDK points at is:
#   https://staging.lightning.engineering/walletdk/<version>/<file>
#
# <asset-dir> defaults to bin/wasm, where `make wasm-wallet` writes the built
# runtime. The `make wasm-publish` target builds first, then invokes this.

set -euo pipefail

# The runtime asset set. This list is the source of truth on the publishing
# side; it must stay in sync with the wavelength-sdk consumer, specifically
# RUNTIME_ASSET_FILES in packages/web/src/runtime-manifest.ts and the FILES
# array in apps/web-wallet-demo/scripts/fetch-runtime-assets.sh.
FILES=(
  wavewalletdk.wasm
  wavewalletdk.wasm.gz
  wasm_exec.js
  sqlite-bridge.js
  sqlite-worker.js
  sqlite3.js
  sqlite3.wasm
  sqlite3-opfs-async-proxy.js
)

function usage() {
  echo "usage: $0 <version> <gcs-bucket-uri> [<asset-dir>]" >&2
  echo "  <version>        version segment, e.g. v0.1.0 (must match the SDK's" >&2
  echo "                   RUNTIME_MANIFEST_VERSION)" >&2
  echo "  <gcs-bucket-uri> destination bucket root, e.g. gs://my-walletdk-assets" >&2
  echo "  <asset-dir>      built asset directory (default: bin/wasm)" >&2
  exit 1
}

version=${1:-}
bucket=${2:-}
asset_dir=${3:-bin/wasm}

if [[ -z "${version// }" || -z "${bucket// }" ]]; then
  usage
fi

# Normalize: strip a trailing slash from the bucket, and tolerate a bucket
# that already includes the version segment (a common copy-paste slip that
# would otherwise silently nest <version>/<version>/).
bucket=${bucket%/}
if [[ "$bucket" == */"$version" ]]; then
  echo "warning: bucket ends with /$version; treating the parent as the root" >&2
  bucket=${bucket%/"$version"}
fi

# Catch a malformed bucket (missing gs://, or a plain path) with an actionable
# message here rather than a generic gcloud error mid-publish.
if [[ "$bucket" != gs://* ]]; then
  echo "error: bucket must be a gs:// URI (got: $bucket)" >&2
  exit 1
fi

if ! command -v gcloud >/dev/null 2>&1; then
  echo "error: gcloud CLI not found; install the Google Cloud SDK and run" >&2
  echo "       'gcloud auth login' before publishing." >&2
  exit 1
fi

# Fail early with an actionable message if the build outputs are missing,
# rather than letting gcloud report a per-file not-found halfway through.
missing=0
for f in "${FILES[@]}"; do
  if [[ ! -s "$asset_dir/$f" ]]; then
    echo "error: missing or empty asset: $asset_dir/$f" >&2
    missing=1
  fi
done
if [[ "$missing" -ne 0 ]]; then
  echo "build the runtime first: make wasm-wallet" >&2
  exit 1
fi

dest="$bucket/$version"

# Fail fast on an expired/missing gcloud session or an unreachable bucket,
# rather than surfacing it on the first upload — which (see the immutability
# note below) could leave the version prefix half-published. Listing the bucket
# root exercises auth and access in one cheap call.
if ! gcloud storage ls "$bucket/" >/dev/null 2>&1; then
  echo "error: cannot list $bucket/ — run 'gcloud auth login' and confirm the" >&2
  echo "       bucket exists and your account has access." >&2
  exit 1
fi

# Each <bucket>/<version>/ prefix is meant to be immutable: the SDK derives the
# URL from the pinned version, and browsers cache it indefinitely. Refuse to
# overwrite an already-published version unless the caller explicitly sets
# FORCE=1, so an accidental re-run can't silently replace bytes clients have
# already cached.
if gcloud storage ls "$dest/" 2>/dev/null | grep -q .; then
  if [[ "${FORCE:-}" != "1" ]]; then
    echo "error: $dest/ already has objects; refusing to overwrite an" >&2
    echo "       already-published (immutable) version. Re-run with FORCE=1" >&2
    echo "       only if you are deliberately replacing this version." >&2
    exit 1
  fi
  echo "warning: $dest/ already populated; FORCE=1 set, overwriting." >&2
fi

echo "Publishing ${#FILES[@]} runtime assets to $dest/"

# Upload each file explicitly (rather than a recursive copy of the directory)
# so we publish exactly the manifest set and never leak stray files that a
# previous build may have left in the asset dir.
#
# Set Content-Type explicitly per extension. A bucket that serves
# wavewalletdk.wasm as application/octet-stream makes a browser's
# WebAssembly.instantiateStreaming reject the module, and gcloud's guess from
# the local MIME database is unreliable. We never set Content-Encoding:
# wavewalletdk.wasm.gz is a pre-gzipped payload the consumer fetches and stores
# verbatim, so it is labelled application/gzip (its true type) rather than
# application/wasm — the latter without Content-Encoding: gzip would hand a
# browser gzip bytes as if they were a raw module.
for f in "${FILES[@]}"; do
  echo " - $f"
  case "$f" in
    *.wasm.gz) content_type=application/gzip ;;
    *.wasm)    content_type=application/wasm ;;
    *.js)      content_type=text/javascript ;;
    *)         content_type= ;;
  esac
  if [[ -n "$content_type" ]]; then
    gcloud storage cp --content-type="$content_type" "$asset_dir/$f" "$dest/$f"
  else
    gcloud storage cp "$asset_dir/$f" "$dest/$f"
  fi
done

# Per-file cp is not atomic across the set, so a mid-loop failure (auth expiry,
# a transient error) could leave the version prefix partially published and
# silently serving 404s for the missing files. Re-list the prefix and confirm
# every file landed before declaring success.
echo "Verifying the published set..."
remote_list="$(gcloud storage ls "$dest/" 2>/dev/null || true)"
verify_missing=0
for f in "${FILES[@]}"; do
  if ! printf '%s\n' "$remote_list" | grep -qxF "$dest/$f"; then
    echo "error: $f is not present at $dest/ after upload" >&2
    verify_missing=1
  fi
done
if [[ "$verify_missing" -ne 0 ]]; then
  echo "publish incomplete — re-run (with FORCE=1) to finish the set." >&2
  exit 1
fi

echo "Done. Public read URL base: $dest/ (via the bucket's HTTPS front)."
