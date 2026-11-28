#!/bin/bash
# Behaviour test for the close-on-lock service: runs bin/cs-closeonlock.sh against a fake gdbus
# whose DataValues are files, flips the lock state and checks which requests the service sends.
# Bash 3.2 safe, like the car. Usage: close-on-lock/test/run.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
pkg="$here/../coilshift/io.github.ivan-prodanov.close-on-lock"
work=$(mktemp -d "${TMPDIR:-/tmp}/closeonlock.XXXXXX")
state="$work/dv"; mkdir -p "$state"
fails=0 svc=''
cleanup() { [ -n "$svc" ] && kill "$svc" 2>/dev/null; rm -rf "$work"; }
trap cleanup EXIT

# The fake gdbus: "call ... --method com.tesla.LocalDataValue.<M> NAME [VALUE]".
cat > "$work/gdbus" <<'EOF'
#!/bin/bash
m='' name='' value=''
while [ $# -gt 0 ]; do
    case "$1" in
        --method) m=${2##*.}; name=$3; value=${4-}; break ;;
        *) shift ;;
    esac
done
case "$m" in
    DataGetValueRequest)
        if [ -f "$DV/$name" ] && [ "$(cat "$DV/$name")" != INVALID ]; then
            printf "(true, '%s')\n" "$(cat "$DV/$name")"
        else
            printf "(false, '')\n"
        fi ;;
    DataSetValueRequest)
        printf '%s %s\n' "$name" "$value" >> "$DV/sets"
        printf '(true,)\n' ;;
    *) exit 1 ;;
esac
EOF
chmod 755 "$work/gdbus"

set_dv() { printf '%s' "$2" > "$state/$1"; }
settle() { sleep 0.4; }
check() { # check NAME EXPECTED-SETS: compare the requests sent since the last check
    local got
    got=$(cat "$state/sets" 2>/dev/null)
    : > "$state/sets"
    if [ "$got" = "$2" ]; then
        echo "ok   $1"
    else
        echo "FAIL $1"; echo "  want: $(printf '%s' "$2" | tr '\n' '|')"; echo "  got:  $(printf '%s' "$got" | tr '\n' '|')"
        fails=$((fails + 1))
    fi
}
lock_cycle() { set_dv VAPI_isLocked false; settle; set_dv VAPI_isLocked true; settle; }

both="GUI_frontTrunkRequest <'Pressed'>
GUI_rearTrunkRequest <'Close'>
GUI_frontTrunkRequest <'None'>
GUI_rearTrunkRequest <'None'>"
trunk="GUI_rearTrunkRequest <'Close'>
GUI_rearTrunkRequest <'None'>"
frunk="GUI_frontTrunkRequest <'Pressed'>
GUI_frontTrunkRequest <'None'>"

set_dv VAPI_isLocked true; set_dv VAPI_doorState 48; set_dv VAPI_shiftState P
DV="$state" CSDK_PACKAGE_DIR="$pkg" CSDK_API_SOCKET="$work/no.sock" CSDK_TOKEN=x \
    CL_GDBUS="$work/gdbus" CL_POLL=0.05 CL_HOLD=0.1 bash "$pkg/bin/cs-closeonlock.sh" &
svc=$!
settle
check "starting up already locked sends nothing" ""

lock_cycle;                                   check "lock, both open, P: frunk re-sent, trunk closed" "$both"
set_dv VAPI_doorState 0;  lock_cycle;         check "lock, both shut: nothing" ""
set_dv VAPI_doorState 32; set_dv VAPI_shiftState D; lock_cycle
                                              check "lock in D (drive-away): nothing" ""
set_dv VAPI_shiftState INVALID; lock_cycle;   check "lock, gear unreadable, trunk open: trunk closed" "$trunk"
set_dv VAPI_shiftState P
set_dv VAPI_doorState 16; lock_cycle;         check "lock, frunk open: frunk re-sent" "$frunk"
set_dv VAPI_doorState TrunkRear; lock_cycle;  check "door state as names: trunk closed" "$trunk"
set_dv VAPI_doorState 0x30; lock_cycle;       check "door state as hex: both" "$both"
set_dv VAPI_doorState 048; lock_cycle;        check "leading zero is decimal: both" "$both"
set_dv VAPI_doorState INVALID; lock_cycle;    check "door state unreadable: nothing" ""
set_dv VAPI_doorState 48
set_dv VAPI_isLocked INVALID; settle; set_dv VAPI_isLocked true; settle
                                              check "locked -> unreadable -> locked: nothing" ""

kill -TERM "$svc"
i=0; while kill -0 "$svc" 2>/dev/null && [ "$i" -lt 20 ]; do sleep 0.1; i=$((i + 1)); done
if kill -0 "$svc" 2>/dev/null; then
    echo "FAIL TERM stops the service"; fails=$((fails + 1))
else
    wait "$svc"; rc=$?; svc=''
    if [ "$rc" -eq 0 ]; then echo "ok   TERM stops the service (exit 0)"; else echo "FAIL TERM exit $rc"; fails=$((fails + 1)); fi
fi

[ "$fails" -eq 0 ] && echo "all passed" || echo "$fails failed"
exit "$fails"
