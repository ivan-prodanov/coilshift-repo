#!/bin/sh
# fart-in-drive.sh - keep the Emissions "fart" button usable while driving and
# let the external fart actually play in gear. Builds 2026.8.6-amd, 2026.38.3.2 and 2026.38.6
# (offsets are per-build; the build is detected from the bytes in memory, and an
# unknown build is refused). No rootfs writes - patches /proc/<QtCar pid>/mem;
# nothing persists a UI restart, so re-run apply after every restart.
#
#   fart-in-drive.sh            apply   |   status   |   restore
#
# Derived 2026-11-08 from the car's own libQtCarGUI.so.1.0.0 (BuildID 2595fa05...,
# .symtab-stripped but full .dynsym); 2026.38.3.2 (BuildID 0a199995...) added
# 2026-11-20 from the OTA dump - same three gates, same callees, only addresses and
# gate 2's branch encoding (je rel8 -> je rel32) changed. Addresses below are for
# 2026.8.6. Three gates, same family as boombox-in-drive:
#
#   1) BUTTON ENABLE - FartModeView::updateInfo (0x26fdb40): the on-demand fart
#      button's enabled state is set to VehicleUtils::customPedestrianWarningSoundAllowed()
#      (false while driving). Call at 0x26fde4d; force it to return 1 (mov $1,%eax)
#      so the button stays enabled. THIS alone fixes "button greys out in gear".
#   2) EXTERNAL ACTION - FartModeManager::playExternalFart (0x2701020): gated by
#      VehicleUtils::isCarParked at 0x2701094; while driving it tail-calls
#      playInteriorFart instead. Force isCarParked=1 so the external fart plays.
#   3) SOUND PLAYBACK - ExternalSoundPlayer::playSound (0x2708e00): returns early
#      via VehicleUtils::inDrivingGear(true) at 0x2708e29. Force it to 0 (not in
#      gear) so the external speaker actually emits in motion.
#
# Patch 1 is the minimum (button usable; plays INTERIOR fart in gear). Add 2+3
# for the EXTERNAL fart (pedestrian speaker) in motion.
set -eu
PATH=/usr/local/bin:/usr/bin:/sbin:/bin; export PATH
GUI_LIB=/usr/tesla/UI/lib/libQtCarGUI.so.1.0.0

# name|vaddr|orig|patched - one table per build.
#  vaddr is the ELF virtual address (load base + vaddr = address in the process),
#  NOT the file offset: on 2026.38.3.2 the code segment sits 0x1000 above its file
#  offset, so csdk's `rules test` offsets there are vaddr - 0x1000.
#  Gate1/2 force a call result to 1 (b8 01 00 00 00 = mov $1,%eax).
#  Gate3 forces inDrivingGear to 0 (31 c0 90 90 90 = xor %eax,%eax; nop; nop; nop).
ROWS_2026_8_6='
fart-button-enable|26fde4d|e8cef9d5fe|b801000000
fart-external-action|2701094|e84726d7fe|b801000000
fart-sound-playback|2708e29|e8f248d7fe|31c0909090'
ROWS_2026_38_3_2='
fart-button-enable|2e95f2d|e82ef255ff|b801000000
fart-external-action|2e9b3e9|e8628b55ff|b801000000
fart-sound-playback|2ea8169|e8c2bd54ff|31c0909090'
ROWS_2026_38_6='
fart-button-enable|2f070ed|e83eb155ff|b801000000
fart-external-action|2f0c5a9|e8424a55ff|b801000000
fart-sound-playback|2f19329|e8a27c54ff|31c0909090'
# build name : table variable
BUILDS='2026.8.6-amd:ROWS_2026_8_6 2026.38.3.2:ROWS_2026_38_3_2 2026.38.6:ROWS_2026_38_6'

