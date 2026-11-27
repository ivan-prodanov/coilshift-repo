#!/bin/sh
# airgapp gateway: car install. Run by the owner, as root, over SSH, with the
# car awake and the owner present. POSIX sh (the car's /bin/sh is BusyBox).
#
#   PI_IP=<the Pi's hotspot address> sh install-car.sh [path/to/agw [path/to/camhelper [path/to/summonhelper]]]
#   sh install-car.sh --after-boot      (PI_IP: as installed, unless given)
#
# The car dials out to the Pi: its Wi-Fi module NATs the MCU and drops every
# inbound packet (car-proven 2026-11-21), so agw runs `serve -dial
# PI_IP:17101` and nothing connects in. CAR_IP is no longer used.
#
# SUMMON=1 in front serves SUMMON: it installs summonhelper (uid 1337), which
# takes com.tesla.Webrtc from stock webrtc-comms (stopped first), as its own
# runit service; agw always runs plain serve and relays to it. SUMMON unset
# keeps what is installed: an installed helper stays, anything else gets
# nothing for Summon. SUMMON=0 removes an installed helper, handing the name
# back to webrtc-comms (see "SUMMON" below).
#
# AUTHD_SHA256=<64 hex> in front passes camhelper (and summonhelper)
# -authd-sha256: the /usr/bin/authd build whose KeyInfo was verified, for the
# gateway's sign-in. Unset, the installed camhelper run script's pin is kept
# (a full install as after a boot); with none installed, camhelper's
# compiled-in pin (the 2026.8.6 build) applies. Before any write the script
# compares the effective pin with this car's authd (check_authd_pin). Set it
# only after KeyInfo was re-verified for a new authd (an OTA).
#
# uid 4950 gets no account and no system bus access: camhelper (root) asks
# authd for it on its sign-in socket (docs/CAMERA-HELPER.md).
#
# Every write is announced and needs a typed "yes". Nothing here touches the
# vehicle LAN 192.168.90.0/24 (R22): the only network change is the uid 4950
# egress lock (step 5c), which lets agw open TCP to PI_IP:17101 on wlan0 and
# nothing else. There is no INPUT rule.
#
# --after-boot is the one idempotent per-boot step: /run is tmpfs and
# neither iptables rules nor mounts are saved, so after every car boot it
# re-adds the egress lock (5c), redoes the exec mounts (S0), stops stock
# webrtc-comms again when summonhelper is installed (S0b) and starts our own
# runsvdir again (S3), which supervises agw, camhelper and summonhelper.
# It installs no binary and no key; it rewrites a service script only if it
# changed (a new PI_IP or authd pin).
set -eu

AFTER_BOOT=0
if [ "${1:-}" = --after-boot ]; then
	AFTER_BOOT=1
	shift
fi
AGW_SRC=${1:-./agw}
HELPER_SRC=${2:-./camhelper}
SUMMON_SRC=${3:-./summonhelper} # used only with SUMMON=1
DIR=/var/lib/airgapp-gw
STATE=$DIR/state
SV=$DIR/sv # the runit service directories (never the sv command: that is SV_BIN)
UID_GW=4950
GID_GW=4950
PI_PORT=17101 # the Pi's car-side port, on its hotspot address
RUN_AS=":$UID_GW:$GID_GW"

die() { printf 'install-car: %s\n' "$*" >&2; exit 1; }

ask() {
	printf '\n==> %s\n    type yes to do this: ' "$1"
	read -r ans || die "no answer (stdin closed)"
	[ "$ans" = yes ] || die "not confirmed, stopping (nothing further was changed)"
}

# confirm <question>: like ask, but anything other than a typed yes (no stdin
# too) answers false instead of stopping the script.
confirm() {
	printf '\n==> %s\n    type yes to do this: ' "$1"
	read -r ans || { echo; return 1; }
	[ "$ans" = yes ]
}

# Strict IPv4 check: exactly four decimal octets 0-255, no leading zeros
# (iptables/inet_aton may read 0177 as octal), and never an address the
# gateway must not use (R22 safety net). Prints a reason and returns 1.
check_ip() {
	_ip=$1
	case "$_ip" in
	*[!0-9.]*|""|.*|*.|*..*) echo "$_ip is not a dotted IPv4 address"; return 1 ;;
	esac
	_oldifs=$IFS; IFS=.
	# shellcheck disable=SC2086
	set -- $_ip
	IFS=$_oldifs
	[ "$#" -eq 4 ] || { echo "$_ip is not four octets"; return 1; }
	for _o in "$1" "$2" "$3" "$4"; do
		case "$_o" in
		0) ;;
		0*) echo "$_ip has a leading zero octet"; return 1 ;;
		esac
		[ "${#_o}" -le 3 ] && [ "$_o" -le 255 ] || { echo "$_ip octet out of range"; return 1; }
	done
	[ "$1.$2.$3" = 192.168.90 ] && { echo "$_ip is on the vehicle LAN 192.168.90.0/24 (R22)"; return 1; }
	[ "$1" -eq 127 ] && { echo "$_ip is loopback"; return 1; }
	[ "$1.$2.$3.$4" = 0.0.0.0 ] && { echo "$_ip is unspecified"; return 1; }
	[ "$1" -eq 100 ] && [ "$2" -ge 64 ] && [ "$2" -le 127 ] && { echo "$_ip is in the tailnet range 100.64.0.0/10"; return 1; }
	return 0
}

# Test hook: AGW_CHECK_IP_ONLY=1 sh install-car.sh <ip> checks one address and exits.
if [ "${AGW_CHECK_IP_ONLY:-}" = 1 ]; then
	if _r=$(check_ip "${1:-}"); then echo ok; exit 0; else echo "refused: $_r"; exit 1; fi
fi

# ---- runit helpers (used by the CAMERA ROOT HELPER section at the end) ----
# PROCFS or RUN_ROOT set in the environment (the camera-era per-boot command
# set RUN_ROOT to /nonexistent) is no longer read: noted at the start (review D5).
_env_layout="${PROCFS+ PROCFS}${RUN_ROOT+ RUN_ROOT}"
PROCFS=/proc # the real /proc: only a test hook points it elsewhere (test_layout_paths)
OWN_SVDIR=$DIR/svdir # our own runsvdir: S3 always uses it, never another supervisor's

# test_layout_paths: in a test hook only, AGW_TEST_PROCFS replaces /proc. The
# real path never reads it: the AGW_TEST_* guard refuses it there.
test_layout_paths() {
	PROCFS=${AGW_TEST_PROCFS:-$PROCFS}
}

# detect_svdirs prints the directory argument of every running runsvdir
# (runsvdir [-P] dir [log]) except our own fallback one.
detect_svdirs() {
	for _p in "$PROCFS"/[0-9]*; do
		[ -r "$_p/cmdline" ] || continue
		_args=$(tr '\0' '\n' <"$_p/cmdline" 2>/dev/null) || continue
		_prog=$(printf '%s\n' "$_args" | head -n 1)
		case "${_prog##*/}" in runsvdir) ;; *) continue ;; esac
		_d=$(printf '%s\n' "$_args" | sed -n '2,$p' | grep -v '^-' | head -n 1)
		if [ -n "$_d" ] && [ "$_d" != "$OWN_SVDIR" ]; then printf '%s\n' "$_d"; fi
	done | sort -u
}

# running_pids <cmdline prefix> prints the pids whose cmdline starts with it.
running_pids() {
	for _p in "$PROCFS"/[0-9]*; do
		[ -r "$_p/cmdline" ] || continue
		case "$(tr '\0' ' ' <"$_p/cmdline" 2>/dev/null)" in "$1"*) printf '%s ' "${_p##*/}" ;; esac
	done
}

# exec_mount <dir>: the car mounts /var (and /home) noexec,nosymfollow, so a
# binary, a runit run script or a runsvdir symlink under /var/lib can't run.
# Bind the directory onto itself without those two flags (nosuid,nodev stay),
# the way the car's own mcu-root-utils runs files kept on /var. A bind mount does
# not survive a reboot: --after-boot redoes it before runsvdir starts.
mount_opts() { awk -v d="$1" '$2 == d {o = $4} END {print o}' "$PROCFS/self/mounts"; }
exec_mount() {
	_m=$(mount_opts "$1")
	if [ -z "$_m" ]; then
		mount --bind "$1" "$1" || die "mount --bind $1 failed"
		_m=$(mount_opts "$1")
	fi
	case ",$_m," in
	*,noexec,*|*,nosymfollow,*) mount -o remount,bind,nosuid,nodev,exec,symfollow "$1" || die "remount $1 failed" ;;
	esac
	_m=$(mount_opts "$1")
	case ",$_m," in
	,,|*,noexec,*|*,nosymfollow,*) die "could not make $1 executable (mount options: ${_m:-none})" ;;
	esac
	echo "$1: mounted $_m"
}

# ---- the gateway egress lock (uid 4950) ----
# agw's only network traffic is its own TCP connections to the Pi: the camera
# capture runs in the helper's children (uid 4951), and the bus and the
# helper are unix sockets. The car's OUTPUT policy is ACCEPT, with an owner
# chain per Tesla service and none for 4950, so without this a compromised agw
# could reach the vehicle LAN and every loopback service. The same pattern as
# Tesla's own services: an owner chain, first in OUTPUT, that lets uid 4950
# send only TCP to PI_IP:PI_PORT on wlan0 (the dial and everything on those
# connections) and drops everything else, on IPv6 too. iptables rules are not
# saved, so this is per boot, and agw's run script refuses to start without it.
EGRESS_CHAIN=AIRGAPP_GW_OUT
EGRESS_JUMP="-m owner --uid-owner $UID_GW -j $EGRESS_CHAIN"
IPT=${AGW_TEST_IPT:-$(command -v iptables || echo iptables)}
IP6T=${AGW_TEST_IP6T:-$(command -v ip6tables || echo ip6tables)}

# egress_chain_ok <iptables|ip6tables> [accept rule]: the chain holds exactly
# that rule (if any) and then DROP, nothing else.
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

# egress_jump_first <iptables|ip6tables>: our jump is the first OUTPUT rule.
egress_jump_first() {
	[ "$($1 -S OUTPUT | grep '^-A ' | head -n 1)" = "-A OUTPUT $EGRESS_JUMP" ]
}

# egress_lock <iptables|ip6tables> [accept rule]: idempotent. A chain that is
# not exactly right is rebuilt behind a temporary DROP (fails closed), and the
# jump is put first in OUTPUT before any duplicate is removed.
egress_lock() {
	$1 -N "$EGRESS_CHAIN" 2>/dev/null || true
	if ! egress_chain_ok "$1" "${2:-}"; then
		$1 -I "$EGRESS_CHAIN" 1 -j DROP || die "$1: cannot write $EGRESS_CHAIN"
		while $1 -D "$EGRESS_CHAIN" 2 2>/dev/null; do :; done
		# shellcheck disable=SC2086
		if [ -n "${2:-}" ]; then $1 -A "$EGRESS_CHAIN" $2; fi
		$1 -A "$EGRESS_CHAIN" -j DROP
		$1 -D "$EGRESS_CHAIN" 1
	fi
	if ! egress_jump_first "$1"; then
		# shellcheck disable=SC2086
		$1 -I OUTPUT 1 $EGRESS_JUMP || die "$1: cannot insert the OUTPUT jump"
		for _n in $($1 -S OUTPUT | grep '^-A ' | grep -n -x -F -- "-A OUTPUT $EGRESS_JUMP" | cut -d: -f1 | sort -rn); do
			[ "$_n" = 1 ] || $1 -D OUTPUT "$_n"
		done
	fi
	if ! { egress_chain_ok "$1" "${2:-}" && egress_jump_first "$1"; }; then
		die "$1: the uid $UID_GW egress lock did not check out"
	fi
	echo "$1: uid $UID_GW egress lock in place ($EGRESS_CHAIN, first in OUTPUT)"
}

