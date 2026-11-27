#!/bin/bash
# Direct Link apply (late, every boot, and in place on an update): what
# `install-car.sh --after-boot` did by hand, unattended (direct-link/DESIGN.md).
#   1. refuse while today's install-car.sh supervisor runs (migrate first), and a bad
#      direct-link.conf, before anything changes;
#   2. mount the image, check agw and camhelper against lib/gateway.sh, bind them at
#      /run/direct-link-bin, and check uid 4950 can run agw there;
#   3. first run only: the state and key a fresh car lacks (nothing that exists is
#      touched);
#   4. the uid 4950 egress lock (IPv4: TCP to PI_IP:17101 on wlan0, then DROP; IPv6: DROP),
#      jumped to first in OUTPUT; without it no agw service is written;
#   5. agw's and camhelper's runit scripts (install-car.sh's, binary paths changed), linked
#      into our svdir, bound at /run/direct-link-sv;
#   6. the boot marker bin/supervise.sh waits for, the status, and a restart on an update.
# Exits 0 (ok or degraded), 11 (refused, nothing changed) or 1 (failed). bash 3.2.
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
# direct-link.conf: made once from today's installed scripts (or the defaults), then the
# owner's to edit over SSH. It is never a CoilShift setting: nothing on the drive path is.
if [ ! -f "$R$CONF" ]; then
    pi=$(sed -n 's/.* -dial "\([0-9.]*\):[0-9]*".*/\1/p' "$R$GW_DIR/sv/agw/run" 2>/dev/null | head -n 1)
    wr=$(sed -n 's/.* -writer \([a-z-]*\).*/\1/p' "$R$GW_DIR/sv/camhelper/run" 2>/dev/null | head -n 1)
    mkdir -p "$R$GW_DIR"
    printf '# Direct Link: edit over SSH, then re-apply (reboot, or update the package).\nPI_IP=%s\nHELPER_WRITER=%s\n' \
        "${pi:-192.168.4.1}" "${wr:-pw-cat}" >"$R$CONF"
    as_root "$R$CONF"
    chmod 0644 "$R$CONF"
fi
PI_IP=$(conf_get PI_IP)
WRITER=$(conf_get HELPER_WRITER)
why=$(check_ip "$PI_IP") || fail "direct-link.conf PI_IP: ${why:-missing}"
case "$WRITER" in pw-cat|aplay) ;; *) fail "direct-link.conf HELPER_WRITER must be pw-cat or aplay" ;; esac

# 2. binaries
img=$CSDK_RUN/images/direct-link
csdk_api PUT /images/direct-link "{\"payload\":\"payload/direct-link-$GW_BUILD.sqfs\"}" >/dev/null \
    || fail "the image did not mount"
[ "$(sha256_of "$img/agw")" = "$AGW_SHA256" ] && [ "$(sha256_of "$img/camhelper")" = "$CAMHELPER_SHA256" ] \
    || fail "the image's binaries are not build $GW_BUILD"
csdk_api PUT /binds/direct-link-bin "{\"source\":\"$img\",\"target\":\"$DL_BIN\"}" >/dev/null \
    || fail "cannot bind the binaries at $DL_BIN"
do_chpst -u "$RUN_AS" test -x "$AGW_BIN" || fail "uid $UID_GW cannot run $AGW_BIN"

# 3. first run (install-car.sh steps 1, 3 and 4)
if [ ! -d "$R$GW_STATE" ]; then
    mkdir -p "$R$GW_STATE"
    as_root "$R$GW_DIR"
    chmod 0755 "$R$GW_DIR"
    owned_by "$UID_GW:$GID_GW" "$R$GW_STATE"
    chmod 0700 "$R$GW_STATE"
fi
if [ ! -e "$R$GW_STATE/key.pem" ]; then
    do_chpst -u "$RUN_AS" "$AGW_BIN" keygen -key "$GW_STATE/key.pem" >/dev/null || fail "agw keygen failed"
fi
mkdir -p "$R$HELPER_DIR"

# 4. egress lock
egress_lock "$IPT" "$(egress_accept_rule "$PI_IP")" >/dev/null 2>&1 \
    && egress_lock "$IP6T" >/dev/null 2>&1 \
    || fail "the uid $UID_GW egress lock did not check out; agw not started"

# The 2026-11-20 /etc/passwd account overlay is only reported (it is a mount: gone at reboot).
if awk '$2 == "/etc/passwd" {f = 1} END {exit !f}' "$PROC/self/mounts" 2>/dev/null \
    && [ "$(tail -n 1 "$R/etc/passwd" 2>/dev/null)" = "airgapp-gw:x:$UID_GW:$GID_GW::/nonexistent:/bin/false" ]; then
    notes="the old airgapp-gw /etc/passwd overlay is mounted: reboot the car"
fi

# 5. services
pin=$(authd_pin)
[ -n "$pin" ] || notes="${notes:+$notes; }unknown authd build: camera sign-in stays closed; run the sign-in pre-check, then a new package version adds it"
gen_agw_scripts "$PI_IP"
gen_helper_scripts "$WRITER" "$pin"
write_service agw "$AGW_RUN" "$AGW_FINISH" "$AGW_LOG" "Direct Link"
write_service camhelper "$HELPER_RUN" "$HELPER_FINISH" "$HELPER_LOG" "Direct Link"
csdk_api PUT /binds/direct-link-sv "{\"source\":\"$SVROOT\",\"target\":\"$DL_SV\"}" >/dev/null \
    || notes="${notes:+$notes; }no /run/direct-link-sv bind (use $SVROOT)"
restart_changed camhelper agw

# 6. marker, status
if [ -n "$notes" ]; then finish_apply degraded "$notes"; else finish_apply ok ""; fi
exit 0