PROC_ROOT=${PROC_ROOT:-/proc}
pids_for() { pgrep -f "$1( |\$)" 2>/dev/null || true; }
ui_pids() {
    got=$(pids_for '/usr/tesla/UI/bin/QtCar'); [ -n "$got" ] && { printf '%s\n' "$got"; return; }
    for d in "$PROC_ROOT"/[0-9]*; do [ -r "$d/maps" ] || continue
        grep -qm1 -E "$GUI_LIB( |\$)" "$d/maps" 2>/dev/null && printf '%s\n' "${d#"$PROC_ROOT"/}"; done
}
base_for() { grep -m1 -E "$2( |\$)" "/proc/$1/maps" 2>/dev/null | cut -d- -f1; }
read_bytes() { dd if="/proc/$1/mem" bs=1 count="$3" skip="$2" 2>/dev/null | od -An -v -tx1 | tr -d ' \n'; }
write_bytes() { msg=$(printf "$(printf '%s' "$3" | sed 's/../\\x&/g')" | dd of="/proc/$1/mem" bs=1 seek="$2" conv=notrunc 2>&1) || { echo "  pid $1: write @0x$(printf '%x' "$2") failed: $msg" >&2; return 1; }; }

patch_site() {
    pid=$1 base=$2 va=$3 orig=$4 new=$5 mode=$6 label=$7
    addr=$((0x$base + 0x$va)); len=$(( ${#orig} / 2 )); cur=$(read_bytes "$pid" "$addr" "$len")
    if [ "$mode" = apply ]; then
        if [ "$cur" = "$new" ]; then echo "  pid $pid: $label already patched"
        elif [ "$cur" = "$orig" ]; then write_bytes "$pid" "$addr" "$new" || return 1
            got=$(read_bytes "$pid" "$addr" "$len"); [ "$got" = "$new" ] && echo "  pid $pid: $label patched" || { echo "  pid $pid: $label readback failed ($got)" >&2; return 1; }
        else echo "  pid $pid: $label UNEXPECTED bytes ($cur) - wrong build? refused" >&2; return 1; fi
    elif [ "$mode" = restore ]; then
        if [ "$cur" = "$orig" ]; then echo "  pid $pid: $label already stock"
        elif [ "$cur" = "$new" ]; then write_bytes "$pid" "$addr" "$orig" || return 1; echo "  pid $pid: $label restored"
        else echo "  pid $pid: $label UNEXPECTED bytes ($cur) - left alone" >&2; return 1; fi
    else
        if [ "$cur" = "$new" ]; then st=PATCHED; elif [ "$cur" = "$orig" ]; then st=stock; else st="UNKNOWN($cur)"; fi
        echo "  pid $pid: $label $st"
    fi
}
# detect_build PID BASE: print "<build> <table var>" for the one table whose every gate
# reads as stock or patched in this process; fail when none fits (unknown build).
detect_build() {
    dpid=$1 dbase=$2
    for b in $BUILDS; do
        eval "rows=\$${b#*:}"; fits=1
        for row in $rows; do IFS='|'; set -- $row; unset IFS
            cur=$(read_bytes "$dpid" $((0x$dbase + 0x$2)) $(( ${#3} / 2 )))
            [ "$cur" = "$3" ] || [ "$cur" = "$4" ] || { fits=0; break; }
        done
        [ "$fits" = 1 ] && { echo "${b%%:*} ${b#*:}"; return 0; }
    done
    return 1
}
patch_pid() { pid=$1 mode=$2; gbase=$(base_for "$pid" "$GUI_LIB"); [ -n "$gbase" ] || return 0
    found=$(detect_build "$pid" "$gbase") || {
        echo "  pid $pid: libQtCarGUI build not recognised (known: $(for b in $BUILDS; do printf '%s ' "${b%%:*}"; done)) - refused; offsets are per-build" >&2
        return 1; }
    echo "  pid $pid: build ${found% *}"
    eval "rows=\$${found#* }"
    for row in $rows; do IFS='|'; set -- $row; unset IFS; patch_site "$pid" "$gbase" "$2" "$3" "$4" "$mode" "$1" || :; done; }
iter() { mode=$1; seen=0; for pid in $(ui_pids); do seen=1; patch_pid "$pid" "$mode" || :; done
    [ "$seen" = 1 ] || { echo "  no process with $GUI_LIB mapped"; echo "  (is the UI running? pgrep -f QtCar)"; }; }

[ "$(id -u)" = 0 ] || { echo "fart-in-drive: root required" >&2; exit 1; }
case "${1:-apply}" in
    apply)   echo "fart-in-drive: applying"; iter apply; echo "done: re-open Toybox -> Emissions; nothing persists a UI restart";;
    status)  echo "fart-in-drive: status"; iter status;;
    restore) echo "fart-in-drive: restoring stock bytes"; iter restore;;
    *) echo "usage: $0 [apply|status|restore]" >&2; exit 1;;
esac
