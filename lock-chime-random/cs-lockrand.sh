#!/bin/sh
# cs-lockrand.sh - the random-lock-chime runtime daemon (plain / standalone form).
#
# WHY THIS WORKS (verified live 2026-11-12): the internal lock chime is played in-process by
# QtCarAudiod via ALSA (NOT gstreamer, NOT the libQtCarGUI gst path), and QtCarAudiod RE-READS
# /home/tesla/.Tesla/data/LockChime.wav from disk on EVERY lock (proven: one OPEN..CLOSE per lock,
# not cached across locks). So we watch that file for CLOSE (audiod finished reading for this lock)
# and then atomically stage a fresh random *.wav from the USB LockChime/ folder, priming the NEXT
# lock. The swap happens between locks -> no race, no binary patch.
#
# This is a long-running process (inotifywait -m). Launch it so it survives SSH disconnect, e.g.:
#     setsid sh /path/to/cs-lockrand.sh </dev/null >/dev/null 2>&1 &
# NOTE: /home is mounted noexec, so run it as `sh <script>`, never execute the file directly.
# The CoilShift form runs the equivalent as a supervised `service` (see coilshift/ + README).
#
# Config via env (all optional):
LCR_TARGET=${LCR_TARGET:-/home/tesla/.Tesla/data/LockChime.wav}   # the file audiod reads per lock
LCR_BACKUP=${LCR_BACKUP:-/home/tesla/.cs-lockrand/LockChime.orig.wav} # stock sound, restored on exit
LCR_LOG=${LCR_LOG:-/home/tesla/.cs-lockrand/cs-lockrand.log}
# folders searched for *.wav, first existing one wins (USB first, matches the on-screen model):
LCR_POOLS=${LCR_POOLS:-"/mnt/media/*/LockChime /mnt/usb*/LockChime /home/tesla/.Tesla/data/LockChime"}

DATADIR=$(dirname "$LCR_TARGET")
BASENAME=$(basename "$LCR_TARGET")
TMP="$DATADIR/.cs-lockrand.tmp"

log() { echo "$(date '+%F %T') $*" >> "$LCR_LOG" 2>/dev/null; }

find_pool() {
    for d in $LCR_POOLS; do [ -d "$d" ] && { echo "$d"; return 0; }; done
    return 1
}

# Only stage files whose WAV header matches what audiod plays here (mono / 48000 / 16-bit),
# so a stereo or odd-rate clip can't come out garbled. Skips e.g. a stereo PH.wav.
compatible() {
    [ "$(od -An -tu2 -j22 -N2 "$1" 2>/dev/null | tr -d ' ')" = "1" ]     || return 1
    [ "$(od -An -tu4 -j24 -N4 "$1" 2>/dev/null | tr -d ' ')" = "48000" ] || return 1
    [ "$(od -An -tu2 -j34 -N2 "$1" 2>/dev/null | tr -d ' ')" = "16" ]    || return 1
    return 0
}

pick() {
    pool=$(find_pool) || return 1
    seed=$(od -An -N4 -tu4 /dev/urandom 2>/dev/null | tr -dc 0-9); [ -n "$seed" ] || seed=$$
    find "$pool" -maxdepth 1 -type f -iname '*.wav' 2>/dev/null \
        | while IFS= read -r f; do compatible "$f" && printf '%s\n' "$f"; done \
        | awk -v s="$seed" 'NF{a[++n]=$0} END{if(n){srand(s); print a[int(rand()*n)+1]}}'
}

stage() {
    f=$(pick) || return 0
    [ -n "$f" ] || return 0
    if cp "$f" "$TMP" 2>/dev/null && mv "$TMP" "$LCR_TARGET" 2>/dev/null; then
        log "staged $(basename "$f")"
    fi
}

# On stop, put the stock sound back so the lock chime returns to normal.
teardown() {
    [ -f "$LCR_BACKUP" ] && cp "$LCR_BACKUP" "$LCR_TARGET" 2>/dev/null && log "restored stock lock sound"
    exit 0
}
trap teardown TERM INT HUP

stage   # prime the first lock
# Watch the data dir (the file's inode is replaced on each stage, so watch the dir, not the file).
inotifywait -m -e close_nowrite --format '%f' "$DATADIR" 2>/dev/null | while read -r name; do
    [ "$name" = "$BASENAME" ] && stage
done
