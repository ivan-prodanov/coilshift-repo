#!/bin/bash
# Fast VNC touch: turns x11vnc's -pipeinput pointer lines into touchscreen events.
#
# x11vnc writes "Pointer <client> <x> <y> <buttonmask> <hint>" for every pointer event, with x and y
# in touchscreen pixels (x11vnc undoes -scale first). The left button becomes one finger on
# /dev/input/touch, the evdev node of Tesla's touch-gateway: tracking id, X, Y, then SYN_REPORT.
#
# Each batch goes out in ONE write(). evdev only takes whole 24-byte input_events, and bash
# flushes its output at every 0x0A byte, so a printf straight to the device would split any event
# holding a newline byte (an X or Y of 10, 266, 522, ...). The batch is written to a file first,
# then cat copies it to the device in one write. Bash 3.2 safe, like the car.
set -u
dev=${FV_TOUCH_DEV:-/dev/input/touch}
buf=${FV_RUN:-/tmp}/touch.$$
trap 'rm -f "$buf"' EXIT
exec 3>"$dev" || exit 1

EV_SYN=0
SYN_REPORT=0
EV_ABS=3
ABS_MT_POSITION_X=53
ABS_MT_POSITION_Y=54
ABS_MT_TRACKING_ID=57
TIME='\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00'
events=''

# le BYTES VALUE: append VALUE as BYTES little-endian bytes, as \xHH escapes.
le() {
    local i h
    for ((i = 0; i < $1; i++)); do
        printf -v h '\\x%02x' $(( ($2 >> (8 * i)) & 255 ))
        events=$events$h
    done
}
# ev TYPE CODE VALUE: append one struct input_event (x86-64: zero timeval, which the kernel
# replaces, then u16 type, u16 code, s32 value).
ev() { events=$events$TIME; le 2 "$1"; le 2 "$2"; le 4 "$3"; }
finger() { ev $EV_ABS $ABS_MT_TRACKING_ID "$1"; ev $EV_ABS $ABS_MT_POSITION_X "$2"; ev $EV_ABS $ABS_MT_POSITION_Y "$3"; ev $EV_SYN $SYN_REPORT 0; }
send() { printf "$events" > "$buf" && cat "$buf" >&3; events=''; }

while read -r tag client x y mask hint; do
    [ "${tag:-}" = Pointer ] || continue
    case "${x:-}" in ''|*[!0-9]*) continue ;; esac
    case "${y:-}" in ''|*[!0-9]*) continue ;; esac
    case "${mask:-}" in ''|*[!0-9]*) continue ;; esac
    case "${hint:-}" in
        *ButtonPress-1*)
            finger 1 "$x" "$y"
            # A 3-pixel move right after the press, as the built-in VNC's touch script sends:
            # that script is the known-good reference for this device.
            ev $EV_ABS $ABS_MT_POSITION_Y $((y + 3)); ev $EV_SYN $SYN_REPORT 0
            ;;
        *ButtonRelease-1*)
            finger -1 "$x" "$y"
            ;;
        *)
            (( mask & 1 )) || continue   # moving without the button down is not a touch
            finger 1 "$x" "$y"
            ;;
    esac
    send
done
