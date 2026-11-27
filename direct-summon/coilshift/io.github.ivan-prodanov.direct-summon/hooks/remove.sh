#!/bin/bash
# Direct Summon remove: today's SUMMON=0. CoilShift has already stopped our service
# (bin/supervise.sh took summonhelper down, so its finish sent cmd_abort and the disconnect).
#   1. summonhelper down, if its runsv still answers (its finish runs);
#   2. up to 10 s until no summonhelper process runs; if one lingers, fail and leave stock alone
#      (it may still hold the name), and CoilShift retries the removal;
#   3. remove a stale live file;
#   4. hand com.tesla.Webrtc back to stock webrtc-comms (install-car.sh's hand_back).
# /var/lib/airgapp-summon/state (the key) stays. Needs nothing from Direct Link. bash 3.2.
set -eu
: "${CSDK_PACKAGE_DIR:?}" "${CSDK_RUN:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
. "$CSDK_PACKAGE_DIR/lib/gateway.sh"
. "$CSDK_PACKAGE_DIR/lib/common.sh"

# install-car.sh's stock_hand_back order: unlink, down (finish runs), gone, then stock.
rm -f "$SVDIR/summonhelper"
if [ -e "$SVROOT/summonhelper/supervise/ok" ]; then do_sv -w 15 down "$SVROOT/summonhelper" || true; fi
summon_gone || { set_status failed "a summonhelper process still runs (pid $(summon_pids)); removal retried"; exit 1; }
rm -f "$R$SUMMON_RUN_DIR/live" "$R$SUMMON_RUN_DIR"/.live.*
if ! name_owned; then
    hand_back || { set_status failed "stock webrtc-comms did not take com.tesla.Webrtc back; removal retried"; exit 1; }
fi
rm -f "$MARKER"
exit 0
