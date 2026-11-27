#!/bin/bash
# supervise.sh: the one CoilShift service ("supervise", restart always) of Direct Link and of
# Direct Summon. It runs today's runit under CoilShift, so the reviewed run, finish and log
# scripts work unchanged (summonhelper's finish still aborts and disconnects after any exit):
#   1. wait until this boot's apply has written the marker (egress lock and takeover first);
#   2. stop any runsvdir already on our svdir (a previous instance's; checked by cmdline), then
#      start ours;
#   3. on TERM, INT or HUP, before anything else (no API call first): `sv -w 15 down` every
#      service, summonhelper first (its finish runs), then HUP every runsvdir on our svdir, and
#      exit 0;
#   4. if runsvdir dies on its own, exit 1 so CoilShift restarts us;
#   5. Direct Summon only, "take it back": at start and every 10 s, at most once a minute, when
#      summonhelper is set up (linked) and not running and com.tesla.Webrtc has an owner, the
#      same bounded takeover as apply; if the name stays taken, stock is handed back and the
#      status says so. It runs as a background job, so a stop signal is never held up by it;
#   6. Direct Link only: every 10 s, from the start, keep the panel's pairing address current
#      (publish_car_host: the car's tailnet IPv4, which the panel shows as a QR), and keep the
#      uid 4950 egress lock as apply left it, its jump first in OUTPUT (egress_keep).
# bash 3.2. Under CSDK_FAKE_ROOT (direct-link/test) every tool is a fake.
set -u
: "${CSDK_PACKAGE_DIR:?}" "${CSDK_RUN:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
. "$CSDK_PACKAGE_DIR/lib/gateway.sh"
. "$CSDK_PACKAGE_DIR/lib/common.sh"
CSDK_MAX_TIME=3

PIDFILE=$CSDK_RUN/runsvdir.pid
TOOK=$CSDK_RUN/take-back.attempted
TAKE_BACK=0
[ "${CSDK_PKG:-}" = io.github.ivan-prodanov.direct-summon ] && TAKE_BACK=1
PANEL=0
[ "${CSDK_PKG:-}" = io.github.ivan-prodanov.direct-link ] && PANEL=1
panel_tick() {
    [ "$PANEL" = 1 ] && [ $((NOW % 10)) = 0 ] || return 0
    publish_car_host
    # Only once this boot's apply is done (runsvdir started), so it never races apply's own writes.
    [ -n "$RSV" ] && [ "$(egress_keep)" = restored ] && say "egress lock restored (a rule had been put above its jump)"
    return 0
}
WAIT_NOTE=600 TAKE_EVERY=10 TAKE_MIN=60
if [ -n "$R" ]; then # test seams, fake root only
    WAIT_NOTE=${T_WAIT_NOTE:-$WAIT_NOTE} TAKE_EVERY=${T_TAKE_EVERY:-$TAKE_EVERY} TAKE_MIN=${T_TAKE_MIN:-$TAKE_MIN}
fi
RSV= TB=
LAST_TAKE=-1000000
NOW=0 # seconds since start, counted by the loops (no date arithmetic on the car's busybox)

say() { csdk_log info "supervise: $*" >/dev/null 2>&1 || true; }

# stop_all: every service down (Summon first, so its finish runs before the others go), then
# HUP each runsvdir on our svdir (checked by cmdline), which stops its runsv processes.
stop_all() {
    local svc pid
    for svc in summonhelper camhelper agw; do
        [ -d "$SVROOT/$svc" ] || continue
        do_sv -w 15 down "$SVROOT/$svc" >/dev/null 2>&1 || true
    done
    for pid in $RSV $(runsvdir_pids "$SVDIR"); do
        is_runsvdir_on "$pid" "$SVDIR" && { do_kill -HUP "$pid" || true; }
    done
}

on_signal() {
    trap '' TERM INT HUP
    stop_all
    if [ -n "$TB" ]; then kill -TERM "$TB" 2>/dev/null; wait "$TB" 2>/dev/null; fi # our own take-back job
    [ -n "$RSV" ] && wait "$RSV" 2>/dev/null
    rm -f "$PIDFILE"
    say "stopped: services down, runsvdir gone"
    exit 0
}
trap on_signal TERM INT HUP

# take_back_job: the takeover itself, run in the background. Writes $TOOK when it acted.
take_back_job() {
    trap - TERM INT HUP
    name_owned || exit 0
    : >"$TOOK"
    say "com.tesla.Webrtc is taken and summonhelper is not running: taking it back"
    if take_name || [ -n "$(summon_pids)" ]; then
        set_status "$(cut -d' ' -f1 "$CSDK_RUN/status.apply" 2>/dev/null || echo ok)" "$(cut -d' ' -f2- "$CSDK_RUN/status.apply" 2>/dev/null || true)"
    else
        hand_back || true
        set_status degraded "Summon waiting: com.tesla.Webrtc taken"
    fi
}

# take_back: Direct Summon's "take it back" (owner, 2026-11-26), only when summonhelper is set up.
take_back() {
    [ "$TAKE_BACK" = 1 ] && [ -L "$SVDIR/summonhelper" ] || return 0
    [ -z "$(summon_pids)" ] || return 0
    [ $((NOW - LAST_TAKE)) -ge "$TAKE_MIN" ] || return 0
    rm -f "$TOOK"
    take_back_job &
    TB=$!
    wait "$TB" 2>/dev/null
    TB=
    [ -e "$TOOK" ] && LAST_TAKE=$NOW
    return 0
}

# 1. this boot's marker
noted=0
panel_tick
until [ -f "$MARKER" ] && [ "$(cut -d' ' -f2 "$MARKER" 2>/dev/null)" = "$(boot_id)" ]; do
    sleep 1 &
    wait $! 2>/dev/null
    NOW=$((NOW + 1))
    panel_tick
    if [ "$noted" = 0 ] && [ "$NOW" -ge "$WAIT_NOTE" ]; then
        noted=1
        set_status degraded "waiting for this boot's apply"
    fi
done

# 2. any runsvdir already on our svdir, then ours
if [ -n "$(runsvdir_pids "$SVDIR")" ]; then
    say "stopping the runsvdir a previous instance left"
    stop_all
    i=0
    while [ -n "$(runsvdir_pids "$SVDIR")" ] && [ "$i" -lt 20 ]; do sleep 1; i=$((i + 1)); done
fi
take_back
rsvdir=$(tool runsvdir) || exit 1
"$rsvdir" -P "$SVDIR" </dev/null >/dev/null 2>&1 & # not via a function: $! must be runsvdir itself
RSV=$!
echo "$RSV" >"$PIDFILE"
say "runsvdir $RSV on $SVDIR"

# 3-5. watch
since=0
while kill -0 "$RSV" 2>/dev/null; do
    sleep 1 &
    wait $! 2>/dev/null
    NOW=$((NOW + 1))
    panel_tick
    since=$((since + 1))
    if [ "$since" -ge "$TAKE_EVERY" ]; then since=0; take_back; fi
done
wait "$RSV" 2>/dev/null
say "runsvdir exited on its own; CoilShift restarts us"
rm -f "$PIDFILE"
exit 1