# egress_accept_rule: uid 4950's one ACCEPT, new TCP connections to the Pi's
# dial port on wlan0 (and every later packet of them: same destination).
egress_accept_rule() {
	printf '%s' "-o wlan0 -d $PI_IP -p tcp --dport $PI_PORT -j ACCEPT"
}

# Test hook: AGW_EGRESS_RULE_ONLY=1 PI_IP=<ip> sh install-car.sh prints that rule.
if [ "${AGW_EGRESS_RULE_ONLY:-}" = 1 ]; then
	egress_accept_rule
	echo
	exit 0
fi

# Test hook: AGW_EGRESS_ONLY=1 AGW_TEST_IPT=<fake iptables> sh install-car.sh [accept rule]
if [ "${AGW_EGRESS_ONLY:-}" = 1 ]; then
	egress_lock "$IPT" "${1:-}"
	exit 0
fi

# ---- Summon: values the tests override, baked into the generated scripts the way IPT is ----
GDBUS=${AGW_TEST_GDBUS:-$(command -v gdbus || echo gdbus)}
TIMEOUT=${AGW_TEST_TIMEOUT:-$(command -v timeout || echo timeout)}
SV_BIN=${AGW_TEST_SV:-$(command -v sv || echo sv)}
KILL_BIN=${AGW_TEST_KILL:-$(command -v kill || echo kill)}
WEBRTC_SV=${AGW_TEST_WEBRTC_SV:-/etc/sv/webrtc-comms}
WEBRTC_PIDFILE=${AGW_TEST_WEBRTC_PIDFILE:-/run/service/webrtc-comms/webrtc-comms.pid}
PROC=${AGW_TEST_PROC:-/proc}
# summonhelper's relay directory. The helper and agw have its socket and the
# session marker compiled in (/run/airgapp-summon/relay.sock and
# /run/airgapp-summon/session), and the generated scripts pass no flag for
# them, so on the car this must stay /run/airgapp-summon: the test override
# only feeds the run script's own steps (the directory, the stale socket and
# the stale live file) and summon_remove's rm of the live file; the hooks that
# can reach that rm require it, so a hand-run hook never touches the real one.
SUMMON_RUN_DIR=${AGW_TEST_SUMMON_RUN_DIR:-/run/airgapp-summon}
VIN_FILE=${AGW_TEST_VIN_FILE:-/var/etc/vin}
SUMMON_UID=${AGW_TEST_SUMMON_UID:-1337} # user webrtc: the helper runs as it, with no other group
GW_GID=${AGW_TEST_GW_GID:-$GID_GW}       # the relay directory's group (agw's), so uid 4950 reaches the socket
SUMMON_HOME=/var/lib/airgapp-summon
SUMMON_STATE=$SUMMON_HOME/state # the helper's key.pem and log (its compiled-in defaults)
SUMMON_INSTALLED_RUN=$SV/summonhelper/run
SUMMON_GIVEN=0  # 1 when SUMMON was given (summon_mode): only then does SUMMON=0 remove or SUMMON=1 install
SUMMON_OK=1     # 0 when Summon is skipped or failed before S4 in this run (summon_skip, summon_fail: a missing tool, a decline, a name that stays taken, a failing 4b/4c command, an unsupervised summonhelper, a helper not running after S3): the helper is neither linked nor started. A failed check after S4 leaves it 1: the helper stays linked and running
WRITTEN=0       # 1 from a full install's first write on (step 1's mkdir): from there a Summon failure never stops it halfway
SUMMON_KEYGEN=0 # 1 when step 4c (the helper's keygen) was asked about and is due
SUMMON_CHECKED=0 # 1 once the checks after the last restart passed (the DONE text then gives the helper's pin route)
STOCK_STOPPED=0  # 1 once S0b's stop sequence ran, until our helper's checks passed or a hand-back settled it: a Summon skip (stock_hand_back) or any exit (exit_hand_back) then gives com.tesla.Webrtc back to stock
SUMMON_REPORT=  # the Summon failures that did not stop the run, reported at the end with a non-zero exit

# write_if_changed <path> <content>: atomic, root:root 0755, only when different.
write_if_changed() {
	if [ -f "$1" ] && [ "$(cat "$1")" = "$2" ]; then return 0; fi
	printf '%s\n' "$2" >"$1.new"
	chown 0:0 "$1.new"
	chmod 0755 "$1.new"
	mv "$1.new" "$1"
	echo "wrote $1"
}

supervised() { $SV_BIN status "$1" 2>/dev/null | grep -q '^run:'; }

# summon_on: SUMMON=1, and the helper is linked and started in this run.
summon_on() { [ "$SUMMON" = 1 ] && [ "$SUMMON_OK" = 1 ]; }

# services prints the runit services this run links, starts and checks.
services() {
	if summon_on; then echo "agw camhelper summonhelper"; else echo "agw camhelper"; fi
}

attached() {
	for _svc in $(services); do [ -L "$1/airgapp-$_svc" ] || return 1; done
}

all_supervised() {
	for _svc in $(services); do supervised "$SV/$_svc" || return 1; done
}

# link_into <svdir>: our service links, never touching anything else.
link_into() {
	for _svc in $(services); do
		_l="$1/airgapp-$_svc"
		if [ -L "$_l" ] && [ "$(readlink "$_l")" = "$DIR/sv/$_svc" ]; then continue; fi
		if [ -e "$_l" ] || [ -L "$_l" ]; then die "$_l exists and is not ours; not touching the car's services"; fi
		ln -s "$DIR/sv/$_svc" "$_l"
		echo "linked $_l -> $DIR/sv/$_svc"
	done
}

# ---- camhelper's run script and its authd pin ----
HELPER_DIR=/var/lib/airgapp-camhelper
HELPER_BIN=$HELPER_DIR/camhelper
HELPER_STATE=/run/airgapp-camhelper-state
HELPER_WRITER=${HELPER_WRITER:-pw-cat}
HELPER_INSTALLED_RUN=$DIR/sv/camhelper/run
if [ "${AGW_HELPER_RUN_ONLY:-}" = 1 ] || [ "${AGW_SUMMON_SCRIPTS_ONLY:-}" = 1 ] || [ "${AGW_AUTHD_PIN_ONLY:-}" = 1 ]; then HELPER_INSTALLED_RUN=${AGW_TEST_HELPER_RUN:-/nonexistent}; fi
# The authd build camhelper and summonhelper trust when no -authd-sha256 is
# given: authority.DefaultAuthdSHA256 (the 2026.8.6 build). A Go test keeps
# the two equal.
AUTHD_COMPILED_PIN=33d62d9e818afcb9b4a4af8cec6ecda2ff1801c5a667bd527e39a2b4f6f477e9
AUTHD_BIN=${AGW_TEST_AUTHD:-/usr/bin/authd}  # test hooks only: the AGW_TEST_* guard refuses these on the real path
SHA256SUM=${AGW_TEST_SHA256SUM:-sha256sum}
AUTHD_PIN_FROM=compiled-in # where the effective pin came from (resolve_authd_pin)
AUTHD_PIN_EFFECTIVE=$AUTHD_COMPILED_PIN
AUTHD_NOTE=                # after a boot: the pin check's note, printed at the end

# resolve_authd_pin sets AUTHD_SHA256, the -authd-sha256 both helpers' run
# scripts pass: as given; else the pin the installed camhelper run script
# names, on a full install as after a boot, so a reinstall that forgets
# AUTHD_SHA256 keeps the verified pin; else empty, the compiled-in pin.
# AUTHD_PIN_EFFECTIVE is the pin the helpers will check and AUTHD_PIN_FROM its
# source. A value that is not 64 lowercase hex digits stops the script.
resolve_authd_pin() {
	if [ -n "${AUTHD_SHA256:-}" ]; then
		AUTHD_PIN_FROM=given
	else
		AUTHD_SHA256=$(sed -n 's/.* -authd-sha256 \([^ ]*\).*/\1/p' "$HELPER_INSTALLED_RUN" 2>/dev/null | head -n 1)
		if [ -n "$AUTHD_SHA256" ]; then AUTHD_PIN_FROM="kept from the installed run script"; else AUTHD_PIN_FROM=compiled-in; fi
	fi
	if [ -n "$AUTHD_SHA256" ]; then
		case "$AUTHD_SHA256" in *[!0-9a-f]*) die "AUTHD_SHA256 ($AUTHD_PIN_FROM) must be 64 lowercase hex digits" ;; esac
		[ "${#AUTHD_SHA256}" -eq 64 ] || die "AUTHD_SHA256 ($AUTHD_PIN_FROM) must be 64 lowercase hex digits"
	fi
	AUTHD_PIN_EFFECTIVE=${AUTHD_SHA256:-$AUTHD_COMPILED_PIN}
}

# authd_pin_line prints the plan's sign-in line: the effective pin's first 8
# hex digits, and where it came from.
authd_pin_line() {
	printf '  sign-in: camhelper (root) answers uid %s on %s from authd KeyInfo, authd pin %.8s (%s)\n' \
		"$UID_GW" /run/airgapp-camhelper/auth.sock "$AUTHD_PIN_EFFECTIVE" "$AUTHD_PIN_FROM"
}

# check_authd_pin compares the effective pin with this car's authd, hashed the
# way the camera's own pre-check does. It catches both directions: a pin given
# for another build, and a kept or compiled-in pin gone stale after an OTA.
# Either way camhelper's sign-in fails closed, quietly. It never adopts the
# car's hash as the pin: a pin is only ever given after the runbook's step 2b
# proved sign-in on that build. On a full install, a mismatch or an authd it
# cannot hash asks before the first write, and anything but yes stops it with
# nothing changed. After a boot it is a loud note at the end that never blocks
# anything, and the run's exit status is non-zero for it (end_report).
check_authd_pin() {
	_h=$($SHA256SUM "$AUTHD_BIN" 2>/dev/null | cut -d' ' -f1) || _h=
	[ "$_h" = "$AUTHD_PIN_EFFECTIVE" ] && return 0
	case "$_h" in
	*[!0-9a-f]*|"") _w="cannot hash $AUTHD_BIN" ;;
	*) if [ "${#_h}" -eq 64 ]; then _w="does not match this car's $AUTHD_BIN $(printf '%.8s' "$_h")"; else _w="cannot hash $AUTHD_BIN"; fi ;;
	esac
	_m="camhelper's authd pin $(printf '%.8s' "$AUTHD_PIN_EFFECTIVE") ($AUTHD_PIN_FROM) $_w: camera sign-in will fail closed until you re-run with a verified AUTHD_SHA256 (runbook step 2b; after an OTA, its section \"After an OTA (gateway already installed)\")"
	if [ "$AFTER_BOOT" = 1 ]; then
		AUTHD_NOTE=$_m
		echo "NOTE: $_m (repeated at the end)"
	else
		ask "$_m. Install anyway?"
	fi
}

# end_report: the run's last word, after the DONE text. After a boot the pin
# check's note is repeated, and it makes the exit status non-zero, though
# every service was started (the camera session's request: a stale pin means
# camera sign-in is dead, and --after-boot is run by hand, so its status must
# say so). It never blocks anything. The Summon failures that did not stop
# the run come last (summon_report). Either one exits non-zero.
end_report() {
	if [ -n "$AUTHD_NOTE" ]; then printf '\n!!! NOTE: %s\n' "$AUTHD_NOTE" >&2; fi
	summon_report
	[ -z "$AUTHD_NOTE" ] || die "the authd pin check failed (the NOTE above); every service was started"
}

