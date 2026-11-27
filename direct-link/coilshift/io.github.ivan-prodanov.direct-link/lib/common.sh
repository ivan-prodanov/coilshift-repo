# common.sh: shared by Direct Link and Direct Summon (hooks and bin/supervise.sh). bash 3.2.
#
# Everything here is ported from the gateway's deploy/install-car.sh at the commit in
# lib/gateway.sh: the same egress lock, the same takeover and hand-back of com.tesla.Webrtc, and
# the same runit run/finish/log scripts. Only the binary paths change (the binaries now come
# from the package images, bound at /run/direct-link-bin and /run/direct-summon-bin); every
# other path, uid and name is today's. One deliberate difference (direct mode, 2026-11-27):
# agw serves the car's own tailnet address (`serve -tailnet`) instead of dialling the Pi, so
# its run script waits for tailscale0 and the egress lock lets out only its replies there. direct-summon carries a byte-identical copy
# (direct-link/sync.sh; direct-link/test/run.sh checks it).
#
# CSDK_FAKE_ROOT is the SDK harness only (empty on a car). Under it every system tool runs as a
# fake from $CSDK_FAKE_ROOT/.fakes, never the real one, and host paths are read and written
# under the fake root.

R=${CSDK_FAKE_ROOT:-}
PROC=$R/proc

GW_DIR=/var/lib/airgapp-gw
GW_STATE=$GW_DIR/state
CONF=$GW_DIR/direct-link.conf
HELPER_DIR=/var/lib/airgapp-camhelper
HELPER_STATE=/run/airgapp-camhelper-state
INSTALLER_SVDIR=$GW_DIR/svdir
UID_GW=4950
GID_GW=4950
RUN_AS=":$UID_GW:$GID_GW"
# agw listens on the car's own tailnet address (the IPv4 on tailscale0, from CoilShift's Tailscale
# package) at GW_PORT; nothing dials out (direct mode, 2026-11-27; the Pi forwarder is gone).
TS_IFACE=tailscale0
GW_PORT=17100
TS_PKG_DIR=/var/lib/coilshift/pkgs/dev.coilshift.tailscale
SUMMON_UID=1337
GW_GID=$GID_GW
SUMMON_HOME=/var/lib/airgapp-summon
SUMMON_STATE=$SUMMON_HOME/state
SUMMON_RUN_DIR=/run/airgapp-summon
VIN_FILE=/var/etc/vin
WEBRTC_SV=/etc/sv/webrtc-comms
WEBRTC_PIDFILE=/run/service/webrtc-comms/webrtc-comms.pid
EGRESS_CHAIN=AIRGAPP_GW_OUT
EGRESS_JUMP="-m owner --uid-owner $UID_GW -j $EGRESS_CHAIN"

DL_BIN=/run/direct-link-bin
DS_BIN=/run/direct-summon-bin
DL_SV=/run/direct-link-sv
DS_SV=/run/direct-summon-sv
AGW_BIN=$DL_BIN/agw
HELPER_BIN=$DL_BIN/camhelper
SUMMON_BIN=$DS_BIN/summonhelper
AGW_LOG=$GW_DIR/svlog/agw
HELPER_LOG=$HELPER_DIR/svlog
SUMMON_LOG=$GW_DIR/svlog/summonhelper

SVROOT=${CSDK_RUN:-/nonexistent}/sv
SVDIR=${CSDK_RUN:-/nonexistent}/svdir
MARKER=${CSDK_RUN:-/nonexistent}/applied

# ---- tools ----

# tool NAME prints the tool to run: the car's own, or under the harness its fake. A missing
# fake is an error, never a fallback to the real tool.
tool() {
    if [ -n "$R" ]; then
        if [ -x "$R/.fakes/$1" ]; then printf '%s' "$R/.fakes/$1"; return 0; fi
        echo "no fake for $1 under CSDK_FAKE_ROOT" >&2
        return 127
    fi
    command -v "$1" 2>/dev/null || printf '%s' "$1"
}
do_sv()        { local t; t=$(tool sv) || return 127; "$t" "$@"; }
do_iptables()  { local t; t=$(tool iptables) || return 127; "$t" "$@"; }
do_ip6tables() { local t; t=$(tool ip6tables) || return 127; "$t" "$@"; }
do_kill()      { local t; t=$(tool kill) || return 127; "$t" "$@"; }
do_chpst()     { local t; t=$(tool chpst) || return 127; "$t" "$@"; }
do_ip()        { local t; t=$(tool ip) || return 127; "$t" "$@"; }

