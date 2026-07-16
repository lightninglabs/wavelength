#!/bin/bash

# Simple bash script to build waved and wavecli for all the platforms we
# support with the golang cross-compiler, producing reproducible,
# independently-verifiable release archives.
#
# Ported from lightningnetwork/lnd's scripts/release.sh.

set -e

WAVED_VERSION_REGEX="waved version (.+)"
PKG="github.com/lightninglabs/wavelength"
PACKAGE=wavelength

# Needed for setting file timestamps to get reproducible archives.
BUILD_DATE="2020-01-01 00:00:00"
BUILD_DATE_STAMP="202001010000.00"

# reproducible_tar_gzip creates a reproducible tar.gz file of a directory.
# This includes setting all file timestamps and ownership settings
# uniformly.
function reproducible_tar_gzip() {
  local dir=$1
  local tar_cmd=tar
  local gzip_cmd=gzip

  # MacOS has a version of BSD tar which doesn't support setting the --mtime
  # flag. We need gnu-tar, or gtar for short to be installed for this script
  # to work properly.
  tar_version=$(tar --version)
  if [[ ! "$tar_version" =~ "GNU tar" ]]; then
    if ! command -v "gtar"; then
      echo "GNU tar is required but cannot be found!"
      echo "On MacOS please run 'brew install gnu-tar' to install gtar."
      exit 1
    fi

    # We have gtar installed, use that instead.
    tar_cmd=gtar
  fi

  # On MacOS, the default BSD gzip produces a different output than the GNU
  # gzip on Linux. To ensure reproducible builds, we need to use GNU gzip.
  gzip_version=$(gzip --version 2>&1 || true)
  if [[ ! "$gzip_version" =~ "GNU" ]]; then
    if ! command -v "ggzip" >/dev/null 2>&1; then
      echo "GNU gzip is required but cannot be found!"
      echo "On MacOS please run 'brew install gzip' to install ggzip."
      exit 1
    fi

    # We have ggzip installed, use that instead.
    gzip_cmd=ggzip
  fi

  # Pin down the timestamp time zone.
  export TZ=UTC

  # Go project file names never contain spaces or newlines, so a plain
  # newline-separated list is safe here and avoids the GNU-only sort -z
  # flag (BSD sort on macOS doesn't support it).
  find "${dir}" | LC_ALL=C sort -r | $tar_cmd \
    "--mtime=${BUILD_DATE}" --no-recursion --mode=u+rw,go+r-w,a+X \
    --owner=0 --group=0 --numeric-owner -c -T - | $gzip_cmd \
    -9n > "${dir}.tar.gz"

  rm -r "${dir}"
}

# reproducible_zip creates a reproducible zip file of a directory. This
# includes setting all file timestamps.
function reproducible_zip() {
  local dir=$1

  # Pin down file name encoding and timestamp time zone.
  export TZ=UTC

  # Set the date of each file in the directory that's about to be packaged
  # to the same timestamp and make sure the same permissions are used
  # everywhere.
  chmod -R 0755 "${dir}"
  touch -t "${BUILD_DATE_STAMP}" "${dir}"
  # xargs -r is a GNU extension that BSD/macOS xargs doesn't support, so
  # use find -exec instead, which is fully portable.
  find "${dir}" -exec touch -t "${BUILD_DATE_STAMP}" {} +

  find "${dir}" | LC_ALL=C sort -r | zip -o -X -r -@ "${dir}.zip"

  rm -r "${dir}"
}

# green prints one line of green text (if the terminal supports it).
function green() {
  echo -e "\e[0;32m${1}\e[0m"
}

# red prints one line of red text (if the terminal supports it).
function red() {
  echo -e "\e[0;31m${1}\e[0m"
}

# check_tag_correct makes sure the given git tag is checked out and the git
# tree is not dirty.
#   arguments: <version-tag>
function check_tag_correct() {
  local tag=$1

  # For automated builds we can skip this check as they will only be
  # triggered on tags.
  if [[ "$SKIP_VERSION_CHECK" -eq "1" ]]; then
    green "skipping version check, assuming automated build"
    exit 0
  fi

  # If a tag is specified, ensure that that tag is present and checked
  # out. The --dirty flag appends a marker when the working tree has
  # local modifications, so a dirty checkout never matches the tag: we
  # must not sign a manifest whose binaries were built from sources that
  # differ from the tagged (and archived) tree.
  if [[ $tag != $(git describe --tags --dirty) ]]; then
    red "tag $tag not checked out (or working tree dirty)"
    exit 1
  fi

  # Build waved to extract version. Vendoring is workspace-incompatible,
  # so disable go.work for the duration of the release build. The output
  # name must be set explicitly: the default output name "waved" collides
  # with the waved/ package directory at the repo root.
  export GOWORK=off
  version_check_bin=$(mktemp -u /tmp/waved-version-check-XXXXXX)
  go build -o "$version_check_bin" "${PKG}/cmd/waved"

  # Extract version command output.
  waved_version_output=$("$version_check_bin" --version)
  rm -f "$version_check_bin"

  # Use a regex to isolate the version string.
  if [[ $waved_version_output =~ $WAVED_VERSION_REGEX ]]; then
    # Prepend 'v' to match git tag naming scheme.
    waved_version="v${BASH_REMATCH[1]}"
    green "version: $waved_version"

    # Match git tag with waved version.
    if [[ $tag != "${waved_version}" ]]; then
      red "waved version $waved_version does not match tag $tag"
      exit 1
    fi
  else
    red "malformed waved version output"
    exit 1
  fi
}