gen_helper_run() {
	HELPER_RUN=$(cat <<EOF
#!/bin/sh
# camera root helper (runit run, as root). Generated by install-car.sh.
exec 2>&1
exec $HELPER_BIN run -state $HELPER_STATE -writer $HELPER_WRITER${AUTHD_SHA256:+ -authd-sha256 $AUTHD_SHA256}
EOF
)
}

# Test hook: AGW_HELPER_RUN_ONLY=1 [AUTHD_SHA256=<pin>] [AGW_TEST_HELPER_RUN=<installed run>]
# sh install-car.sh [--after-boot] prints camhelper's run script.
if [ "${AGW_HELPER_RUN_ONLY:-}" = 1 ]; then
	resolve_authd_pin
	gen_helper_run
	printf '%s\n' "$HELPER_RUN"
	exit 0
fi

# ---- agw's run and finish scripts ----
# agw always runs plain `agw serve`: Summon is summonhelper's own service
# (below), which agw only relays to. An agw run script left by the old SUMMON=1
# install is rewritten plain by S2, like any changed script.
AGW_INSTALLED_RUN=$DIR/sv/agw/run
if [ "${AGW_SCRIPTS_ONLY:-}" = 1 ]; then AGW_INSTALLED_RUN=${AGW_TEST_RUN:-/nonexistent}; fi

# resolve_pi_ip sets PI_IP: as given, or on --after-boot the address the
# installed agw run script dials. It must pass check_ip (R22, no loopback,
# no tailnet address): the car reaches the Pi on its hotspot, through the
# car's Wi-Fi module.
resolve_pi_ip() {
	if [ -z "${PI_IP:-}" ] && [ "$AFTER_BOOT" = 1 ]; then
		PI_IP=$(sed -n 's/.* -dial "\([0-9.]*\):[0-9]*".*/\1/p' "$AGW_INSTALLED_RUN" 2>/dev/null | head -n 1)
	fi
	[ -n "${PI_IP:-}" ] || die "set PI_IP (the Pi's address on its hotspot, which the car dials)"
	_r=$(check_ip "$PI_IP") || die "PI_IP: $_r"
}

# gen_agw_scripts sets AGW_RUN and AGW_FINISH. stderr goes to svlogd
# (3 x 1 MB), never an unbounded state/agw.out; the session log (-log) is
# unchanged.
gen_agw_scripts() {
	AGW_RUN=$(cat <<EOF
#!/bin/sh
# airgapp gateway (runit run). Generated by install-car.sh.
exec 2>&1
if ! $IPT -C OUTPUT $EGRESS_JUMP 2>/dev/null || ! $IPT -C $EGRESS_CHAIN -j DROP 2>/dev/null || ! $IP6T -C OUTPUT $EGRESS_JUMP 2>/dev/null; then
	echo "agw run: the uid $UID_GW egress lock is missing (run install-car.sh --after-boot); not starting"
	sleep 10
	exit 1
fi
exec chpst -u $RUN_AS "$DIR/agw" serve -dial "$PI_IP:$PI_PORT" -key "$STATE/key.pem" -vin-file /var/etc/vin -log "$STATE/log"
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

# Test hook: AGW_SCRIPTS_ONLY=1 PI_IP=<ip> [AGW_TEST_RUN=<installed run>]
# sh install-car.sh [--after-boot] <agw> prints the agw run and finish scripts.
if [ "${AGW_SCRIPTS_ONLY:-}" = 1 ]; then
	resolve_pi_ip
	gen_agw_scripts
	printf '%s\n---- finish ----\n%s\n' "$AGW_RUN" "$AGW_FINISH"
	exit 0
fi

# ---- SUMMON: summonhelper's service (design §6.1, §6.6) ----
# With SUMMON=1, summonhelper (uid 1337, user webrtc, no other group) owns
# com.tesla.Webrtc and serves the inner SUMMON session on its relay socket,
# which agw (uid 4950) dials. Its runit run script (root) prepares
# /run/airgapp-summon, reads the VIN and starts `summonhelper run`, the
# supervisor, only while the name has no owner; its finish runs `summonhelper
# finish`, which aborts and disconnects a session a killed supervisor left
# open. Stock webrtc-comms, a firmware runit service in the infotainment and
# sentry run modes, under minijail, must let go of the name first: the stop
# sequence (S0b) runs at install and after every boot, before the helper
# starts. SUMMON unset keeps what is installed, on a full install as after a
# boot: the helper's run script decides. Only an explicit SUMMON=0 removes the
# helper and hands the name back to webrtc-comms (plan call 11), and only an
# explicit SUMMON=1 on a full install installs a binary and a key; a kept
# helper is stopped (S0b) and started fresh like a new one.

# helper_installed: our helper's service directory exists. A removal cut short
# leaves it without its run script; the next SUMMON=0 run finishes the removal.
helper_installed() { [ -d "$SV/summonhelper" ]; }

# summon_mode sets SUMMON to 0 or 1, or stops on a value that is neither.
# SUMMON unset keeps what is installed, on a full install as after a boot: 1
# when the helper's run script is installed, else 0. A forgotten SUMMON (a
# camera-only re-install) then neither offers to remove the helper nor needs a
# new binary. After a boot, SUMMON=1 with no helper installed is a state
# mismatch, not a typo: Summon is skipped for the boot (summon_skip).
summon_mode() {
	if [ -z "${SUMMON:-}" ]; then
		SUMMON_GIVEN=0
		if [ -f "$SUMMON_INSTALLED_RUN" ]; then SUMMON=1; else SUMMON=0; fi
	else
		SUMMON_GIVEN=1
		case "$SUMMON" in 0|1) ;; *) die "SUMMON must be 0 or 1" ;; esac
	fi
	if [ "$AFTER_BOOT" = 1 ] && [ "$SUMMON" = 1 ] && [ ! -f "$SUMMON_INSTALLED_RUN" ]; then
		summon_skip "SUMMON=1, but summonhelper is not installed ($SUMMON_INSTALLED_RUN is missing): install with SUMMON=1 first"
	fi
}

# summon_removing: an explicit SUMMON=0 on a car that has our helper.
summon_removing() { [ "$SUMMON_GIVEN" = 1 ] && [ "$SUMMON" = 0 ] && helper_installed; }

# summon_installing: an explicit SUMMON=1 on a full install, which installs the
# binary and the key (steps 0, 4b and 4c). A kept helper installs neither.
summon_installing() { [ "$SUMMON_GIVEN" = 1 ] && [ "$SUMMON" = 1 ] && [ "$AFTER_BOOT" = 0 ]; }

# report_later <message>: a Summon failure that must not keep agw or camhelper
# down: it is reported at the end, with a non-zero exit.
report_later() {
	echo "$1 (reported at the end; agw and camhelper go on)"
	SUMMON_REPORT=${SUMMON_REPORT:+$SUMMON_REPORT; }$1
}

# summon_soft: a Summon failure or decline must not stop this run: after a
# boot, where the camera must come up, or on a full install once it has
# written anything (WRITTEN), which must never stop halfway (F1). Before its
# first write a full install still stops: the pre-write checks.
summon_soft() { [ "$AFTER_BOOT" = 1 ] || [ "$WRITTEN" = 1 ]; }

# summon_fail <reason>: a SUMMON=1 step before S4 failed, so Summon does not
# run. Before a full install's first write it stops the install; otherwise it
# is reported at the end (summon_soft), and the caller sets SUMMON_OK=0 and
# unlinks the helper. A failed check after S4 is check_fail's.
summon_fail() {
	summon_soft || die "$1"
	report_later "Summon is not running: $1"
}

# check_fail <reason>: a check after the last restart failed (retry_check).
# It comes after a full install's first write or after a boot, so it never
# stops the run: it is reported at the end, non-zero. SUMMON_OK stays 1: the
# helper stays linked and running on purpose (it is harmless, and the owner
# may look at it), and the DONE text shows no pin route for it.
check_fail() {
	report_later "Summon checks failed: $1; summonhelper stays linked and running (its log: $DIR/svlog/summonhelper/current)"
}

# removal_fail <reason>: the SUMMON=0 removal could not finish. Like
# summon_fail: reported at the end once anything was written.
removal_fail() {
	summon_soft || die "SUMMON=0 removal not finished: $1"
	SUMMON_OK=0
	report_later "SUMMON=0 removal not finished: $1"
}

# summon_skip <reason>: Summon is skipped for this run (after a boot: a
# missing gdbus or timeout, or SUMMON=1 with no helper installed; after the
# first write, a full install too: no typed yes at S0b's stop sequence or
# removal). No stop sequence and no removal; the helper is neither linked nor
# started (S0b unlinks it); agw and camhelper go on; the reason is reported at
# the end.
summon_skip() {
	SUMMON_OK=0
	if [ "$AFTER_BOOT" = 1 ]; then _when="this boot"; else _when="by this install"; fi
	if [ "$SUMMON" = 1 ]; then
		report_later "Summon is not running: skipped $_when: $1"
	else
		report_later "SUMMON=0 removal skipped $_when: $1"
	fi
}

# summon_report: the Summon failures that did not stop the run, last, with a
# non-zero exit.
summon_report() {
	[ -z "$SUMMON_REPORT" ] || die "$SUMMON_REPORT"
}

# summon_tools: the name checks need gdbus and timeout, with SUMMON=1 or to
# remove a helper; a SUMMON=0 car with nothing to remove never needs them. If
# one is missing, a full install stops, and after a boot Summon is skipped.
summon_tools() {
	[ "$SUMMON_OK" = 1 ] || return 0
	if [ "$SUMMON" = 1 ] || summon_removing; then
		for _t in "$GDBUS" "$TIMEOUT"; do
			command -v "$_t" >/dev/null && continue
			[ "$AFTER_BOOT" = 1 ] || die "${_t##*/} not found (the com.tesla.Webrtc name checks need it)"
			summon_skip "${_t##*/} not found (the com.tesla.Webrtc name checks need it)"
			return 0
		done
	fi
}

# summon_plan prints the install plan's Summon lines, as this run will really
# do them: skipped, installing, kept, removing, or nothing.
summon_plan() {
	if [ "$SUMMON_OK" = 0 ] && [ "$SUMMON" = 1 ]; then
		printf '  SUMMON=1: SKIPPED for this run (the reason is above): summonhelper is neither linked nor started\n'
	elif [ "$SUMMON_OK" = 0 ]; then
		printf '  SUMMON=0: the removal is SKIPPED for this run (the reason is above): summonhelper is unlinked and not started\n'
	elif [ "$SUMMON" = 1 ]; then
		if summon_installing; then
			printf '  SUMMON=1: install summonhelper (%s/summonhelper, uid %s): it SERVES SUMMON (car motion)\n' "$DIR" "$SUMMON_UID"
		else
			printf '  SUMMON=1 (kept: summonhelper is installed; SUMMON=0 removes it): it SERVES SUMMON (car\n'
			printf '     motion) from the installed %s/summonhelper, no new binary (a full SUMMON=1 install copies one)\n' "$DIR"
		fi
		printf '     as its own runit service and owns com.tesla.Webrtc: stock webrtc-comms is stopped first\n'
		printf '     (S0b, again after each boot); agw runs plain "agw serve" and relays to it\n'
	elif summon_removing; then
		printf '  SUMMON=0: REMOVE the summonhelper service and hand com.tesla.Webrtc back to webrtc-comms (S0b)\n'
	else
		printf '  SUMMON=0: no Summon; agw runs plain "agw serve" (SUMMON=1 sh install-car.sh … installs summonhelper)\n'
	fi
}

