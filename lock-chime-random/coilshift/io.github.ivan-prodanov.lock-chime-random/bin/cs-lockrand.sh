#!/bin/bash
# cs-lockrand - CoilShift service form of the random lock chime (restart:"always").
# Bash 3.2 only (runs on the MCU). Supervised by ssp, so it starts at boot and is kept alive -
# this is the durable form the standalone ../../cs-lockrand.sh stands in for off-package.
#
# The car re-reads /home/tesla/.Tesla/data/LockChime.wav every time it plays the USB lock sound.
# We watch that file for CLOSE and, right after each read, atomically stage a fresh random
# mono/48k/16-bit clip from the USB folder for the next lock. On stop (TERM/INT/HUP) we put the
# stock sound back from CSDK_STATE. The backup is made in hooks/apply.sh. After start and each
# lock the package status tells the driver what is still to set up, or that it's ready.
#
# The watcher runs in the background with a known pid, writing to a FIFO, and the loop reads it
# with a timeout. Up to 1.1.0 it was `inotifywait | while read` in the foreground: bash holds a
# trap until a foreground pipeline ends, so a stop never ran the teardown, and once the service
# was killed both halves of the pipeline kept running under init.
set -u
: "${CSDK_PACKAGE_DIR:?}" "${CSDK_STATE:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
. "$CSDK_PACKAGE_DIR/lib/lockrand.sh"

TARGET=$LR_TARGET
BACKUP="$CSDK_STATE/LockChime.orig.wav"
DATADIR=$(dirname "$TARGET")
BASENAME=$(basename "$TARGET")
TMP="$DATADIR/.cs-lockrand.$$.tmp"
FIFO="$CSDK_STATE/lockrand.events"
PIDFILE="$CSDK_STATE/lockrand.pids"
FOLDER=$(lr_folder)
WATCHER=''

pick() {
    local pool seed
    pool=$(lr_pool "$FOLDER") || return 1
    seed=$(od -An -N4 -tu4 /dev/urandom 2>/dev/null | tr -dc 0-9); [ -n "$seed" ] || seed=$$
    lr_clips "$pool" | awk -v s="$seed" 'NF{a[++n]=$0} END{if(n){srand(s); print a[int(rand()*n)+1]}}'
}
stage() {
    local f
    f=$(pick) || return 0
    [ -n "$f" ] || return 0
    if lr_install "$f"; then
        csdk_log info "staged $(basename "$f")" 2>/dev/null || true
    fi
}
stop_watcher() {
    [ -n "$WATCHER" ] && kill "$WATCHER" 2>/dev/null
    WATCHER=''
    rm -f "$FIFO" "$PIDFILE" "$TMP" 2>/dev/null
}
teardown() {
    trap - TERM INT HUP
    stop_watcher
    [ -f "$BACKUP" ] && lr_install "$BACKUP"
    csdk_log info "stopped; stock lock sound restored" 2>/dev/null || true
    exit 0
}
trap teardown TERM INT HUP

lr_reap_stale "$PIDFILE"
csdk_log info "random lock chime active (folder=$FOLDER)" 2>/dev/null || true
stage       # prime the first lock
lr_status

rm -f "$FIFO"
if ! mkfifo -m 600 "$FIFO"; then
    csdk_log error "cannot create $FIFO" 2>/dev/null || true
    exit 1
fi
# Watch the dir (the file's inode is replaced each stage). The opens below pair up: the
# watcher's write end and our read end each wait for the other.
inotifywait -m -e close_nowrite --format '%f' "$DATADIR" > "$FIFO" 2>/dev/null &
WATCHER=$!
printf '%s %s\n' "$$" "$WATCHER" > "$PIDFILE"
exec 3< "$FIFO"

# The 2 s timeout only gives a pending stop signal its turn. bash 3.2 (the car's) returns 1 for a
# timeout and for EOF alike, so ask the watcher itself: still running means it was a quiet spell;
# gone (bash has reaped it) means it died, and the supervisor restarts us. 1.1.1 tested the
# status (>128) instead, and so exited at the first quiet 2 s and restart-looped.
while :; do
    if read -r -t 2 name <&3; then
        [ "$name" = "$BASENAME" ] && { stage; lr_status; }
    elif ! kill -0 "$WATCHER" 2>/dev/null; then
        break
    fi
done
csdk_log warn "watcher exited; restarting" 2>/dev/null || true
stop_watcher
exit 1
