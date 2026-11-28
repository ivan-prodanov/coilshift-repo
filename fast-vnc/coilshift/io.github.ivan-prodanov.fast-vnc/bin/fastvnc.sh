#!/bin/bash
# Fast VNC service: the car's own x11vnc on port 5901, over Tailscale only, with -snapfb.
#
# Why it is faster than the built-in VNC: the X server here is amdgpu + glamor with MIT-SHM off,
# so x11vnc's default change polling (many small XGetImage reads, each a GPU readback) takes 2-3 s
# per update. -snapfb reads the whole screen in one request and compares the copy in memory:
# ~0.4 -> ~1.9 updates/s in Screen Sharing (Zlib) and ~5 in TigerVNC (Tight JPEG), measured on
# 2026.38.6. -cursor arrow sends a pointer shape; the touchscreen itself has none.
#
# Runs as root under CoilShift (restart=always): when something it needs is missing it says so in
# the status line, waits a few seconds and exits, and CoilShift starts it again. Bash 3.2 safe.
set -u
: "${CSDK_RUN:?}" "${CSDK_SETTINGS:?}" "${CSDK_PACKAGE_DIR:?}"
export LC_ALL=C
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
. "$CSDK_PACKAGE_DIR/lib/settings.sh"

src=${FV_X11VNC_SRC:-/usr/bin/x11vnc}
port=${FV_PORT:-5901}
touch_dev=${FV_TOUCH:-/dev/input/touch}
xsock=${FV_XSOCK:-/tmp/.X11-unix/X0}
proc=${FV_PROC:-/proc}
aa_mode=${FV_AA_MODE:-/sys/module/apparmor/parameters/mode}
delay=${FV_DELAY:-5}
bin=$CSDK_RUN/bin/x11vnc
passwd=$CSDK_RUN/passwd

status() { csdk_api PUT /status "{\"state\":\"$1\",\"reason\":$(csdk_json_string "$2")}" >/dev/null 2>&1 || true; }
nap_pid=''
trap '[ -z "$nap_pid" ] || kill "$nap_pid" 2>/dev/null; exit 0' TERM INT HUP
nap() { sleep "$1" & nap_pid=$!; wait "$nap_pid" 2>/dev/null; nap_pid=''; }
# not_yet REASON: show why it can't serve yet, wait, then exit so CoilShift starts it again.
not_yet() { echo "fast-vnc: $1" >&2; status degraded "$1"; nap "$delay"; exit 1; }

tailscale_ip() {
    if [ -n "${FV_TSIP-}" ]; then printf '%s' "$FV_TSIP"; return; fi
    ip -4 -o addr show dev tailscale0 2>/dev/null | awk '{ sub(/\/.*/, "", $4); print $4; exit }'
}
# port_taken: something already listens on $port (any address).
port_taken() {
    local f p
    p=$(printf ':%04X' "$port")
    for f in "$proc/net/tcp" "$proc/net/tcp6"; do
        [ -r "$f" ] || continue
        awk -v p="$p" '$4 == "0A" && substr($2, length($2) - 4) == p { found = 1 } END { exit !found }' "$f" && return 0
    done
    return 1
}

[ -x "$src" ] || not_yet "This firmware has no $src."
[ -S "$xsock" ] || not_yet "Waiting for the touchscreen's display."
[ -c "$touch_dev" ] || not_yet "No touch device at $touch_dev."
ts=$(tailscale_ip)
[ -n "$ts" ] || not_yet "Waiting for Tailscale (needs CoilShift's Tailscale)."
port_taken && not_yet "Port $port is already in use."

case $(fv_setting size) in
    half) scale=1/2 label='1280x800' ;;
    mbp16) scale=0.675 label='1728x1080' ;;
    *) scale='' label='2560x1600' ;;
esac
password=$(fv_setting password)
[ -n "$password" ] || password=coilshift

# A copy of x11vnc, not /usr/bin/x11vnc itself: Tesla's AppArmor profile for that path is meant for
# its own sandboxed screenshare, and it must not apply to this one.
umask 077
mkdir -p "$CSDK_RUN/bin" || not_yet "Can't create $CSDK_RUN/bin."
if ! cmp -s "$src" "$bin"; then
    { cp "$src" "$bin.new" && chmod 755 "$bin.new" && mv -f "$bin.new" "$bin"; } || not_yet "Can't copy $src."
fi
{ printf '%s\n' "$password" > "$passwd.new" && mv -f "$passwd.new" "$passwd"; } || not_yet "Can't write the password file."
fv_settings_sum > "$CSDK_RUN/settings.used"   # hooks/apply.sh restarts us when settings change

note=''
if [ -r "$aa_mode" ]; then
    mode=$(cat "$aa_mode" 2>/dev/null)
    [ "$mode" = complain ] || note=" AppArmor is in $mode mode: touch may not work."
fi
status ok "Serving the screen at $label on port $port over Tailscale.$note"

export FV_TOUCH_DEV=$touch_dev FV_RUN=$CSDK_RUN
exec "$bin" -display :0 -rfbport "$port" -listen "$ts" -noipv6 -passwdfile "$passwd" \
    -forever -shared -nokeyboard -cursor arrow -nowf -noshm -noxdamage -threads -snapfb \
    ${scale:+-scale "$scale"} -q \
    -pipeinput "/bin/bash $CSDK_PACKAGE_DIR/bin/touch.sh"
