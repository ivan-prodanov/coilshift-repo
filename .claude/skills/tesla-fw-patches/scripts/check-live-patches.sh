#!/bin/sh
# check-live-patches.sh - READ-ONLY. Report the live byte-patch state of every
# gate in this repo's .sh patchers, on the car, without copying anything to it.
#
# It pipes a probe over SSH stdin, finds the process mapping libQtCarGUI,
# resolves its load base, and reads the bytes at each gate - comparing to the
# known stock / patched values. It NEVER writes to /proc/mem or to the car's
# filesystem. (apply/restore live in each feature's own .sh.)
#
#   .claude/skills/tesla-fw-patches/scripts/check-live-patches.sh
#
# Override the connection with CAR_SSH, e.g.:
#   CAR_SSH="-p 13400 root@<car host>" check-live-patches.sh
# (default: CAR_SSH from the repo's local.env, which is gitignored)
#
# Offsets are for libQtCarGUI build 2026.8.6-amd only; on another build the gates
# read UNKNOWN(<bytes>). Keep the rows below in sync with the feature .sh files.
set -eu
root=$(cd "$(dirname "$0")/../../../.." && pwd)
[ -n "${CAR_SSH:-}" ] || [ ! -f "$root/local.env" ] || . "$root/local.env"
: "${CAR_SSH:?set CAR_SSH or put it in local.env}"

ssh -o ConnectTimeout=20 $CAR_SSH 'bash -s' <<'REMOTE'
GUI_LIB=/usr/tesla/UI/lib/libQtCarGUI.so.1.0.0
# feature|name|vaddr|orig|patched   (mirror of the feature .sh rows)
ROWS='
fart|button-enable|26fde4d|e8cef9d5fe|b801000000
fart|external-action|2701094|e84726d7fe|b801000000
fart|sound-playback|2708e29|e8f248d7fe|31c0909090
boombox|driving-gate|2723dcb|e8c0b93801|31c0909090
boombox|parked-updatebtn-1|2724bb2|e829ebd4fe|b801000000
boombox|parked-updatebtn-2|2724c3b|e8a0ead4fe|b801000000
boombox|parked-updatebtn-3|2724c51|e88aead4fe|b801000000
boombox|parked-updatebtn-4|2724d65|e876e9d4fe|b801000000
horn|screen-stayparked|2725148|e893e5d4fe|b801000000
horn|switch-enable|2725245|e8d685d3fe|b801000000
horn|activestate-nogear|270b89a|e8811ed7fe|31c0909090
horn|avail-nogear|270bacd|e84e1cd7fe|31c0909090
horn|noshiftreset|2721b87|7417|9090
horn|replace-switch|2727019|e8920dd6fe|b801000000
reverse|launcher-reverseGate|1c67928|0f95c3|b30190
reverse|AppWindow-allowInReverse|2581eb0|0fb687760800|b801000000c3
reverse|AppWindow-dismissInReverse|1c9f250|4883ec08488b|b800000000c3
reverse|Coriander-allowInReverse|1c8faf0|488bbf200800|b801000000c3
reverse|Coriander-dismissInReverse|1c8fb20|488bbf200800|b800000000c3
reverse|CamV2-gearChanged-noPin|259392d|75a5|eba5
reverse|Cam-gearChanged-noPin|258e17d|75b5|ebb5
reverse|CamV2-dismissDisplaced|25932e4|75e5|ebe5
reverse|Cam-dismissDisplaced|258c3d4|75e5|ebe5'
PIDS=""
for d in /proc/[0-9]*; do [ -r "$d/maps" ] || continue
  grep -qm1 -E "$GUI_LIB( |\$)" "$d/maps" 2>/dev/null && PIDS="$PIDS ${d#/proc/}"; done
[ -n "$PIDS" ] || { echo "no process maps $GUI_LIB (is the UI running / car parked?)"; exit 0; }
echo "libQtCarGUI mapped in PID(s):$PIDS"
for pid in $PIDS; do
  comm=$(tr '\0' ' ' < /proc/$pid/cmdline 2>/dev/null | cut -c1-40)
  base=$(grep -m1 -E "$GUI_LIB( |\$)" "/proc/$pid/maps" 2>/dev/null | cut -d- -f1)
  echo "=== pid $pid ($comm) base=0x$base ==="
  prev=""
  for row in $ROWS; do
    IFS='|'; set -- $row; unset IFS
    feat=$1 name=$2 va=$3 orig=$4 new=$5
    addr=$((0x$base + 0x$va)); len=$(( ${#orig} / 2 ))
    cur=$(dd if="/proc/$pid/mem" bs=1 count="$len" skip="$addr" 2>/dev/null | od -An -v -tx1 | tr -d ' \n')
    if [ "$cur" = "$new" ]; then st="PATCHED"; elif [ "$cur" = "$orig" ]; then st="stock"; else st="UNKNOWN($cur)"; fi
    [ "$feat" != "$prev" ] && { echo "  [$feat]"; prev=$feat; }
    printf "    %-26s %s\n" "$name" "$st"
  done
done
REMOTE
