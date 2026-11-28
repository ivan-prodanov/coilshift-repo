#!/bin/bash
# cs-closeonlock: CoilShift service (restart: always). Bash 3.2, the car's.
#
# Every time the car locks, unless it is in a driving gear:
#   frunk open -> send the touchscreen's "open frunk" again (GUI_frontTrunkRequest=Pressed).
#                 An aftermarket power frunk takes a second release as "close". A stock frunk
#                 is already open, so nothing happens. Sent first, because the touchscreen
#                 stops offering frunk control once the alarm arms.
#   trunk open -> send the touchscreen trunk popup's "Close" (GUI_rearTrunkRequest=Close).
#                 Close only: it never opens a shut trunk. A car without a power liftgate
#                 ignores it.
# Each request goes back to None a second later, so the next lock is a fresh press.
#
# The requests go to the touchscreen process (com.tesla.CenterDisplay). It owns the GUI_
# requests and hands them to the vehicle process that puts them on the CAN bus, exactly like
# a tap. The SDK's PUT /v1/dv is not used: it skips a value the car already shows and keeps
# a ledger entry, which is wrong for a press.
#
# The car API sends no lock event, so VAPI_isLocked is read every POLL seconds. Only an
# unlocked -> locked change counts: starting up, or waking up, already locked does nothing.
set -u
: "${CSDK_PACKAGE_DIR:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"

GDBUS=${CL_GDBUS:-/usr/bin/gdbus}
POLL=${CL_POLL:-0.5}   # seconds between lock-state reads
HOLD=${CL_HOLD:-1}     # seconds a request is held before it goes back to None

log() { csdk_log "$1" "$2" 2>/dev/null || true; }

# ldv METHOD NAME [VALUE]: one call on the touchscreen process's LocalDataValue interface,
# what /usr/local/bin/get-local-dv and set-local-dv do, without their extra bash. The timeout
# keeps a restarting touchscreen from holding the loop for gdbus's default 25 s.
ldv() {
    "$GDBUS" call --system --timeout 5 --dest com.tesla.CenterDisplay \
        --object-path /LocalDataValue --method "com.tesla.LocalDataValue.$1" "$2" ${3+"$3"} 2>/dev/null
}

# dv NAME: print the value of a valid DataValue. The car answers "(true, 'value')", or
# "(false, '')" when the value is invalid. Fails when invalid or unreadable.
dv() {
    local out
    out=$(ldv DataGetValueRequest "$1") || return 1
    case "$out" in "(true, "*) ;; *) return 1 ;; esac
    out=${out#(true, }; out=${out%)}; out=${out#\'}; out=${out%\'}
    printf '%s\n' "$out"
}

# put NAME VALUE: set a DataValue. The car answers "(true,)" when it took it.
put() {
    case "$(ldv DataSetValueRequest "$1" "<'$2'>")" in
        "(true,)") return 0 ;;
        *) log warn "the car did not take $1=$2"; return 1 ;;
    esac
}

# open_in DOORS BIT NAME: whether VAPI_doorState shows that closure open (DoorType: 16 is
# TrunkFront, 32 TrunkRear). It may read as the bitmask, decimal or hex, or as DoorType
# names; all are handled. Arithmetic only ever sees digits.
open_in() {
    local hex
    case "$1" in
        0x*|0X*)
            hex=${1#0[xX]}
            case "$hex" in ''|*[!0-9a-fA-F]*) return 1 ;; esac
            [ $(( 16#$hex & $2 )) -ne 0 ] ;;
        ''|*[!0-9]*)
            case "$1" in *"$3"*) return 0 ;; esac
            return 1 ;;
        *) [ $(( 10#$1 & $2 )) -ne 0 ] ;;
    esac
}

on_lock() {
    local gear doors sent='' d
    # A drive-away lock happens on the move: leave the closures alone in R, N and D. An
    # unreadable gear right after the driver has left still counts as parked.
    gear=$(dv VAPI_shiftState) || gear=''
    case "$gear" in
        R|N|D) log info "locked in $gear: closures left alone"; return 0 ;;
    esac
    if ! doors=$(dv VAPI_doorState); then
        log warn "locked, but the door state is unreadable"
        return 0
    fi
    if open_in "$doors" 16 TrunkFront && put GUI_frontTrunkRequest Pressed; then
        sent="$sent GUI_frontTrunkRequest"
    fi
    if open_in "$doors" 32 TrunkRear && put GUI_rearTrunkRequest Close; then
        sent="$sent GUI_rearTrunkRequest"
    fi
    if [ -z "$sent" ]; then
        log info "locked (doors=$doors): nothing to close"
        return 0
    fi
    log info "locked (doors=$doors): sent$sent"
    sleep "$HOLD"
    for d in $sent; do put "$d" None; done
}

trap 'log info "stopped"; exit 0' TERM INT HUP
csdk_api PUT /status '{"state":"ok","reason":"Watching for lock."}' >/dev/null 2>&1 || true
log info "close-on-lock active"

was=''
while :; do
    if now=$(dv VAPI_isLocked); then
        case "$now" in
            true|True|1) now=1 ;;
            false|False|0) now=0 ;;
            *) now='' ;;
        esac
        if [ -n "$now" ]; then
            [ "$was" = 0 ] && [ "$now" = 1 ] && on_lock
            was=$now
        fi
    fi
    sleep "$POLL"
done
