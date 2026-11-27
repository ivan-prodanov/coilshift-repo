#!/bin/bash
# Direct Summon apply (late, after Direct Link; every boot, and in place on an update).
#   1. refuse while today's install-car.sh supervisor runs, or when Direct Link is another
#      build (a mismatched pair would fail closed anyway: camhelper refuses SUMMON mode);
#   2. mount the image, check summonhelper against lib/gateway.sh, bind it at
#      /run/direct-summon-bin, and check uid 1337 can run it;
#   3. first run only: /var/lib/airgapp-summon/state and the helper key;
#   4. the takeover (install-car.sh S0b), unattended: unless our summonhelper already runs,
#      stock webrtc-comms down, its orphaned minijail-init killed by its checked pidfile, then
#      up to about a minute for com.tesla.Webrtc to have no owner. If it stays taken: stock is
#      handed back, no Summon service is written, status degraded, exit 0 (the camera is fine);
#   5. summonhelper's runit scripts (install-car.sh's, binary path changed), bound at
#      /run/direct-summon-sv;
#   6. the marker, the status, a restart on an update.
# Exits 0, 11 (refused, nothing changed) or 1 (failed). bash 3.2.
set -eu
: "${CSDK_PACKAGE_DIR:?}" "${CSDK_RUN:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
. "$CSDK_PACKAGE_DIR/lib/gateway.sh"
. "$CSDK_PACKAGE_DIR/lib/common.sh"

refuse() { set_status failed "$1"; exit 11; }
fail()   { set_status failed "$1"; exit 1; }
notes=

# 1. refusals
if installer_live; then
    refuse "install-car.sh's own supervisor is running: migrate first (direct-link README)"
fi
# Direct Link applies at boot too, in no fixed order with us (no requires.packages: CoilShift's
# plan rejects an outside package's dependency on another as "unrequested or duplicate
# selections" (the likely cause of its 2026-11-27 install failure), so wait up to 2 minutes for its binaries to be bound.
i=0
while [ ! -s "$R$DL_BIN/BUILD" ] && [ "$i" -lt 60 ]; do sleep 2; i=$((i + 1)); done
[ -s "$R$DL_BIN/BUILD" ] || refuse "Direct Link is not installed or not applied: install it first"
dl_build=$(sed -n 's/^build //p' "$R$DL_BIN/BUILD" 2>/dev/null | head -n 1)
[ "$dl_build" = "$GW_BUILD" ] || refuse "Direct Link and Direct Summon are different builds (${dl_build:-none} and $GW_BUILD): update both"

# 2. binary
img=$CSDK_RUN/images/direct-summon
csdk_api PUT /images/direct-summon "{\"payload\":\"payload/direct-summon-$GW_BUILD.sqfs\"}" >/dev/null \
    || fail "the image did not mount"
[ "$(sha256_of "$img/summonhelper")" = "$SUMMONHELPER_SHA256" ] || fail "the image's summonhelper is not build $GW_BUILD"
csdk_api PUT /binds/direct-summon-bin "{\"source\":\"$img\",\"target\":\"$DS_BIN\"}" >/dev/null \
    || fail "cannot bind summonhelper at $DS_BIN"
# An update keeps the previous build's bind (CoilShift treats a PUT of the same name, source and target as
# done; seen on 1.0.3 -> 1.0.4): drop it and bind again with summonhelper down meanwhile (its finish aborts
# and disconnects; updates install only while parked), then check the bound binary. A reboot clears it too.
rebound=0
if [ -e "$R$DS_BIN/BUILD" ] && [ "$(sed -n 's/^build //p' "$R$DS_BIN/BUILD" 2>/dev/null | head -n 1)" != "$GW_BUILD" ]; then
    [ -d "$SVROOT/summonhelper" ] && { do_sv -w 15 down "$SVROOT/summonhelper" >/dev/null 2>&1 || true; }
    csdk_api DELETE /binds/direct-summon-bin >/dev/null 2>&1 \
        && csdk_api PUT /binds/direct-summon-bin "{\"source\":\"$img\",\"target\":\"$DS_BIN\"}" >/dev/null \
        || fail "the previous build's bind at $DS_BIN did not let go: reboot the car"
    rebound=1
fi
if [ -e "$R$SUMMON_BIN" ]; then
    [ "$(sha256_of "$R$SUMMON_BIN")" = "$SUMMONHELPER_SHA256" ] \
        || fail "the summonhelper at $DS_BIN is not build $GW_BUILD: reboot the car"
fi
do_chpst -u ":$SUMMON_UID:$SUMMON_UID" test -x "$SUMMON_BIN" || fail "uid $SUMMON_UID cannot run $SUMMON_BIN"

# 3. first run (install-car.sh steps 4b and 4c)
if [ ! -d "$R$SUMMON_STATE" ]; then
    mkdir -p "$R$SUMMON_STATE"
    as_root "$R$SUMMON_HOME"
    chmod 0755 "$R$SUMMON_HOME"
    owned_by "$SUMMON_UID:$SUMMON_UID" "$R$SUMMON_STATE"
    chmod 0700 "$R$SUMMON_STATE"
fi
if [ ! -e "$R$SUMMON_STATE/key.pem" ]; then
    do_chpst -u ":$SUMMON_UID:$SUMMON_UID" "$SUMMON_BIN" keygen -key "$SUMMON_STATE/key.pem" >/dev/null \
        || fail "summonhelper keygen failed"
fi

# 4. takeover. From here until the service is committed, any exit (a failure, the hook timeout)
#    gives the name back the installer's way (its exit_hand_back), so stock is never left down
#    with nothing of ours to take the name.
if [ -z "$(summon_pids)" ]; then
    trap 'rc=$?; [ "$rc" = 0 ] || give_back || true' EXIT
    trap 'exit 1' TERM INT HUP
    if ! take_name; then
        give_back || notes="a summonhelper process lingers; stock webrtc-comms left alone; "
        trap - EXIT TERM INT HUP
        finish_apply degraded "${notes}Summon skipped: com.tesla.Webrtc stayed taken"
        exit 0
    fi
fi

# 5. service
pin=$(authd_pin)
[ -n "$pin" ] || notes="unknown authd build: Summon sign-in stays closed; run the sign-in pre-check, then a new package version adds it"
gen_summon_scripts "$pin"
write_service summonhelper "$SUMMON_RUN" "$SUMMON_FINISH" "$SUMMON_LOG" "Direct Summon"
csdk_api PUT /binds/direct-summon-sv "{\"source\":\"$SVROOT\",\"target\":\"$DS_SV\"}" >/dev/null \
    || notes="${notes:+$notes; }no /run/direct-summon-sv bind (use $SVROOT)"
restart_changed summonhelper
# Back up after a rebind (restart_changed only restarts what is up).
if [ "$rebound" = 1 ] && [ -d "$SVROOT/summonhelper" ]; then
    do_sv up "$SVROOT/summonhelper" >/dev/null 2>&1 || true
fi
trap - EXIT TERM INT HUP

# 6. marker, status
if [ -n "$notes" ]; then finish_apply degraded "$notes"; else finish_apply ok ""; fi
exit 0