# summon_install_asks: the questions for steps 4b and 4c (an explicit SUMMON=1
# on a full install), asked with step 0's before the first write, so a "no"
# stops the install with nothing changed. The steps run after step 4
# (summon_install_files).
summon_install_asks() {
	ask "4b. (done after step 4) copy $SUMMON_SRC to $DIR/summonhelper (root:root, mode 0755); mkdir $SUMMON_HOME (root:root 0755) and $SUMMON_STATE ($SUMMON_UID:$SUMMON_UID 0700)"
	SUMMON_KEYGEN=0
	if [ -e "$SUMMON_STATE/key.pem" ]; then
		printf '\n==> 4c. the helper key already exists at %s/key.pem, leaving it alone\n' "$SUMMON_STATE"
	else
		ask "4c. (done after 4b) generate the helper's key $SUMMON_STATE/key.pem as uid $SUMMON_UID (mode 0400; it never leaves the car)"
		SUMMON_KEYGEN=1
	fi
}

# summon_step <command...>: one command of steps 4b and 4c, unless one already
# failed. A failure is recorded in _f and is never fatal: set -e cannot stop
# the install halfway here.
summon_step() {
	[ -z "$_f" ] || return 0
	"$@" || _f="$*"
}

# summon_install_files: steps 4b and 4c, after step 4. Their questions were
# asked before the first write. Each command is checked (summon_step), and the
# first failure skips Summon for the run like any Summon failure after the
# first write: SUMMON_OK=0, the helper unlinked, the failure reported at the
# end, while agw and camhelper are still set up (F1).
summon_install_files() {
	_f=
	_moved=0
	summon_step cp "$SUMMON_SRC" "$DIR/summonhelper.new"
	summon_step chown 0:0 "$DIR/summonhelper.new"
	summon_step chmod 0755 "$DIR/summonhelper.new"
	summon_step mv "$DIR/summonhelper.new" "$DIR/summonhelper"
	if [ -z "$_f" ]; then _moved=1; fi
	summon_step mkdir -p "$SUMMON_STATE"
	summon_step chown 0:0 "$SUMMON_HOME"
	summon_step chmod 0755 "$SUMMON_HOME"
	summon_step chown "$SUMMON_UID:$SUMMON_UID" "$SUMMON_STATE"
	summon_step chmod 0700 "$SUMMON_STATE"
	if [ "$SUMMON_KEYGEN" = 1 ]; then
		summon_step chpst -u ":$SUMMON_UID:$SUMMON_UID" "$DIR/summonhelper" keygen -key "$SUMMON_STATE/key.pem"
	fi
	if [ -z "$_f" ]; then
		echo "4b. installed $DIR/summonhelper (root:root 0755) and $SUMMON_STATE ($SUMMON_UID:$SUMMON_UID 0700)"
		if [ "$SUMMON_KEYGEN" = 1 ]; then echo "4c. generated the helper key $SUMMON_STATE/key.pem as uid $SUMMON_UID"; fi
		return 0
	fi
	rm -f "$DIR/summonhelper.new" || true
	SUMMON_OK=0
	if [ "$_moved" = 1 ]; then
		summon_fail "step 4b/4c failed ($_f): the new summonhelper is in place at $DIR/summonhelper, but it was not linked or started"
	else
		summon_fail "step 4b/4c failed ($_f), so summonhelper was not installed, linked or started"
	fi
	unlink_helper
}

# summon_preflight: summon_tools, and an install needs the summonhelper binary.
summon_preflight() {
	summon_tools
	if summon_installing; then
		[ -f "$SUMMON_SRC" ] || die "summonhelper binary not found at $SUMMON_SRC"
	fi
}

# name_check asks whether com.tesla.Webrtc has an owner (the run script's
# check, under timeout -k 1 5, so at most about 6 s) and leaves the raw answer
# in _o: '(false,)' when it is free, '(true,)' when it has an owner, anything
# else when the check failed.
name_check() {
	_o=$("$TIMEOUT" -k 1 5 "$GDBUS" call --system --dest org.freedesktop.DBus --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.NameHasOwner com.tesla.Webrtc 2>/dev/null) || _o="failed${_o:+: $_o}"
}
name_free() { name_check; [ "$_o" = '(false,)' ]; }
name_owned() { name_check; [ "$_o" = '(true,)' ]; }

# wait_name_free polls name_free every 1 s, up to 10 times: up to about a
# minute, as each check may take 6 s.
wait_name_free() {
	_i=1
	while ! name_free; do
		if [ "$_i" -ge 10 ]; then
			echo "com.tesla.Webrtc still has an owner after 10 checks, 1 s apart (last NameHasOwner answer: ${_o:-none})"
			return 1
		fi
		_i=$((_i + 1))
		sleep 1
	done
	echo "com.tesla.Webrtc has no owner"
}

# wait_name_owned polls name_owned every 1 s, up to 10 times (up to about a
# minute).
wait_name_owned() {
	_i=1
	while ! name_owned; do
		[ "$_i" -lt 10 ] || return 1
		_i=$((_i + 1))
		sleep 1
	done
}

# helper_gone polls every 1 s, up to 10 times, until no summonhelper process
# runs: run (the supervisor), serve (its child), or any other summonhelper
# command (a probe holds the name too, and a finish may still be sending). _pids
# holds the last ones seen.
helper_gone() {
	_i=1
	while _pids=$(running_pids "$DIR/summonhelper ") && [ -n "$_pids" ]; do
		[ "$_i" -lt 10 ] || return 1
		_i=$((_i + 1))
		sleep 1
	done
}

# stop_webrtc_jail: the firmware's own StopSandbox step for webrtc-comms
# (KillOrphanedMinijailProcessComm, /etc/sandbox/sandbox.bash): SIGKILL the pid
# in its pidfile only if it is not 1 and its comm is minijail-init (the init of
# the sandbox's PID namespace), then remove the pidfile.
stop_webrtc_jail() {
	[ -f "$WEBRTC_PIDFILE" ] || return 0
	_pid=$(tr -dc '0-9' <"$WEBRTC_PIDFILE" 2>/dev/null) || _pid=
	if [ -n "$_pid" ] && [ "$_pid" -gt 1 ] 2>/dev/null && [ "$(cat "$PROC/$_pid/comm" 2>/dev/null)" = minijail-init ]; then
		echo "webrtc-comms: SIGKILL its orphaned minijail-init ($_pid)"
		$KILL_BIN -KILL "$_pid" || echo "note: kill -KILL $_pid failed"
	fi
	rm -f "$WEBRTC_PIDFILE"
}

# stop_webrtc: S0b steps 1-4, freeing com.tesla.Webrtc for our helper: our
# helper down first (its takeover and finish run, and it lets go of the name),
# stock webrtc-comms down, its orphaned minijail-init killed, then the name
# polled. sv down does not survive a reboot or a run-mode switch, so the
# helper's run script checks the name again at every start.
stop_webrtc() {
	if $SV_BIN status "$SV/summonhelper" >/dev/null 2>&1; then
		$SV_BIN -w 15 down "$SV/summonhelper" || echo "note: summonhelper did not report down within 15 s"
	fi
	$SV_BIN down "$WEBRTC_SV" || echo "note: webrtc-comms is not supervised now (it has a runsv only in the infotainment and sentry run modes)"
	stop_webrtc_jail
	wait_name_free
}

# helper_up: S0b step 5, once S3 has linked the helper. A runsv that S3 just
# started starts the helper by itself, so sv can fail here until that runsv is
# up; the wait loop checks the helper either way.
helper_up() {
	$SV_BIN up "$SV/summonhelper" || echo "note: summonhelper's runsv is not up yet; it starts the helper itself"
}

# unlink_helper removes our helper's link from the runsvdir dir S3 picks
# (SVDIR) and from our own, wherever the link is ours.
unlink_helper() {
	for _d in ${SVDIR:+"$SVDIR"} "$OWN_SVDIR"; do
		_l="$_d/airgapp-summonhelper"
		if [ -L "$_l" ] && [ "$(readlink "$_l")" = "$SV/summonhelper" ]; then
			rm -f "$_l"
			echo "unlinked $_l"
		fi
	done
}

# summon_stop_stage: S0b with SUMMON=1, after S0, before S2 writes any service
# script, which is after a full install's first write. If the name stays
# taken, or the answer is anything but a typed yes, the helper is neither
# linked nor started: its link from the install goes before S3 can start a
# runsvdir with it, and its run script stays, so the next run tries again. agw
# and camhelper are set up as usual, and the run fails at the end (F1). Once
# the stop sequence ran, a skip hands the name back to stock after S3
# (stock_hand_back).
summon_stop_stage() {
	_q="S0b. stop stock webrtc-comms so summonhelper can own com.tesla.Webrtc: sv -w 15 down our summonhelper if its runsv runs; sv down $WEBRTC_SV; SIGKILL the pid in $WEBRTC_PIDFILE only if its comm is minijail-init, then remove the pidfile; wait until the name has no owner (10 checks, 1 s apart: up to about a minute); summonhelper starts after S3. sv down does not survive a reboot or a run-mode switch (--after-boot redoes this)."
	# S0b always follows a full install's first write (and after a boot a
	# Summon problem never stops the run), so a decline skips, never stops (F1).
	if ! confirm "$_q Any other answer, or a name that stays taken, skips Summon for this run: summonhelper is neither linked nor started, and agw and camhelper are set up as usual"; then
		summon_skip "the S0b stop sequence was not confirmed"
		unlink_helper
		return 0
	fi
	STOCK_STOPPED=1 # from here stock may be down: a skip hands it the name back (stock_hand_back)
	if stop_webrtc; then return 0; fi
	SUMMON_OK=0
	summon_fail "com.tesla.Webrtc still has an owner after webrtc-comms was stopped, so summonhelper was not linked or started"
	unlink_helper
}

# hand_back: removal step 5, and stock_hand_back's and exit_hand_back's, with
# our helper gone and the name free: restart stock webrtc-comms; bringing it up
# is not enough. Stock registers com.tesla.Webrtc once, at its start, and never
# queues for it, so a stock that a run-mode switch started while our helper
# held the name runs without it, and sv up does nothing to a service that
# runs. Then wait for stock to register. It returns 1 only when stock did not
# register (hand_back_failed words it); the caller reports that, and it never
# keeps agw or camhelper down. Once stock was brought up (or has no runsv
# now), S0b's stop is settled: STOCK_STOPPED=0, so no exit hands back again.
hand_back() {
	$SV_BIN -w 15 down "$WEBRTC_SV" || echo "note: webrtc-comms did not report down within 15 s, or has no runsv now"
	stop_webrtc_jail
	if ! $SV_BIN up "$WEBRTC_SV"; then
		STOCK_STOPPED=0
		echo "note: webrtc-comms has no runsv now (a loadshed or lowpower run mode); it registers com.tesla.Webrtc at its next infotainment or sentry start"
		return 0
	fi
	STOCK_STOPPED=0
	if wait_name_owned; then
		echo "stock webrtc-comms owns com.tesla.Webrtc again"
		return 0
	fi
	return 1
}

# hand_back_failed <what happened>: hand_back's failure, after <what happened>,
# with the last NameHasOwner answer (_o).
hand_back_failed() {
	echo "$1, but stock webrtc-comms did not register com.tesla.Webrtc after its restart (10 checks, 1 s apart; last NameHasOwner answer: ${_o:-none}); the Tesla app's WebRTC features need it: reboot the car, or run sv restart $WEBRTC_SV"
}