# build_release builds the actual release binaries.
#   arguments: <version-tag> <build-system(s)> <build-tags> <ldflags>
#              <go-version>
function build_release() {
  local tag=$1
  local sys=$2
  local buildtags=$3
  local ldflags=$4
  local goversion=$5

  # Vendoring and building against a vendored tree are not supported under
  # go.work workspace mode, so it's disabled for the whole release build.
  export GOWORK=off

  # Check if the active Go version matches the specified Go version.
  active_go_version=$(go version | awk '{print $3}' | sed 's/go//')
  if [ "$active_go_version" != "$goversion" ]; then
    echo "Error: active Go version ($active_go_version) does not match \
required Go version ($goversion)."
    exit 1
  fi

  echo "Building release for tag $tag with Go version $goversion"

  green " - Packaging vendor"
  go mod vendor
  reproducible_tar_gzip vendor

  # Start from a clean output directory. A retried build must never
  # append to a previous attempt's manifest or reuse its archives.
  maindir=$PACKAGE-$tag
  rm -rf "$maindir"
  mkdir -p "$maindir"
  mv vendor.tar.gz "${maindir}/"

  # Don't use tag in source directory, otherwise our file names get too
  # long and tar starts to package them non-deterministically.
  package_source="${PACKAGE}-source"

  # The git archive command doesn't support setting timestamps and file
  # permissions. That's why we unpack the tar again, then use our
  # reproducible method to create the final archive.
  git archive -o "${maindir}/${package_source}.tar" HEAD

  cd "${maindir}"
  mkdir -p ${package_source}
  tar -xf "${package_source}.tar" -C ${package_source}
  rm "${package_source}.tar"
  reproducible_tar_gzip ${package_source}
  mv "${package_source}.tar.gz" "${package_source}-$tag.tar.gz"

  for i in $sys; do
    os=$(echo $i | cut -f1 -d-)
    arch=$(echo $i | cut -f2 -d-)
    arm=

    if [[ $arch == "armv6" ]]; then
      arch=arm
      arm=6
    elif [[ $arch == "armv7" ]]; then
      arch=arm
      arm=7
    fi

    dir="${PACKAGE}-${i}-${tag}"
    mkdir "${dir}"
    pushd "${dir}"

    green " - Building: ${os} ${arch} ${arm} with build tags '${buildtags}'"
    env CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=$arm go build -v -trimpath -ldflags="${ldflags}" -tags="${buildtags}" ${PKG}/cmd/waved
    env CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=$arm go build -v -trimpath -ldflags="${ldflags}" -tags="${buildtags}" ${PKG}/cmd/wavecli
    popd

    # Clear Go build cache to prevent disk space issues during
    # multi-platform builds.
    go clean -cache

    # Add the hashes for the individual binaries as well for easy
    # verification of a single installed binary.
    shasum -a 256 "${dir}/"* >> "manifest-$tag.txt"

    if [[ $os == "windows" ]]; then
      reproducible_zip "${dir}"
    else
      reproducible_tar_gzip "${dir}"
    fi
  done

  # Add the hash of the packages too, then sort by the second column
  # (name).
  shasum -a 256 wavelength-* vendor* >> "manifest-$tag.txt"
  LC_ALL=C sort -k2 -o "manifest-$tag.txt" "manifest-$tag.txt"
  cat "manifest-$tag.txt"
}

# usage prints the usage of the whole script.
function usage() {
  red "Usage: "
  red "release.sh check-tag <version-tag>"
  red "release.sh build-release <version-tag> <build-system(s)> <build-tags> <ldflags> <go-version>"
}

# Whatever sub command is passed in, we need at least 2 arguments.
if [ "$#" -lt 2 ]; then
  usage
  exit 1
fi

# Extract the sub command and remove it from the list of parameters by
# shifting them to the left.
SUBCOMMAND=$1
shift

# Call the function corresponding to the specified sub command or print the
# usage if the sub command was not found.
case $SUBCOMMAND in
check-tag)
  green "Checking if version tag exists"
  check_tag_correct "$@"
  ;;
build-release)
  green "Building release"
  build_release "$@"
  ;;
*)
  usage
  exit 1
  ;;
esac
