#!/usr/bin/env bash
#
# gen_bindings.sh builds the gomobile bindings for sdk/wavewalletdk/mobile.
#
# It produces an Android .aar and/or an iOS .xcframework from the gomobile-safe
# facade. The embedded daemon (btcwallet / neutrino / lnd deps) is cross
# compiled via cgo using the Android NDK, so ANDROID_HOME and a modern JDK must
# be available.
#
# Usage:
#   ./gen_bindings.sh android   # build the Android .aar (default)
#   ./gen_bindings.sh ios       # build the iOS .xcframework (macOS + Xcode)
#   ./gen_bindings.sh all       # build both

set -euo pipefail

# The gomobile-safe package and the build tags it requires. The mobile tag is
# what gates the facade; wavewalletrpc + swapruntime pull in the embedded wallet
# RPC runtime (both are required by wavewalletdk.requireEmbeddedWalletRuntime).
MOBILE_PKG="github.com/lightninglabs/wavelength/sdk/wavewalletdk/mobile"
MOBILE_TAGS="mobile wavewalletrpc swapruntime"

# Minimum Android API level. 21 (Lollipop) matches lnd-mobile.
ANDROID_API=21

# Newer Android devices use 16KB memory pages; link with a matching max page
# size so the .so loads on them. Mirrors lnd-mobile's ANDROID_EXTLDFLAGS.
ANDROID_MAX_PAGE_SIZE=16384

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_DIR="${SCRIPT_DIR}/build"

# Resolve the Android SDK. The android CLI defaults to ~/Library/Android/sdk on
# macOS; honor an explicit ANDROID_HOME / ANDROID_SDK_ROOT if set.
ANDROID_HOME="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-${HOME}/Library/Android/sdk}}"
export ANDROID_HOME
export ANDROID_SDK_ROOT="${ANDROID_HOME}"

# gomobile finds the NDK under ${ANDROID_HOME}/ndk/<version>; pick the newest
# installed one unless ANDROID_NDK_HOME is already set.
if [[ -z "${ANDROID_NDK_HOME:-}" && -d "${ANDROID_HOME}/ndk" ]]; then
	# Use find rather than a glob: under `set -euo pipefail` an empty ndk
	# directory would make `ls -d .../*` exit non-zero and abort the script.
	ndk_dir="$(
		find "${ANDROID_HOME}/ndk" -mindepth 1 -maxdepth 1 -type d \
			2>/dev/null | sort -V | tail -1
	)"
	if [[ -n "${ndk_dir}" ]]; then
		export ANDROID_NDK_HOME="${ndk_dir}"
	fi
fi

build_android() {
	echo "==> building Android .aar -> ${BUILD_DIR}/android/Wavewalletdk.aar"
	mkdir -p "${BUILD_DIR}/android"
	gomobile bind \
		-target=android \
		-androidapi "${ANDROID_API}" \
		-javapkg=engineering.lightning.wavewalletdk \
		-tags="${MOBILE_TAGS}" \
		-ldflags="-extldflags '-Wl,-z,max-page-size=${ANDROID_MAX_PAGE_SIZE}'" \
		-v \
		-o "${BUILD_DIR}/android/Wavewalletdk.aar" \
		"${MOBILE_PKG}"
}

build_ios() {
	echo "==> building iOS .xcframework -> ${BUILD_DIR}/ios/Wavewalletdk.xcframework"
	mkdir -p "${BUILD_DIR}/ios"
	gomobile bind \
		-target=ios,iossimulator \
		-tags="${MOBILE_TAGS}" \
		-v \
		-o "${BUILD_DIR}/ios/Wavewalletdk.xcframework" \
		"${MOBILE_PKG}"
}

target="${1:-android}"
case "${target}" in
android) build_android ;;
ios) build_ios ;;
all)
	build_android
	build_ios
	;;
*)
	echo "unknown target: ${target} (want android|ios|all)" >&2
	exit 1
	;;
esac

echo "==> done"
