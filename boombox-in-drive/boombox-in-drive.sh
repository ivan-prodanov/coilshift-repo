#!/bin/sh
# boombox-in-drive.sh - enable the Boombox/Megaphone buttons while driving,
# with no rootfs writes (2026.8.6-amd build only; offsets are per-build, see
# AGENTS.md).
#
#   boombox-in-drive.sh           apply the patch to every running UI process
#   boombox-in-drive.sh status    show patch state per pid
#   boombox-in-drive.sh restore   write the stock bytes back
#
# Mechanism (re-derived 2026-11-08 from the car's OWN dump,
# /usr/tesla/UI/lib/libQtCarGUI.so.1.0.0, BuildID 2595fa05...; the .so is
# .symtab-stripped but keeps a full .dynsym, so the functions below are named
# and addressed directly - no Ghidra pass needed). VERIFIED LIVE on-car:
# applying all five rows leaves "Play Current Media" and "Megaphone" enabled
# in Drive.
#
# ** TWO gates must be patched on this build - the gear check and the
#    park check are in DIFFERENT functions: **
#
# 1) Construction-time initial state - ExternalSpeakerSettings::makeBoomboxButtons
#    (0x2723b70). Builds the two buttons once and calls
#    VehicleUtils::inDrivingGear(true) (0x3aaf790) at 0x2723dcb; the result in
#    %r14d disables both buttons in a driving gear. Patch the 5-byte call to
#    `xor %eax,%eax; nop` (31 c0 90 90 90) -> result 0 -> neither built-in
#    disable fires. (This is the ONLY site coilshift's enable-boombox and the
#    old 2026.32.7 script touched - and it is NOT enough here, see below.)
#
# 2) RUNTIME re-evaluation - ExternalSpeakerSettings::updateButtonStates
#    (0x2724b20). Runs on EVERY state change (gear shift, screen open, phone
#    call) and re-computes enable/disable from VehicleUtils::isCarParked(b,b)
#    (0x3aaf880), called at 0x2724bb2 / 0x2724c3b / 0x2724c51 / 0x2724d65.
#    The moment you shift into Drive this fires, isCarParked() is false, and it
#    re-disables the buttons - OVERRIDING gate (1). This is why patching only
#    makeBoomboxButtons (coilshift / the old script) has no effect while
#    driving. Patch each 5-byte `call isCarParked` to `mov $1,%eax`
#    (b8 01 00 00 00) -> every in-function check reads "parked" -> the buttons
#    stay enabled. Scoped to this screen; the global isCarParked is untouched.
#
# What is deliberately NOT touched:
#   - VehicleUtils::isCarParked itself (0x3aaf880): shared by the whole UI;
#     we neutralise it only at the call sites inside updateButtonStates.
#   - ExternalSpeakerSettings::updateBoomboxButtonState (0x2724820): only sets
#     the button labels (no gear/park gate) - leave it.
#   - PhoneManager::inCall (the mic-conflict gate for Megaphone): untouched, so
#     Megaphone is still correctly disabled during a phone/video call.
#   - VehicleUtils::boomboxAllowed (0x3abcb80): the hardware/country gate. If
#     the car has no external speaker, no patch should or can help.
#
# NOTE ON THIS DUMP: the libQtCarGUI we extracted ALREADY has gate (1) at
# 0x2723dcb patched (reads 31 c0 90 90 90, not the stock call) - the car runs
# dev.coilshift.enable-boombox, which makes that one edit. Its `orig` bytes
# below (e8 c0 b9 38 01) are therefore the DERIVED stock call (e8 + rel32 to
# 0x3aaf790); the four isCarParked `orig` bytes were read back live and are
# exact. On a clean 2026.8.6-amd car `apply` matches `orig` and patches; on
# this coilshift car gate (1) reports "already patched" and gates (2) patch.
#
# NOTE: the buttons read their state at screen construction and on each
# updateButtonStates run. After apply, shift gear or re-open the Toybox ->
# Boombox screen to see the new state. Nothing persists a reboot or UI restart
# - re-run apply after every restart.
#
# restore writes the original bytes back. Offsets were derived from
# 2026.8.6-amd (libQtCarGUI BuildID 2595fa05...); they are NOT valid for any
# other build. tools/verify-car.sh should assert the rootfs fingerprint
# pre-flight.
set -eu

PATH=/usr/local/bin:/usr/bin:/sbin:/bin
export PATH

GUI_LIB=/usr/tesla/UI/lib/libQtCarGUI.so.1.0.0

# name|vaddr|orig|patched  (all five must be patched; see header)
#  gate 1 - inDrivingGear call in makeBoomboxButtons:
#    stock e8 c0 b9 38 01 = call 0x3aaf790;  patch 31 c0 90 90 90 = xor eax,eax;nop;nop;nop
#  gate 2 - the four isCarParked calls in updateButtonStates:
#    stock e8 .. = call 0x3aaf880 (@plt 0x14736e0);  patch b8 01 00 00 00 = mov $1,%eax
BB_ROWS='
boombox-driving-gate|2723dcb|e8c0b93801|31c0909090
parked-gate-updatebtn-1|2724bb2|e829ebd4fe|b801000000
parked-gate-updatebtn-2|2724c3b|e8a0ead4fe|b801000000
parked-gate-updatebtn-3|2724c51|e88aead4fe|b801000000
parked-gate-updatebtn-4|2724d65|e876e9d4fe|b801000000'

