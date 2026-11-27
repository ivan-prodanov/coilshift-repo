#!/bin/sh
# apply-all.sh - re-apply every tesla-fw-patch in one shot. Run ON THE CAR, as root.
#
# WHEN TO RUN IT. The patches are cleared by two events, neither of which is a normal
# park/sleep (the MCU suspends to RAM - /sys/power/state = freeze/mem - it does NOT
# reboot; uptime keeps counting, /run and the UI survive):
#   * QtCar UI restart  -> clears the /proc/<QtCar>/mem byte patches (fart/boombox/horn)
#   * true MCU reboot    -> clears the /run binds (netflix CDM) AND restarts the UI (so a
#                           reboot clears everything; covering it covers both cases)
# True reboots are rare (OTA, crash/watchdog reset, scroll-wheel reset, 12V loss). So you
# typically only need this after an OTA or a UI crash. Every feature script is idempotent
# (checks its own state, readback-verified), so running this any time is safe / cheap.
#
# NOT a boot hook. There is no writable boot seam wired here (the rootfs /etc/sv is
# read-only). Trigger this yourself after a reboot/OTA, or hook it into whatever boot
# mechanism you add later. CoilShift's own mods re-apply via the ssp daemon; a feature
# run from its .sh doesn't get that, which is what this script stands in for. A feature
# installed as a package from a repository (tools/build.sh, see README) re-applies
# itself; drop it from PATCHES below so it isn't applied twice.
#
# STAGING. This expects the repo's feature folders present under $ROOT on the car (their
# *.sh), plus the netflix CDM payload at /opt/games/usr/local/netflix-fix/widevine-3050.sqfs.
# $ROOT defaults to this script's own directory. One-time sync from the Mac (sshd here has
# no sftp, so tar over ssh):
#   tar czf - -C <this repo> . \
#     | ssh -p 13400 root@<car> 'mkdir -p /opt/games/usr/local/tesla-fw-patches \
#        && tar xzf - -C /opt/games/usr/local/tesla-fw-patches'
# (that also carries the netflix payload; then once:
#   cp /opt/games/usr/local/tesla-fw-patches/netflix-cdm/payloads/widevine-3050.sqfs \
#      /opt/games/usr/local/netflix-fix/widevine-3050.sqfs   -- it persists on /opt/games/usr.)
set -u
ROOT=${ROOT:-$(CDPATH= cd "$(dirname "$0")" && pwd)}
export PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin:$PATH
[ "$(id -u)" = 0 ] || { echo "apply-all: must run as root on the car" >&2; exit 1; }

# Feature apply scripts, in order. Edit this list to enable/disable patches.
# (reverse-locks is abandoned -> intentionally omitted.)
PATCHES="
fart-in-drive/fart-in-drive.sh
boombox-in-drive/boombox-in-drive.sh
horn-in-drive/horn-in-drive.sh
grok/grok-eu.sh
netflix-cdm/netflix-cdm.sh
lock-chime-random/lock-chime-random.sh
"
# Note: lock-chime-random starts a persistent daemon (random lock chime from a USB folder). Unlike
# the byte patches it survives a UI restart and sleep; it only dies on a true reboot - which is
# exactly when apply-all runs - and its script is idempotent (restarts the daemon, no duplicate).

echo "apply-all @ $(date) | uptime $(cut -d. -f1 /proc/uptime 2>/dev/null)s | root=$ROOT"
ok=0; fail=0; skip=0
for rel in $PATCHES; do
    f="$ROOT/$rel"
    if [ ! -f "$f" ]; then echo "[skip] $rel (not staged under \$ROOT)"; skip=$((skip + 1)); continue; fi
    echo "--- $rel ---"
    out=$(sh "$f" apply 2>&1); rc=$?            # fart/boombox/horn take 'apply'; grok/netflix ignore the arg
    printf '%s\n' "$out" | sed 's/^/    /'
    if [ "$rc" = 0 ]; then ok=$((ok + 1)); else echo "    [FAILED rc=$rc]"; fail=$((fail + 1)); fi
done
echo "apply-all: $ok applied, $fail failed, $skip skipped"
[ "$fail" = 0 ]