# The absolute paths baked into the generated run scripts, as install-car.sh bakes them
# (`command -v`). Under the harness they are the fakes; such scripts are never run.
IPT=$(tool iptables || true)
IP6T=$(tool ip6tables || true)
GDBUS=$(tool gdbus || true)
TIMEOUT=$(tool timeout || true)

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" 2>/dev/null | cut -d' ' -f1
    else shasum -a 256 "$1" 2>/dev/null | cut -d' ' -f1; fi
}

# as_root PATH...: chown 0:0 on a car; the harness records no owners for plain files.
as_root() { [ -n "$R" ] || chown 0:0 "$@"; }
owned_by() { local o=$1; shift; [ -n "$R" ] || chown "$o" "$@"; }

boot_id() { cat "$PROC/sys/kernel/random/boot_id" 2>/dev/null || echo unknown; }

# ---- checks (install-car.sh) ----

# tailscale_addr prints the car's tailnet IPv4 (the one on tailscale0, inside 100.64.0.0/10), or nothing.
tailscale_addr() {
    local a o2
    a=$(do_ip -4 -o addr show dev "$TS_IFACE" 2>/dev/null |
        sed -n 's/.* inet \(100\.[0-9]\{1,3\}\.[0-9]\{1,3\}\.[0-9]\{1,3\}\)\/.*/\1/p' | head -n 1)
    [ -n "$a" ] || return 0
    o2=${a#100.}; o2=${o2%%.*}
    [ "$o2" -ge 64 ] && [ "$o2" -le 127 ] && printf '%s' "$a"
    return 0
}

# publish_car_host: Direct Link's panel shows the pairing QR for this address (panel/panel.js);
# keep $CSDK_RUN/panel/car-host.txt equal to it, or absent while Tailscale has none. A changed
# address is rewritten whole (new file, then rename), so the panel never reads half of it.
publish_car_host() {
    local d=$CSDK_RUN/panel a
    a=$(tailscale_addr)
    if [ -z "$a" ]; then rm -f "$d/car-host.txt"; return 0; fi
    [ "$(cat "$d/car-host.txt" 2>/dev/null)" = "$a" ] && return 0
    mkdir -p "$d" && printf '%s\n' "$a" >"$d/car-host.txt.new.$$" && mv -f "$d/car-host.txt.new.$$" "$d/car-host.txt"
}

# tailscale_present: CoilShift's Tailscale package is installed, or its interface is up.
tailscale_present() { [ -d "$R$TS_PKG_DIR" ] || [ -e "$R/sys/class/net/$TS_IFACE" ]; }

# running_pids PREFIX prints the pids whose cmdline starts with PREFIX.
running_pids() {
    for _p in "$PROC"/[0-9]*; do
        [ -r "$_p/cmdline" ] || continue
        case "$(tr '\0' ' ' <"$_p/cmdline" 2>/dev/null)" in "$1"*) printf '%s ' "${_p##*/}" ;; esac
    done
}

