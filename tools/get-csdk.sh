#!/bin/bash
# Fetches the pinned csdk + csdk-harness release for this machine into tools/bin/
# and checks them against the release's SHA256SUMS. csdk finds csdk-harness next
# to itself, so both land in the same directory. Needs `gh` with read access to
# the private mLupine/coilshift-zero-sdk repository.
#
#   tools/get-csdk.sh            # installs CSDK_VERSION (below) for this OS/arch
#   CSDK_VERSION=0.1.3 tools/get-csdk.sh
set -euo pipefail

CSDK_VERSION=${CSDK_VERSION:-0.1.2}
REPO=mLupine/coilshift-zero-sdk
here=$(cd "$(dirname "$0")" && pwd)
bin="$here/bin"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64)  arch=amd64 ;;
    *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

stage=$(mktemp -d "${TMPDIR:-/tmp}/csdk-dl.XXXXXX")
trap 'rm -rf "$stage"' EXIT

gh release download "v$CSDK_VERSION" --repo "$REPO" --dir "$stage" \
    --pattern "csdk-$os-$arch" --pattern "csdk-harness-$os-$arch" --pattern SHA256SUMS
(cd "$stage" && shasum -a 256 -c SHA256SUMS --ignore-missing)

mkdir -p "$bin"
install -m 0755 "$stage/csdk-$os-$arch" "$bin/csdk"
install -m 0755 "$stage/csdk-harness-$os-$arch" "$bin/csdk-harness"
echo "$CSDK_VERSION" > "$bin/VERSION"
echo "csdk $CSDK_VERSION installed in $bin"