# summon_remove: SUMMON=0 on a car that has our helper hands com.tesla.Webrtc
# back to stock webrtc-comms (plan call 11). The links and the run script go
# first, so no later --after-boot can bring the helper back. It decides on our
# helper's processes, never on the name: after a reboot stock owns the name
# legitimately, and our helper does not run (our runsvdir starts only at S3).
# Only a summonhelper process that lingers keeps the directory and leaves stock
# alone. If the name is then free, stock is restarted (hand_back). The
# directory goes last; once no summonhelper process runs, a stale live file in
# the run directory goes too. Any answer but a typed yes skips the removal for the
# run instead of stopping it (after a boot, and after a full install's first
# write, F1): the helper is unlinked, so it is neither linked nor started, and
# its directory stays as it is.
summon_remove() {
	_q="S0b. SUMMON=0: remove the summonhelper service and hand com.tesla.Webrtc back to stock webrtc-comms: unlink it and delete its run script, sv -w 15 down it and wait until no summonhelper process runs (up to 10 s); if the name is then free, restart webrtc-comms and wait for it to register (10 checks, 1 s apart: up to about a minute); then delete $SV/summonhelper ($DIR/summonhelper and its key in $SUMMON_STATE stay)."
	# Like S0b's stop sequence: always after a full install's first write, so
	# a decline skips, never stops (F1).
	if ! confirm "$_q Any other answer skips the removal for this run: summonhelper is unlinked and not started, its directory stays as it is, and agw and camhelper are set up as usual"; then
		summon_skip "the removal was declined at its ask (summonhelper is unlinked and not started; its directory is unchanged)"
		unlink_helper
		return 0
	fi
	unlink_helper
	rm -f "$SV/summonhelper/run"
	$SV_BIN -w 15 down "$SV/summonhelper" || true
	if ! helper_gone; then
		removal_fail "a summonhelper process still runs (pid ${_pids% }) after sv down, so webrtc-comms was left alone and $SV/summonhelper stays without its run script: kill it (kill ${_pids% }) or reboot the car, then run install-car.sh with SUMMON=0 again"
		return 0
	fi
	# No summonhelper process runs, so no flock holds the live file: a file
	# left by a killed helper must not outlive the removal (camhelper waives
	# gates while it is locked, and only then).
	rm -f "$SUMMON_RUN_DIR/live"
	if name_owned; then
		echo "com.tesla.Webrtc already has an owner, stock webrtc-comms (no summonhelper process runs): nothing to start"
	else
		hand_back || report_later "$(hand_back_failed "SUMMON=0 removal: summonhelper is removed")"
	fi
	rm -rf "$SV/summonhelper"
	echo "removed $SV/summonhelper"
}

# summon_stage: S0b, after S0 and before S2 writes any service script. When
# Summon is skipped this boot (summon_skip), it only unlinks the helper.
summon_stage() {
	if [ "$SUMMON_OK" = 0 ]; then
		unlink_helper
	elif [ "$SUMMON" = 1 ]; then
		summon_stop_stage
	elif summon_removing; then
		summon_remove
	fi
}

# summon_unsupervised, before S3 with SUMMON=1: no summonhelper may run outside
# our supervision (one helper, never two). One that does skips Summon for the
# run, like any Summon failure after the first write: SUMMON_OK=0, the helper
# unlinked, and a report naming the pids, on a full install and after a boot.
# S0b has stopped stock by then, so stock_hand_back runs after S3.
summon_unsupervised() {
	_pids="$(running_pids "$DIR/summonhelper run")$(running_pids "$DIR/summonhelper serve")"
	[ -n "$_pids" ] || return 0
	supervised "$SV/summonhelper" && return 0
	SUMMON_OK=0
	summon_fail "an unsupervised summonhelper is running (pid ${_pids% }): kill it (kill ${_pids% }), then run install-car.sh again"
	unlink_helper
}

# summon_up_check, after S3 and the wait loop, with SUMMON=1: the helper must
# be running. If it is not, Summon fails for the run like any other Summon
# failure: SUMMON_OK=0, the helper unlinked (neither linked nor started), and
# the failure reported at the end, while agw and camhelper go on (F1). S0b
# stopped stock, so stock_hand_back runs next.
summon_up_check() {
	supervised "$SV/summonhelper" && return 0
	SUMMON_OK=0
	summon_fail "summonhelper is not running; see $DIR/svlog/summonhelper/current"
	unlink_helper
}

# stock_hand_back, after S3 and summon_up_check: Summon was skipped after S0b
# stopped stock webrtc-comms (the name stayed taken, an unsupervised
# summonhelper ran, or the helper was not running after S3), so stock gets
# com.tesla.Webrtc back for this boot, the way the SUMMON=0 removal hands it
# back: our helper unlinked, and sv -w 15 down if its runsv runs (a runsv
# that outlives its link would start it again, and it could take the name
# from a restarting stock); no summonhelper process left (helper_gone, up to
# 10 s); then, only if the name is free, stock restarted and checked
# (hand_back). An owned name starts nothing. It runs once, and only when S0b
# stopped stock and Summon is off for the run: a skip before the stop
# sequence (a decline, a missing tool, a failing 4b/4c command, which all come
# before S0b) never stopped stock, and a failed check after S4 leaves the
# helper running and owning the name. A summonhelper process that does not go
# away (it may hold the name), or a stock that does not register, is reported
# at the end; neither ever stops the run. STOCK_STOPPED stays 1 until it has
# decided (hand_back clears it once stock is up), so an exit that cuts it short
# still hands stock back (exit_hand_back), and none after it does so again.
stock_hand_back() {
	[ "$STOCK_STOPPED" = 1 ] && [ "$SUMMON_OK" = 0 ] || return 0
	unlink_helper
	if $SV_BIN status "$SV/summonhelper" >/dev/null 2>&1; then
		$SV_BIN -w 15 down "$SV/summonhelper" || echo "note: summonhelper did not report down within 15 s"
	fi
	if ! helper_gone; then
		STOCK_STOPPED=0
		report_later "stock webrtc-comms stays stopped: a summonhelper process still runs (pid ${_pids% }) and may hold com.tesla.Webrtc; kill it (kill ${_pids% }) or reboot the car"
		return 0
	fi
	if name_owned; then
		STOCK_STOPPED=0
		echo "com.tesla.Webrtc already has an owner (no summonhelper process runs): stock webrtc-comms is not restarted"
		return 0
	fi
	hand_back || report_later "$(hand_back_failed "Summon was skipped after S0b stopped stock webrtc-comms, which was restarted")"
}

# helper_linked: our helper's link is in the runsvdir dir S3 picks (SVDIR) or
# in our own, where S3 links it and where it stays from one boot to the next.
helper_linked() {
	for _d in ${SVDIR:+"$SVDIR"} "$OWN_SVDIR"; do
		_l="$_d/airgapp-summonhelper"
		if [ -L "$_l" ] && [ "$(readlink "$_l")" = "$SV/summonhelper" ]; then return 0; fi
	done
	return 1
}

# helper_supervised: our helper is supervised by a running runsv: sv status
# answers for it (its runsv is up), and that runsv has not been told to stop it
# or keep it down: no "want down" (told to stop, the helper is still stopping:
# "run:" or "finish:") and no "down:" without "want up" (stopped and kept down).
# S0b's sv down leaves one of the two. A link with no such runsv starts
# nothing: after a boot until S3 starts our runsvdir, or once S0b brought the
# helper down in a run whose runsvdir was already up. Only the helper's own
# part of the status counts, not its log service's.
helper_supervised() {
	_st=$($SV_BIN status "$SV/summonhelper" 2>/dev/null) || return 1
	_st=${_st%%;*}
	case "$_st" in
	*"want down"*) return 1 ;;
	down:*"want up"*) return 0 ;;
	down:*) return 1 ;;
	esac
	return 0
}

# exit_hand_back, the real run's EXIT trap (exit_trap_on). It acts only when
# S0b's stop sequence stopped stock webrtc-comms for our helper
# (STOCK_STOPPED=1) and the run exits before a hand-back settled that or the
# helper's checks passed: a die (a "no" at S2, S3 or S4, an unsupervised agw or
# camhelper, agw or camhelper not running, the report at the end), an error
# under set -e, INT or TERM (exit_trap_on), or the normal end. Best effort:
# - our helper is linked and supervised by a running runsv (helper_linked,
#   helper_supervised): it is the installed owner of com.tesla.Webrtc, so
#   nothing is touched (after failed checks it stays linked and running on
#   purpose, as DONE says);
# - otherwise it starts nothing: not linked (a first install that stopped
#   before S3, or a skip that unlinked it), or linked with no runsv to run it
#   (after a boot until S3 starts our runsvdir; a link left on /var/lib between
#   boots) or with a runsv told to stop it or keep it down (S0b's sv down; the
#   helper may still be stopping). Its runsv, if one still runs, is brought
#   down; then, checking every 1 s up to 10 times, once no summonhelper
#   process runs and the name is free, stock is restarted and checked
#   (hand_back). If a summonhelper process remains, or the name stays owned
#   (stock still letting go of it, say), a line says so and stock is not
#   restarted.
# It never dies, never asks, and keeps the run's exit status. It clears
# STOCK_STOPPED and its EXIT, INT and TERM traps first, so it runs once; HUP
# and PIPE stay ignored (exit_trap_on), so neither a dropped SSH session nor a
# dead reader of the output can cut it short. Subshells reset traps, so it
# never runs inside one.
exit_hand_back() {
	_rc=$?
	set +e
	trap - EXIT INT TERM
	[ "$STOCK_STOPPED" = 1 ] || exit "$_rc"
	STOCK_STOPPED=0
	helper_linked && helper_supervised && exit "$_rc"
	if $SV_BIN status "$SV/summonhelper" >/dev/null 2>&1; then
		$SV_BIN -w 15 down "$SV/summonhelper" || echo "note: summonhelper did not report down within 15 s"
	fi
	_i=1
	while _pids=$(running_pids "$DIR/summonhelper "); [ -n "$_pids" ] || ! name_free; do
		if [ "$_i" -ge 10 ]; then
			if [ -n "$_pids" ]; then
				echo "install-car: on the way out: a summonhelper process still runs (pid ${_pids% }), so stock webrtc-comms was not restarted"
			else
				echo "install-car: on the way out: com.tesla.Webrtc is still not free after 10 checks, 1 s apart (last NameHasOwner answer: ${_o:-none}), so stock webrtc-comms was not restarted"
			fi
			exit "$_rc"
		fi
		_i=$((_i + 1))
		sleep 1
	done
	echo "install-car: on the way out: S0b stopped stock webrtc-comms and nothing of ours serves com.tesla.Webrtc: restarting stock"
	hand_back || echo "install-car: $(hand_back_failed "stock webrtc-comms was restarted on the way out")"
	exit "$_rc"
}

