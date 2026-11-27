#!/bin/bash
# Netflix CDM fix, removal. The daemon runs this hook BEFORE it reverts the ledger, so
# the overlays, the bind and the image are taken down here in reverse order (the
# revert that follows then has nothing left to undo), and only then are the Netflix
# procs dropped, so the fresh instance launches on the stock CDM 4.10.2934.0 and the
# stock TESLA_ UA. Always exits 0. bash 3.2 (the car's).
set -eu
: "${CSDK_PACKAGE_DIR:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
R=${CSDK_FAKE_ROOT:-}

for route in /overlays/sandbox-vars /overlays/netflix-tile /binds/netflix-cdm /images/widevine; do
    csdk_api DELETE "$route" >/dev/null || csdk_log warn "netflix-cdm: DELETE $route failed; the ledger revert retries it" || true
done

if [ -z "$R" ]; then
    for d in /proc/[0-9]*; do
        [ -r "$d/cmdline" ] || continue
        cmd=$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null) || continue
        case "$cmd" in *dev-coilshift-theater-webapps.netflix*) kill -9 "${d#/proc/}" 2>/dev/null || true ;; esac
    done
fi

csdk_log info "netflix-cdm removed; Netflix sandbox back to stock CDM + TESLA_ UA" || true
exit 0
