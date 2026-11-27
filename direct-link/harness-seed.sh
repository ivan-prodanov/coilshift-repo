#!/bin/bash
# harness-seed.sh ROOT KIND: a fake root for Direct Link (tools/build.sh runs each kind of
# `--list` with apply,apply and apply,remove). Every tool the hooks call is a fake from
# test/fakes, copied into ROOT/.fakes.
#   migrated   a car today's install-car.sh set up and the owner migrated: must apply cleanly
#   newauthd   the same with an authd no pre-check proved: applies, status degraded
#   installer  install-car.sh's own runsvdir still runs: refused (11), nothing changed
#   badconf    direct-link.conf names a vehicle-LAN Pi address (R22): fails (1), nothing changed
set -euo pipefail
case "${1:-}" in
--list) printf 'migrated pass\nnewauthd pass\ninstaller exit-11\nbadconf exit-1\n'; exit 0 ;;
--image-tree) echo yes; exit 0 ;;
esac
root=${1:?root} kind=${2:?kind}
. "$(dirname "$0")/test/seed-common.sh"
seed_car "$root"
case "$kind" in
migrated) ;;
newauthd) printf 'not an authd any pre-check proved\n' >"$root/usr/bin/authd" ;;
installer) fake_installer_runsvdir "$root" ;;
badconf) printf 'PI_IP=192.168.90.30\nHELPER_WRITER=pw-cat\n' >"$root/var/lib/airgapp-gw/direct-link.conf" ;;
*) echo "unknown kind: $kind" >&2; exit 2 ;;
esac
