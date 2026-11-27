# supervise_test.sh: sourced by test/run.sh (uses its ok/bad/work/DL/DS). Runs bin/supervise.sh
# under CSDK_FAKE_ROOT, where every sv, kill, gdbus and runsvdir is a fake (test/fakes). The
# only signals sent are TERM to a supervise.sh this file started, and the fake kill's "HUP",
# which is a file the fake runsvdir watches.
# shellcheck source=/dev/null
. "$here/seed-common.sh"
set +e # every check below is explicit; expected non-zero waits and greps must not end the run

sup_start() { # NAME PKGDIR PKGID [env...]: a fresh fake root, then supervise.sh in the background
    local name=$1 pkg=$2 id=$3; shift 3
    S_ROOT=$work/$name/root S_RUN=$work/$name/run S_LOG=$work/$name/log
    mkdir -p "$S_RUN/svdir"
    seed_car "$S_ROOT" >/dev/null
    : >"$S_LOG"
    S_ENV="CSDK_FAKE_ROOT=$S_ROOT CSDK_RUN=$S_RUN CSDK_PACKAGE_DIR=$pkg CSDK_PKG=$id CSDK_API_SOCKET=/nonexistent CSDK_TOKEN=x FAKE_LOG=$S_LOG $*"
}
sup_go() { env $S_ENV bash "$1/bin/supervise.sh" >/dev/null 2>&1 & S_PID=$!; }
marker() { printf 'd91f0e1 %s\n' "$1" >"$S_RUN/applied"; }
link() { local s; for s in "$@"; do mkdir -p "$S_RUN/sv/$s"; ln -s "$S_RUN/sv/$s" "$S_RUN/svdir/$s"; done; }
waitlog() { # PATTERN SECONDS
    local i=0
    while ! grep -q -- "$1" "$S_LOG"; do
        [ "$i" -lt $(($2 * 5)) ] || return 1
        i=$((i + 1)); sleep 0.2
    done
}
line_of() { grep -n -- "$1" "$S_LOG" | head -n 1 | cut -d: -f1; }
sup_stop() { # TERM our supervise.sh, then make sure no fake runsvdir is left
    kill -TERM "$S_PID" 2>/dev/null || true
    wait "$S_PID" 2>/dev/null; S_RC=$?
    local p
    for p in "$S_ROOT"/proc/[0-9]*; do [ -d "$p" ] && : >"$S_ROOT/.fakes/state/hup-${p##*/}"; done
    sleep 0.5
}
BOOT=$(cat "$work/.boot" 2>/dev/null || echo 6a1c0b5e-0000-4000-8000-000000000001)

# A: waits for this boot's marker; on TERM: summonhelper, camhelper, agw down, then HUP runsvdir
sup_start a "$DL" io.github.ivan-prodanov.direct-link
link summonhelper camhelper agw
marker another-boot
sup_go "$DL"
sleep 2
if grep -q 'runsvdir -P' "$S_LOG"; then bad "supervise: started on another boot's marker"; else ok "supervise: waits on another boot's marker"; fi
marker "$BOOT"
if waitlog 'runsvdir -P' 4; then ok "supervise: starts runsvdir on this boot's marker"; else bad "supervise: no runsvdir after the marker"; fi
sup_stop
s=$(line_of 'down .*/summonhelper') c=$(line_of 'down .*/camhelper') a=$(line_of 'down .*/agw') h=$(line_of 'kill -HUP')
if [ "$S_RC" = 0 ] && [ -n "$s" ] && [ -n "$c" ] && [ -n "$a" ] && [ -n "$h" ] && [ "$s" -lt "$c" ] && [ "$c" -lt "$a" ] && [ "$a" -lt "$h" ]; then
    ok "supervise: TERM takes summonhelper, camhelper, agw down in that order, then HUPs runsvdir, exit 0"
else bad "supervise: stop order/exit (rc $S_RC; lines s=$s c=$c a=$a hup=$h)"; fi

# B: runsvdir dies on its own -> exit 1 (CoilShift restarts us)
sup_start b "$DL" io.github.ivan-prodanov.direct-link
link agw
marker "$BOOT"
sup_go "$DL"
waitlog 'runsvdir -P' 4 || true
: >"$S_ROOT/.fakes/state/hup-$(cat "$S_RUN/runsvdir.pid")"
i=0; while kill -0 "$S_PID" 2>/dev/null && [ "$i" -lt 20 ]; do sleep 0.2; i=$((i + 1)); done
if kill -0 "$S_PID" 2>/dev/null; then bad "supervise: still running after runsvdir died"; sup_stop
else wait "$S_PID"; rc=$?; [ "$rc" = 1 ] && ok "supervise: exits 1 when runsvdir dies" || bad "supervise: exit $rc when runsvdir died"; fi

