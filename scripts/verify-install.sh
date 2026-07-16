#!/bin/bash

# Fail hard on any command or pipeline failure: a silently failed
# download or gpg call must never let verification continue.
set -eo pipefail

REPO=lightninglabs
PROJECT=wavelength

RELEASE_URL=https://github.com/$REPO/$PROJECT/releases
API_URL=https://api.github.com/repos/$REPO/$PROJECT/releases
MANIFEST_SELECTOR=". | select(.name | test(\"manifest-v.*(\\\\.txt)$\")) | .name"
SIGNATURE_SELECTOR=". | select(.name | test(\"manifest-.*(\\\\.sig)$\")) | .name"
HEADER_GH_JSON="Accept: application/vnd.github.v3+json"
# NOTE: wavelength is bootstrapping this multi-signer verification
# system. Only a single trusted key is seeded below, so the minimum
# required signature count is 1 for now. As more wavelength
# maintainers add their key to scripts/keys/ and this list, raise
# MIN_REQUIRED_SIGNATURES accordingly (lnd itself requires 5-of-16).
MIN_REQUIRED_SIGNATURES=1

# All keys that can sign wavelength releases. The key must be added as a
# file to the keys directory, for example: scripts/keys/<username>.asc
# The username in the key file must match the username used for signing a
# manifest (manifest-<username>-v0.xx.yy.sig), otherwise the signature
# won't be counted.
# NOTE: Reviewers of this file must make sure that both the key IDs and
# usernames in the list below are unique!
KEYS=()
KEYS+=("A5B61896952D9FDA83BC054CDC42612E89237182 roasbeef")

TEMP_DIR=$(mktemp -d /tmp/wavelength-sig-verification-XXXXXX)
trap 'rm -rf "$TEMP_DIR"' EXIT

function check_command() {
  echo -n "Checking if $1 is installed... "
  if ! command -v "$1"; then
    echo "ERROR: $1 is not installed or not in PATH!"
    exit 1
  fi
}

function verify_version() {
  version_regex="^v[[:digit:]]+\.[[:digit:]]+\.[[:digit:]]"
  if [[ ! "$1" =~ $version_regex ]]; then
    echo "ERROR: Invalid expected version detected: $1"
    exit 1
  fi
  echo "Expected version for binaries: $1"
}

function import_keys() {
  # A trick to get the absolute directory where this script is located, no
  # matter how or from where it was called. We'll need it to locate the key
  # files which are located relative to this script.
  DIR="$(cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd)"

  # Import all the signing keys. We'll create a key ring for each user and use
  # that exact key ring when verifying a user's signature. That way we can make
  # sure one user cannot just upload multiple signatures to reach the 5/7
  # required sigs.
  for key in "${KEYS[@]}"; do
    KEY_ID=$(echo $key | cut -d' ' -f1)
    USERNAME=$(echo $key | cut -d' ' -f2)
    IMPORT_FILE="keys/$USERNAME.asc"
    KEY_FILE="$DIR/$IMPORT_FILE"
    KEYRING_UNTRUSTED="$USERNAME.pgp-untrusted"
    KEYRING_TRUSTED="$USERNAME.pgp"

    # Because a key file could contain multiple keys, we need to be careful. To
    # make sure we only import and use the key with the hard coded key ID of
    # this script, we first import the file into a temporary untrusted keyring
    # and then only export the specific key with the given ID into our final,
    # trusted keyring that we later use for verification. This is exactly what
    # https://github.com/Kixunil/sqck does but we didn't want to add another
    # binary dependency to this script so we re-implemented it in the following
    # few lines.
    echo ""
    echo "Importing key(s) from $KEY_FILE into temporary keyring $KEYRING_UNTRUSTED"
    gpg --homedir "$TEMP_DIR" --no-default-keyring --keyring "$KEYRING_UNTRUSTED" \
      --import < "$KEY_FILE"

    echo ""
    echo "Exporting key $KEY_ID from untrusted keyring to trusted keyring $KEYRING_TRUSTED"
    gpg --homedir "$TEMP_DIR" --no-default-keyring --keyring "$KEYRING_UNTRUSTED" \
      --export "$KEY_ID" | \
      gpg --homedir "$TEMP_DIR" --no-default-keyring --keyring "$KEYRING_TRUSTED" --import

  done
}

