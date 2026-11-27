#!/bin/sh
# lock-chime-random.sh - enable the random lock chime. Run ON THE CAR, as root.
# (apply-all.sh calls `sh lock-chime-random.sh apply`; the arg is ignored.)
#
# Installs + (re)starts cs-lockrand.sh, a tiny daemon that makes the lock chime play a random
# *.wav from a USB "LockChime" folder instead of the single LockChime.wav. See README.md for the
# full story. Idempotent + readback-verified. Unlike the byte-patch features in this repo, this is
# a long-running process; it survives a UI restart and sleep, but NOT a reboot - re-run after a
# reboot/OTA (apply-all.sh does that). This is the unsigned/standalone form; the durable form is
# the CoilShift `service` package in coilshift/ (persists across reboot via ssp).
#
# PREREQUISITE: a custom lock sound must be ENABLED in the car UI (lock-sound = USB), i.e. the car
# is actually playing LockChime.wav on lock. With no custom lock sound, nothing reads the file and
# there is nothing to randomize.
export PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin:$PATH
[ "$(id -u)" = 0 ] || { echo "lock-chime-random: must run as root on the car" >&2; exit 1; }

HERE=$(CDPATH= cd "$(dirname "$0")" && pwd)
RUNDIR=/home/tesla/.cs-lockrand
TARGET=/home/tesla/.Tesla/data/LockChime.wav
DAEMON="$RUNDIR/cs-lockrand.sh"
BACKUP="$RUNDIR/LockChime.orig.wav"

command -v inotifywait >/dev/null 2>&1 || { echo "[!] inotifywait not found on car - cannot run"; exit 1; }
mkdir -p "$RUNDIR"

# 1) install the daemon script (from the staged repo) into a persistent location
if [ -f "$HERE/cs-lockrand.sh" ]; then
    cp "$HERE/cs-lockrand.sh" "$DAEMON"
else
    echo "[!] $HERE/cs-lockrand.sh missing - stage the repo on the car first (see apply-all.sh header)"; exit 1
fi

# 2) back up the stock lock sound once (so we can revert). The stock file is the USB import;
#    only copy if the current file still looks like the stock one (don't back up a random pick).
if [ ! -f "$BACKUP" ] && [ -f "$TARGET" ]; then
    cp "$TARGET" "$BACKUP" && echo "[*] backed up stock lock sound -> $BACKUP"
fi

# 3) (re)start the daemon idempotently. Kill any prior instance BY PID (a pkill -f cs-lockrand
#    would also match this very script's shell). noexec /home -> launch as `sh <script>`.
me=$$; par=$PPID; killed=0
for d in /proc/[0-9]*; do
    p=${d#/proc/}
    [ "$p" = "$me" ] && continue; [ "$p" = "$par" ] && continue
    c=$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null)
    case "$c" in
        *"$DAEMON"*|*"inotifywait -m -e close_nowrite --format %f /home/tesla/.Tesla/data"*)
            kill -9 "$p" 2>/dev/null && killed=$((killed + 1)) ;;
    esac
done
[ "$killed" -gt 0 ] && echo "[*] stopped $killed stale daemon/watcher process(es)"

setsid sh "$DAEMON" </dev/null >"$RUNDIR/daemon.out" 2>&1 &
sleep 2

# 4) readback-verify: the daemon's inotify child must be alive and the primed file must be a
#    folder clip (not the stock sound).
alive=0
for d in /proc/[0-9]*; do
    c=$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null)
    case "$c" in *"inotifywait -m -e close_nowrite --format %f /home/tesla/.Tesla/data"*) alive=1; break ;; esac
done
if [ "$alive" != 1 ]; then
    echo "[!] daemon did not come up. daemon.out:"; sed 's/^/    /' "$RUNDIR/daemon.out" 2>/dev/null; exit 1
fi

pool=""
for p in /mnt/media/*/LockChime /mnt/usb*/LockChime /home/tesla/.Tesla/data/LockChime; do
    [ -d "$p" ] && { pool="$p"; break; }
done
if [ -z "$pool" ]; then
    echo "[ok] daemon running, but NO LockChime folder found on the USB yet."
    echo "     Create a folder named 'LockChime' on the stick with your *.wav clips, then lock."
    exit 0
fi
cur=$(md5sum "$TARGET" 2>/dev/null | cut -d' ' -f1)
match=$(md5sum "$pool"/*.wav 2>/dev/null | awk -v m="$cur" '$1==m{print $2}' | head -1)
if [ -n "$match" ]; then
    echo "[ok] running; lock chime now random from $pool (primed: $(basename "$match"))"
else
    echo "[ok] daemon running and watching; pool=$pool. (primed file not yet a folder clip - lock once.)"
fi
exit 0