# C: a runsvdir of ours left by a previous instance is stopped (checked pid) before ours starts
sup_start c "$DL" io.github.ivan-prodanov.direct-link
link agw
marker "$BOOT"
FAKE_LOG=$S_LOG "$S_ROOT/.fakes/runsvdir" -P "$S_RUN/svdir" >/dev/null 2>&1 &
old=$!
echo "$old" >"$S_RUN/runsvdir.pid"
sleep 0.5
sup_go "$DL"
i=0; while [ "$(grep -c 'runsvdir -P' "$S_LOG")" -lt 2 ] && [ "$i" -lt 25 ]; do sleep 0.2; i=$((i + 1)); done
h=$(line_of "kill -HUP $old") n=$(grep -n 'runsvdir -P' "$S_LOG" | sed -n 2p | cut -d: -f1)
if [ -n "$h" ] && [ -n "$n" ] && [ "$h" -lt "$n" ] && ! kill -0 "$old" 2>/dev/null; then
    ok "supervise: stops the leftover runsvdir before starting its own"
else bad "supervise: leftover runsvdir (hup line $h, new start line $n)"; fi
sup_stop

# D: Direct Summon takes com.tesla.Webrtc back at start, then at most once per TAKE_MIN
sup_start d "$DS" io.github.ivan-prodanov.direct-summon T_TAKE_EVERY=1 T_TAKE_MIN=3
link summonhelper
marker "$BOOT"
sup_go "$DS"
waitlog 'runsvdir -P' 4 || true
t=$(line_of 'sv down /etc/sv/webrtc-comms') r=$(line_of 'runsvdir -P')
if [ -n "$t" ] && [ -n "$r" ] && [ "$t" -lt "$r" ]; then ok "take back: at start, before runsvdir"; else bad "take back at start (take $t, runsvdir $r)"; fi
echo true >"$S_ROOT/.fakes/state/owner"
sleep 1.2
n1=$(grep -c 'sv down /etc/sv/webrtc-comms' "$S_LOG")
sleep 3.5
n2=$(grep -c 'sv down /etc/sv/webrtc-comms' "$S_LOG")
if [ "$n1" = 1 ] && [ "$n2" = 2 ]; then ok "take back: again once the minimum gap passed, not before"
else bad "take back rate limit (after 1.2 s: $n1, after 4.7 s: $n2)"; fi
# ...and never while our summonhelper runs
mkdir -p "$S_ROOT/proc/777"
printf '/run/direct-summon-bin/summonhelper\0run\0' >"$S_ROOT/proc/777/cmdline"
echo true >"$S_ROOT/.fakes/state/owner"
sleep 4.5
n3=$(grep -c 'sv down /etc/sv/webrtc-comms' "$S_LOG")
rm -rf "$S_ROOT/proc/777"
[ "$n3" = 2 ] && ok "take back: never while our summonhelper runs" || bad "take back while summonhelper runs ($n3)"
sup_stop

# F: Direct Summon with summonhelper not set up (apply skipped it): never stops stock
sup_start f "$DS" io.github.ivan-prodanov.direct-summon T_TAKE_EVERY=1 T_TAKE_MIN=1
marker "$BOOT"
sup_go "$DS"
waitlog 'runsvdir -P' 4 || true
sleep 2.5
grep -q 'sv down /etc/sv/webrtc-comms' "$S_LOG" && bad "take back acted with summonhelper not set up" || ok "take back: never when summonhelper is not set up"
sup_stop

# G: TERM during a take-back (name stays taken: a ~10 s wait) still stops at once, Summon first
sup_start g "$DS" io.github.ivan-prodanov.direct-summon
link summonhelper
: >"$S_ROOT/.fakes/state/sticky"
marker "$BOOT"
sup_go "$DS"
waitlog 'sv down /etc/sv/webrtc-comms' 4 || true
sleep 0.5
t0=$SECONDS
sup_stop
if [ $((SECONDS - t0)) -le 3 ] && [ "$S_RC" = 0 ] && grep -q 'sv -w 15 down .*/summonhelper' "$S_LOG"; then
    ok "supervise: a stop during take-back is not held up by it"
else bad "supervise: stop during take-back took $((SECONDS - t0)) s (rc $S_RC)"; fi

# E: Direct Link never touches webrtc-comms
sup_start e "$DL" io.github.ivan-prodanov.direct-link T_TAKE_EVERY=1 T_TAKE_MIN=1
link agw
marker "$BOOT"
sup_go "$DL"
waitlog 'runsvdir -P' 4 || true
sleep 2
grep -q 'webrtc-comms' "$S_LOG" && bad "Direct Link touched webrtc-comms" || ok "Direct Link never touches webrtc-comms"
sup_stop
set -e