function verify_signatures() {
  # Download the JSON of the release itself. That'll contain the release ID we
  # need for the next call. Use the documented API endpoint for looking up a
  # release by tag rather than relying on the GitHub frontend returning JSON
  # for a content-negotiated release page.
  RELEASE_JSON=$(wget -q --header="$HEADER_GH_JSON" -O - "$API_URL/tags/$VERSION") || {
    echo "ERROR: Failed to download release JSON from $API_URL/tags/$VERSION"
    exit 1
  }

  TAG_NAME=$(echo "$RELEASE_JSON" | jq -r '.tag_name')
  RELEASE_ID=$(echo "$RELEASE_JSON" | jq -r '.id')
  echo "Release $TAG_NAME found with ID $RELEASE_ID"

  # Now download the asset list and filter by the manifest and the signatures.
  # Keep the raw JSON in a quoted variable and let jq do all the filtering:
  # expanding a variable holding JSON unquoted would subject it to word
  # splitting and glob expansion, corrupting the document.
  ASSETS_JSON=$(wget -q --header="$HEADER_GH_JSON" -O - "$API_URL/$RELEASE_ID") || {
    echo "ERROR: Failed to download asset list from $API_URL/$RELEASE_ID"
    exit 1
  }
  MANIFEST=$(echo "$ASSETS_JSON" | jq -r ".assets[] | $MANIFEST_SELECTOR")

  # We need to make sure we have unique signature file names. Otherwise someone
  # could just upload the same signature multiple times (if GH allows it for
  # some reason). Just adding the same files under different names also won't
  # work because we parse the signing user's name from the file. If a random
  # username is chosen then a signing key won't be found for it.
  SIGNATURES=$(echo "$ASSETS_JSON" | jq -r ".assets[] | $SIGNATURE_SELECTOR" | sort | uniq)

  # A release without a manifest asset can't be verified at all, so fail
  # with a clear error instead of invoking wget with an empty file name.
  if [[ -z "$MANIFEST" ]]; then
    echo "ERROR: No manifest file found in the release assets!"
    exit 1
  fi

  # Download the main "manifest-*.txt" and all "manifest-*.sig" files containing
  # the detached signatures.
  echo "Downloading $MANIFEST"
  wget -q -O "$TEMP_DIR/$MANIFEST" "$RELEASE_URL/download/$VERSION/$MANIFEST" || {
    echo "ERROR: Failed to download $MANIFEST from $RELEASE_URL/download/$VERSION/$MANIFEST"
    exit 1
  }

  for signature in $SIGNATURES; do
    echo "Downloading $signature"
    wget -q -O "$TEMP_DIR/$signature" "$RELEASE_URL/download/$VERSION/$signature" || {
      echo "ERROR: Failed to download $signature from $RELEASE_URL/download/$VERSION/$signature"
      exit 1
    }
  done

  echo ""

  # Before we even look at the content of the manifest, we first want to make sure
  # the signatures actually sign that exact manifest.
  NUM_CHECKS=0
  for signature in $SIGNATURES; do
    # Remove everything from the filename after the username. We start with
    # "manifest-USERNAME-v0.xx.yy.sig" and have "manifest-USERNAME" after
    # this step.
    USERNAME=${signature%-$VERSION.sig}

    # Remove the manifest- part before the username.
    USERNAME=${USERNAME##manifest-}

    # If the user is known, they should have a key ring file with only their key.
    KEYRING="$USERNAME.pgp"
    if [[ ! -f "$TEMP_DIR/$KEYRING" ]]; then
      echo "User $USERNAME does not have a known key, skipping"
      continue
    fi

    # We'll write the status of the verification to a special file that we can
    # then inspect.
    STATUS_FILE="$TEMP_DIR/$USERNAME.sign-status"

    # Make sure we haven't yet tried to verify a signature for that user.
    if [[ -f "$STATUS_FILE" ]]; then
      echo "ERROR: A signature for user $USERNAME was already verified!"
      echo "  Either file name $signature is wrong or multiple files of same "
      echo "  user were uploaded."
      exit 1
    fi

    # Run the actual verification.
    gpg --homedir "$TEMP_DIR" --no-default-keyring --keyring "$KEYRING" --status-fd=1 \
      --verify "$TEMP_DIR/$signature" "$TEMP_DIR/$MANIFEST" \
      > "$STATUS_FILE" 2>&1 || {
        echo "ERROR: Invalid signature $signature from user $USERNAME!"
        echo "  GPG output:"
        cat "$STATUS_FILE" | sed 's/^/    /'
        exit 1
      }

    echo "Verifying $signature of user $USERNAME against key ring $KEYRING"
    if grep -q "Good signature" "$STATUS_FILE"; then
      echo "Signature for $signature appears valid: "
      grep "VALIDSIG" "$STATUS_FILE"
    elif grep -q "No public key" "$STATUS_FILE"; then
      # Because we checked above if the user has a key, getting the "No public
      # key" error now means the key used for signing doesn't match the key we
      # have in our repo and is now a failure case.
      echo "ERROR: Unable to verify signature $signature, no key available"
      echo "  The signature $signature was signed with a different key than was"
      echo "  imported for user $USERNAME."
      exit 1
    else
      echo "ERROR: Did not get valid signature for $MANIFEST in $signature!"
      echo "  The developer signature $signature disagrees on the expected"
      echo "  release binaries in $MANIFEST. The release may have been faulty or"
      echo "  was backdoored."
      exit 1
    fi

    echo "Verified $signature against $MANIFEST"
    echo ""
    ((NUM_CHECKS=NUM_CHECKS+1))
  done

  # We want at least five signatures (out of seven public keys) that sign the
  # hashes of the binaries we have installed. If we arrive here without exiting,
  # it means no signature manifests were uploaded (yet) with the correct naming
  # pattern.
  if [[ $NUM_CHECKS -lt $MIN_REQUIRED_SIGNATURES ]]; then
    echo "ERROR: Not enough valid signatures found!"
    echo "  Valid signatures found: $NUM_CHECKS"
    echo "  Valid signatures required: $MIN_REQUIRED_SIGNATURES"
    echo
    echo "  Make sure the release $VERSION contains the required "
    echo "  number of signatures on the manifest, or wait until more "
    echo "  signatures have been added to the release."
    exit 1
  fi
}

function check_hash() {
  # Make this script compatible with both linux and *nix.
  SHA_CMD="sha256sum"
  if ! command -v "$SHA_CMD" > /dev/null; then
    if command -v "shasum"; then
      SHA_CMD="shasum -a 256"
    else
      echo "ERROR: no SHA256 sum binary installed!"
      exit 1
    fi
  fi
  SUM=$($SHA_CMD "$1" | cut -d' ' -f1)

  # Make sure the hash was actually calculated by looking at its length.
  if [[ ${#SUM} -ne 64 ]]; then
    echo "ERROR: Invalid hash for $2: $SUM!"
    exit 1
  fi

  echo "Verifying $1 as version $VERSION with SHA256 sum $SUM"

  # If we're inside the docker image, there should be a shasums.txt file in the
  # root directory. If that's the case, we first want to make sure we still have
  # the same hash as we did when building the image.
  if [[ -f /shasums.txt ]]; then
    if ! grep -q "$SUM" /shasums.txt; then
      echo "ERROR: Hash $SUM for $2 not found in /shasums.txt: "
      cat /shasums.txt
      exit 1
    fi
  fi

  if ! grep "^$SUM" "$TEMP_DIR/$MANIFEST" | grep -q "$VERSION"; then
    echo "ERROR: Hash $SUM for $2 not found in $MANIFEST: "
    cat "$TEMP_DIR/$MANIFEST"
    echo "  The expected release binaries have been verified with the developer "
    echo "  signatures. Your binary's hash does not match the expected release "
    echo "  binary hashes. Make sure you're using an official binary."
    exit 1
  fi
}

# By default we're picking up waved and wavecli from the system $PATH.
# The binaries not being installed is fine at this point (verifying a
# downloaded archive doesn't need them), so don't let set -e trip here.
WAVED_BIN=$(which waved || true)
WAVECLI_BIN=$(which wavecli || true)

if [[ $# -eq 0 ]]; then
  echo "ERROR: missing expected version!"
  echo "Usage: verify-install.sh expected-version [path-to-waved-binary-or-download-archive [path-to-wavecli-binary]]"
  exit 1
fi

# The first argument should be the expected version of the binaries.
VERSION=$1
shift

# Verify that the expected version is well-formed.
verify_version "$VERSION"

# Make sure we have all tools needed for the verification.
check_command wget
check_command jq
check_command gpg

# If exactly two parameters are specified, we expect the first one to be waved and
# the second one to be wavecli. One parameter is either just a single binary or a
# packaged release archive. No parameters means picking up waved and wavecli from
# the system path.
if [[ $# -eq 2 ]]; then
  WAVED_BIN=$(realpath $1)
  WAVECLI_BIN=$(realpath $2)

  # Make sure both files actually exist.
  if [[ ! -f $WAVED_BIN ]]; then
    echo "ERROR: $WAVED_BIN not found!"
    exit 1
  fi
  if [[ ! -f $WAVECLI_BIN ]]; then
    echo "ERROR: $WAVECLI_BIN not found!"
    exit 1
  fi

  # Make sure both binaries can be found and are executable.
  check_command "$WAVED_BIN"
  check_command "$WAVECLI_BIN"

elif [[ $# -eq 1 ]]; then
  # We're verifying a single binary or a packaged release archive.
  PACKAGE_BIN=$(realpath $1)

elif [[ $# -eq 0 ]]; then
  # By default we're picking up waved and wavecli from the system $PATH.
  WAVED_BIN=$(which waved)
  WAVECLI_BIN=$(which wavecli)

  # Make sure both binaries can be found and are executable.
  check_command "$WAVED_BIN"
  check_command "$WAVECLI_BIN"

else
  echo "ERROR: invalid number of parameters!"
  echo "Usage: verify-install.sh [waved-binary wavecli-binary]"
  exit 1
fi

# Import all the signing keys.
import_keys

echo ""

# Verify and count the signatures.
verify_signatures

# Then make sure that the hash of the installed binaries can be found in the
# manifest that we now have verified the signatures for.
if [[ "$PACKAGE_BIN" != "" ]]; then
  check_hash "$PACKAGE_BIN" "$PACKAGE_BIN"

  echo ""
  echo "SUCCESS! Verified $PACKAGE_BIN against $MANIFEST signed by $NUM_CHECKS developers."

else
  check_hash "$WAVED_BIN" "waved"
  check_hash "$WAVECLI_BIN" "wavecli"

  echo ""
  echo "SUCCESS! Verified waved and wavecli against $MANIFEST signed by $NUM_CHECKS developers."
fi