# exit_trap_on: the real run's exit path (exit_hand_back) on every exit. INT
# (Ctrl-C) and TERM become an exit with their usual status (130, 143), so the
# trap runs for them too; a second one during the trap ends it at once. HUP and
# PIPE are ignored, by the run and, inherited, by every command it starts (our
# runsvdir from S3 too: the run scripts have no pipeline, and the Go services
# set their own PIPE handling): neither a dropped SSH session nor a dead reader
# of the output can kill the run or its exit path. Over the dead session the
# run ends at its next question (EOF: "no answer"; S0b's confirm takes it as a
# no, a skip, and goes on) or its next write there that set -e checks (EIO on a
# terminal, EPIPE on a pipe or socket), and the EXIT trap runs. A failed write
# that set -e does not check (in a condition, or before || or &&) ends
# nothing: the next question, checked write or the run's end does. A run whose
# answers and output do not go through the session goes on to its end. Not
# covered: SIGKILL and the other signals the run does not trap, which end it
# without the EXIT trap.
exit_trap_on() {
	trap exit_hand_back EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	trap '' HUP
	trap '' PIPE
}

# retry_check <what> <command...>: runs the command every 1 s, up to 15 times,
# until it exits 0. The exit status decides, never the text; a failure carries
# the last output, verbatim (check_fail).
retry_check() {
	_what=$1
	shift
	_n=1
	until _out=$("$@" 2>&1); do
		if [ "$_n" -ge 15 ]; then
			check_fail "the $_what check failed 15 times, 1 s apart; its last output: $_out"
			return 1
		fi
		_n=$((_n + 1))
		sleep 1
	done
	printf '%s\n' "$_out"
}

# summon_checks, after the last restart, with SUMMON=1: uid 4950 reaches the
# helper's socket and the helper owns the name (probe-summon, the cross-uid
# connect test), and the route gate passes as uid 4950 (probe-route). Both are
# retried: a stale socket and the helper's first D2 stamp need it. The
# installer runs no pin: the DONE text gives the owner the helper's pin route,
# which never prints the pin, so the VIN never reaches its output or a command
# line.
summon_checks() {
	[ -z "$SUMMON_REPORT" ] || return 0
	retry_check "summon relay" chpst -u "$RUN_AS" "$DIR/agw" probe-summon || return 0
	retry_check "route gate" chpst -u "$RUN_AS" "$DIR/agw" probe-route || return 0
	SUMMON_CHECKED=1
	STOCK_STOPPED=0 # our helper owns the name (OWNED): stock stays down on purpose
}