pids_for() { # path -> pids whose cmdline runs that path
    pgrep -f "$1( |\$)" 2>/dev/null || true
}

PROC_ROOT=${PROC_ROOT:-/proc}

ui_pids() { # -> pids of every process that runs the Boombox gate
    # The gate lives in libQtCarGUI, and bin/QtCar is the only binary in the
    # 2026.8.6-amd UI tree that links it (verified in the extracted rootfs),
    # so the target set is the QtCar process(es). Same anchored pgrep + maps
    # fallback as video-in-drive.sh (verify-car.sh asserts the pgrep form).
    got=$(pids_for '/usr/tesla/UI/bin/QtCar')
    if [ -n "$got" ]; then
        printf '%s\n' "$got"
        return
    fi
    for d in "$PROC_ROOT"/[0-9]*; do
        [ -r "$d/maps" ] || continue
        if grep -qm1 -E "$GUI_LIB( |\$)" "$d/maps" 2>/dev/null; then
            printf '%s\n' "${d#"$PROC_ROOT"/}"
        fi
    done
}

base_for() { # pid file -> load base of exactly that file (maps line ends
             # with the path, or a space for "(deleted)")
    grep -m1 -E "$2( |\$)" "/proc/$1/maps" 2>/dev/null | cut -d- -f1
}

read_bytes() { # pid addr count -> hex
    dd if="/proc/$1/mem" bs=1 count="$3" skip="$2" 2>/dev/null \
        | od -An -v -tx1 | tr -d ' \n'
}

write_bytes() { # pid addr hexbytes -> prints the real dd error on failure
    msg=$(printf "$(printf '%s' "$3" | sed 's/../\\x&/g')" \
        | dd of="/proc/$1/mem" bs=1 seek="$2" conv=notrunc 2>&1) || {
        echo "  pid $1: write @0x$(printf '%x' "$2") failed: $msg" >&2
        return 1
    }
}

patch_site() { # pid base vaddr orig patched mode label
    pid=$1 base=$2 va=$3 orig=$4 new=$5 mode=$6 label=$7
    addr=$((0x$base + 0x$va))
    len=$(( ${#orig} / 2 ))
    cur=$(read_bytes "$pid" "$addr" "$len")
    if [ "$mode" = apply ]; then
        if [ "$cur" = "$new" ]; then
            echo "  pid $pid: $label already patched"
        elif [ "$cur" = "$orig" ]; then
            write_bytes "$pid" "$addr" "$new" || return 1
            got=$(read_bytes "$pid" "$addr" "$len")
            if [ "$got" != "$new" ]; then
                echo "  pid $pid: $label readback failed ($got)" >&2
                return 1
            fi
            echo "  pid $pid: $label patched"
        else
            echo "  pid $pid: $label UNEXPECTED bytes ($cur) - wrong build? refused" >&2
            return 1
        fi
    elif [ "$mode" = restore ]; then
        if [ "$cur" = "$orig" ]; then
            echo "  pid $pid: $label already stock"
        elif [ "$cur" = "$new" ]; then
            write_bytes "$pid" "$addr" "$orig" || return 1
            echo "  pid $pid: $label restored"
        else
            echo "  pid $pid: $label UNEXPECTED bytes ($cur) - left alone" >&2
            return 1
        fi
    else
        if [ "$cur" = "$new" ]; then
            st=PATCHED
        elif [ "$cur" = "$orig" ]; then
            st=stock
        else
            st="UNKNOWN($cur)"
        fi
        echo "  pid $pid: $label $st"
    fi
}

patch_pid() { # pid mode
    pid=$1 mode=$2
    gbase=$(base_for "$pid" "$GUI_LIB")
    [ -n "$gbase" ] || return 0
    for row in $BB_ROWS; do
        IFS='|'
        set -- $row
        unset IFS
        patch_site "$pid" "$gbase" "$2" "$3" "$4" "$mode" "$1" || :
    done
}

iter() { # mode
    mode=$1
    seen=0
    for pid in $(ui_pids); do
        seen=1
        patch_pid "$pid" "$mode" || :
    done
    [ "$seen" = 1 ] || echo "  no process with $GUI_LIB mapped"
    [ "$seen" = 1 ] || echo "  (is the UI running? check: pgrep -f QtCar)"
}

do_apply() {
    echo "boombox-in-drive: applying to running UI processes"
    iter apply
    echo "done: shift gear or re-open the Toybox -> Boombox screen; nothing"
    echo "      persists a UI restart - re-run apply"
    echo "      status: $0 status   restore: $0 restore"
}

do_status() {
    echo "boombox-in-drive: status (2026.8.6-amd offsets)"
    iter status
}

do_restore() {
    echo "boombox-in-drive: restoring stock bytes"
    iter restore
}

[ "$(id -u)" = 0 ] || { echo "boombox-in-drive: root required" >&2; exit 1; }
case "${1:-apply}" in
    apply)   do_apply ;;
    status)  do_status ;;
    restore) do_restore ;;
    *)       echo "usage: $0 [apply|status|restore]" >&2; exit 1 ;;
esac
