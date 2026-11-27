#!/bin/bash
# harness-seed.sh ROOT KIND: a fake root for Direct Summon, with Direct Link's binary bind and
# its BUILD in place (tools/build.sh runs each kind with apply,apply and apply,remove). The
# fakes and the car seed are Direct Link's (direct-link/test).
#   migrated   stock webrtc-comms owns com.tesla.Webrtc: taken over, handed back on removal
#   taken      the name stays taken after stock is down: Summon skipped, stock handed back,
#              status degraded, exit 0 (about 10 s per apply)
#   mismatch   Direct Link is another build: refused (11), nothing changed
#   installer  install-car.sh's own runsvdir still runs: refused (11), nothing changed
set -euo pipefail
case "${1:-}" in
--list) printf 'migrated pass\ntaken pass\nmismatch exit-11\ninstaller exit-11\n'; exit 0 ;;
--image-tree) echo yes; exit 0 ;;
esac
root=${1:?root} kind=${2:?kind}
here=$(cd "$(dirname "$0")" && pwd)
. "$here/../direct-link/test/seed-common.sh"
seed_car "$root"
build=$(sed -n 's/^GW_BUILD=//p' "$here/coilshift/io.github.ivan-prodanov.direct-summon/lib/gateway.sh")
mkdir -p "$root/run/direct-link-bin"
echo "build $build" >"$root/run/direct-link-bin/BUILD"
case "$kind" in
migrated) ;;
taken) : >"$root/.fakes/state/sticky" ;;
mismatch) echo "build 0000000" >"$root/run/direct-link-bin/BUILD" ;;
installer) fake_installer_runsvdir "$root" ;;
*) echo "unknown kind: $kind" >&2; exit 2 ;;
esac
