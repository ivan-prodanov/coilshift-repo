#!/usr/bin/env bash
# =====================================================================================================================
#  probe_media.sh : READ-ONLY media-access probe for the shared car gateway (plan PLAN-B2 §4.9, gateway core 4d).
#  Run ON THE CAR as root (owner present), e.g. over the admin SSH:
#      ssh -p 13400 root@<car> 'bash -s' < probe_media.sh            # default: read-only
#      ssh -p 13400 root@<car> 'bash -s -- --with-stream' < probe_media.sh
#
#  Answers, for the gateway uid 4950 and for the helper child uid 4951 (+ groups audio 29, pipewire 1021):
#    1. Can that uid RECEIVE the camera multicast 224.0.0.155:<port> under the car's firewall (INPUT DROP)?
#       Root is captured alongside as the control: root>0 and uid=0 => firewall/permission; root=0 => no stream now.
#    2. Can that uid reach PipeWire (/var/run/pipewire), which the talk writer (pw-cat --target webcam) needs?
#
#  Default mode writes NOTHING: no DataValue, no file outside /tmp, no service change, and it sends nothing to
#  192.168.90.0/24 (a multicast RECEIVE is an IGMP join only, exactly what the stock dashcam recorders do; R22).
#  --with-stream (opt-in) additionally holds WEBCAM_webcamSessionActive=1 for the probe (the parked APE multicasts
#  the per-camera ports only then). That is a keep-awake DV, motion-safe, cleared by a trap on ANY exit plus a 60 s
#  watchdog. Use it only if the default run reports "no stream".
# =====================================================================================================================
set -u
PORTS="9901 9900 9896"                       # front (parked), back, right repeater
WITH_STREAM=0
[ "${1:-}" = "--with-stream" ] && WITH_STREAM=1
. /etc/gst-registry.vars 2>/dev/null
export GST_REGISTRY_UPDATE=no GST_REGISTRY_FORK=no
PW="PIPEWIRE_RUNTIME_DIR=/var/run/pipewire PIPEWIRE_CONFIG_NAME=client.conf"

RUN_ROOT=""
RUN_GW="chpst -u :4950:4950"                 # the gateway (agw) uid:gid
RUN_HELPER="chpst -u :4951:4951:29:1021"     # candidate helper child uid with audio + pipewire supplementary groups

# Temporary files live in a fresh folder only root can open (mktemp -d, mode 0700), never at fixed names in /tmp:
# another uid could plant a symlink or a file of its own there, and root would follow it, or run it (the watchdog).
T=$(mktemp -d /tmp/probe_media.XXXXXX) || { echo "mktemp -d failed; stop"; exit 1; }
cleanup() { trap '' INT TERM HUP; rm -rf "$T"; }   # a second signal must not cut the cleanup short
trap cleanup EXIT
trap 'exit 1' INT TERM HUP                   # a signal exits, so the EXIT trap runs once

if [ "$WITH_STREAM" = 1 ]; then
  D=/usr/local/bin/set-local-dv
  cat > "$T/wd.sh" <<'WD'
sleep 60
/usr/local/bin/set-local-dv CarServer WEBCAM_webcamSessionActive 0 >/dev/null 2>&1
WD
  setsid sh "$T/wd.sh" </dev/null >/dev/null 2>&1 &
  cleanup() {
    trap '' INT TERM HUP                     # a second signal must not cut the release short
    $D CarServer WEBCAM_webcamSessionActive 0 >/dev/null 2>&1
    for p in $(ls /proc | grep -E '^[0-9]+$'); do
      c=$(tr '\0' ' ' 2>/dev/null </proc/$p/cmdline 2>/dev/null)
      case "$c" in "sh $T/wd.sh"*) kill "$p" 2>/dev/null;; esac
    done 2>/dev/null
    rm -rf "$T"
    echo "STREAM_RELEASED sa=$(/usr/local/bin/get-local-dv CarServer WEBCAM_webcamSessionActive)"
  }
  $D CarServer WEBCAM_webcamSessionActive 1 >/dev/null 2>&1
  sleep 3
  echo "STREAM_HELD sa=$(/usr/local/bin/get-local-dv CarServer WEBCAM_webcamSessionActive)"
fi

cap_count() {   # $1 = runner prefix (may be empty), $2 = port  -> number of RTP buffers received in 4 s
  sh -c "$1 timeout 4 gst-launch-1.0 -v udpsrc multicast-group=224.0.0.155 multicast-iface=eth0 port=$2 \
    caps=\"application/x-rtp,media=video,encoding-name=H264,payload=96,clock-rate=90000\" ! fakesink silent=false" \
    2>/dev/null | grep -c "last-message = chain"
}
pw_ok() {       # $1 = runner prefix -> number of PipeWire nodes visible (0 = no access)
  sh -c "$PW $1 timeout 4 pw-cli ls Node" 2>/dev/null | grep -c "node.name"
}