# summon_done prints the DONE text's Summon lines, with SUMMON=1: the helper's
# pin route once its checks passed. The pin holds the VIN, and the car records
# SSH sessions, so the route never prints it: a fresh mktemp file (0600, made
# with O_EXCL, so nothing planted in /tmp is ever opened), sent to the laptop
# with socat, then removed (docs/SUMMON.md, "The pins"). The installer only
# prints the command. After a failed check the helper stays linked and running,
# on purpose (it is harmless, and the owner may look at it), so no route is
# shown: the lines say so and point to the report.
summon_done() {
	summon_on || return 0
	if [ "$SUMMON_CHECKED" = 1 ]; then
		cat <<PIN
   With SUMMON=1, summonhelper has a pin of its own (its key, for the inner
   session). It holds the VIN: never print it here (the car records SSH
   sessions). Start nc -l <port> > sumpin on the laptop, set LAPTOP (the
   laptop's tailnet address) and PORT here, then run (docs/SUMMON.md, "The pins"):
PIN
		# shellcheck disable=SC2016 # $P, $LAPTOP and $PORT are the owner's, expanded when the owner runs the line
		printf '  P=$(mktemp) && chpst -u :%s:%s %s/summonhelper pin -vin-file %s > "$P" && wc -c < "$P" && socat -u FILE:"$P" "TCP:$LAPTOP:$PORT"; rm -f "$P"\n' \
			"$SUMMON_UID" "$SUMMON_UID" "$DIR" "$VIN_FILE"
	else
		printf '   summonhelper'"'"'s pin is not shown: its checks failed (the report below). It stays linked\n'
		printf '   and running on purpose, so you can look at it: %s/svlog/summonhelper/current\n' "$DIR"
	fi
}

# gen_summon_scripts sets SUMMON_RUN and SUMMON_FINISH, summonhelper's runit
# scripts (as root). Every refusal of run follows agw's: the reason, a 10 s
# pause (runsv restarts run at once), exit 1. An empty -authd-sha256 would stop
# the helper, so the flag appears only with a pin (as in gen_helper_run).
gen_summon_scripts() {
	SUMMON_RUN=$(cat <<EOF
#!/bin/sh
# summonhelper (runit run, as root). Generated by install-car.sh. It prepares
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
SUMMONHELPER_VIN="\$VIN" exec chpst -u :$SUMMON_UID:$SUMMON_UID "$DIR/summonhelper" run${AUTHD_SHA256:+ -authd-sha256 $AUTHD_SHA256}
EOF
)
	SUMMON_FINISH=$(cat <<EOF
#!/bin/sh
# summonhelper (runit finish). Generated by install-car.sh. It runs after every
# exit of run, also when the supervisor itself was killed: as uid $SUMMON_UID,
# summonhelper finish sends cmd_abort and the disconnect on com.tesla.Webrtc if
# a session marker is left, and first removes a stale live file.
exec 2>&1
exec chpst -u :$SUMMON_UID:$SUMMON_UID "$DIR/summonhelper" finish
EOF
)
}

# The Summon test hooks below run on a test layout and fakes only: DIR (and so
# SV and our runsvdir) from AGW_TEST_DIR, the runsvdir dir S3 would pick from
# AGW_TEST_SVDIR, the helper's state from AGW_TEST_SUMMON_HOME, the installed
# helper and agw run scripts from AGW_TEST_SUMMON_RUN and AGW_TEST_RUN
# (default: the test layout's), and /proc and /run from test_layout_paths.
summon_hook_layout() {
	test_layout_paths
	DIR=${AGW_TEST_DIR:-$DIR}
	SUMMON_HOME=${AGW_TEST_SUMMON_HOME:-$SUMMON_HOME}
	SUMMON_STATE=$SUMMON_HOME/state
	SV=$DIR/sv
	OWN_SVDIR=$DIR/svdir
	SVDIR=${AGW_TEST_SVDIR:-}
	SUMMON_INSTALLED_RUN=${AGW_TEST_SUMMON_RUN:-$SV/summonhelper/run}
	AGW_INSTALLED_RUN=${AGW_TEST_RUN:-$DIR/sv/agw/run}
}

# hook_needs <variable>...: a test hook that changes state runs only with these
# overrides set, so it can never reach the car's real services.
hook_needs() {
	for _v in "$@"; do
		eval "_x=\${$_v:-}"
		[ -n "$_x" ] || die "this test hook needs $_v (it never touches the car's real services)"
	done
}

# Test hook: AGW_SUMMON_SCRIPTS_ONLY=1 [AUTHD_SHA256=<pin>] [AGW_TEST_HELPER_RUN=<camhelper's installed run>]
# sh install-car.sh [--after-boot] prints summonhelper's run and finish scripts.
if [ "${AGW_SUMMON_SCRIPTS_ONLY:-}" = 1 ]; then
	resolve_authd_pin
	gen_summon_scripts
	printf '%s\n---- finish ----\n%s\n' "$SUMMON_RUN" "$SUMMON_FINISH"
	exit 0
fi

# Test hook: AGW_STOP_WEBRTC_ONLY=1 (fakes, test layout) sh install-car.sh [--after-boot]
# resolves SUMMON and its tools and, with SUMMON=1, runs only S0b (the stop
# sequence, or the skip), the unsupervised-helper check before S3
# (summon_unsupervised), step 5 right after them (there is no S3 here) and
# the hand-back after a skip (stock_hand_back), then prints the services S3
# would link and start, and the Summon report.
if [ "${AGW_STOP_WEBRTC_ONLY:-}" = 1 ]; then
	hook_needs AGW_TEST_SV AGW_TEST_KILL AGW_TEST_GDBUS AGW_TEST_TIMEOUT AGW_TEST_WEBRTC_PIDFILE AGW_TEST_PROC AGW_TEST_PROCFS AGW_TEST_DIR
	summon_hook_layout
	summon_mode
	summon_tools
	WRITTEN=1 # S0b comes after a full install's first write
	if [ "$SUMMON" = 1 ]; then
		summon_stage
		if summon_on; then summon_unsupervised; fi
		if summon_on; then helper_up; fi
		stock_hand_back
	fi
	echo "S3 services: $(services)"
	summon_report
	exit 0
fi

# Test hook: AGW_SUMMON_REMOVE_ONLY=1 (fakes, test layout, a fake /proc in
# AGW_TEST_PROCFS) sh install-car.sh [--after-boot] resolves SUMMON and its tools and,
# with SUMMON=0, runs only S0b (the removal, or the skip), then prints the
# services S3 would link and start, and the Summon report.
if [ "${AGW_SUMMON_REMOVE_ONLY:-}" = 1 ]; then
	hook_needs AGW_TEST_SV AGW_TEST_KILL AGW_TEST_GDBUS AGW_TEST_TIMEOUT AGW_TEST_WEBRTC_PIDFILE AGW_TEST_PROC AGW_TEST_PROCFS AGW_TEST_DIR AGW_TEST_SUMMON_RUN_DIR
	summon_hook_layout
	summon_mode
	summon_tools
	WRITTEN=1 # S0b comes after a full install's first write
	if [ "$SUMMON" = 0 ]; then summon_stage; fi
	echo "S3 services: $(services)"
	summon_report
	exit 0
fi

# Test hook: AGW_SUMMON_PREFLIGHT_ONLY=1 [AGW_TEST_DIR=<layout>] sh install-car.sh [--after-boot]
# [<agw> <camhelper> <summonhelper>] resolves SUMMON, runs only the Summon
# preflight (before a full install's first write), and prints the plan's Summon
# lines.
if [ "${AGW_SUMMON_PREFLIGHT_ONLY:-}" = 1 ]; then
	summon_hook_layout
	summon_mode
	summon_preflight
	echo "preflight ok (SUMMON=$SUMMON)"
	summon_plan
	summon_report
	exit 0
fi

# Test hook: AGW_SUMMON_CHECKS_ONLY=1 (fakes, a fake chpst on PATH, test layout) sh install-car.sh [--after-boot]
# runs only what follows S3: the helper must be running (with SUMMON=1), and
# if it is not, the hand-back (stock_hand_back: Summon on at S3 means S0b's
# stop sequence ran), then the services S4 restarts are printed, then the
# checks.
if [ "${AGW_SUMMON_CHECKS_ONLY:-}" = 1 ]; then
	hook_needs AGW_TEST_SV AGW_TEST_DIR AGW_TEST_VIN_FILE AGW_TEST_GDBUS AGW_TEST_TIMEOUT AGW_TEST_KILL AGW_TEST_WEBRTC_PIDFILE AGW_TEST_PROC AGW_TEST_PROCFS
	summon_hook_layout
	summon_mode
	WRITTEN=1 # all of this comes after a full install's first write
	if summon_on; then
		STOCK_STOPPED=1 # Summon still on at S3: S0b's stop sequence ran
		summon_up_check
	fi
	stock_hand_back
	echo "S4 services: $(services)"
	summon_checks
	summon_done
	summon_report
	exit 0
fi

# Test hook: AGW_EXIT_PATH_ONLY=1 (fakes, a fake chpst on PATH, test layout) sh install-car.sh [--after-boot]
# runs the main flow's Summon steps, in its order, under the real run's exit
# path (exit_trap_on): the tools (a die there comes before S0b), S0b, the S2
# question, the unsupervised-helper check, the S3 question, step 5, the check
# that the helper runs, the hand-back after a skip, the S4 question, the
# checks, and the real end (end_report). The questions are the real ask, each
# standing in for its step's: a "no" dies. Every exit runs exit_hand_back.
if [ "${AGW_EXIT_PATH_ONLY:-}" = 1 ]; then
	hook_needs AGW_TEST_SV AGW_TEST_KILL AGW_TEST_GDBUS AGW_TEST_TIMEOUT AGW_TEST_WEBRTC_PIDFILE AGW_TEST_PROC AGW_TEST_PROCFS AGW_TEST_DIR AGW_TEST_VIN_FILE AGW_TEST_SUMMON_RUN_DIR
	summon_hook_layout
	exit_trap_on
	summon_mode
	summon_tools
	WRITTEN=1 # S0b comes after a full install's first write
	summon_stage
	ask "S2. write the service scripts (this hook's stand-in for the real S2 question)"
	if summon_on; then summon_unsupervised; fi
	ask "S3. link the services and start our runsvdir (this hook's stand-in for the real S3 question)"
	if summon_on; then helper_up; fi
	if summon_on; then summon_up_check; fi
	stock_hand_back
	ask "S4. restart agw and camhelper (this hook's stand-in for the real S4 question)"
	if summon_on; then summon_checks; fi
	echo "S4 services: $(services)"
	end_report
	exit 0
fi

# Test hook: AGW_SUMMON_FILES_ONLY=1 SUMMON=1 (test layout, fakes for chown and
# chpst on PATH) sh install-car.sh <agw> <camhelper> <summonhelper> asks steps
# 4b and 4c's questions (before a full install's first write), then runs their
# commands (after it), then prints the services S3 would link, and the report.
if [ "${AGW_SUMMON_FILES_ONLY:-}" = 1 ]; then
	hook_needs AGW_TEST_DIR AGW_TEST_SUMMON_HOME
	summon_hook_layout
	summon_mode
	summon_preflight
	if summon_installing; then summon_install_asks; fi
	WRITTEN=1 # 4b and 4c's commands come after a full install's first write
	if summon_installing; then summon_install_files; fi
	echo "S3 services: $(services)"
	summon_report
	exit 0
fi

# Test hook: AGW_AUTHD_PIN_ONLY=1 [AUTHD_SHA256=<pin>] AGW_TEST_HELPER_RUN=<camhelper's installed run>
# AGW_TEST_AUTHD=<authd> AGW_TEST_SHA256SUM=<sha256sum> sh install-car.sh [--after-boot] resolves the authd pin,
# prints the plan's sign-in line, runs the check against the car's authd (a full
# install's ask comes before its first write), then prints both helpers' run
# lines, and ends as the real run does (end_report: after a boot, the note and
# a non-zero exit).
if [ "${AGW_AUTHD_PIN_ONLY:-}" = 1 ]; then
	hook_needs AGW_TEST_AUTHD AGW_TEST_SHA256SUM
	resolve_authd_pin
	authd_pin_line
	check_authd_pin
	gen_helper_run
	gen_summon_scripts
	printf 'camhelper run: %s\nsummonhelper run: %s\n' "$(printf '%s\n' "$HELPER_RUN" | tail -n 1)" "$(printf '%s\n' "$SUMMON_RUN" | tail -n 1)"
	end_report
	exit 0
fi

# The real install path. Any AGW_TEST_* override in the environment, even an
# empty one, is for the test hooks above only: refuse it here, before the root
# check, so none can reach a real install (AGW_TEST_SUMMON_UID=0 would bake a
# root summonhelper into its run script; AGW_TEST_IPT a fake iptables into
# agw's).
if env | grep -q '^AGW_TEST_'; then die "AGW_TEST_* is set: test hooks only, refusing the real install"; fi

[ "$(id -u)" = 0 ] || die "run as root"
# From here every exit, a die included, runs exit_hand_back: after S0b's stop
# sequence, no exit leaves com.tesla.Webrtc without an owner.
exit_trap_on
resolve_pi_ip
if [ -n "${CAR_IP:-}" ]; then echo "note: CAR_IP is no longer used (the car dials out to the Pi); ignoring it"; fi
if [ -n "$_env_layout" ]; then echo "note:$_env_layout no longer read from the environment (S3 always uses our own runsvdir on $OWN_SVDIR); ignoring it"; fi
if [ "$AFTER_BOOT" = 0 ]; then
	[ -f "$AGW_SRC" ] || die "agw binary not found at $AGW_SRC"
	[ -f "$HELPER_SRC" ] || die "camhelper binary not found at $HELPER_SRC"
fi
command -v chpst >/dev/null || die "chpst not found"
command -v iptables >/dev/null || die "iptables not found"
command -v ip6tables >/dev/null || die "ip6tables not found"
for _t in runsv runsvdir sv svlogd setsid readlink; do command -v "$_t" >/dev/null || die "$_t not found"; done
summon_mode
summon_preflight
resolve_authd_pin

printf 'Gateway install plan (the car dials the Pi at %s:%s, uid:gid %s:%s):\n' \
	"$PI_IP" "$PI_PORT" "$UID_GW" "$GID_GW"
printf '  1. create %s (root:root 0755) and %s/state (%s:%s 0700)\n' "$DIR" "$DIR" "$UID_GW" "$GID_GW"
printf '  2. install the agw binary to %s/agw (root:root 0755)\n' "$DIR"
printf '  3. (no key list: a phone signs in when the car lists its key as owner or driver)\n'
printf '  4. generate the long-term key ON THE CAR in state/ (%s:%s, 0400)\n' "$UID_GW" "$GID_GW"
printf '  5c. lock uid %s egress to TCP to %s:%s on wlan0 (per-boot OUTPUT owner chain, IPv4 and IPv6); no INPUT rule\n' "$UID_GW" "$PI_IP" "$PI_PORT"
authd_pin_line
printf '  6. (camera root helper section) install camhelper, write the runit services for\n'
printf '     agw and camhelper, and start them under runit (re-attached after each boot\n'
printf '     with --after-boot)\n'
summon_plan

# The authd pin against this car's authd: before a full install's first write,
# so a "no" stops it with nothing changed; after a boot, a note at the end.
check_authd_pin

if [ "$AFTER_BOOT" = 0 ]; then

if summon_installing; then
	ask "0. SUMMON=1: install summonhelper, which serves SUMMON (car motion) and takes com.tesla.Webrtc from stock webrtc-comms; motion tests are owner-only (docs/SUMMON.md)"
	summon_install_asks
fi

ask "1. mkdir $DIR (root:root 0755) and $STATE ($UID_GW:$GID_GW 0700)"
# The first write of a full install (F1). Everything above is a check or a
# question, and a "no" so far has changed nothing; steps 4b and 4c were asked
# about there too. From here on no Summon failure or question stops the
# install halfway: 4b and 4c's commands, S0b, the unsupervised-helper check
# and the check that the helper runs after S3 all skip Summon for the run
# instead, and agw and camhelper are still set up (S2-S4).
WRITTEN=1
mkdir -p "$DIR" "$STATE"
chown 0:0 "$DIR"
chmod 0755 "$DIR"
chown "$UID_GW:$GID_GW" "$STATE"
chmod 0700 "$STATE"

ask "1b. bind-mount $DIR onto itself without noexec/nosymfollow (util-linux needs exec,symfollow named to clear them; lost at reboot, --after-boot redoes it)"
exec_mount "$DIR"

ask "2. copy $AGW_SRC to $DIR/agw (root:root, mode 0755)"
cp "$AGW_SRC" "$DIR/agw.new"
chown 0:0 "$DIR/agw.new"
chmod 0755 "$DIR/agw.new"
mv "$DIR/agw.new" "$DIR/agw"

# 3. No key list to create: access is the car's own. A phone signs in when it
# proves possession of its key and authd lists that key as OWNER or DRIVER
# (the keys managed in the Tesla app or with the key card). An allow file an
# older install left is no longer read; it is left alone.
if [ -e "$DIR/allow" ]; then
	printf '\n==> 3. note: %s/allow is left from an older install; nothing reads it now (rm it at will)\n' "$DIR"
fi

if [ -e "$STATE/key.pem" ]; then
	printf '\n==> 4. key already exists at %s/key.pem, leaving it alone\n' "$STATE"
else
	ask "4. generate $STATE/key.pem as uid $UID_GW (mode 0400; it never leaves the car)"
	chpst -u "$RUN_AS" "$DIR/agw" keygen -key "$STATE/key.pem"
fi

# An explicit SUMMON=1: steps 4b and 4c, already asked about before the first
# write (summon_install_asks). A failure skips Summon for the run.
if summon_installing; then summon_install_files; fi

fi # AFTER_BOOT = 0

# Per-boot: the egress lock (iptables rules are not saved). After each boot
# run this script with --after-boot (steps 5c, S0-S3 only). No INPUT rule:
# the car's Wi-Fi module drops every inbound packet, so agw dials out.
EGRESS_ACCEPT=$(egress_accept_rule)
ask "5c. egress lock for uid $UID_GW: chain $EGRESS_CHAIN = [$EGRESS_ACCEPT; DROP], jumped to first in OUTPUT; ip6tables: [DROP] (lost at reboot; re-run after each boot)"
egress_lock "$IPT" "$EGRESS_ACCEPT"
egress_lock "$IP6T"

# uid 4950 must not reach the system bus. The 0c5315d installer could lay an
# account overlay over the passwd file; one still mounted in this boot is
# removed (asked). dbus-daemon keeps a uid it once resolved until it restarts,
# so after one was in place uid 4950 can reach the bus until the car reboots.
PASSWD_FILE=/etc/passwd
if [ -n "$(mount_opts "$PASSWD_FILE")" ]; then
	if [ "$(tail -n 1 "$PASSWD_FILE")" = "airgapp-gw:x:$UID_GW:$GID_GW::/nonexistent:/bin/false" ]; then
		ask "remove the old airgapp-gw account overlay: umount $PASSWD_FILE (uid $UID_GW must not reach the system bus; reboot the car afterwards, dbus-daemon remembers the uid)"
		umount "$PASSWD_FILE" || die "umount $PASSWD_FILE failed"
	else
		echo "note: $PASSWD_FILE is a mount that is not ours; leaving it"
	fi
fi

# 6. Starting agw is the supervision section below (runit), never a bare
# nohup here: one supervised agw, never two.

# ==== CAMERA ROOT HELPER (owned by the camera session) ====
# This section owns the runit services, agw and camhelper (and summonhelper
# with SUMMON=1), and the per-boot re-attach (docs/CAMERA-HELPER.md, PLAN §4.9):
#   helper binary:   /var/lib/airgapp-camhelper/camhelper (root:root 0755, outside $DIR)
#   runit services:  $DIR/sv/agw and $DIR/sv/camhelper (run + finish + log/run, root 0755);
#                    SUMMON=1 adds $DIR/sv/summonhelper (see "SUMMON" above)
#   logs:            svlogd, 3 x 1 MB each, in $DIR/svlog/agw and /var/lib/airgapp-camhelper/svlog
#                    (and $DIR/svlog/summonhelper)
#   socket path:     /run/airgapp-camhelper/camhelper.sock (dir root:4950 0750, socket 0660)
#   helper uid:      root; its children run as uid 4951, groups audio 29 + pipewire 1021
# The helper creates its own /run dirs at start and refuses to run twice. The
# camhelper finish hook runs `camhelper sweep`, which clears whatever the
# helper's markers record (requestAudio 0, sessionActive 0, selectable 0, gain
# -200 only if marked), so a crash never strands a keep-awake latch.

AGW_LOG=$DIR/svlog/agw
HELPER_LOG=$HELPER_DIR/svlog
SUMMON_LOG=$DIR/svlog/summonhelper
case "$HELPER_WRITER" in pw-cat|aplay) ;; *) die "HELPER_WRITER must be pw-cat or aplay" ;; esac

