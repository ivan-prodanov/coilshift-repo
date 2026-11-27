# lockrand.sh: shared by hooks/apply.sh and bin/cs-lockrand.sh. Source it after lib/csdk.sh.
# bash 3.2 (the car's). CSDK_FAKE_ROOT is the SDK test harness only; on a car it is empty.

# The writable copy of the custom lock sound that QtCarAudiod re-reads on every lock. The car
# only creates it once a USB lock sound is picked in Toybox (LockChime.wav at the USB root).
LR_TARGET=${LCR_TARGET:-${CSDK_FAKE_ROOT:-}/home/tesla/.Tesla/data/LockChime.wav}

# lr_folder: the USB folder name from the package setting (default LockChime).
lr_folder() {
    local f=''
    [ -r "${CSDK_SETTINGS:-}/folder" ] && f=$(tr -d '[:space:]' < "$CSDK_SETTINGS/folder" 2>/dev/null)
    printf '%s\n' "${f:-LockChime}"
}

# lr_pool FOLDER: the first directory named FOLDER on a mounted USB stick.
lr_pool() {
    local d
    for d in "${CSDK_FAKE_ROOT:-}"/mnt/media/*/"$1" "${CSDK_FAKE_ROOT:-}"/mnt/usb*/"$1"; do
        [ -d "$d" ] && { printf '%s\n' "$d"; return 0; }
    done
    return 1
}

# lr_compatible FILE: a WAV this car's audiod plays: mono, 48 kHz, 16-bit.
lr_compatible() {
    [ "$(od -An -tu2 -j22 -N2 "$1" 2>/dev/null | tr -d ' ')" = "1" ]     || return 1
    [ "$(od -An -tu4 -j24 -N4 "$1" 2>/dev/null | tr -d ' ')" = "48000" ] || return 1
    [ "$(od -An -tu2 -j34 -N2 "$1" 2>/dev/null | tr -d ' ')" = "16" ]    || return 1
}

# lr_wavs POOL / lr_clips POOL: every *.wav in POOL / the usable ones, one path per line.
lr_wavs() { find "$1" -maxdepth 1 -type f -iname '*.wav' 2>/dev/null; }
lr_clips() {
    local f
    lr_wavs "$1" | while IFS= read -r f; do lr_compatible "$f" && printf '%s\n' "$f"; done
}

# lr_install SRC: put a copy of SRC in place as the lock sound, readable by the UI. QtCar runs
# the lock sound's gst-launch as user tesla (uid 1111), but a copy root makes under the service
# supervisor's umask (077) is root-only 0700. gst then can't open it, exits at once, and the lock
# is silent (up to 1.1.1). So give the copy mode 0644 and the data directory's owner, then
# rename it into place.
lr_install() {
    local dir tmp own
    dir=$(dirname "$LR_TARGET")
    tmp="$dir/.cs-lockrand.$$.tmp"
    cp "$1" "$tmp" 2>/dev/null || { rm -f "$tmp" 2>/dev/null; return 1; }
    chmod 0644 "$tmp" 2>/dev/null
    own=$(stat -c '%u:%g' "$dir" 2>/dev/null) && [ -n "$own" ] && chown "$own" "$tmp" 2>/dev/null
    mv -f "$tmp" "$LR_TARGET" 2>/dev/null || { rm -f "$tmp" 2>/dev/null; return 1; }
}

# lr_cmdline PID: the process's argv joined by spaces, or nothing if it's gone.
lr_cmdline() { tr '\0' ' ' 2>/dev/null < "${CSDK_FAKE_ROOT:-}/proc/$1/cmdline"; }
lr_ppid() { awk '/^PPid:/{print $2}' "${CSDK_FAKE_ROOT:-}/proc/$1/status" 2>/dev/null; }

# lr_is_ours PID: PID is this package's watcher or service loop (any version), judged by argv.
lr_is_ours() {
    local c
    c=$(lr_cmdline "$1")
    case "$c" in
        "inotifywait -m -e close_nowrite --format %f $(dirname "$LR_TARGET") ") return 0 ;;
        *io.github.ivan-prodanov.lock-chime-random/*bin/cs-lockrand.sh*) return 0 ;;
    esac
    return 1
}

# lr_reap_stale PIDFILE: stop watchers an earlier run of the service left behind. Up to 1.1.0
# the loop was `inotifywait | while read`, and a service killed while blocked in it left both
# halves running under init, still swapping clips (and undoing the restore on stop). Kills the
# pids a previous run recorded in PIDFILE, plus any orphan (parent = init) of ours. Never
# touches this process, its parent, or a copy still under its supervisor.
lr_reap_stale() {
    local pidfile=$1 p n=0 d
    if [ -r "$pidfile" ]; then
        for p in $(cat "$pidfile" 2>/dev/null); do
            case "$p" in ''|*[!0-9]*) continue ;; esac
            [ "$p" = "$$" ] || [ "$p" = "$PPID" ] && continue
            lr_is_ours "$p" && kill "$p" 2>/dev/null && n=$((n + 1))
        done
    fi
    for d in "${CSDK_FAKE_ROOT:-}"/proc/[0-9]*; do
        p=${d##*/}
        [ "$p" = "$$" ] || [ "$p" = "$PPID" ] && continue
        [ "$(lr_ppid "$p")" = "1" ] || continue
        lr_is_ours "$p" && kill "$p" 2>/dev/null && n=$((n + 1))
    done
    [ "$n" -gt 0 ] && csdk_log info "stopped $n stale watcher process(es)" 2>/dev/null
    return 0
}

# lr_status: put the driver's next setup step, or "Ready", in the package status CoilShift
# shows under the patch. "degraded" (amber) means something is still to do.
lr_status() {
    local folder pool total n state reason
    folder=$(lr_folder)
    if [ ! -f "$LR_TARGET" ]; then
        state=degraded
        reason="First turn on a USB lock sound: copy any LockChime.wav to the USB stick's top level, then choose USB in Toybox > Boombox > Lock Sound."
    elif ! pool=$(lr_pool "$folder"); then
        state=degraded
        reason="Add a '$folder' folder with your clips (WAV, mono, 48 kHz, 16-bit) to the USB stick. It isn't on any USB partition the car has mounted yet."
    else
        total=$(lr_wavs "$pool" | wc -l | tr -d ' ')
        n=$(lr_clips "$pool" | wc -l | tr -d ' ')
        if [ "$n" -eq 0 ]; then
            state=degraded
            reason="The '$folder' folder has $total .wav file(s), but none is mono, 48 kHz, 16-bit, so none can play. Convert them (see the folder setting)."
        else
            state=ok
            reason="Ready: $n clip(s) in '$folder'"
            [ "$total" -gt "$n" ] && reason="$reason, $((total - n)) skipped (not mono, 48 kHz, 16-bit)"
            reason="$reason. Each lock plays a random one."
        fi
    fi
    reason=${reason:0:250}
    csdk_api PUT /status "{\"state\":\"$state\",\"reason\":$(csdk_json_string "$reason")}" >/dev/null 2>&1 || true
}
