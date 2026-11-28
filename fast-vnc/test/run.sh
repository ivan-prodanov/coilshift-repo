#!/bin/bash
# Behaviour test for Fast VNC, off the car. Bash 3.2 safe, like the car; needs python3 to decode.
#   touch.sh:   feeds x11vnc pipeinput lines, decodes the input_events written to a file.
#   fastvnc.sh: runs the service against a fake x11vnc that records its arguments.
# Usage: fast-vnc/test/run.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
pkg="$here/../coilshift/io.github.ivan-prodanov.fast-vnc"
work=$(mktemp -d "${TMPDIR:-/tmp}/fastvnc.XXXXXX")
fails=0
trap 'rm -rf "$work"' EXIT

check() { # check NAME WANT GOT
    if [ "$2" = "$3" ]; then echo "ok   $1"; else
        echo "FAIL $1"; echo "  want: $2"; echo "  got:  $3"; fails=$((fails + 1)); fi
}

# decode FILE: the input_events in FILE as "type,code,value" words; "BAD-LENGTH" if not whole events.
decode() {
    python3 -c '
import struct, sys
b = open(sys.argv[1], "rb").read()
if len(b) % 24: print("BAD-LENGTH %d" % len(b)); sys.exit()
print(" ".join("%d,%d,%d" % struct.unpack("<HHi", b[i + 16:i + 24]) for i in range(0, len(b), 24)))
' "$1"
}
touch_run() { # touch_run LINES...: run touch.sh on LINES, print the decoded events
    : > "$work/dev"
    printf '%s\n' "$@" | FV_TOUCH_DEV="$work/dev" FV_RUN="$work" bash "$pkg/bin/touch.sh"
    decode "$work/dev"
}

# --- touch.sh ---
check "press at y=10 (a 0x0A byte) is exact, then the 3-pixel move" \
    "3,57,1 3,53,100 3,54,10 0,0,0 3,54,13 0,0,0" "$(touch_run 'Pointer 0 100 10 1 ButtonPress-1')"
check "drag with the button down (266 = 0x010A, 522 = 0x020A)" \
    "3,57,1 3,53,266 3,54,522 0,0,0" "$(touch_run 'Pointer 0 266 522 1 None')"
check "release lifts the finger (tracking id -1)" \
    "3,57,-1 3,53,2559 3,54,1599 0,0,0" "$(touch_run 'Pointer 0 2559 1599 0 ButtonRelease-1')"
check "moving without the button sends nothing" "" "$(touch_run 'Pointer 0 300 300 0 None')"
check "other lines and bad numbers are ignored" "" \
    "$(touch_run '# x11vnc pipeinput help text' 'Keysym 1 0 65 a KeyPress' 'Pointer 0 1x 5 1 ButtonPress-1' 'Pointer 0 5 5')"
check "right button alone is not a touch" "" "$(touch_run 'Pointer 0 50 60 4 ButtonPress-3')"
check "a whole tap" "3,57,1 3,53,7 3,54,8 0,0,0 3,54,11 0,0,0 3,57,-1 3,53,7 3,54,8 0,0,0" \
    "$(touch_run 'Pointer 0 7 8 1 ButtonPress-1' 'Pointer 0 7 8 0 ButtonRelease-1')"
check "no batch file left behind" "" "$(ls "$work" | grep '^touch\.' || true)"

# --- fastvnc.sh ---
cat > "$work/x11vnc" <<'EOF'
#!/bin/bash
printf '%s\n' "$@" > "$FAKE_ARGS"
EOF
chmod 755 "$work/x11vnc"
python3 -c 'import socket, sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' "$work/X0"
mkdir -p "$work/proc/net" "$work/run" "$work/settings"
: > "$work/proc/net/tcp6"
free_port() { printf '  sl  local_address rem_address   st\n   0: 0100007F:1F90 00000000:0000 0A\n' > "$work/proc/net/tcp"; }
take_port() { printf '  sl  local_address rem_address   st\n   0: 6440A572:170D 00000000:0000 0A\n' > "$work/proc/net/tcp"; }

