#!/bin/sh
# horn-in-drive.sh - make the Boombox "Horn Sound" + "Replace Horn" toggles behave in
# Drive exactly like in Park: freely checkable/uncheckable, and the chosen horn sound
# (GUI_pesTheChosenHorn, e.g. OldHorn) actually plays when you honk while driving.
# 2026.8.6-amd build; offsets are per-build. Patches /proc/<QtCar pid>/mem; reverts on
# UI restart (re-run apply). Derived by local RE of libQtCarGUI in tesla-dump 2026-11-10.
#
# The horn settings UI greys/clears the toggles in Drive via gear checks. We force each
# to read "parked"/"not-in-gear" so the screen behaves identically to Park. Playback is
# NOT drive-gated (only a factory GPS geofence), so once a toggle is on, the horn sounds.
#
#   1) SCREEN GREY-OUT (the one that actually disables "Horn Sound"):
#      ExternalSpeakerSettings::externalSpeakerStateChanged @0x2725148 calls isCarParked;
#      when not parked it calls ExternalSoundDropdown::enableSwitch(false). Force
#      isCarParked=1 so it takes the parked path and never disables the switch.
#   2) ExternalSoundDropdown::updateActiveState @0x270b89a inDrivingGear -> 0 (behave parked)
#   3) ExternalSoundDropdown::updateButtonAvailabilty @0x270bacd inDrivingGear -> 0
#   4) ExternalSpeakerManager::onShiftStateChanged @0x2721b87: skip the setValue(false) that
#      clears Replace Horn (GUI_disableHorn) on shift-to-Drive, so your choice persists.
#   6) REPLACE HORN SWITCH VISIBLE: the ExternalSpeakerSettings constructor @0x2727019 only
#      creates the "Replace Horn" switch when VehicleUtils::canSuppressHorn() is true, i.e.
#      VAPI_countryCode is US, CA or MX. Force it to 1 so the switch exists elsewhere too.
#      The constructor runs when the Boombox screen is first built in a UI session, so apply
#      BEFORE opening Toybox > Boombox after a UI restart; if it was already opened, the
#      switch shows only after the screen is rebuilt (a UI restart, which also undoes this
#      live patch - use the coilshift package for this one).
#      With it on, GUI_disableHorn silences the real horn (and gate 4 keeps that in Drive).
#
# NOTE: boombox-in-drive already patches ExternalSpeakerSettings::updateButtonStates
# (the 4 isCarParked for the play buttons on this same screen) - keep it applied too.
set -eu
PATH=/usr/local/bin:/usr/bin:/sbin:/bin; export PATH
GUI_LIB=/usr/tesla/UI/lib/libQtCarGUI.so.1.0.0

# name|vaddr|orig|patched
#  Gate 1 routes externalSpeakerStateChanged to the parked path (where enableSwitch is called);
#  Gate 5 forces customPedestrianWarningSoundAllowed=1 so that enableSwitch(TRUE) -> Horn Sound
#  stays interactive. BOTH are needed (gate 1 alone -> enableSwitch(false) in Drive).
HORN_ROWS='
horn-screen-stayparked|2725148|e893e5d4fe|b801000000
horn-switch-enable|2725245|e8d685d3fe|b801000000
horn-activestate-nogear|270b89a|e8811ed7fe|31c0909090
horn-avail-nogear|270bacd|e84e1cd7fe|31c0909090
horn-noshiftreset|2721b87|7417|9090
horn-replace-switch|2727019|e8920dd6fe|b801000000'

PROC_ROOT=${PROC_ROOT:-/proc}
ui_pids() {
    got=$(pgrep -f '/usr/tesla/UI/bin/QtCar( |$)' 2>/dev/null); [ -n "$got" ] && { printf '%s\n' "$got"; return; }
    for d in "$PROC_ROOT"/[0-9]*; do [ -r "$d/maps" ] || continue
        grep -qm1 -E "$GUI_LIB( |\$)" "$d/maps" 2>/dev/null && printf '%s\n' "${d#"$PROC_ROOT"/}"; done
}
base_for() { grep -m1 -E "$2( |\$)" "/proc/$1/maps" 2>/dev/null | cut -d- -f1; }
read_bytes() { dd if="/proc/$1/mem" bs=1 count="$3" skip="$2" 2>/dev/null | od -An -v -tx1 | tr -d ' \n'; }
write_bytes() { printf "$(printf '%s' "$3" | sed 's/../\\x&/g')" | dd of="/proc/$1/mem" bs=1 seek="$2" conv=notrunc 2>/dev/null || { echo "  pid $1: write @0x$(printf '%x' "$2") failed" >&2; return 1; }; }

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
patch_pid() { pid=$1 mode=$2; gbase=$(base_for "$pid" "$GUI_LIB"); [ -n "$gbase" ] || return 0
    for row in $HORN_ROWS; do IFS='|'; set -- $row; unset IFS; patch_site "$pid" "$gbase" "$2" "$3" "$4" "$mode" "$1" || :; done; }
iter() { mode=$1; seen=0; for pid in $(ui_pids); do seen=1; patch_pid "$pid" "$mode" || :; done
    [ "$seen" = 1 ] || echo "  no process with $GUI_LIB mapped (is QtCar running?)"; }

[ "$(id -u)" = 0 ] || { echo "horn-in-drive: root required" >&2; exit 1; }
case "${1:-apply}" in
    apply)   echo "horn-in-drive: applying (toggles behave like Park in Drive)"; iter apply; echo "done: open Boombox settings in Drive - Horn Sound + Replace Horn should be fully interactive; honk plays GUI_pesTheChosenHorn. Reverts on UI restart.";;
    status)  echo "horn-in-drive: status (2026.8.6-amd)"; iter status;;
    restore) echo "horn-in-drive: restoring stock bytes"; iter restore;;
    *) echo "usage: $0 [apply|status|restore]" >&2; exit 1;;
esac