# is_runsvdir_on PID DIR: PID runs `runsvdir -P DIR`, started by name or by path (its argv[0]'s
# basename is runsvdir; `command -v` gives the path, install-car.sh's setsid the name).
is_runsvdir_on() {
    [ -n "$1" ] && [ -r "$PROC/$1/cmdline" ] || return 1
    local a0 rest
    a0=$(tr '\0' '\n' <"$PROC/$1/cmdline" 2>/dev/null | head -n 1)
    rest=$(tr '\0' ' ' <"$PROC/$1/cmdline" 2>/dev/null)
    rest=${rest#"$a0" }
    [ "${a0##*/}" = runsvdir ] && [ "$rest" = "-P $2 " ]
}
# runsvdir_pids DIR: every runsvdir on DIR.
runsvdir_pids() {
    for _p in "$PROC"/[0-9]*; do
        is_runsvdir_on "${_p##*/}" "$1" && printf '%s ' "${_p##*/}"
    done
    return 0
}

# installer_live: today's install-car.sh supervisor still runs (migrate first).
installer_live() { [ -n "$(runsvdir_pids "$INSTALLER_SVDIR")" ]; }

# csdk_api with a time limit (csdk.sh's has none): a stalled daemon socket must never hold up a
# hook, and above all never supervise.sh's stop path. CSDK_MAX_TIME seconds, default 30.
csdk_api() {
    curl -sS --fail-with-body --max-time "${CSDK_MAX_TIME:-30}" --unix-socket "$CSDK_API_SOCKET" \
        -H "Authorization: Bearer $CSDK_TOKEN" -H 'Content-Type: application/json' \
        -X "$1" "http://csdk/v1$2" ${3:+--data-binary "$3"}
}

# conf_get KEY: the value of KEY=VALUE in direct-link.conf (never sourced).
conf_get() { sed -n "s/^$1=\\([^[:space:]]*\\)[[:space:]]*\$/\\1/p" "$R$CONF" 2>/dev/null | tail -n 1; }

# authd_pin prints this car's authd sha256 if it is one the sign-in pre-check proved
# (AUTHD_PINS in lib/gateway.sh), else nothing: the helpers then fall back to their
# compiled-in pin, so a new authd build fails sign-in closed.
authd_pin() {
    local h p
    h=$(sha256_of "$R/usr/bin/authd")
    [ -n "$h" ] || return 0
    for p in $AUTHD_PINS; do [ "$p" = "$h" ] && { printf '%s' "$p"; return 0; }; done
    return 0
}

# ---- the uid 4950 egress lock (install-car.sh) ----

egress_chain_ok() {
    _s=$($1 -S "$EGRESS_CHAIN" 2>/dev/null) || return 1
    [ "$(printf '%s\n' "$_s" | tail -n 1)" = "-A $EGRESS_CHAIN -j DROP" ] || return 1
    _c=$(printf '%s\n' "$_s" | grep -c '^-A ')
    if [ -n "${2:-}" ]; then
        # shellcheck disable=SC2086
        [ "$_c" = 2 ] && $1 -C "$EGRESS_CHAIN" $2 2>/dev/null
    else
        [ "$_c" = 1 ]
    fi
}

egress_jump_first() {
    [ "$($1 -S OUTPUT | grep '^-A ' | head -n 1)" = "-A OUTPUT $EGRESS_JUMP" ]
}

# egress_lock IPTABLES [accept rule]: idempotent; returns 1 when it does not check out.
egress_lock() {
    $1 -N "$EGRESS_CHAIN" 2>/dev/null || true
    if ! egress_chain_ok "$1" "${2:-}"; then
        $1 -I "$EGRESS_CHAIN" 1 -j DROP || return 1
        while $1 -D "$EGRESS_CHAIN" 2 2>/dev/null; do :; done
        # shellcheck disable=SC2086
        if [ -n "${2:-}" ]; then $1 -A "$EGRESS_CHAIN" $2; fi
        $1 -A "$EGRESS_CHAIN" -j DROP
        $1 -D "$EGRESS_CHAIN" 1
    fi
    if ! egress_jump_first "$1"; then
        # shellcheck disable=SC2086
        $1 -I OUTPUT 1 $EGRESS_JUMP || return 1
        for _n in $($1 -S OUTPUT | grep '^-A ' | grep -n -x -F -- "-A OUTPUT $EGRESS_JUMP" | cut -d: -f1 | sort -rn); do
            [ "$_n" = 1 ] || $1 -D OUTPUT "$_n"
        done
    fi
    egress_chain_ok "$1" "${2:-}" && egress_jump_first "$1"
}

# egress_unlock IPTABLES: every jump to the chain out of OUTPUT, then the chain itself.
egress_unlock() {
    # shellcheck disable=SC2086
    while $1 -D OUTPUT $EGRESS_JUMP 2>/dev/null; do :; done
    $1 -F "$EGRESS_CHAIN" 2>/dev/null || true
    $1 -X "$EGRESS_CHAIN" 2>/dev/null || true
}

# The one thing uid 4950 may send: its replies on connections the phone opened to agw over the
# tailnet. A new connection from agw, to anywhere, is dropped.
egress_accept_rule() {
    printf '%s' "-o $TS_IFACE -p tcp -m tcp --sport $GW_PORT -m conntrack --ctstate ESTABLISHED -j ACCEPT"
}

# ---- com.tesla.Webrtc: takeover and hand-back (install-car.sh S0b) ----

name_check() {
    _o=$("$TIMEOUT" -k 1 5 "$GDBUS" call --system --dest org.freedesktop.DBus --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.NameHasOwner com.tesla.Webrtc 2>/dev/null) || _o="failed${_o:+: $_o}"
}
name_free()  { name_check; [ "$_o" = '(false,)' ]; }
name_owned() { name_check; [ "$_o" = '(true,)' ]; }

# wait_name_free / wait_name_owned: every 1 s, up to 10 checks (up to about a minute).
wait_name_free() {
    _i=1
    while ! name_free; do
        [ "$_i" -lt 10 ] || return 1
        _i=$((_i + 1))
        sleep 1
    done
}
wait_name_owned() {
    _i=1
    while ! name_owned; do
        [ "$_i" -lt 10 ] || return 1
        _i=$((_i + 1))
        sleep 1
    done
}

# stop_webrtc_jail: SIGKILL the pid in webrtc-comms' pidfile only if it is not 1 and its comm
# is minijail-init (the firmware's own KillOrphanedMinijailProcessComm), then remove the file.
stop_webrtc_jail() {
    [ -f "$R$WEBRTC_PIDFILE" ] || return 0
    _pid=$(tr -dc '0-9' <"$R$WEBRTC_PIDFILE" 2>/dev/null) || _pid=
    if [ -n "$_pid" ] && [ "$_pid" -gt 1 ] 2>/dev/null && [ "$(cat "$PROC/$_pid/comm" 2>/dev/null)" = minijail-init ]; then
        do_kill -KILL "$_pid" || true
    fi
    rm -f "$R$WEBRTC_PIDFILE"
}

# take_name: stock webrtc-comms down, its orphaned jail killed, then wait for the name.
take_name() {
    do_sv down "$WEBRTC_SV" || true
    stop_webrtc_jail
    wait_name_free
}

# hand_back: stock webrtc-comms up again; 0 once it owns the name, or when it has no runsv in
# this run mode (it registers the name at its next start).
hand_back() {
    do_sv -w 15 down "$WEBRTC_SV" || true
    stop_webrtc_jail
    do_sv up "$WEBRTC_SV" || return 0
    wait_name_owned
}

summon_pids() { running_pids "$SUMMON_BIN "; }

# give_back: install-car.sh's stock_hand_back order. Our summonhelper unlinked and down (its
# finish runs) and gone first, then stock back up. Unlike stock_hand_back, stock is restarted
# even when the name has an owner: with ours gone that owner is stock itself, still letting go
# (it then registers again), or a stranger (stock then just runs): never a free name with
# stock stopped. 1 when a summonhelper process lingers (stock is then left alone).
give_back() {
    rm -f "$SVDIR/summonhelper"
    if [ -e "$SVROOT/summonhelper/supervise/ok" ]; then do_sv -w 15 down "$SVROOT/summonhelper" || true; fi
    summon_gone || return 1
    hand_back
}

# summon_gone: no summonhelper process (run, serve, finish) within 10 s.
summon_gone() {
    _i=1
    while [ -n "$(summon_pids)" ]; do
        [ "$_i" -lt 10 ] || return 1
        _i=$((_i + 1))
        sleep 1
    done
}

# ---- runit scripts (install-car.sh S2, binary paths changed) ----

gen_agw_scripts() {
    AGW_RUN=$(cat <<EOF
#!/bin/sh
# airgapp gateway (runit run). Generated by Direct Link.
exec 2>&1
if ! $IPT -C OUTPUT $EGRESS_JUMP 2>/dev/null || ! $IPT -C $EGRESS_CHAIN -j DROP 2>/dev/null || ! $IP6T -C OUTPUT $EGRESS_JUMP 2>/dev/null; then
	echo "agw run: the uid $UID_GW egress lock is missing (apply Direct Link again); not starting"
	sleep 10
	exit 1
fi
if [ ! -e /sys/class/net/$TS_IFACE ]; then
	echo "agw run: $TS_IFACE is not up yet (Tailscale); waiting"
	sleep 5
	exit 1
fi
exec chpst -u $RUN_AS "$AGW_BIN" serve -tailnet -key "$GW_STATE/key.pem" -vin-file /var/etc/vin -log "$GW_STATE/log"
EOF
)
    AGW_FINISH=$(cat <<'EOF'
#!/bin/sh
# airgapp gateway (runit finish): runs after every agw exit (crash, kill, sv
# down). Nothing to do: camhelper releases the CAMERA/DASHCAM state as soon as
# agw's helper sessions drop, and Summon's finish is summonhelper's own.
exit 0
EOF
)
}

gen_helper_scripts() { # WRITER PIN
    HELPER_RUN=$(cat <<EOF
#!/bin/sh
# camera root helper (runit run, as root). Generated by Direct Link.
exec 2>&1
exec $HELPER_BIN run -state $HELPER_STATE -writer $1${2:+ -authd-sha256 $2}
EOF
)
    HELPER_FINISH=$(cat <<EOF
#!/bin/sh
# camera root helper (runit finish): clear whatever the markers record
# (requestAudio 0, sessionActive 0, selectable 0, gain -200 only if marked).
exec 2>&1
exec $HELPER_BIN sweep -state $HELPER_STATE
EOF
)
}

gen_summon_scripts() { # PIN
    SUMMON_RUN=$(cat <<EOF
#!/bin/sh
# summonhelper (runit run, as root). Generated by Direct Summon. It prepares
# the relay directory (removing a stale socket and live file), reads the VIN, and starts the supervisor as uid
# $SUMMON_UID only while com.tesla.Webrtc has no owner.
exec 2>&1
if [ -L "$SUMMON_RUN_DIR" ] || { [ -e "$SUMMON_RUN_DIR" ] && [ ! -d "$SUMMON_RUN_DIR" ]; }; then
	echo "summonhelper run: $SUMMON_RUN_DIR is a symlink or not a directory; not starting"
	sleep 10
	exit 1
fi
if ! { mkdir -p "$SUMMON_RUN_DIR" && chown $SUMMON_UID:$GW_GID "$SUMMON_RUN_DIR" && chmod 2750 "$SUMMON_RUN_DIR"; }; then
	echo "summonhelper run: cannot prepare $SUMMON_RUN_DIR ($SUMMON_UID:$GW_GID 2750); not starting"
	sleep 10
	exit 1
fi
rm -f "$SUMMON_RUN_DIR/relay.sock"
rm -f "$SUMMON_RUN_DIR/live" "$SUMMON_RUN_DIR"/.live.*
VIN=\$(tr -d '[:space:]' <"$VIN_FILE")
if [ -z "\$VIN" ]; then
	echo "summonhelper run: no VIN in $VIN_FILE; not starting"
	sleep 10
	exit 1
fi
if ! { _o=\$($TIMEOUT -k 1 5 $GDBUS call --system --dest org.freedesktop.DBus --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.NameHasOwner com.tesla.Webrtc 2>/dev/null) && [ "\$_o" = '(false,)' ]; }; then
	echo "summonhelper run: com.tesla.Webrtc is not free (NameHasOwner answered: \$_o); not starting"
	sleep 10
	exit 1
fi
SUMMONHELPER_VIN="\$VIN" exec chpst -u :$SUMMON_UID:$SUMMON_UID "$SUMMON_BIN" run${1:+ -authd-sha256 $1}
EOF
)
    SUMMON_FINISH=$(cat <<EOF
#!/bin/sh
# summonhelper (runit finish). Generated by Direct Summon. It runs after every
# exit of run, also when the supervisor itself was killed: as uid $SUMMON_UID,
# summonhelper finish sends cmd_abort and the disconnect on com.tesla.Webrtc if
# a session marker is left, and first removes a stale live file.
exec 2>&1
exec chpst -u :$SUMMON_UID:$SUMMON_UID "$SUMMON_BIN" finish
EOF
)
}

log_run() { printf '#!/bin/sh\n# svlogd for %s (runit log). Generated by %s.\nexec svlogd -tt %s\n' "$1" "$3" "$2"; }

# ---- writing services ----

CHANGED=
# write_if_changed PATH CONTENT: atomic, root 0755, only when different; adds PATH's service
# to CHANGED.
write_if_changed() {
    if [ -f "$1" ] && [ "$(cat "$1")" = "$2" ]; then return 0; fi
    printf '%s\n' "$2" >"$1.new"
    as_root "$1.new"
    chmod 0755 "$1.new"
    mv "$1.new" "$1"
    CHANGED="$CHANGED $1"
}

# log_dir DIR: an svlogd directory (root 0750) with today's config, 3 x 1 MB.
log_dir() {
    mkdir -p "$R$1"
    as_root "$R$1"
    chmod 0750 "$R$1"
    if [ "$(cat "$R$1/config" 2>/dev/null)" != "$(printf 's1000000\nn3')" ]; then
        printf 's1000000\nn3\n' >"$R$1/config"
        chmod 0640 "$R$1/config"
    fi
}

# write_service NAME RUN FINISH LOGDIR GENERATOR: $SVROOT/NAME/{run,finish,log/run}, linked
# into our svdir.
write_service() {
    mkdir -p "$SVROOT/$1/log" "$SVDIR"
    write_if_changed "$SVROOT/$1/run" "$2"
    write_if_changed "$SVROOT/$1/finish" "$3"
    write_if_changed "$SVROOT/$1/log/run" "$(log_run "$1" "$4" "$5")"
    log_dir "$4"
    [ -L "$SVDIR/$1" ] || ln -s "$SVROOT/$1" "$SVDIR/$1"
}

# restart_changed: a run script that changed takes effect on its next start, so restart a
# service that runs now (the installer's S4), never one that is down.
restart_changed() {
    local svc
    for svc in "$@"; do
        case "$CHANGED" in *"$SVROOT/$svc/"*) ;; *) continue ;; esac
        [ -e "$SVROOT/$svc/supervise/ok" ] || continue
        do_sv restart "$SVROOT/$svc" || true
    done
}

# ---- status ----

set_status() { # STATE REASON
    csdk_api PUT /status "{\"state\":\"$1\",\"reason\":$(csdk_json_string "$(printf '%.256s' "$2")")}" >/dev/null 2>&1 || true
    csdk_log info "$CSDK_PKG: $1${2:+: $2}" >/dev/null 2>&1 || true
}

# finish_apply STATE REASON: the boot marker, the status, and on an in-place update (the
# marker of this boot names another build) a restart of our service, so every finish script
# runs before the new binaries start.
finish_apply() {
    local prev
    prev=$(cut -d' ' -f1 "$MARKER" 2>/dev/null || true)
    printf '%s %s\n' "$GW_BUILD" "$(boot_id)" >"$MARKER"
    printf '%s %s\n' "$1" "$2" >"$CSDK_RUN/status.apply"
    set_status "$1" "$2"
    if [ -n "$prev" ] && [ "$prev" != "$GW_BUILD" ]; then
        csdk_api POST /services/supervise/restart >/dev/null 2>&1 || true
    fi
}