svc() { # svc [TSIP]: run the service once; prints its exit code
    rm -f "$work/args"
    CSDK_RUN="$work/run" CSDK_SETTINGS="$work/settings" CSDK_PACKAGE_DIR="$pkg" \
    CSDK_API_SOCKET="$work/no.sock" CSDK_TOKEN=x FAKE_ARGS="$work/args" \
    FV_X11VNC_SRC="$work/x11vnc" FV_XSOCK="$work/X0" FV_TOUCH=/dev/null FV_PROC="$work/proc" \
    FV_AA_MODE="$work/none" FV_DELAY=0 FV_TSIP="${1-100.1.2.3}" \
        bash "$pkg/bin/fastvnc.sh" 2>/dev/null
    echo $?
}
args() { tr '\n' ' ' < "$work/args" 2>/dev/null | sed 's/ $//'; }
has() { case " $(args) " in *" $1 "*) echo yes ;; *) echo no ;; esac; }

free_port
check "defaults: starts" 0 "$(svc)"
check "defaults: port 5901 on the Tailscale address only" yes "$(has '-rfbport 5901 -listen 100.1.2.3 -noipv6')"
check "defaults: -snapfb and a pointer" "yes yes" "$(has -snapfb) $(has '-cursor arrow')"
check "defaults: full size (no -scale)" no "$(has -scale)"
check "defaults: password coilshift" coilshift "$(cat "$work/run/passwd")"
check "defaults: touch helper wired" yes "$(has "-pipeinput /bin/bash $pkg/bin/touch.sh")"
check "x11vnc copied into the run dir" yes "$(cmp -s "$work/x11vnc" "$work/run/bin/x11vnc" && echo yes)"

printf 'half\n' > "$work/settings/size"; svc >/dev/null
check "size half: -scale 1/2" yes "$(has '-scale 1/2 -q')"
printf 'mbp16' > "$work/settings/size"; svc >/dev/null
check "size mbp16: -scale 0.675" yes "$(has '-scale 0.675 -q')"
printf 'full' > "$work/settings/size"; svc >/dev/null
check "size full: no -scale" no "$(has -scale)"
printf 'my secret\n' > "$work/settings/password"; svc >/dev/null
check "password setting is used" "my secret" "$(cat "$work/run/passwd")"

take_port
check "port 5901 taken: exits 1 without starting x11vnc" "1 no-args" "$(svc) $([ -f "$work/args" ] && echo args || echo no-args)"
free_port
check "no Tailscale address: exits 1 without starting x11vnc" "1 no-args" "$(svc '') $([ -f "$work/args" ] && echo args || echo no-args)"

# --- hooks/apply.sh: restarts the service only when its settings changed since it started ---
mkdir -p "$work/fakebin"
cat > "$work/fakebin/curl" <<'EOF'
#!/bin/bash
for a in "$@"; do last=$a; done
printf '%s\n' "$last" >> "$FAKE_CALLS"
EOF
chmod 755 "$work/fakebin/curl"
apply() { # apply: run the apply hook once; prints its exit code and the car API calls it made
    : > "$work/calls"
    PATH="$work/fakebin:$PATH" FAKE_CALLS="$work/calls" CSDK_RUN="$work/run2" CSDK_SETTINGS="$work/settings" \
    CSDK_PACKAGE_DIR="$pkg" CSDK_API_SOCKET="$work/no.sock" CSDK_TOKEN=x CSDK_VERSION=1.0.0 \
        bash "$pkg/hooks/apply.sh" >/dev/null 2>&1
    echo "$? $(tr '\n' ' ' < "$work/calls" | sed 's/ $//')"
}
started() { # started: the service's record of the settings it started with, for the current settings
    ( CSDK_SETTINGS="$work/settings"; . "$pkg/lib/settings.sh"; fv_settings_sum ) > "$work/run2/settings.used"
}
rm -f "$work/settings/size" "$work/settings/password"; mkdir -p "$work/run2"
check "apply before the service ever started: no restart" "0 " "$(apply)"
check "apply records the version" 1.0.0 "$(cat "$work/run2/applied")"
started
check "apply with unchanged settings: no restart" "0 " "$(apply)"
printf 'half' > "$work/settings/size"
check "apply after the size changed: restarts the service" "0 http://csdk/v1/services/vnc/restart" "$(apply)"
started; printf 'new pass' > "$work/settings/password"
check "apply after the password changed: restarts the service" "0 http://csdk/v1/services/vnc/restart" "$(apply)"
started
check "apply after the service restarted with them: no restart" "0 " "$(apply)"
svc >/dev/null; cp "$work/run/settings.used" "$work/run2/settings.used"
check "the service's own record matches the hook's sum" "0 " "$(apply)"

echo
[ "$fails" -eq 0 ] && echo "all passed" || { echo "$fails failed"; exit 1; }