echo "== identities =="
echo "root:   $(id)"
echo "gw:     $(sh -c "$RUN_GW id" 2>&1)"
echo "helper: $(sh -c "$RUN_HELPER id" 2>&1)"

echo "== 1. multicast receive (RTP buffers in 4 s; root = control) =="
for p in $PORTS; do
  r=$(cap_count "$RUN_ROOT" "$p"); g=$(cap_count "$RUN_GW" "$p"); h=$(cap_count "$RUN_HELPER" "$p")
  if [ "$r" -eq 0 ]; then v="NO_STREAM (nothing flowing now; re-run with --with-stream)"
  elif [ "$g" -gt 0 ] && [ "$h" -gt 0 ]; then v="OK"
  else v="BLOCKED_FOR_NONROOT (firewall or permission)"; fi
  echo "port $p: root=$r gw=$g helper=$h -> $v"
done

echo "== 2. PipeWire access (nodes visible) =="
echo "root=$(pw_ok "$RUN_ROOT") gw=$(pw_ok "$RUN_GW") helper=$(pw_ok "$RUN_HELPER")  (want helper > 0; gw may be 0)"

echo "== 3. talk writer argv (raw PCM on stdin; 0.5 s of SILENCE into the unrouted 'webcam' node) =="
# The exterior route is NOT armed here, so the webcam node feeds nothing: this is inaudible by construction.
F=$(pw-link -l 2>/dev/null | grep -c "webcam:monitor")
if [ "$F" -gt 0 ]; then
  echo "SKIPPED: webcam:monitor is linked to something (route armed elsewhere); not writing even silence"
else
  rc_pw=$(sh -c "head -c 48000 /dev/zero | $PW $RUN_HELPER timeout 5 pw-cat --playback --target webcam --format s16 --rate 48000 --channels 1 - >$T/pwcat.err 2>&1; echo \$?")
  echo "pw-cat stdin: rc=$rc_pw $(head -c 160 "$T/pwcat.err" 2>/dev/null | tr '\n' ' ')"
  if command -v aplay >/dev/null 2>&1; then
    rc_ap=$(sh -c "head -c 48000 /dev/zero | $PW $RUN_HELPER timeout 5 aplay -q -D webcam -t raw -f S16_LE -r 48000 -c 1 - >$T/aplay.err 2>&1; echo \$?")
    echo "aplay stdin:  rc=$rc_ap $(head -c 160 "$T/aplay.err" 2>/dev/null | tr '\n' ' ')"
  else
    echo "aplay: not installed"
  fi
  rm -f "$T/pwcat.err" "$T/aplay.err"
  echo "(want rc=0 for the writer the helper will use)"
fi
echo "== 4. gateway host checks (for the shared gateway core) =="
# VIN readable by the gateway uid (prints only yes/no, never the VIN)
sh -c "$RUN_GW test -r /var/etc/vin" 2>/dev/null && echo "vin readable by 4950: yes" || echo "vin readable by 4950: NO"
# /var/lib persistence + exec: mount options of the filesystem holding /var/lib
awk '$2=="/var" || $2=="/var/lib" {print "mount", $2, $3, $4}' /proc/mounts
# outbound TCP from uid 4950 on the Wi-Fi side (pass PI_IP/PI_PORT in the env to test; refused = got out, timeout = blocked)
if [ -n "${PI_IP:-}" ]; then
  out=$(sh -c "$RUN_GW timeout 4 socat -u /dev/null TCP:${PI_IP}:${PI_PORT:-22},connect-timeout=3" 2>&1); rc=$?
  echo "4950 -> ${PI_IP}:${PI_PORT:-22}: rc=$rc $(echo "$out" | head -c 120 | tr '\n' ' ')"
else
  echo "4950 outbound to the Pi: skipped (set PI_IP=<pi wifi ip> [PI_PORT=22])"
fi
# the directory the car's own runsvdir supervises (where per-boot service links would go)
for p in $(ls /proc | grep -E '^[0-9]+$'); do
  c=$(tr '\0' ' ' 2>/dev/null </proc/$p/cmdline 2>/dev/null)
  case "$c" in runsvdir*|*/runsvdir\ *) echo "runsvdir: $c" | cut -c1-160;; esac
done 2>/dev/null

echo "== done (read-only$([ "$WITH_STREAM" = 1 ] && echo ' + keep-awake DV, released')) =="
