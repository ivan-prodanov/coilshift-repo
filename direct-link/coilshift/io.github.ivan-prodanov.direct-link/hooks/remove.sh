#!/bin/bash
# Direct Link remove. CoilShift has already stopped our service (bin/supervise.sh took agw and
# camhelper down, so their finish scripts ran) and removed Direct Summon, which depends on us.
# Then: make sure no agw runs, remove the egress lock. Keys, direct-link.conf and
# logs stay. CoilShift's ledger then undoes the binds and the image. A failure stops the
# removal, which CoilShift retries. bash 3.2.
set -eu
: "${CSDK_PACKAGE_DIR:?}" "${CSDK_RUN:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
. "$CSDK_PACKAGE_DIR/lib/gateway.sh"
. "$CSDK_PACKAGE_DIR/lib/common.sh"

for svc in camhelper agw; do
    [ -e "$SVROOT/$svc/supervise/ok" ] && { do_sv -w 15 down "$SVROOT/$svc" || true; }
done
i=0
while [ -n "$(running_pids "$AGW_BIN ")$(running_pids "$HELPER_BIN ")" ]; do
    [ "$i" -lt 10 ] || { set_status failed "agw or camhelper still runs; removal retried"; exit 1; }
    i=$((i + 1))
    sleep 1
done
egress_unlock "$IPT"
egress_unlock "$IP6T"
rm -f "$MARKER"
exit 0