if [ "$AFTER_BOOT" = 0 ]; then
	ask "S1. copy $HELPER_SRC to $HELPER_BIN (dir and binary root:root 0755)"
	mkdir -p "$HELPER_DIR"
	chown 0:0 "$HELPER_DIR"
	chmod 0755 "$HELPER_DIR"
	cp "$HELPER_SRC" "$HELPER_BIN.new"
	chown 0:0 "$HELPER_BIN.new"
	chmod 0755 "$HELPER_BIN.new"
	mv "$HELPER_BIN.new" "$HELPER_BIN"
fi

# The service scripts (gen_agw_scripts and gen_summon_scripts above).
gen_agw_scripts
gen_helper_run
gen_summon_scripts
HELPER_FINISH=$(cat <<EOF
#!/bin/sh
# camera root helper (runit finish): clear whatever the markers record
# (requestAudio 0, sessionActive 0, selectable 0, gain -200 only if marked).
exec 2>&1
exec $HELPER_BIN sweep -state $HELPER_STATE
EOF
)

# log/run for each service: svlogd, at most 3 files of 1 MB (log dir config).
log_run() {
	printf '#!/bin/sh\n# svlogd for %s (runit log). Generated by install-car.sh.\nexec svlogd -tt %s\n' "$1" "$2"
}

ask "S0. bind-mount $DIR and $HELPER_DIR executable (no noexec/nosymfollow; lost at reboot, --after-boot redoes it)"
exec_mount "$DIR"
exec_mount "$HELPER_DIR"

# S3 links only into our own runsvdir (review D2). The firmware's runsvdir
# watches /service, which resolves to the rootfs, and every runsvdir under /run
# on this car belongs to another supervisor (coilshift's /run/ssp/services,
# 2026-11-25) or to whoever started it: a runsvdir is found from any process's
# cmdline, so agw could fake one. SVDIR stays empty; unlink_helper and
# helper_linked still look in it, for links an older installer made.
SVDIR=

# S0b. SUMMON=1: stop stock webrtc-comms (summon_stop_stage). An explicit
# SUMMON=0 on a car that has our helper: remove it (summon_remove). Both ask
# first. Summon skipped this boot (summon_skip) only unlinks the helper.
summon_stage

printf '\nagw run script:\n%s\n\nagw finish script:\n%s\n\ncamhelper run script:\n%s\n' "$AGW_RUN" "$AGW_FINISH" "$HELPER_RUN"
if summon_on; then
	printf '\nsummonhelper run script:\n%s\n\nsummonhelper finish script:\n%s\n' "$SUMMON_RUN" "$SUMMON_FINISH"
fi
_agw_was_up=0
if supervised "$SV/agw"; then _agw_was_up=1; fi
_agw_changed=0
if [ "$(cat "$SV/agw/run" 2>/dev/null)" != "$AGW_RUN" ] || [ "$(cat "$SV/agw/finish" 2>/dev/null)" != "$AGW_FINISH" ]; then _agw_changed=1; fi
_helper_was_up=0
if supervised "$SV/camhelper"; then _helper_was_up=1; fi
_helper_changed=0
if [ "$(cat "$SV/camhelper/run" 2>/dev/null)" != "$HELPER_RUN" ]; then _helper_changed=1; fi
if summon_on; then _s2=" and $SV/summonhelper/{run,finish,log/run}, log dir $SUMMON_LOG"; else _s2=; fi
ask "S2. write $SV/agw/{run,finish,log/run} and $SV/camhelper/{run,finish,log/run}, log dirs $AGW_LOG and $HELPER_LOG$_s2 (root, only if changed)"
mkdir -p "$SV/agw/log" "$SV/camhelper/log" "$AGW_LOG" "$HELPER_LOG"
chown 0:0 "$SV" "$SV/agw" "$SV/camhelper" "$SV/agw/log" "$SV/camhelper/log" "$DIR/svlog" "$AGW_LOG" "$HELPER_LOG"
chmod 0755 "$SV" "$SV/agw" "$SV/camhelper" "$SV/agw/log" "$SV/camhelper/log"
chmod 0750 "$DIR/svlog" "$AGW_LOG" "$HELPER_LOG"
write_if_changed "$SV/agw/run" "$AGW_RUN"
write_if_changed "$SV/agw/finish" "$AGW_FINISH"
write_if_changed "$SV/agw/log/run" "$(log_run agw "$AGW_LOG")"
write_if_changed "$SV/camhelper/run" "$HELPER_RUN"
write_if_changed "$SV/camhelper/finish" "$HELPER_FINISH"
write_if_changed "$SV/camhelper/log/run" "$(log_run camhelper "$HELPER_LOG")"
_logs="$AGW_LOG $HELPER_LOG"
if summon_on; then
	mkdir -p "$SV/summonhelper/log" "$SUMMON_LOG"
	chown 0:0 "$SV/summonhelper" "$SV/summonhelper/log" "$SUMMON_LOG"
	chmod 0755 "$SV/summonhelper" "$SV/summonhelper/log"
	chmod 0750 "$SUMMON_LOG"
	write_if_changed "$SV/summonhelper/run" "$SUMMON_RUN"
	write_if_changed "$SV/summonhelper/finish" "$SUMMON_FINISH"
	write_if_changed "$SV/summonhelper/log/run" "$(log_run summonhelper "$SUMMON_LOG")"
	_logs="$_logs $SUMMON_LOG"
fi
for _l in $_logs; do
	if [ "$(cat "$_l/config" 2>/dev/null)" != "$(printf 's1000000\nn3')" ]; then
		printf 's1000000\nn3\n' >"$_l/config"
		chmod 0640 "$_l/config"
		echo "wrote $_l/config"
	fi
done

# One of each service, never two: refuse while an unsupervised agw or
# camhelper runs (the camera's rule). An unsupervised summonhelper skips Summon
# for the run instead (summon_unsupervised).
for _svc in agw camhelper; do
	if supervised "$SV/$_svc"; then continue; fi
	if [ "$_svc" = agw ]; then _pids="$(running_pids "$DIR/agw serve")$(running_pids "$DIR/agw supervise")"; else _pids=$(running_pids "$HELPER_BIN run"); fi
	if [ -n "$_pids" ]; then
		die "an unsupervised $_svc is running (pid $_pids): stop it first (kill $_pids), then re-run"
	fi
done
if summon_on; then summon_unsupervised; fi

# S3. Attach the services to our own runsvdir (never another one, see SVDIR
# above): started with setsid, its links on /var/lib persist, and --after-boot
# starts it again. After a boot whose S0b failed, S0b has already unlinked the
# helper, and it is not in the list.
_svcs=$(services)
printf '\nUsing our own runsvdir on %s (runsvdirs running: %s; another supervisor is never linked into).\n' "$OWN_SVDIR" "$(detect_svdirs | tr '\n' ' ')"
if [ -n "$(running_pids "runsvdir -P $OWN_SVDIR")" ]; then
	echo "S3. our runsvdir on $OWN_SVDIR is already running"
	attached "$OWN_SVDIR" || { ask "S3. link $_svcs into $OWN_SVDIR"; link_into "$OWN_SVDIR"; }
else
	ask "S3. link $_svcs into $OWN_SVDIR and start: setsid runsvdir -P $OWN_SVDIR (lost at reboot; run --after-boot after each boot)"
	mkdir -p "$OWN_SVDIR"
	chown 0:0 "$OWN_SVDIR"
	chmod 0755 "$OWN_SVDIR"
	link_into "$OWN_SVDIR"
	setsid runsvdir -P "$OWN_SVDIR" </dev/null >/dev/null 2>&1 &
fi

# S0b step 5: the helper is down (step 1) or new; start it now that S3 has
# linked it.
if summon_on; then helper_up; fi

# runsvdir rescans every 5 s; runsv then starts run.
_i=0
while [ $_i -lt 15 ] && ! all_supervised; do sleep 1; _i=$((_i + 1)); done
if summon_on; then
	$SV_BIN status "$SV/agw" "$SV/camhelper" "$SV/summonhelper" || true
else
	$SV_BIN status "$SV/agw" "$SV/camhelper" || true
fi
supervised "$SV/camhelper" || die "camhelper is not running; see $HELPER_LOG/current"
supervised "$SV/agw" || die "agw is not running; see $AGW_LOG/current"
if summon_on; then summon_up_check; fi
# A Summon skipped after S0b stopped stock webrtc-comms hands com.tesla.Webrtc
# back to it for this boot (stock_hand_back); it never stops the run.
stock_hand_back

# S4 restarts only what changed. summonhelper is never restarted here: S0b
# stopped it, S2 wrote its scripts, and step 5 (or its new runsv) started it,
# so it already runs the binary and scripts just installed.
if [ "$AFTER_BOOT" = 0 ]; then
	ask "S4. restart agw and camhelper so they run the binaries just installed (sv restart)"
	$SV_BIN restart "$SV/camhelper" "$SV/agw"
else
	if [ "$_helper_was_up" = 1 ] && [ "$_helper_changed" = 1 ]; then
		ask "S4. restart camhelper so it runs the changed run script (a new authd pin; sv restart)"
		$SV_BIN restart "$SV/camhelper"
	fi
	if [ "$_agw_was_up" = 1 ] && [ "$_agw_changed" = 1 ]; then
		ask "S4. restart agw so it runs the changed run/finish script (sv restart)"
		$SV_BIN restart "$SV/agw"
	fi
fi

# The checks after the last restart (SUMMON=1): probe-summon and probe-route
# as uid 4950, retried. A failure is reported at the end, non-zero, on a full
# install as after a boot ("Summon checks failed", check_fail). The helper
# stays linked and running on purpose (it is harmless, and the owner may look
# at it); the DONE text then shows no pin route for it (summon_done).
if summon_on; then summon_checks; fi

if summon_on; then
	_done_svcs="$SV/agw $SV/camhelper $SV/summonhelper"
	_done_logs="$AGW_LOG/current, $HELPER_LOG/current, $SUMMON_LOG/current"
else
	_done_svcs="$SV/agw $SV/camhelper"
	_done_logs="$AGW_LOG/current, $HELPER_LOG/current"
fi
cat <<DONE

Services: sv status $_done_svcs   (stop: sv down ...; logs: $_done_logs)
After every car boot: sh install-car.sh --after-boot   (it dials $PI_IP:$PI_PORT as installed; PI_IP=<ip> in front changes it)

Next:
1. Access is the car's own: a phone signs in with a key the car lists as
   owner or driver (added in the Tesla app or with the key card). There is no
   key to add here. To check one key (read-only, no motion; agw asks
   camhelper's /run/airgapp-camhelper/auth.sock, since uid $UID_GW has no bus
   access; role=2 or role=3 signs in):
  chpst -u $RUN_AS $DIR/agw probe-auth -pub <hex>
2. Send agw's pin to the laptop for the phone QR. It holds the VIN: never
   print it here (the car records SSH sessions). Start nc -l <port> > gwpin on
   the laptop, set LAPTOP (the laptop's tailnet address) and PORT here, then
   run (the camera runbook's step 6, as uid $UID_GW):
  P=\$(mktemp) && chpst -u $RUN_AS $DIR/agw pin -key $STATE/key.pem > "\$P" && wc -c < "\$P" && socat -u FILE:"\$P" "TCP:\$LAPTOP:\$PORT"; rm -f "\$P"
DONE
summon_done
# After a boot the authd pin check's note, then the Summon report: either one
# exits non-zero (end_report).
end_report
# ==== END CAMERA ROOT HELPER ====
