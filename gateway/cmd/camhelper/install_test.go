package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"airgapp/gateway/internal/authority"
)

// The install script's test hooks (it never runs for real on the desk: every
// other path writes to the car's filesystem as root). The hooks are read-only,
// or (the Summon stop sequence and removal) run only on fakes and a test
// layout under t.TempDir.

func script(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"../../deploy/install-car.sh"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallScriptSyntax(t *testing.T) {
	if out, err := exec.Command("sh", "-n", "../../deploy/install-car.sh").CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
}

// The camera runbook's car scripts (docs/CAMERA-INSTALL.md steps 1 and 2b) parse, and the pre-check's 8.6 authd
// hash is camhelper's compiled-in pin (the build it installs WITHOUT AUTHD_SHA256).
func TestCarScriptsSyntax(t *testing.T) {
	for _, c := range [][]string{{"sh", "-n", "../../deploy/signin_precheck.sh"}, {"bash", "-n", "../../deploy/probe_media.sh"}} {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", c, err, out)
		}
	}
	b, err := os.ReadFile("../../deploy/signin_precheck.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^PIN_86=([0-9a-f]{64})$`).FindSubmatch(b)
	if m == nil || string(m[1]) != authority.DefaultAuthdSHA256 {
		t.Fatalf("signin_precheck.sh PIN_86 %q, want the compiled-in pin %s", m, authority.DefaultAuthdSHA256)
	}
}

// S3 links only into our own runsvdir, never another supervisor's (review D2):
// coilshift's /run/ssp/services, or a runsvdir faked from any process's
// cmdline. Nothing selects one any more: SVDIR is empty, and link_into is only
// ever called with OWN_SVDIR. A PROCFS or RUN_ROOT in the environment is
// noted, never read (review D5).
func TestInstallScriptOnlyItsOwnRunsvdir(t *testing.T) {
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "usable_svdir") || strings.Contains(s, "AGW_DETECT_SVDIR_ONLY") || !strings.Contains(s, "\nSVDIR=\n") {
		t.Fatal("a runsvdir other than our own can still be selected")
	}
	for _, m := range regexp.MustCompile(`link_into "([^"]*)"`).FindAllStringSubmatch(s, -1) {
		if m[1] != "$OWN_SVDIR" {
			t.Errorf("link_into %q: only our own runsvdir", m[1])
		}
	}
	if strings.Count(s, "SVDIR=") != strings.Count(s, "OWN_SVDIR=")+2 { // SVDIR= (empty) and the test hook's AGW_TEST_SVDIR
		t.Errorf("SVDIR is assigned somewhere else")
	}
	if !strings.Contains(s, "_env_layout=\"${PROCFS+ PROCFS}${RUN_ROOT+ RUN_ROOT}\"") || !strings.Contains(s, "if [ -n \"$_env_layout\" ]; then echo \"note:") {
		t.Error("a PROCFS or RUN_ROOT from the environment is not noted")
	}
}

func TestInstallScriptRefusesTheVehicleLAN(t *testing.T) {
	out, err := script(t, []string{"AGW_CHECK_IP_ONLY=1"}, "192.168.90.100")
	if err == nil || !strings.Contains(out, "R22") {
		t.Fatalf("%q %v", out, err)
	}
	out, err = script(t, []string{"AGW_CHECK_IP_ONLY=1"}, "10.0.0.5")
	if err != nil || strings.TrimSpace(out) != "ok" {
		t.Fatalf("%q %v", out, err)
	}
}

// fakeAgw writes an executable that prints agw's no-argument usage line. The
// AGW_SCRIPTS_ONLY hook takes the binary's path but never runs it.
func fakeAgw(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	usage := "usage: agw serve|keygen|pin|probe-auth|probe-route|probe-summon [flags]"
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+usage+"' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// agwScripts runs the AGW_SCRIPTS_ONLY hook and splits run from finish.
func agwScripts(t *testing.T, env []string, args ...string) (run, finish string, err error) {
	t.Helper()
	out, err := script(t, append([]string{"AGW_SCRIPTS_ONLY=1", "PI_IP=10.0.0.9"}, env...), args...)
	run, finish, _ = strings.Cut(out, "---- finish ----\n")
	return run, finish, err
}

// agw always runs plain serve: Summon is summonhelper's own service (design
// §6.6), whatever SUMMON says. An agw run script left by the old SUMMON=1
// (supervise, then serve) is regenerated plain after a boot.
func TestAgwRunIsPlainServe(t *testing.T) {
	dir := t.TempDir()
	bin := fakeAgw(t, dir, "agw")
	const serve = `exec chpst -u :4950:4950 "/var/lib/airgapp-gw/agw" serve -dial "10.0.0.9:17101" -key "/var/lib/airgapp-gw/state/key.pem" -vin-file /var/etc/vin -log "/var/lib/airgapp-gw/state/log"`
	plain := func(name, run, finish string, err error) {
		t.Helper()
		if err != nil || !strings.HasSuffix(strings.TrimSpace(run), "\n"+serve) || strings.Contains(run, "supervise") ||
			strings.Contains(run, "airgapp-summon") || strings.Contains(finish, "chpst") ||
			!strings.HasSuffix(strings.TrimSpace(finish), "\nexit 0") {
			t.Fatalf("%s: run=%q finish=%q %v", name, run, finish, err)
		}
	}
	for _, s := range []string{"SUMMON=", "SUMMON=0", "SUMMON=1"} {
		run, finish, err := agwScripts(t, []string{s}, bin)
		plain(s, run, finish, err)
	}
	// Migration: the old SUMMON=1 run script (supervise, then serve) comes
	// back plain, dialling the address it dialled.
	installed := filepath.Join(dir, "run")
	old := strings.Replace(serve, " serve -dial", " supervise -- serve -dial", 1)
	if err := os.WriteFile(installed, []byte("#!/bin/sh\nexec 2>&1\n"+old+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"SUMMON=", "SUMMON=0", "SUMMON=1"} {
		run, finish, err := agwScripts(t, []string{s, "PI_IP=", "AGW_TEST_RUN=" + installed}, "--after-boot", bin)
		plain("after-boot "+s, run, finish, err)
	}
	// Migration: a run script from before the allowlist was removed (serve
	// -allow) comes back without it: agw no longer has the flag.
	withAllow := strings.Replace(serve, ` -vin-file`, ` -allow "/var/lib/airgapp-gw/allow" -vin-file`, 1)
	if err := os.WriteFile(installed, []byte("#!/bin/sh\nexec 2>&1\n"+withAllow+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run, finish, err := agwScripts(t, []string{"PI_IP=", "AGW_TEST_RUN=" + installed}, "--after-boot", bin)
	plain("after-boot from -allow", run, finish, err)
	if strings.Contains(run, "-allow") {
		t.Fatalf("the regenerated run script still passes -allow: %q", run)
	}
	// The old SUMMON path is gone (the names are split so the final review's
	// retired-names grep does not find them here).
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"summon" + "-finish", "agw_has" + "_summon", "summon_binary_ok", "resolve_summon",
		"/run/airgapp-gw", "supervise --",
		// No key list: nothing creates the allow file or tells the owner to fill it.
		"allow add", "-allow ", `: > "$DIR/allow"`, "probe-auth -allow"} {
		if strings.Contains(string(b), s) {
			t.Errorf("install-car.sh still has %q", s)
		}
	}
}

// The car dials out to the Pi: its Wi-Fi module NATs the MCU and drops every
// inbound packet (car-proven 2026-11-21). agw runs serve -dial PI_IP:17101,
// the egress lock lets uid 4950 open TCP only to that address, and there is
// no INPUT rule at all.
func TestInstallScriptDialsOut(t *testing.T) {
	dir := t.TempDir()
	bin := fakeAgw(t, dir, "agw")
	run, _, err := agwScripts(t, nil, bin)
	if err != nil || !strings.Contains(run, ` serve -dial "10.0.0.9:17101" -key `) || strings.Contains(run, "-listen") {
		t.Fatalf("run: %q %v", run, err)
	}
	// CAR_IP is not used any more: one left in front changes nothing.
	if run2, _, err := agwScripts(t, []string{"CAR_IP=10.0.0.5"}, bin); err != nil || run2 != run {
		t.Fatalf("CAR_IP must be ignored: %q %v", run2, err)
	}
	// A full install needs PI_IP, and it is checked (R22).
	if out, err := script(t, []string{"AGW_SCRIPTS_ONLY=1", "PI_IP="}, bin); err == nil || !strings.Contains(out, "PI_IP") {
		t.Fatalf("no PI_IP: %q %v", out, err)
	}
	if out, err := script(t, []string{"AGW_SCRIPTS_ONLY=1", "PI_IP=192.168.90.30"}, bin); err == nil || !strings.Contains(out, "R22") {
		t.Fatalf("PI_IP on the vehicle LAN: %q %v", out, err)
	}
	// --after-boot without PI_IP dials the address the installed run script dials.
	installed := filepath.Join(dir, "run")
	os.WriteFile(installed, []byte(run), 0o755)
	out, err := script(t, []string{"AGW_SCRIPTS_ONLY=1", "PI_IP=", "SUMMON=", "AGW_TEST_RUN=" + installed}, "--after-boot", bin)
	if err != nil || !strings.Contains(out, ` serve -dial "10.0.0.9:17101" -key `) {
		t.Fatalf("after-boot keeps the installed PI_IP: %q %v", out, err)
	}
	// ...and one given in front wins.
	out, err = script(t, []string{"AGW_SCRIPTS_ONLY=1", "PI_IP=10.0.0.8", "SUMMON=", "AGW_TEST_RUN=" + installed}, "--after-boot", bin)
	if err != nil || !strings.Contains(out, ` serve -dial "10.0.0.8:17101" -key `) {
		t.Fatalf("after-boot with PI_IP: %q %v", out, err)
	}
	// The egress accept rule: new TCP connections to the Pi's dial port only.
	out, err = script(t, []string{"AGW_EGRESS_RULE_ONLY=1", "PI_IP=10.0.0.9"})
	if err != nil || strings.TrimSpace(out) != "-o wlan0 -d 10.0.0.9 -p tcp --dport 17101 -j ACCEPT" {
		t.Fatalf("egress rule: %q %v", out, err)
	}
	// No INPUT rule anywhere: nothing may connect in.
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "#") {
			continue
		}
		if strings.Contains(l, "-I INPUT") || strings.Contains(l, "-A INPUT") || strings.Contains(l, "-C INPUT") || strings.Contains(l, "--dport $PORT") || strings.Contains(l, "$CAR_IP:") {
			t.Fatalf("the installer must not open or name an inbound port: %q", l)
		}
	}
}

// camhelper's run script carries the authd pin only when one is set, and
// --after-boot keeps the installed one unless AUTHD_SHA256 is given.
func TestInstallScriptAuthdPin(t *testing.T) {
	dir := t.TempDir()
	pin := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	helperRun := func(env []string, args ...string) (string, error) {
		return script(t, append([]string{"AGW_HELPER_RUN_ONLY=1"}, env...), args...)
	}
	const base = "exec /var/lib/airgapp-camhelper/camhelper run -state /run/airgapp-camhelper-state -writer pw-cat"
	if out, err := helperRun([]string{"AUTHD_SHA256="}); err != nil || !strings.Contains(out, base+"\n") || strings.Contains(out, "authd-sha256") {
		t.Fatalf("unset: %q %v", out, err)
	}
	if out, err := helperRun([]string{"AUTHD_SHA256=" + pin}); err != nil || !strings.Contains(out, base+" -authd-sha256 "+pin+"\n") {
		t.Fatalf("set: %q %v", out, err)
	}
	for _, bad := range []string{strings.ToUpper(pin), pin[:62], pin + "00", "x" + pin[1:]} {
		if out, err := helperRun([]string{"AUTHD_SHA256=" + bad}); err == nil {
			t.Errorf("AUTHD_SHA256=%s must be refused: %q", bad, out)
		}
	}
	// --after-boot reads the installed pin back, and a given one replaces it.
	installed := filepath.Join(dir, "run")
	os.WriteFile(installed, []byte("#!/bin/sh\nexec 2>&1\n"+base+" -authd-sha256 "+pin+"\n"), 0o755)
	if out, err := helperRun([]string{"AUTHD_SHA256=", "AGW_TEST_HELPER_RUN=" + installed}, "--after-boot"); err != nil || !strings.Contains(out, "-authd-sha256 "+pin+"\n") {
		t.Fatalf("after-boot keeps the pin: %q %v", out, err)
	}
	if out, err := helperRun([]string{"AUTHD_SHA256=" + other, "AGW_TEST_HELPER_RUN=" + installed}, "--after-boot"); err != nil || !strings.Contains(out, "-authd-sha256 "+other+"\n") {
		t.Fatalf("after-boot with a new pin: %q %v", out, err)
	}
	// A full install inherits it too: a reinstall that forgets AUTHD_SHA256
	// keeps the verified pin (the camera session's ruling).
	if out, err := helperRun([]string{"AUTHD_SHA256=", "AGW_TEST_HELPER_RUN=" + installed}); err != nil || !strings.Contains(out, "-authd-sha256 "+pin+"\n") {
		t.Fatalf("full install: %q %v", out, err)
	}
	// An installed run script with no pin: none after boot either.
	os.WriteFile(installed, []byte("#!/bin/sh\n"+base+"\n"), 0o755)
	if out, err := helperRun([]string{"AUTHD_SHA256=", "AGW_TEST_HELPER_RUN=" + installed}, "--after-boot"); err != nil || strings.Contains(out, "authd-sha256") {
		t.Fatalf("after-boot without a pin: %q %v", out, err)
	}
	// A mangled installed pin stops --after-boot loudly.
	os.WriteFile(installed, []byte("#!/bin/sh\n"+base+" -authd-sha256 "+pin[:20]+"\n"), 0o755)
	if out, err := helperRun([]string{"AUTHD_SHA256=", "AGW_TEST_HELPER_RUN=" + installed}, "--after-boot"); err == nil {
		t.Fatalf("a mangled installed pin must stop: %q", out)
	}
}

// The account overlay is gone: uid 4950 gets no passwd entry. The script may
// only remove an old overlay (umount), never lay one.
func TestInstallScriptGivesNoAccount(t *testing.T) {
	b, _ := os.ReadFile("../../deploy/install-car.sh")
	for _, s := range []string{"--account", "gen_passwd", "airgapp-gw-account"} {
		if strings.Contains(string(b), s) {
			t.Errorf("install-car.sh still has %q", s)
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "mount --bind") && !strings.Contains(line, `mount --bind "$1" "$1"`) {
			t.Errorf("a bind mount other than the exec mounts: %q", line)
		}
		if cmd := strings.TrimSpace(line); strings.HasPrefix(cmd, "mount ") && (strings.Contains(cmd, "PASSWD_FILE") || strings.Contains(cmd, "/etc/passwd")) {
			t.Errorf("the passwd file may only be unmounted: %q", line)
		}
	}
}

// fakeIptables is a tiny iptables for one table: a file per chain under dir,
// one rule spec per line. It knows -N -S -C -I -A -D the way the installer
// uses them, and prints -S like iptables does.
const fakeIptables = `#!/bin/sh
d=$FAKE_IPT_DIR
op=$1; c=$2; shift 2
f=$d/$c
case "$op" in
-N) [ -e "$f" ] && exit 1; : >"$f" ;;
-S) [ -e "$f" ] || exit 1
    if [ "$c" = OUTPUT ]; then echo "-P OUTPUT ACCEPT"; else echo "-N $c"; fi
    while IFS= read -r r; do echo "-A $c $r"; done <"$f" ;;
-C) [ -e "$f" ] && grep -q -x -F -- "$*" "$f" ;;
-A) [ -e "$f" ] || exit 1; echo "$*" >>"$f" ;;
-I) [ -e "$f" ] || exit 1; n=$1; shift
    awk -v n="$n" -v r="$*" 'NR==n{print r} {print} END{if (NR<n) print r}' "$f" >"$f.t"; mv "$f.t" "$f" ;;
-D) [ -e "$f" ] || exit 1
    case "$1" in
    [0-9]*) [ "$(wc -l <"$f")" -ge "$1" ] || exit 1; awk -v n="$1" 'NR!=n' "$f" >"$f.t" ;;
    *) grep -q -x -F -- "$*" "$f" || exit 1; awk -v r="$*" '!done && $0==r {done=1; next} {print}' "$f" >"$f.t" ;;
    esac; mv "$f.t" "$f" ;;
*) exit 2 ;;
esac
`

func TestInstallScriptEgressLock(t *testing.T) {
	dir := t.TempDir()
	ipt := filepath.Join(dir, "iptables")
	os.WriteFile(ipt, []byte(fakeIptables), 0o755)
	tables := filepath.Join(dir, "t")
	os.MkdirAll(tables, 0o755)
	const accept = "-o wlan0 -d 10.0.0.9 -p tcp --dport 17101 -j ACCEPT"
	const jump = "-m owner --uid-owner 4950 -j AIRGAPP_GW_OUT"
	const webrtc = "-m owner --uid-owner 1337 -j WEBRTC"
	read := func(c string) string { b, _ := os.ReadFile(filepath.Join(tables, c)); return string(b) }
	setOutput := func(rules ...string) {
		os.WriteFile(filepath.Join(tables, "OUTPUT"), []byte(strings.Join(rules, "\n")+"\n"), 0o644)
	}
	lock := func(rule string) {
		t.Helper()
		env := []string{"AGW_EGRESS_ONLY=1", "AGW_TEST_IPT=" + ipt, "FAKE_IPT_DIR=" + tables}
		var args []string
		if rule != "" {
			args = []string{rule}
		}
		if out, err := script(t, env, args...); err != nil || !strings.Contains(out, "egress lock in place") {
			t.Fatalf("lock: %q %v", out, err)
		}
	}
	want := func(name, chain, output string) {
		t.Helper()
		if got := read("AIRGAPP_GW_OUT"); got != chain {
			t.Fatalf("%s: chain %q, want %q", name, got, chain)
		}
		if got := read("OUTPUT"); got != output {
			t.Fatalf("%s: OUTPUT %q, want %q", name, got, output)
		}
	}

	// Fresh: the chain is [accept, DROP] and the jump goes first in OUTPUT.
	setOutput(webrtc, "-m owner --uid-owner 111 -j DROP")
	lock(accept)
	want("fresh", accept+"\n-j DROP\n", jump+"\n"+webrtc+"\n-m owner --uid-owner 111 -j DROP\n")
	// Again: nothing changes.
	lock(accept)
	want("again", accept+"\n-j DROP\n", jump+"\n"+webrtc+"\n-m owner --uid-owner 111 -j DROP\n")
	// A chain that is not exactly right (an ACCEPT after the DROP, another
	// Pi address) is rebuilt; a jump pushed down by another rule, and a
	// duplicate, end as one jump, first.
	os.WriteFile(filepath.Join(tables, "AIRGAPP_GW_OUT"), []byte("-o wlan0 -d 10.0.0.7 -p tcp --dport 17101 -j ACCEPT\n-j DROP\n-j ACCEPT\n"), 0o644)
	setOutput("-o lo -j ACCEPT", jump, webrtc, jump)
	lock(accept)
	want("repair", accept+"\n-j DROP\n", jump+"\n-o lo -j ACCEPT\n"+webrtc+"\n")
	// The right accept rule plus anything else is not exactly right either:
	// an ACCEPT after the DROP, one before it, or the DROP first.
	for name, chain := range map[string]string{
		"accept after drop":  accept + "\n-j DROP\n-j ACCEPT\n",
		"accept before drop": accept + "\n-j ACCEPT\n-j DROP\n",
		"drop first":         "-j DROP\n" + accept + "\n",
	} {
		os.WriteFile(filepath.Join(tables, "AIRGAPP_GW_OUT"), []byte(chain), 0o644)
		setOutput(jump, webrtc)
		lock(accept)
		want(name, accept+"\n-j DROP\n", jump+"\n"+webrtc+"\n")
	}
	// IPv6: no accept rule, the chain is DROP only.
	os.Remove(filepath.Join(tables, "AIRGAPP_GW_OUT"))
	setOutput(webrtc)
	lock("")
	want("ip6", "-j DROP\n", jump+"\n"+webrtc+"\n")
	os.WriteFile(filepath.Join(tables, "AIRGAPP_GW_OUT"), []byte("-j ACCEPT\n-j DROP\n"), 0o644)
	lock("")
	want("ip6 repair", "-j DROP\n", jump+"\n"+webrtc+"\n")

	// agw's run script checks the lock as root, before anything else runs.
	run, _, err := agwScripts(t, []string{"SUMMON=0", "AGW_TEST_IPT=/x/iptables", "AGW_TEST_IP6T=/x/ip6tables"}, fakeAgw(t, dir, "agw"))
	check := `if ! /x/iptables -C OUTPUT ` + jump + ` 2>/dev/null || ! /x/iptables -C AIRGAPP_GW_OUT -j DROP 2>/dev/null || ! /x/ip6tables -C OUTPUT ` + jump + ` 2>/dev/null; then`
	if i, j := strings.Index(run, check), strings.Index(run, "exec chpst"); err != nil || i < 0 || j < i || !strings.Contains(run, "\tsleep 10\n\texit 1\nfi") {
		t.Fatalf("the run script must refuse to start without the egress lock: %q %v", run, err)
	}
}

// ---- Summon: summonhelper's service, the stop sequence, the removal, the checks ----

// The fakes are POSIX sh and share one log, one external command per line, so
// a test sees the order of everything the installer ran. gdbus pops one answer
// per call ("error" or none left: exit 1; "hold:<answer>" creates FAKE_DIR/held
// and answers only once FAKE_DIR/release exists); timeout logs its options and
// seconds and runs the rest; sv and kill log their argv and exit 1 when told
// (sv status says "run:" only when FAKE_SV_STATUS=0; "down: …, want up" with
// exit 0, a runsv that runs and restarts its service, when FAKE_SV_STATUS=down;
// "down: …" with exit 0 and no "want up" (only its log service's part has one),
// a runsv told to keep its service down, when FAKE_SV_STATUS=wantdown; "run: …,
// want down, got TERM" or "finish: …, want down" with exit 0, a runsv told to
// stop its service, which is still stopping, when FAKE_SV_STATUS=rundown or
// finishdown; "run: …" with exit 0 and only its log service's part told to
// stop, when FAKE_SV_STATUS=logdown; sv logs which
// FAKE_SV_WATCH paths exist at that moment, and a successful down of
// summonhelper removes FAKE_SV_DOWN_RM: the helper's process exits); sleep
// returns at once (removing FAKE_SLEEP_RM at its FAKE_SLEEP_RM_AFTER-th call: a
// process that exits late); chpst plays agw's two probes (answers popped per
// probe) and summonhelper's pin. The fake /proc (PROCFS, PROC) holds what
// the test puts there: comm files for the kill, cmdlines for running_pids.
var summonFakes = map[string]string{
	"gdbus": `#!/bin/sh
echo "gdbus $*" >>"$FAKE_LOG"
a=$(head -n 1 "$FAKE_DIR/answers" 2>/dev/null)
tail -n +2 "$FAKE_DIR/answers" >"$FAKE_DIR/answers.t" 2>/dev/null && mv "$FAKE_DIR/answers.t" "$FAKE_DIR/answers"
case "$a" in
hold:*) a=${a#hold:}; : >"$FAKE_DIR/held"; while [ ! -e "$FAKE_DIR/release" ]; do /bin/sleep 0.01; done ;;
esac
case "$a" in
""|error) echo "Error: GDBus.Error: a fake failure" >&2; exit 1 ;;
esac
printf '%s\n' "$a"
`,
	"timeout": `#!/bin/sh
o=
while :; do
	case "$1" in -k|-s) o="$o $1 $2"; shift 2 ;; *) break ;; esac
done
echo "timeout$o $1" >>"$FAKE_LOG"
shift
exec "$@"
`,
	"sv": `#!/bin/sh
w=
for p in $FAKE_SV_WATCH; do
	if [ -e "$p" ] || [ -L "$p" ]; then w="$w +$p"; else w="$w -$p"; fi
done
echo "sv $*${w:+ |$w}" >>"$FAKE_LOG"
if [ "$1" = status ]; then
	if [ "$FAKE_SV_STATUS" = 0 ]; then echo "run: $2: (pid 99) 3s"; exit 0; fi
	if [ "$FAKE_SV_STATUS" = down ]; then echo "down: $2: 1s, normally up, want up"; exit 0; fi
	if [ "$FAKE_SV_STATUS" = wantdown ]; then echo "down: $2: 5s, normally up; down: log: 1s, normally up, want up"; exit 0; fi
	if [ "$FAKE_SV_STATUS" = rundown ]; then echo "run: $2: (pid 99) 16s, want down, got TERM; run: log: (pid 98) 20s"; exit 0; fi
	if [ "$FAKE_SV_STATUS" = finishdown ]; then echo "finish: $2: (pid 97) 1s, want down; run: log: (pid 98) 20s"; exit 0; fi
	if [ "$FAKE_SV_STATUS" = logdown ]; then echo "run: $2: (pid 99) 3s; run: log: (pid 98) 9s, want down"; exit 0; fi
	echo "fail: $2: runsv not running" >&2
	exit 1
fi
[ "$FAKE_SV_EXIT" = 0 ] || { echo "fail: a fake sv failure" >&2; exit 1; }
for a in "$@"; do last=$a; done
case " $* " in *" down "*) case "$last" in */summonhelper) [ -z "$FAKE_SV_DOWN_RM" ] || rm -rf "$FAKE_SV_DOWN_RM" ;; esac ;; esac
`,
	"kill": `#!/bin/sh
echo "kill $*" >>"$FAKE_LOG"
exit "$FAKE_KILL_EXIT"
`,
	"sha256sum": `#!/bin/sh
[ -r "$1" ] || { echo "sha256sum: $1: No such file or directory" >&2; exit 1; }
[ -n "$FAKE_AUTHD_HASH" ] || { echo "sha256sum: $1: Input/output error" >&2; exit 1; }
printf '%s  %s\n' "$FAKE_AUTHD_HASH" "$1"
`,
	"sleep": `#!/bin/sh
echo "sleep $*" >>"$FAKE_LOG"
if [ -n "$FAKE_SLEEP_RM" ]; then
	n=$(($(cat "$FAKE_DIR/sleeps" 2>/dev/null || echo 0) + 1))
	echo "$n" >"$FAKE_DIR/sleeps"
	[ "$n" -lt "$FAKE_SLEEP_RM_AFTER" ] || rm -rf "$FAKE_SLEEP_RM"
fi
`,
	"chpst": `#!/bin/sh
echo "chpst $*" >>"$FAKE_LOG"
if [ -n "${SUMMONHELPER_VIN:-}" ]; then echo "env SUMMONHELPER_VIN=$SUMMONHELPER_VIN" >>"$FAKE_LOG"; fi
case "$4" in
probe-summon|probe-route)
	f=$FAKE_DIR/$4
	a=$(head -n 1 "$f" 2>/dev/null)
	tail -n +2 "$f" >"$f.t" 2>/dev/null && mv "$f.t" "$f"
	n=$(($(cat "$f.n" 2>/dev/null || echo 0) + 1))
	echo "$n" >"$f.n"
	if [ "$4" = probe-route ]; then
		[ "$a" = ok ] && { echo "route gate: ok"; exit 0; }
		echo "route gate: refused"
		echo "agw probe-route: a fake reason $n" >&2
		exit 1
	fi
	[ "$a" = ok ] && { echo "summon relay: ok"; exit 0; }
	echo "summon relay: refused: a fake reason $n"
	exit 1 ;;
pin) echo "airgapp-gw:1:fakekey:$6" ;;
keygen)
	if [ -n "$FAKE_KEYGEN_FAIL" ]; then echo "chpst: fatal: unable to run: exec format error" >&2; exit 111; fi
	: >"$6" ;;
esac
`,
	"chown": `#!/bin/sh
echo "chown $*" >>"$FAKE_LOG"
`,
}

// must fails the test on a setup error, so no check can pass on a missing file.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// nameHasOwner is the gdbus argv of the name check, after `timeout -k 1 5`.
const nameHasOwner = "gdbus call --system --dest org.freedesktop.DBus --object-path /org/freedesktop/DBus " +
	"--method org.freedesktop.DBus.NameHasOwner com.tesla.Webrtc"

type fakes struct {
	t   *testing.T
	dir string // bin/ (the fakes), log, answers, the test layout gw/, proc/, run/
}

func newFakes(t *testing.T) *fakes {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakes{t: t, dir: dir}
	if err := os.MkdirAll(f.path("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range summonFakes {
		if err := os.WriteFile(f.path("bin", name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fakes) path(p ...string) string { return filepath.Join(append([]string{f.dir}, p...)...) }

// env points every Summon override at the fakes and the test layout, so no
// hook can reach the real sv, kill, gdbus, /proc, /run or /var/lib. Later
// entries win (os/exec keeps the last of duplicate keys).
func (f *fakes) env(extra ...string) []string {
	return append([]string{
		"PATH=" + f.path("bin") + ":" + os.Getenv("PATH"),
		"FAKE_LOG=" + f.path("log"), "FAKE_DIR=" + f.dir,
		"FAKE_SV_STATUS=1", "FAKE_SV_EXIT=0", "FAKE_KILL_EXIT=0", "FAKE_SV_WATCH=", "FAKE_SV_DOWN_RM=",
		"FAKE_SLEEP_RM=", "FAKE_SLEEP_RM_AFTER=1", "AGW_TEST_PROCFS=" + f.path("proc"), "FAKE_KEYGEN_FAIL=",
		"AGW_TEST_SUMMON_HOME=" + f.path("summon-home"),
		"AGW_TEST_GDBUS=" + f.path("bin", "gdbus"), "AGW_TEST_TIMEOUT=" + f.path("bin", "timeout"),
		"AGW_TEST_SV=" + f.path("bin", "sv"), "AGW_TEST_KILL=" + f.path("bin", "kill"),
		"AGW_TEST_WEBRTC_PIDFILE=" + f.path("webrtc-comms.pid"), "AGW_TEST_PROC=" + f.path("proc"),
		"AGW_TEST_DIR=" + f.path("gw"), "AGW_TEST_SVDIR=", "AGW_TEST_SUMMON_RUN=",
		"AGW_TEST_SUMMON_RUN_DIR=" + f.path("run", "airgapp-summon"), "AGW_TEST_VIN_FILE=" + f.path("vin"),
		"AGW_TEST_SUMMON_UID=", "AGW_TEST_GW_GID=", "SUMMON=", "AUTHD_SHA256=",
		"AGW_TEST_AUTHD=" + f.path("authd"), "AGW_TEST_SHA256SUM=" + f.path("bin", "sha256sum"), "FAKE_AUTHD_HASH=",
		"AGW_TEST_HELPER_RUN=" + f.path("camhelper-run"),
	}, extra...)
}

// answers sets gdbus's answers, one per call.
func (f *fakes) answers(a ...string) {
	f.t.Helper()
	if err := os.WriteFile(f.path("answers"), []byte(strings.Join(a, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// calls returns the log, one external command per line, and empties it.
func (f *fakes) calls() []string {
	b, _ := os.ReadFile(f.path("log"))
	os.Remove(f.path("log"))
	if s := strings.TrimRight(string(b), "\n"); s != "" {
		return strings.Split(s, "\n")
	}
	return nil
}

// helperSvc is summonhelper's service directory in the test layout.
func (f *fakes) helperSvc() string { return f.path("gw", "sv", "summonhelper") }

// proc puts a process with this argv in the fake /proc and returns its
// directory.
func (f *fakes) proc(pid string, argv ...string) string {
	f.t.Helper()
	d := f.path("proc", pid)
	must(f.t, os.MkdirAll(d, 0o755))
	must(f.t, os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644))
	return d
}

// helperProc puts a summonhelper process (cmdline "<DIR>/summonhelper <verb>")
// in the fake /proc and returns its directory.
func (f *fakes) helperProc(pid, verb string) string {
	f.t.Helper()
	return f.proc(pid, f.path("gw", "summonhelper"), verb)
}

// installHelper lays out an installed summonhelper service, linked into our
// own runsvdir and into each of svdirs, and returns the links.
func (f *fakes) installHelper(svdirs ...string) []string {
	f.t.Helper()
	svc := f.helperSvc()
	if err := os.MkdirAll(filepath.Join(svc, "log"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	for _, n := range []string{"run", "finish", "log/run"} {
		if err := os.WriteFile(filepath.Join(svc, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	var links []string
	for _, d := range append([]string{f.path("gw", "svdir")}, svdirs...) {
		l := filepath.Join(d, "airgapp-summonhelper")
		if err := os.MkdirAll(d, 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Symlink(svc, l); err != nil {
			f.t.Fatal(err)
		}
		links = append(links, l)
	}
	return links
}

// hook runs install-car.sh with one test hook, on the fakes, with stdin.
func (f *fakes) hook(hook, stdin string, env []string, args ...string) (string, error) {
	f.t.Helper()
	cmd := exec.Command("sh", append([]string{"../../deploy/install-car.sh"}, args...)...)
	cmd.Env = append(os.Environ(), f.env(append([]string{hook + "=1"}, env...)...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// index is the first call that starts with prefix, or -1; count counts them.
func index(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func count(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func repeat(s string, n int) []string {
	a := make([]string, n)
	for i := range a {
		a[i] = s
	}
	return a
}

// inOrder fails unless each part appears in s after the part before it.
func inOrder(t *testing.T, s string, parts ...string) {
	t.Helper()
	at := 0
	for _, p := range parts {
		i := strings.Index(s[at:], p)
		if i < 0 {
			t.Fatalf("%q is missing, or not after the part before it, in:\n%s", p, s)
		}
		// The next part may start with the newline this one ends with.
		at += i + len(strings.TrimSuffix(p, "\n"))
	}
}

// commands are a script's lines without the shebang, comments and blanks.
func commands(script string) []string {
	var c []string
	for _, l := range strings.Split(script, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			c = append(c, l)
		}
	}
	return c
}

// summonScripts runs the AGW_SUMMON_SCRIPTS_ONLY hook and splits run from finish.
func summonScripts(t *testing.T, env []string, args ...string) (run, finish string, err error) {
	t.Helper()
	out, err := script(t, append([]string{"AGW_SUMMON_SCRIPTS_ONLY=1", "AUTHD_SHA256=", "AGW_TEST_SUMMON_UID=",
		"AGW_TEST_GW_GID=", "AGW_TEST_SUMMON_RUN_DIR=", "AGW_TEST_VIN_FILE="}, env...), args...)
	run, finish, _ = strings.Cut(out, "---- finish ----\n")
	return run, finish, err
}

// summonhelper's run script, as root, in order: stderr to svlogd; the relay
// directory (never through a symlink); no stale socket; the VIN; the name is
// free; then the supervisor as uid 1337, with the VIN in its environment.
func TestSummonRunScriptOrder(t *testing.T) {
	env := []string{"AGW_TEST_GDBUS=/x/gdbus", "AGW_TEST_TIMEOUT=/x/timeout"}
	const (
		check = `_o=$(/x/timeout -k 1 5 /x/gdbus call --system --dest org.freedesktop.DBus --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.NameHasOwner com.tesla.Webrtc 2>/dev/null) && [ "$_o" = '(false,)' ]`
		start = `SUMMONHELPER_VIN="$VIN" exec chpst -u :1337:1337 "/var/lib/airgapp-gw/summonhelper" run`
	)
	run, _, err := summonScripts(t, env)
	if err != nil {
		t.Fatalf("%q %v", run, err)
	}
	if c := commands(run); len(c) == 0 || c[0] != "exec 2>&1" {
		t.Fatalf("exec 2>&1 must come first: %q", run)
	}
	inOrder(t, run,
		"\nexec 2>&1\n",
		`if [ -L "/run/airgapp-summon" ] || { [ -e "/run/airgapp-summon" ] && [ ! -d "/run/airgapp-summon" ]; }; then`,
		`mkdir -p "/run/airgapp-summon" && chown 1337:4950 "/run/airgapp-summon" && chmod 2750 "/run/airgapp-summon"`,
		"\n"+`rm -f "/run/airgapp-summon/relay.sock"`+"\n",
		"\n"+`rm -f "/run/airgapp-summon/live" "/run/airgapp-summon"/.live.*`+"\n",
		"\n"+`VIN=$(tr -d '[:space:]' <"/var/etc/vin")`+"\n",
		check,
		"\n"+start,
	)
	// AUTHD_SHA256 unset: no -authd-sha256 at all (the compiled-in pin; an
	// empty value would stop the helper), and the exec is the last line.
	if !strings.HasSuffix(strings.TrimSpace(run), "\n"+start) || strings.Contains(run, "authd-sha256") {
		t.Fatalf("unset pin: %q", run)
	}
	pin := strings.Repeat("ab", 32)
	if run, _, err := summonScripts(t, append(env, "AUTHD_SHA256="+pin)); err != nil ||
		!strings.HasSuffix(strings.TrimSpace(run), "\n"+start+" -authd-sha256 "+pin) {
		t.Fatalf("a valid pin appears verbatim: %q %v", run, err)
	}
	if out, _, err := summonScripts(t, append(env, "AUTHD_SHA256="+strings.ToUpper(pin))); err == nil {
		t.Fatalf("a malformed pin must stop the script: %q", out)
	}
	// --after-boot carries camhelper's installed pin, and so does a full
	// install with AUTHD_SHA256 unset (one resolve_authd_pin for both helpers).
	installed := filepath.Join(t.TempDir(), "run")
	if err := os.WriteFile(installed, []byte("#!/bin/sh\nexec 2>&1\nexec /var/lib/airgapp-camhelper/camhelper run -state /run/airgapp-camhelper-state -writer pw-cat -authd-sha256 "+pin+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if run, _, err := summonScripts(t, append(env, "AGW_TEST_HELPER_RUN="+installed), "--after-boot"); err != nil ||
		!strings.HasSuffix(strings.TrimSpace(run), "\n"+start+" -authd-sha256 "+pin) {
		t.Fatalf("after-boot keeps camhelper's pin: %q %v", run, err)
	}
	if run, _, err := summonScripts(t, append(env, "AGW_TEST_HELPER_RUN="+installed)); err != nil ||
		!strings.HasSuffix(strings.TrimSpace(run), "\n"+start+" -authd-sha256 "+pin) {
		t.Fatalf("a full install inherits the pin: %q %v", run, err)
	}
	// The overridable values are baked in, the way IPT is.
	run, _, err = summonScripts(t, append(env, "AGW_TEST_SUMMON_UID=501", "AGW_TEST_GW_GID=20",
		"AGW_TEST_SUMMON_RUN_DIR=/t/r", "AGW_TEST_VIN_FILE=/t/vin"))
	for _, s := range []string{`chown 501:20 "/t/r"`, `rm -f "/t/r/relay.sock"`, `rm -f "/t/r/live" "/t/r"/.live.*`, `<"/t/vin")`, `exec chpst -u :501:501 "/var/lib/airgapp-gw/summonhelper" run`} {
		if err != nil || !strings.Contains(run, s) {
			t.Fatalf("override %q: %q %v", s, run, err)
		}
	}
}

// Each refusal of the run script exits 1 after a 10 s pause, with its own
// reason, before any later step ran; none prints the VIN.
func TestSummonRunScriptRefusals(t *testing.T) {
	f := newFakes(t)
	ids := []string{"AGW_TEST_SUMMON_UID=" + strconv.Itoa(os.Getuid()), "AGW_TEST_GW_GID=" + strconv.Itoa(os.Getgid())}
	run, _, err := summonScripts(t, f.env(ids...))
	if err != nil {
		t.Fatalf("%q %v", run, err)
	}
	runPath := f.path("summon-run")
	if err := os.WriteFile(runPath, []byte(run), 0o755); err != nil {
		t.Fatal(err)
	}
	const vin = "5YJ3E1EA0JF000001" // made up
	runDir, sock := f.path("run", "airgapp-summon"), f.path("run", "airgapp-summon", "relay.sock")
	start := func(vinText string, gdbus ...string) {
		t.Helper()
		os.RemoveAll(f.path("run"))
		if err := os.MkdirAll(f.path("run"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.path("vin"), []byte(vinText), 0o644); err != nil {
			t.Fatal(err)
		}
		f.answers(gdbus...)
		f.calls()
	}
	exec1 := func() (string, []string, error) {
		cmd := exec.Command("sh", runPath)
		cmd.Env = append(os.Environ(), f.env(ids...)...)
		out, err := cmd.CombinedOutput()
		return string(out), f.calls(), err
	}
	refused := func(name, reason string, out string, calls []string, err error) {
		t.Helper()
		if exitCode(err) != 1 || !strings.Contains(out, reason) || strings.Contains(out, vin) ||
			index(calls, "sleep 10") < 0 || index(calls, "chpst") >= 0 {
			t.Fatalf("%s: want exit 1 with %q after sleep 10: %q %v %v", name, reason, out, calls, err)
		}
	}

	// A symlink at the run dir, and a file there: refused before anything else.
	start(vin+"\n", "(false,)")
	must(t, os.MkdirAll(f.path("elsewhere"), 0o755))
	if err := os.Symlink(f.path("elsewhere"), runDir); err != nil {
		t.Fatal(err)
	}
	out, calls, err := exec1()
	refused("symlink", "is a symlink or not a directory", out, calls, err)
	if index(calls, "gdbus") >= 0 {
		t.Fatalf("symlink: no later step may run: %v", calls)
	}
	start(vin+"\n", "(false,)")
	must(t, os.WriteFile(runDir, nil, 0o644))
	out, calls, err = exec1()
	refused("file", "is a symlink or not a directory", out, calls, err)

	// An empty VIN (whitespace only): the dir was prepared and the stale socket
	// removed first, and the name was never checked.
	start(" \n\t\n", "(false,)")
	must(t, os.MkdirAll(runDir, 0o755))
	must(t, os.WriteFile(sock, nil, 0o644))
	live, tmp := f.path("run", "airgapp-summon", "live"), f.path("run", "airgapp-summon", ".live.x1")
	must(t, os.WriteFile(live, nil, 0o644))
	must(t, os.WriteFile(tmp, nil, 0o644))
	out, calls, err = exec1()
	refused("empty VIN", "no VIN in "+f.path("vin"), out, calls, err)
	for _, p := range []string{live, tmp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("empty VIN: the stale %s must be gone before the VIN is read: %v", p, err)
		}
	}
	if index(calls, "gdbus") >= 0 {
		t.Fatalf("empty VIN: the name check must not run: %v", calls)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("the stale socket must be gone before the VIN is read: %v", err)
	}
	if fi, err := os.Stat(runDir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 || fi.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("the run dir must be 2750: %v %v", fi.Mode(), err)
	}

	// The name is taken, gdbus fails, or it answers garbage: refused, with the raw answer.
	for _, c := range []struct{ name, answer, logged string }{
		{"taken", "(true,)", "(true,)"},
		{"error", "error", "answered: )"},
		{"garbage", "(maybe,)", "(maybe,)"},
	} {
		start(vin+"\n", c.answer)
		must(t, os.MkdirAll(runDir, 0o755))
		must(t, os.WriteFile(live, nil, 0o644))
		must(t, os.WriteFile(tmp, nil, 0o644))
		out, calls, err = exec1()
		for _, p := range []string{live, tmp} {
			if _, serr := os.Stat(p); !os.IsNotExist(serr) {
				t.Fatalf("%s: the stale %s must be gone: %v", c.name, p, serr)
			}
		}
		refused(c.name, "com.tesla.Webrtc is not free", out, calls, err)
		if !strings.Contains(out, c.logged) || count(calls, nameHasOwner) != 1 || index(calls, "timeout -k 1 5") < 0 {
			t.Fatalf("%s: want one name check and the raw answer %q: %q %v", c.name, c.logged, out, calls)
		}
	}

	// Free: the supervisor starts as the helper uid, with the VIN as read
	// (whitespace stripped) in its environment only.
	start("  "+vin+" \n", "(false,)")
	out, calls, err = exec1()
	want := "chpst -u :" + strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getuid()) + " /var/lib/airgapp-gw/summonhelper run"
	if err != nil || strings.Contains(out, vin) || index(calls, want) < 0 || index(calls, "env SUMMONHELPER_VIN="+vin) < 0 ||
		index(calls, "sleep") >= 0 {
		t.Fatalf("free: %q %v %v", out, calls, err)
	}
}

// The finish script: stderr to svlogd first, then summonhelper finish as the
// helper uid, with no arguments.
func TestSummonFinishScript(t *testing.T) {
	_, finish, err := summonScripts(t, nil)
	if c := commands(finish); err != nil || len(c) != 2 || c[0] != "exec 2>&1" ||
		c[1] != `exec chpst -u :1337:1337 "/var/lib/airgapp-gw/summonhelper" finish` {
		t.Fatalf("finish: %q %v", finish, err)
	}
	// R14: the uid is SUMMON_UID, as in the run script.
	_, finish, err = summonScripts(t, []string{"AGW_TEST_SUMMON_UID=501"})
	if c := commands(finish); err != nil || len(c) != 2 || c[1] != `exec chpst -u :501:501 "/var/lib/airgapp-gw/summonhelper" finish` {
		t.Fatalf("finish with SUMMON_UID=501: %q %v", finish, err)
	}
}

// The stop sequence frees com.tesla.Webrtc for the helper: our helper down
// first (its takeover and finish run), stock webrtc-comms down, its orphaned
// minijail-init killed the firmware's way, then the name polled every 1 s, up
// to 10 times; the helper goes up only once the name is free.
func TestStopWebrtcComms(t *testing.T) {
	f := newFakes(t)
	helper, pidfile := f.helperSvc(), f.path("webrtc-comms.pid")
	jail := func(pidText, pid, comm string) {
		t.Helper()
		if err := os.WriteFile(pidfile, []byte(pidText), 0o644); err != nil {
			t.Fatal(err)
		}
		if pid != "" {
			must(t, os.MkdirAll(f.path("proc", pid), 0o755))
			must(t, os.WriteFile(f.path("proc", pid, "comm"), []byte(comm+"\n"), 0o644))
		}
	}
	stop := func(env ...string) ([]string, string, error) {
		t.Helper()
		f.calls()
		out, err := f.hook("AGW_STOP_WEBRTC_ONLY", "yes\n", append([]string{"SUMMON=1"}, env...))
		return f.calls(), out, err
	}
	poll := []string{"timeout -k 1 5", nameHasOwner}

	// The helper is up, the pidfile names a minijail-init, the name frees on the third poll.
	jail("4242\n", "4242", "minijail-init")
	f.answers("(true,)", "(true,)", "(false,)")
	calls, out, err := stop("FAKE_SV_STATUS=0")
	want := append(append(append(append([]string{
		"sv status " + helper,
		"sv -w 15 down " + helper,
		"sv down /etc/sv/webrtc-comms",
		"kill -KILL 4242",
	}, poll...), "sleep 1"), append(poll, "sleep 1")...), append(poll, "sv up "+helper)...)
	if err != nil || strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("helper up:\n got %q\nwant %q\n%s %v", calls, want, out, err)
	}
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatalf("the pidfile must be removed: %v", err)
	}

	// The helper's runsv is not running: no down for it. sv fails (webrtc-comms
	// has no runsv outside the infotainment and sentry run modes): tolerated.
	// A pidfile naming another program: no kill, the pidfile still removed.
	jail("4243\n", "4243", "bash")
	f.answers("(false,)")
	calls, out, err = stop("FAKE_SV_EXIT=1")
	want = append(append([]string{"sv status " + helper, "sv down /etc/sv/webrtc-comms"}, poll...), "sv up "+helper)
	if err != nil || strings.Join(calls, "\n") != strings.Join(want, "\n") || !strings.Contains(out, "webrtc-comms is not supervised now") {
		t.Fatalf("helper down, sv failing, comm bash:\n got %q\nwant %q\n%s %v", calls, want, out, err)
	}
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatalf("the pidfile must be removed after a comm mismatch: %v", err)
	}

	// The firmware's rules: digits only, never pid 1, no kill without the
	// pid's comm, a failed kill tolerated, no pidfile at all.
	for _, c := range []struct{ name, text, pid, comm, kill string }{
		{"digits only", " 4244x\n", "4244", "minijail-init", "kill -KILL 4244"},
		{"pid 1", "1\n", "1", "minijail-init", ""},
		{"no digits", "none\n", "", "", ""},
		{"no process", "4245\n", "", "", ""},
	} {
		jail(c.text, c.pid, c.comm)
		f.answers("(false,)")
		calls, out, err = stop("FAKE_KILL_EXIT=1")
		if err != nil || (c.kill == "") != (index(calls, "kill") < 0) || (c.kill != "" && index(calls, c.kill) < 0) ||
			index(calls, "sv up "+helper) < 0 {
			t.Fatalf("%s: %q %s %v", c.name, calls, out, err)
		}
		if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
			t.Fatalf("%s: the pidfile must be removed: %v", c.name, err)
		}
	}
	os.Remove(pidfile)
	f.answers("(false,)")
	if calls, out, err = stop(); err != nil || index(calls, "kill") >= 0 || index(calls, "sv up "+helper) < 0 {
		t.Fatalf("no pidfile: %q %s %v", calls, out, err)
	}

	// Only (false,) frees the name: a gdbus error and a garbage answer count
	// as taken, like (true,).
	os.Remove(pidfile)
	f.answers("error", "(maybe,)", "(false,)")
	calls, out, err = stop()
	if err != nil || count(calls, nameHasOwner) != 3 || count(calls, "sleep 1") != 2 || index(calls, "sv up "+helper) < index(calls, "sleep 1") {
		t.Fatalf("error, garbage, then free: %q %s %v", calls, out, err)
	}

	// The name stays taken through all 10 polls: the helper never goes up. A
	// full install has written by now, so it does not stop halfway (F1): the
	// helper is unlinked, the camera goes on to S3, and the failure is reported
	// at the end, non-zero. The stop sequence ran, so the hand-back checks the
	// name once more (stock_hand_back): still taken, it starts nothing.
	f.answers(repeat("(true,)", 11)...)
	calls, out, err = stop()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if exitCode(err) != 1 || count(calls, nameHasOwner) != 11 || count(calls, "sleep 1") != 9 ||
		index(calls, "sv up") >= 0 || !strings.Contains(out, "S3 services: agw camhelper\n") ||
		!strings.HasPrefix(lines[len(lines)-1], "install-car: Summon is not running: com.tesla.Webrtc still has an owner") {
		t.Fatalf("10 x (true,), then (true,) at the hand-back: %q %s %v", calls, out, err)
	}
}

// SV is the service directory; the sv command is SV_BIN. The stop sequence,
// the removal and the helper's start call $SV_BIN, and nothing reassigns it.
func TestSvBinIsNotSv(t *testing.T) {
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Count(s, "SV_BIN=") != 1 || !strings.Contains(s, "\nSV_BIN=${AGW_TEST_SV:-$(command -v sv || echo sv)}\n") {
		t.Fatalf("SV_BIN must be defined once, from AGW_TEST_SV, and never assigned again")
	}
	if regexp.MustCompile(`(read|for|local|export|readonly|unset)\s+(-r\s+)?SV_BIN\b`).MatchString(s) {
		t.Fatalf("SV_BIN must never be reassigned")
	}
	// A command position: the line start, after ; & | ( { or a keyword.
	bare := regexp.MustCompile(`(^|[;&|({]\s*|\b(then|else|do|if|!)\s+)(sv|\$SV|\$\{SV\}|"\$SV")\s`)
	body := func(name string) string {
		i := strings.Index(s, "\n"+name+"() {\n")
		if i < 0 {
			t.Fatalf("no function %s", name)
		}
		j := strings.Index(s[i:], "\n}\n")
		return s[i : i+j]
	}
	for _, fn := range []string{"stop_webrtc", "summon_remove", "hand_back", "helper_up"} {
		b := body(fn)
		if !strings.Contains(b, "$SV_BIN ") {
			t.Errorf("%s must call $SV_BIN", fn)
		}
		for _, l := range strings.Split(b, "\n") {
			if bare.MatchString(strings.TrimSpace(l)) {
				t.Errorf("%s runs sv other than through $SV_BIN: %q", fn, l)
			}
		}
	}
	for _, w := range []string{`$SV_BIN -w 15 down "$SV/summonhelper"`, `$SV_BIN down "$WEBRTC_SV"`} {
		if !strings.Contains(body("stop_webrtc"), w) {
			t.Errorf("the stop sequence must run %s", w)
		}
	}
	for _, w := range []string{`$SV_BIN -w 15 down "$WEBRTC_SV"`, `$SV_BIN up "$WEBRTC_SV"`} {
		if !strings.Contains(body("hand_back"), w) {
			t.Errorf("the hand-back must run %s", w)
		}
	}
}

// --after-boot: SUMMON unset follows the installed helper run script; SUMMON=0
// removes the helper; SUMMON=1 needs one installed; agw's old SUMMON=1 run
// script is regenerated plain (TestAgwRunIsPlainServe) and no longer decides.
func TestAfterBootSummonModes(t *testing.T) {
	f := newFakes(t)
	helper := f.helperSvc()
	installedRun := f.path("installed-summon-run")
	must(t, os.WriteFile(installedRun, []byte("#!/bin/sh\n"), 0o755))

	// SUMMON unset, a helper installed: the stop sequence runs and the helper goes up.
	f.answers("(false,)")
	out, err := f.hook("AGW_STOP_WEBRTC_ONLY", "yes\n", []string{"AGW_TEST_SUMMON_RUN=" + installedRun}, "--after-boot")
	calls := f.calls()
	if err != nil || index(calls, "sv down /etc/sv/webrtc-comms") < 0 || index(calls, "sv up "+helper) < 0 ||
		!strings.Contains(out, "S3 services: agw camhelper summonhelper\n") {
		t.Fatalf("installed helper, SUMMON unset: %q %s %v", calls, out, err)
	}
	// SUMMON unset, no helper installed, but agw's run script is still the
	// old SUMMON=1 one (supervise, then serve), where the hooks read agw's
	// installed run script: it no longer counts. SUMMON=0, nothing stopped, no
	// gdbus needed, S3 for agw and camhelper only (m1), after a boot and on a
	// full install.
	oldAgw := f.path("gw", "sv", "agw", "run")
	must(t, os.MkdirAll(filepath.Dir(oldAgw), 0o755))
	must(t, os.WriteFile(oldAgw, []byte("#!/bin/sh\nexec 2>&1\nexec chpst -u :4950:4950 \"/var/lib/airgapp-gw/agw\" supervise -- serve -dial \"10.0.0.9:17101\" -key \"/var/lib/airgapp-gw/state/key.pem\"\n"), 0o755))
	for _, args := range [][]string{{"--after-boot"}, nil} {
		out, err = f.hook("AGW_STOP_WEBRTC_ONLY", "", []string{"AGW_TEST_GDBUS=/nonexistent/gdbus"}, args...)
		if calls := f.calls(); err != nil || len(calls) != 0 || !strings.Contains(out, "S3 services: agw camhelper\n") {
			t.Fatalf("%v, no helper, SUMMON unset, agw's old supervise run script: %q %s %v", args, calls, out, err)
		}
	}
	must(t, os.Remove(oldAgw))

	// SUMMON=1 after a boot with no helper installed is a state mismatch, not
	// a typo: Summon is skipped for the boot (Q1). Nothing Summon runs, the
	// camera goes on, and the end report says so, non-zero. A bad SUMMON value
	// is input validation and still stops before anything.
	for _, c := range []struct{ hook, goesOn string }{
		{"AGW_STOP_WEBRTC_ONLY", "S3 services: agw camhelper\n"},
		{"AGW_SUMMON_REMOVE_ONLY", "S3 services: agw camhelper\n"},
		{"AGW_SUMMON_PREFLIGHT_ONLY", "preflight ok (SUMMON=1)\n"},
	} {
		out, err = f.hook(c.hook, "yes\n", []string{"SUMMON=1"}, "--after-boot")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if calls := f.calls(); exitCode(err) != 1 || len(calls) != 0 || !strings.Contains(out, c.goesOn) ||
			!strings.HasPrefix(lines[len(lines)-1], "install-car: Summon is not running: skipped this boot: SUMMON=1, but summonhelper is not installed") ||
			!strings.HasSuffix(lines[len(lines)-1], "install with SUMMON=1 first") {
			t.Fatalf("%s, SUMMON=1, no helper: %q %s %v", c.hook, calls, out, err)
		}
		out, err = f.hook(c.hook, "yes\n", []string{"SUMMON=yes"}, "--after-boot")
		if calls := f.calls(); exitCode(err) != 1 || len(calls) != 0 || strings.Contains(out, c.goesOn) ||
			strings.TrimSpace(out) != "install-car: SUMMON must be 0 or 1" {
			t.Fatalf("%s, SUMMON=yes: %q %s %v", c.hook, calls, out, err)
		}
	}

	// SUMMON=0 after a boot, with the helper installed: the removal runs.
	// After a boot our helper is not running and stock owns the name, so the
	// removal completes and starts nothing.
	f.installHelper()
	f.answers("(true,)")
	out, err = f.hook("AGW_SUMMON_REMOVE_ONLY", "yes\n", []string{"SUMMON=0"}, "--after-boot")
	calls = f.calls()
	if _, serr := os.Stat(helper); err != nil || index(calls, "sv up /etc/sv/webrtc-comms") >= 0 || !os.IsNotExist(serr) {
		t.Fatalf("SUMMON=0 after boot: %q %s %v", calls, out, err)
	}
	// SUMMON unset after a boot: the installed run script decides. With it, no
	// removal. Without it (a removal cut short after step 2) nothing either:
	// only an explicit SUMMON=0 removes, and it finishes the cut-short removal.
	f.installHelper()
	out, err = f.hook("AGW_SUMMON_REMOVE_ONLY", "", nil, "--after-boot")
	if calls := f.calls(); err != nil || len(calls) != 0 {
		t.Fatalf("SUMMON unset with the run script: no removal: %q %s %v", calls, out, err)
	}
	if _, err := os.Stat(filepath.Join(helper, "run")); err != nil {
		t.Fatal("the helper must be left alone")
	}
	must(t, os.Remove(filepath.Join(helper, "run")))
	out, err = f.hook("AGW_SUMMON_REMOVE_ONLY", "", nil, "--after-boot")
	if calls := f.calls(); err != nil || len(calls) != 0 {
		t.Fatalf("SUMMON unset, run script gone: nothing: %q %s %v", calls, out, err)
	}
	f.answers("(true,)")
	out, err = f.hook("AGW_SUMMON_REMOVE_ONLY", "yes\n", []string{"SUMMON=0"}, "--after-boot")
	if _, serr := os.Stat(helper); err != nil || index(f.calls(), "sv up /etc/sv/webrtc-comms") >= 0 || !os.IsNotExist(serr) {
		t.Fatalf("SUMMON=0, run script gone: the removal finishes: %s %v", out, err)
	}
}

// After a boot, a name that stays taken never keeps the camera down: S3 still
// links and starts agw and camhelper, the helper is neither linked nor
// started, and the run ends non-zero with the Summon report. The stop sequence
// ran, so the hand-back checks the name once more: still owned, it starts
// nothing. (The hook runs the same stop stage and service list as the main
// flow; the main flow's order is TestSummonMainFlowOrder's.)
func TestAfterBootStopFailureStillStartsServices(t *testing.T) {
	f := newFakes(t)
	runsv := f.path("run", "service")
	links := f.installHelper(runsv) // as the install left it: run script and links
	foreign := f.path("gw", "svdir", "airgapp-agw")
	must(t, os.Symlink(f.path("gw", "sv", "agw"), foreign))
	f.answers(repeat("(true,)", 11)...) // the stop sequence's 10 checks, then the hand-back's one
	out, err := f.hook("AGW_STOP_WEBRTC_ONLY", "yes\n", []string{"AGW_TEST_SVDIR=" + runsv}, "--after-boot")
	calls := f.calls()
	if exitCode(err) != 1 || count(calls, nameHasOwner) != 11 || index(calls, "sv up") >= 0 ||
		!strings.Contains(out, "com.tesla.Webrtc already has an owner (no summonhelper process runs): stock webrtc-comms is not restarted\n") {
		t.Fatalf("after-boot, 11 x (true,): %q %s %v", calls, out, err)
	}
	// Neither linked nor started: the helper's links are gone before S3 (so no
	// runsvdir can start it), its run script stays for the next --after-boot,
	// and nothing else is unlinked.
	for _, l := range links {
		if _, err := os.Lstat(l); !os.IsNotExist(err) {
			t.Fatalf("%s must be unlinked: %v", l, err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.helperSvc(), "run")); err != nil {
		t.Fatalf("the helper's run script must stay: %v", err)
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatalf("agw's link must stay: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	s3 := -1
	for i, l := range lines {
		if l == "S3 services: agw camhelper" {
			s3 = i
		}
	}
	if s3 < 0 || !strings.HasPrefix(lines[len(lines)-1], "install-car: Summon is not running: ") || s3 > len(lines)-2 {
		t.Fatalf("want S3 for agw and camhelper only, then the Summon report last: %q", out)
	}
	// The same failure on a full install, which has written by then, does the
	// same (F1): S3 for agw and camhelper, then the report, non-zero.
	f.answers(repeat("(true,)", 11)...)
	out, err = f.hook("AGW_STOP_WEBRTC_ONLY", "yes\n", []string{"SUMMON=1"})
	lines = strings.Split(strings.TrimSpace(out), "\n")
	if exitCode(err) != 1 || !strings.Contains(out, "S3 services: agw camhelper\n") ||
		!strings.HasPrefix(lines[len(lines)-1], "install-car: Summon is not running: ") {
		t.Fatalf("full install, 10 x (true,): %s %v", out, err)
	}
}

// SUMMON=0 on a car with the helper hands com.tesla.Webrtc back to stock, in
// order: the links and the run script first (no later --after-boot can bring
// it back); the helper down; a wait until no summonhelper process runs; then,
// only if the name is free, stock restarted and checked; the service dir last.
// The removal decides on OUR processes, never on the name: after a reboot
// stock owns the name legitimately (I1). And it restarts stock rather than
// only bringing it up: sv up does nothing to a stock that a run-mode switch
// restarted without the name, which it registers once, at its start (I2).
func TestSummonZeroRemovesHelper(t *testing.T) {
	f := newFakes(t)
	helper, run := f.helperSvc(), filepath.Join(f.helperSvc(), "run")
	runsv := f.path("run", "service")
	remove := func(stdin string, env []string, args ...string) ([]string, string, error) {
		t.Helper()
		f.calls()
		os.Remove(f.path("sleeps"))
		out, err := f.hook("AGW_SUMMON_REMOVE_ONLY", stdin, append([]string{"SUMMON=0", "AGW_TEST_SVDIR=" + runsv}, env...), args...)
		return f.calls(), out, err
	}
	reinstall := func(svdirs ...string) []string {
		t.Helper()
		os.RemoveAll(f.path("gw"))
		os.RemoveAll(runsv)
		os.RemoveAll(f.path("proc"))
		return f.installHelper(svdirs...)
	}
	lastLine := func(out string) string {
		l := strings.Split(strings.TrimSpace(out), "\n")
		return l[len(l)-1]
	}
	poll := []string{"timeout -k 1 5", nameHasOwner}

	// The helper is up (its serve child in the fake /proc, gone once sv downs
	// it) and leaves the name free: stock is restarted (down; its orphaned jail
	// killed the firmware's way; up) and registers on the second check.
	links := reinstall(runsv)
	proc := f.helperProc("4300", "serve")
	must(t, os.WriteFile(f.path("webrtc-comms.pid"), []byte("4400\n"), 0o644))
	must(t, os.MkdirAll(f.path("proc", "4400"), 0o755))
	must(t, os.WriteFile(f.path("proc", "4400", "comm"), []byte("minijail-init\n"), 0o644))
	watch := "FAKE_SV_WATCH=" + strings.Join(append(append([]string{}, links...), run, helper), " ")
	gone := " |"
	for _, l := range links {
		gone += " -" + l
	}
	gone += " -" + run + " +" + helper
	f.answers("(false,)", "(false,)", "(true,)")
	liveFile := f.path("run", "airgapp-summon", "live")
	must(t, os.MkdirAll(filepath.Dir(liveFile), 0o755))
	must(t, os.WriteFile(liveFile, nil, 0o644))
	calls, out, err := remove("yes\n", []string{watch, "FAKE_SV_DOWN_RM=" + proc})
	if _, serr := os.Stat(liveFile); !os.IsNotExist(serr) {
		t.Fatalf("a completed removal must delete a stale live file: %v", serr)
	}
	want := append(append(append([]string{"sv -w 15 down " + helper + gone}, poll...),
		"sv -w 15 down /etc/sv/webrtc-comms"+gone, "kill -KILL 4400", "sv up /etc/sv/webrtc-comms"+gone),
		append(append(poll, "sleep 1"), poll...)...)
	if err != nil || strings.Join(calls, "\n") != strings.Join(want, "\n") || !strings.Contains(out, "owns com.tesla.Webrtc again") {
		t.Fatalf("removal, helper up, name free:\n got %q\nwant %q\n%s %v", calls, want, out, err)
	}
	if _, err := os.Stat(helper); !os.IsNotExist(err) {
		t.Fatalf("the service dir must be gone last: %v", err)
	}

	// After a reboot: our runsvdir is not running, so the helper is not (sv
	// fails; the fake /proc holds only look-alikes, which never count: other
	// names in our DIR, and a summonhelper elsewhere), and stock owns the name.
	// The removal completes, starts nothing, and exits 0, on a full install and
	// after a boot. A link that is not ours stays.
	for _, args := range [][]string{nil, {"--after-boot"}} {
		reinstall()
		f.proc("4600", f.path("gw", "summonhelper-old"), "run")
		f.proc("4601", f.path("gw", "summonhelper.new"), "serve")
		f.proc("4602", f.path("gw", "summonhelperx"), "run")
		f.proc("4603", "/tmp/summonhelper", "probe")
		other := filepath.Join(runsv, "airgapp-summonhelper")
		must(t, os.MkdirAll(runsv, 0o755))
		must(t, os.Symlink(f.path("elsewhere"), other))
		f.answers(repeat("(true,)", 10)...)
		calls, out, err = remove("yes\n", []string{"FAKE_SV_EXIT=1"}, args...)
		_, serr := os.Stat(helper)
		if target, _ := os.Readlink(other); err != nil || !os.IsNotExist(serr) || count(calls, nameHasOwner) != 1 ||
			index(calls, "sv up") >= 0 || index(calls, "sv -w 15 down /etc/sv/webrtc-comms") >= 0 ||
			!strings.Contains(out, "already has an owner") || target != f.path("elsewhere") {
			t.Fatalf("%v, after a reboot (stock owns the name): %q %s %v", args, calls, out, err)
		}
	}

	// The helper's last process exits only by the third check: the removal
	// waits for it, 1 s apart, then goes on.
	reinstall()
	proc = f.helperProc("4301", "run")
	f.answers("(true,)")
	calls, out, err = remove("yes\n", []string{"FAKE_SLEEP_RM=" + proc, "FAKE_SLEEP_RM_AFTER=2"})
	want = append([]string{"sv -w 15 down " + helper, "sleep 1", "sleep 1"}, poll...)
	if _, serr := os.Stat(helper); err != nil || strings.Join(calls, "\n") != strings.Join(want, "\n") || !os.IsNotExist(serr) {
		t.Fatalf("a late exit:\n got %q\nwant %q\n%s %v", calls, want, out, err)
	}

	// A summonhelper process that lingers keeps the directory (without its run
	// script) and leaves stock alone, with a removal message of its own naming
	// the pid. The install goes on, a full one too (F1), and the message is the
	// report at the end.
	for _, args := range [][]string{nil, {"--after-boot"}} {
		reinstall()
		f.helperProc("4302", "serve")
		must(t, os.MkdirAll(filepath.Dir(liveFile), 0o755))
		must(t, os.WriteFile(liveFile, nil, 0o644))
		calls, out, err = remove("yes\n", nil, args...)
		if _, lerr := os.Stat(liveFile); lerr != nil {
			t.Fatalf("%v, a lingering helper keeps the live file: %v", args, lerr)
		}
		_, serr := os.Stat(helper)
		_, rerr := os.Stat(run)
		goesOn := strings.Contains(out, "S3 services: agw camhelper\n")
		if exitCode(err) != 1 || count(calls, "sleep 1") != 9 || index(calls, nameHasOwner) >= 0 ||
			index(calls, "/etc/sv/webrtc-comms") >= 0 || serr != nil || !os.IsNotExist(rerr) ||
			!strings.HasPrefix(lastLine(out), "install-car: SUMMON=0 removal not finished: ") ||
			!strings.Contains(lastLine(out), "pid 4302") || !strings.Contains(lastLine(out), "kill 4302") ||
			strings.Contains(out, "Summon is not running") || !goesOn {
			t.Fatalf("%v, a lingering helper: %q %s %v", args, calls, out, err)
		}
	}

	// Stock never registers after its restart: our side is done (the service
	// dir is gone), and the failure never keeps agw or camhelper down: the
	// install goes on, and it is reported at the end, non-zero, on a full
	// install as after a boot.
	for _, args := range [][]string{nil, {"--after-boot"}} {
		reinstall()
		f.answers(repeat("(false,)", 11)...)
		calls, out, err = remove("yes\n", nil, args...)
		_, serr := os.Stat(helper)
		if exitCode(err) != 1 || count(calls, nameHasOwner) != 11 || index(calls, "sv up /etc/sv/webrtc-comms") < 0 ||
			!os.IsNotExist(serr) || !strings.Contains(out, "S3 services: agw camhelper\n") ||
			!strings.HasPrefix(lastLine(out), "install-car: SUMMON=0 removal: summonhelper is removed, but stock webrtc-comms did not register") {
			t.Fatalf("%v, stock never registers: %q %s %v", args, calls, out, err)
		}
	}

	// Stock has no runsv (a loadshed or lowpower run mode): a note, no wait,
	// and the removal completes.
	reinstall()
	f.answers("(false,)")
	calls, out, err = remove("yes\n", []string{"FAKE_SV_EXIT=1"})
	if _, serr := os.Stat(helper); err != nil || count(calls, nameHasOwner) != 1 || index(calls, "sv up /etc/sv/webrtc-comms") < 0 ||
		!os.IsNotExist(serr) || !strings.Contains(out, "next infotainment or sentry") {
		t.Fatalf("stock without a runsv: %q %s %v", calls, out, err)
	}

	// No helper installed: SUMMON=0 does nothing at all.
	os.RemoveAll(f.path("gw"))
	if calls, out, err = remove("", nil); err != nil || len(calls) != 0 {
		t.Fatalf("no helper: %q %s %v", calls, out, err)
	}
}

// The two hooks that can reach summon_remove's rm refuse to run without the
// run directory override, so a hand-run hook never touches /run/airgapp-summon.
func TestSummonHooksNeedRunDirOverride(t *testing.T) {
	f := newFakes(t)
	f.installHelper()
	for _, hook := range []string{"AGW_SUMMON_REMOVE_ONLY", "AGW_EXIT_PATH_ONLY"} {
		out, err := f.hook(hook, "yes\n", []string{"SUMMON=0", "AGW_TEST_SUMMON_RUN_DIR="})
		if exitCode(err) == 0 || !strings.Contains(out, "needs AGW_TEST_SUMMON_RUN_DIR") {
			t.Fatalf("%s without the override: %q %v", hook, out, err)
		}
	}
}

// SUMMON unset keeps what is installed, on a full install as after a boot: a
// camera-only re-install that forgot SUMMON must neither offer to remove an
// installed helper nor need a new summonhelper binary. The helper is stopped
// (S0b) and started fresh, as with SUMMON=1; only an explicit SUMMON=1
// installs a binary and a key (steps 0, 4b and 4c).
func TestSummonUnsetKeepsAnInstalledHelper(t *testing.T) {
	f := newFakes(t)
	links := f.installHelper()
	helper := f.helperSvc()
	binaries := []string{"./agw", "./camhelper", f.path("no-summonhelper-here")}

	// Resolved SUMMON=1, needing gdbus and timeout but no summonhelper binary.
	out, err := f.hook("AGW_SUMMON_PREFLIGHT_ONLY", "", nil, binaries...)
	if err != nil || !strings.Contains(out, "preflight ok (SUMMON=1)") {
		t.Fatalf("preflight, SUMMON unset with a helper: %s %v", out, err)
	}
	if out, err = f.hook("AGW_SUMMON_PREFLIGHT_ONLY", "", []string{"AGW_TEST_GDBUS=/nonexistent/gdbus"}, binaries...); exitCode(err) != 1 ||
		!strings.Contains(out, "gdbus not found") {
		t.Fatalf("preflight, SUMMON unset with a helper, no gdbus: %s %v", out, err)
	}
	// An explicit SUMMON=1 installs, so it needs the binary.
	if out, err = f.hook("AGW_SUMMON_PREFLIGHT_ONLY", "", []string{"SUMMON=1"}, binaries...); exitCode(err) != 1 ||
		!strings.Contains(out, "summonhelper binary not found at "+binaries[2]) {
		t.Fatalf("preflight, SUMMON=1 without the binary: %s %v", out, err)
	}

	// No removal and no removal ask: with no stdin it still exits 0, and the
	// helper is untouched.
	out, err = f.hook("AGW_SUMMON_REMOVE_ONLY", "", nil, binaries...)
	if calls := f.calls(); err != nil || len(calls) != 0 || strings.Contains(out, "remove") {
		t.Fatalf("SUMMON unset with a helper: no removal, no ask: %q %s %v", calls, out, err)
	}
	for _, p := range append([]string{filepath.Join(helper, "run")}, links...) {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s must be untouched: %v", p, err)
		}
	}

	// The stop sequence and a fresh start, as with SUMMON=1.
	f.answers("(false,)")
	out, err = f.hook("AGW_STOP_WEBRTC_ONLY", "yes\n", nil, binaries...)
	calls := f.calls()
	if err != nil || index(calls, "sv down /etc/sv/webrtc-comms") < 0 || index(calls, "sv up "+helper) < index(calls, "timeout -k 1 5") ||
		!strings.Contains(out, "S3 services: agw camhelper summonhelper\n") {
		t.Fatalf("SUMMON unset with a helper: stop and start fresh: %q %s %v", calls, out, err)
	}

	// Steps 0, 4b and 4c (the binary and the key) belong to an explicit SUMMON=1
	// on a full install only (no hook: they write as root).
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"if summon_installing; then\n\task \"0. ",
		"(docs/SUMMON.md)\"\n\tsummon_install_asks\nfi\n", "\nif summon_installing; then summon_install_files; fi\n"} {
		if !strings.Contains(string(b), step) {
			t.Errorf("%q: steps 0 and 4b-4c must run only for an explicit SUMMON=1 on a full install", step)
		}
	}
}

// An explicit SUMMON=0 still removes an installed helper, asking first, on a
// full install and after a boot.
func TestSummonZeroStillRemovesAnInstalledHelper(t *testing.T) {
	f := newFakes(t)
	for _, args := range [][]string{nil, {"--after-boot"}} {
		os.RemoveAll(f.path("gw"))
		links := f.installHelper()
		helper := f.helperSvc()
		// It asks first: no answer declines it for this run, on a full install as
		// after a boot (F1; TestAfterBootDeclinedRemovalSkips): nothing removed,
		// only the helper unlinked.
		out, err := f.hook("AGW_SUMMON_REMOVE_ONLY", "", []string{"SUMMON=0"}, args...)
		_, lerr := os.Lstat(links[0])
		_, rerr := os.Stat(filepath.Join(helper, "run"))
		if exitCode(err) != 1 || !strings.Contains(out, "declined") || len(f.calls()) != 0 || !os.IsNotExist(lerr) || rerr != nil {
			t.Fatalf("%v: no answer must decline the removal: %s %v", args, out, err)
		}
		f.answers("(true,)") // stock owns the name once our helper is gone
		out, err = f.hook("AGW_SUMMON_REMOVE_ONLY", "yes\n", []string{"SUMMON=0"}, args...)
		calls := f.calls()
		if _, serr := os.Stat(helper); err != nil || index(calls, "sv -w 15 down "+helper) < 0 ||
			index(calls, nameHasOwner) < 0 || !os.IsNotExist(serr) {
			t.Fatalf("%v: SUMMON=0 removes the helper: %q %s %v", args, calls, out, err)
		}
	}
}

// SUMMON unset with no helper installed is SUMMON=0 and does nothing for
// Summon, needing neither gdbus nor timeout. A service directory without its
// run script (a removal cut short) is not an installed helper: it is left for
// an explicit SUMMON=0, with no removal and no ask.
func TestSummonUnsetWithoutAHelperDoesNothing(t *testing.T) {
	f := newFakes(t)
	nothing := func(name string, args ...string) {
		t.Helper()
		env := []string{"AGW_TEST_GDBUS=/nonexistent/gdbus", "AGW_TEST_TIMEOUT=/nonexistent/timeout"}
		out, err := f.hook("AGW_SUMMON_PREFLIGHT_ONLY", "", env, args...)
		if err != nil || !strings.Contains(out, "preflight ok (SUMMON=0)") {
			t.Fatalf("%s: preflight: %s %v", name, out, err)
		}
		out, err = f.hook("AGW_SUMMON_REMOVE_ONLY", "", nil, args...)
		if calls := f.calls(); err != nil || len(calls) != 0 || strings.Contains(out, "remove") {
			t.Fatalf("%s: removal hook: %q %s %v", name, calls, out, err)
		}
		out, err = f.hook("AGW_STOP_WEBRTC_ONLY", "", nil, args...)
		if calls := f.calls(); err != nil || len(calls) != 0 || !strings.Contains(out, "S3 services: agw camhelper\n") {
			t.Fatalf("%s: stop hook: %q %s %v", name, calls, out, err)
		}
	}
	for _, args := range [][]string{nil, {"--after-boot"}} {
		os.RemoveAll(f.path("gw"))
		nothing(fmt.Sprintf("%v, no helper", args), args...)
		f.installHelper()
		must(t, os.Remove(filepath.Join(f.helperSvc(), "run")))
		nothing(fmt.Sprintf("%v, a service dir without its run script", args), args...)
		if _, err := os.Stat(filepath.Join(f.helperSvc(), "finish")); err != nil {
			t.Fatalf("%v: the leftover service dir must be left alone: %v", args, err)
		}
	}
}

// After a boot, a missing gdbus or timeout skips Summon for this boot instead
// of keeping the camera down (m2): no stop sequence or removal, the helper
// unlinked (so it is neither linked nor started), S3 for agw and camhelper,
// and the reason reported at the end, non-zero. A full install still stops
// before it changes anything.
func TestAfterBootSkipsSummonWithoutTools(t *testing.T) {
	f := newFakes(t)
	lastLine := func(out string) string {
		l := strings.Split(strings.TrimSpace(out), "\n")
		return l[len(l)-1]
	}
	for _, tool := range []string{"gdbus", "timeout"} {
		missing := "AGW_TEST_" + strings.ToUpper(tool) + "=/nonexistent/" + tool
		reason := tool + " not found (the com.tesla.Webrtc name checks need it)"
		for _, c := range []struct {
			name, hook, report string
			env                []string
		}{
			{"kept helper", "AGW_STOP_WEBRTC_ONLY", "Summon is not running: skipped this boot: ", []string{missing}},
			{"removal", "AGW_SUMMON_REMOVE_ONLY", "SUMMON=0 removal skipped this boot: ", []string{"SUMMON=0", missing}},
		} {
			os.RemoveAll(f.path("gw"))
			links := f.installHelper()
			f.calls()
			out, err := f.hook(c.hook, "yes\n", c.env, "--after-boot")
			calls := f.calls()
			_, lerr := os.Lstat(links[0])
			_, rerr := os.Stat(filepath.Join(f.helperSvc(), "run"))
			if exitCode(err) != 1 || len(calls) != 0 || !strings.Contains(out, "S3 services: agw camhelper\n") ||
				lastLine(out) != "install-car: "+c.report+reason || !os.IsNotExist(lerr) || rerr != nil {
				t.Fatalf("%s, %s missing, after a boot: %q %s %v", c.name, tool, calls, out, err)
			}
			// A full install stops before it changes anything.
			must(t, os.Symlink(f.helperSvc(), links[0]))
			out, err = f.hook(c.hook, "yes\n", c.env)
			_, lerr = os.Lstat(links[0])
			if calls := f.calls(); exitCode(err) != 1 || len(calls) != 0 || strings.Contains(out, "S3 services") ||
				lastLine(out) != "install-car: "+reason || lerr != nil {
				t.Fatalf("%s, %s missing, full install: %q %s %v", c.name, tool, calls, out, err)
			}
		}
	}
}

// After a boot, anything but a typed yes at the SUMMON=0 removal's own ask
// (no stdin too) skips the removal for that boot instead of stopping the run
// (fix round 1b, extending m2): the helper is neither linked nor started (its
// links go; its directory, run script included, stays as it is), the camera
// goes on to S3, and the end report says the removal was declined, non-zero.
// A full install, which has written by then, does the same (F1).
func TestAfterBootDeclinedRemovalSkips(t *testing.T) {
	f := newFakes(t)
	runsv := f.path("run", "service")
	for _, stdin := range []string{"no\n", ""} {
		os.RemoveAll(f.path("gw"))
		os.RemoveAll(runsv)
		links := f.installHelper(runsv)
		f.calls()
		out, err := f.hook("AGW_SUMMON_REMOVE_ONLY", stdin, []string{"SUMMON=0", "AGW_TEST_SVDIR=" + runsv}, "--after-boot")
		calls := f.calls()
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if exitCode(err) != 1 || len(calls) != 0 || !strings.Contains(out, "S3 services: agw camhelper\n") ||
			!strings.HasPrefix(lines[len(lines)-1], "install-car: SUMMON=0 removal skipped this boot: ") ||
			!strings.Contains(lines[len(lines)-1], "declined") {
			t.Fatalf("after a boot, removal answered %q: %q %s %v", stdin, calls, out, err)
		}
		for _, l := range links {
			if _, err := os.Lstat(l); !os.IsNotExist(err) {
				t.Fatalf("after a boot, removal answered %q: %s must be unlinked: %v", stdin, l, err)
			}
		}
		for _, p := range []string{"run", "finish", "log/run"} {
			if _, err := os.Stat(filepath.Join(f.helperSvc(), p)); err != nil {
				t.Fatalf("after a boot, removal answered %q: the directory must stay as it is (%s): %v", stdin, p, err)
			}
		}
	}
	// A full install has written by then, so a "no" skips the removal the same
	// way instead of stopping the install halfway (F1).
	os.RemoveAll(f.path("gw"))
	links := f.installHelper()
	out, err := f.hook("AGW_SUMMON_REMOVE_ONLY", "no\n", []string{"SUMMON=0"})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	_, lerr := os.Lstat(links[0])
	_, rerr := os.Stat(filepath.Join(f.helperSvc(), "run"))
	if calls := f.calls(); exitCode(err) != 1 || !strings.Contains(out, "S3 services: agw camhelper\n") || len(calls) != 0 ||
		!strings.HasPrefix(lines[len(lines)-1], "install-car: SUMMON=0 removal skipped by this install: ") ||
		!os.IsNotExist(lerr) || rerr != nil {
		t.Fatalf("full install, removal answered no: %q %s %v", calls, out, err)
	}
}

// A full install never stops halfway for a Summon reason (F1). Once it has
// written anything (step 1's mkdir, before any of these), each of these cases
// skips Summon for the run the way an after-boot does: SUMMON_OK=0, the helper
// unlinked (neither linked nor started), agw and camhelper set up as usual
// (the run goes on to S3 or S4), and the run fails at the end with the report.
// A failing 4b/4c command says whether the new binary is in place: only a
// failure before its mv leaves it uninstalled.
func TestFullInstallSummonFailuresNeverHalfApply(t *testing.T) {
	f := newFakes(t)
	src := f.path("summonhelper-src")
	unreadable := f.path("summonhelper-unreadable")
	must(t, os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755))
	must(t, os.WriteFile(unreadable, []byte("#!/bin/sh\n"), 0o755))
	must(t, os.Chmod(unreadable, 0))
	defer os.Chmod(unreadable, 0o644)
	for _, c := range []struct {
		name, hook, stdin, goesOn, report string
		env, args                         []string
		setup                             func()
	}{
		{"step 4b's cp fails", "AGW_SUMMON_FILES_ONLY", "yes\nyes\n", "S3 services: agw camhelper\n",
			"install-car: Summon is not running: step 4b/4c failed (cp " + unreadable + " " + f.path("gw", "summonhelper.new") +
				"), so summonhelper was not installed, linked or started", []string{"SUMMON=1"},
			[]string{"./agw", "./camhelper", unreadable}, nil},
		{"step 4c's keygen fails", "AGW_SUMMON_FILES_ONLY", "yes\nyes\n", "S3 services: agw camhelper\n",
			"install-car: Summon is not running: step 4b/4c failed (chpst -u :1337:1337 " + f.path("gw", "summonhelper") + " keygen -key " +
				f.path("summon-home", "state", "key.pem") + "): the new summonhelper is in place at " + f.path("gw", "summonhelper") +
				", but it was not linked or started",
			[]string{"SUMMON=1", "FAKE_KEYGEN_FAIL=1"}, []string{"./agw", "./camhelper", src}, nil},
		{"an unsupervised summonhelper runs before S3", "AGW_STOP_WEBRTC_ONLY", "yes\n", "S3 services: agw camhelper\n",
			"install-car: Summon is not running: an unsupervised summonhelper is running (pid 4800)", []string{"SUMMON=1"}, nil,
			func() { f.answers("(false,)"); f.helperProc("4800", "serve") }},
		{"the stop sequence fails (the name stays taken)", "AGW_STOP_WEBRTC_ONLY", "yes\n", "S3 services: agw camhelper\n",
			"install-car: Summon is not running: com.tesla.Webrtc still has an owner", []string{"SUMMON=1"}, nil,
			func() { f.answers(repeat("(true,)", 11)...) }},
		{"S0b's ask answered no", "AGW_STOP_WEBRTC_ONLY", "no\n", "S3 services: agw camhelper\n",
			"install-car: Summon is not running: skipped by this install: the S0b stop sequence was not confirmed",
			[]string{"SUMMON=1"}, nil, nil},
		{"a summonhelper process lingers at the removal", "AGW_SUMMON_REMOVE_ONLY", "yes\n", "S3 services: agw camhelper\n",
			"install-car: SUMMON=0 removal not finished: a summonhelper process still runs (pid 4500)", []string{"SUMMON=0"}, nil,
			func() { f.helperProc("4500", "serve") }},
		{"the removal's ask answered no", "AGW_SUMMON_REMOVE_ONLY", "no\n", "S3 services: agw camhelper\n",
			"install-car: SUMMON=0 removal skipped by this install: the removal was declined", []string{"SUMMON=0"}, nil, nil},
		{"summonhelper not running after S3 (SUMMON unset keeps it)", "AGW_SUMMON_CHECKS_ONLY", "", "S4 services: agw camhelper\n",
			"install-car: Summon is not running: summonhelper is not running", nil, nil, func() { f.answers("(true,)") }},
	} {
		if strings.Contains(c.name, "cp fails") && os.Getuid() == 0 {
			continue // root reads an unreadable file
		}
		os.RemoveAll(f.path("gw"))
		os.RemoveAll(f.path("proc"))
		os.RemoveAll(f.path("summon-home"))
		links := f.installHelper()
		if c.setup != nil {
			c.setup()
		}
		f.calls()
		out, err := f.hook(c.hook, c.stdin, c.env, c.args...)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if exitCode(err) != 1 || !strings.Contains(out, c.goesOn) || !strings.HasPrefix(lines[len(lines)-1], c.report) {
			t.Fatalf("full install, %s: %s %v", c.name, out, err)
		}
		if strings.Contains(c.name, "keygen") && strings.Contains(out, "not installed") {
			t.Fatalf("full install, %s: the binary was moved into place, so it is installed: %s", c.name, out)
		}
		if _, err := os.Lstat(links[0]); !os.IsNotExist(err) {
			t.Fatalf("full install, %s: the helper must be unlinked: %v", c.name, err)
		}
	}
}

// The checks that come before a full install's first write still stop it
// with nothing changed (F1 keeps them): a missing tool, a missing
// summonhelper binary, a bad SUMMON.
func TestFullInstallPreWriteChecksStillDie(t *testing.T) {
	f := newFakes(t)
	bin := f.path("summonhelper")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))
	f.installHelper()
	for _, c := range []struct {
		name, last string
		env, args  []string
	}{
		{"gdbus missing", "install-car: gdbus not found (the com.tesla.Webrtc name checks need it)",
			[]string{"SUMMON=1", "AGW_TEST_GDBUS=/nonexistent/gdbus"}, []string{"./agw", "./camhelper", bin}},
		{"timeout missing", "install-car: timeout not found (the com.tesla.Webrtc name checks need it)",
			[]string{"AGW_TEST_TIMEOUT=/nonexistent/timeout"}, []string{"./agw", "./camhelper", bin}},
		{"the summonhelper binary missing", "install-car: summonhelper binary not found at " + f.path("none"),
			[]string{"SUMMON=1"}, []string{"./agw", "./camhelper", f.path("none")}},
		{"a bad SUMMON", "install-car: SUMMON must be 0 or 1", []string{"SUMMON=2"}, []string{"./agw", "./camhelper", bin}},
	} {
		out, err := f.hook("AGW_SUMMON_PREFLIGHT_ONLY", "yes\n", c.env, c.args...)
		if exitCode(err) != 1 || strings.TrimSpace(out) != c.last || len(f.calls()) != 0 {
			t.Fatalf("full install, %s: %s %v", c.name, out, err)
		}
	}
}

// The real install refuses any AGW_TEST_* override, even an empty one: they
// are for the test hooks only. The guard sits after every hook and before the
// root check, so no override can reach a real install (AGW_TEST_SUMMON_UID=0
// would bake a root summonhelper into its run script; AGW_TEST_IPT a fake
// iptables into agw's). Without one, the run goes on to the root check. This
// never runs as root: a broken guard would then start a real install.
func TestRealInstallRefusesTestOverrides(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("never as root: a broken guard would start a real install")
	}
	var clean []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "AGW_") && !strings.HasPrefix(e, "SUMMON=") {
			clean = append(clean, e)
		}
	}
	run := func(extra ...string) (string, error) {
		cmd := exec.Command("sh", "../../deploy/install-car.sh")
		cmd.Env = append(append([]string{}, clean...), extra...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, e := range []string{"AGW_TEST_IPT=/x/iptables", "AGW_TEST_SUMMON_UID=0", "AGW_TEST_SV=", "AGW_TEST_AUTHD=/x/authd",
		"AGW_TEST_SHA256SUM=/x/sha256sum", "AGW_TEST_PROCFS=/x/proc", "AGW_TEST_RUN_ROOT=/x/run"} {
		if out, err := run(e, "PI_IP=10.0.0.9"); exitCode(err) != 1 ||
			strings.TrimSpace(out) != "install-car: AGW_TEST_* is set: test hooks only, refusing the real install" {
			t.Fatalf("%s: %s %v", e, out, err)
		}
	}
	if out, err := run("PI_IP=10.0.0.9"); exitCode(err) != 1 || strings.TrimSpace(out) != "install-car: run as root" {
		t.Fatalf("no override: the run must go on to the root check: %s %v", out, err)
	}
	// The old names are no test overrides any more: /proc is fixed, and only a
	// test hook reads AGW_TEST_PROCFS (test_layout_paths), which the guard
	// refuses on the real path. /run is no longer used at all (review D2).
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "${PROCFS:-") || strings.Contains(s, "RUN_ROOT=") || !strings.Contains(s, "\nPROCFS=/proc ") {
		t.Fatal("PROCFS must be /proc on the real path, never taken from the environment, and RUN_ROOT is gone")
	}
	i := strings.Index(s, "\ntest_layout_paths() {\n")
	if i < 0 {
		t.Fatal("no test_layout_paths")
	}
	layout := s[i : i+strings.Index(s[i:], "\n}\n")]
	for _, v := range []string{"${AGW_TEST_PROCFS"} {
		if strings.Count(s, v) != 1 || !strings.Contains(layout, v) {
			t.Errorf("%s must be read only in test_layout_paths", v)
		}
	}
}

// The plan's Summon lines say what this run will really do (deferred minor a):
// a Summon skipped this boot is announced as skipped, never as serving or as
// a removal; and the SUMMON_OK comment says what SUMMON_OK means now.
func TestSummonPlanReflectsTheSkip(t *testing.T) {
	f := newFakes(t)
	bin := f.path("summonhelper")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))
	plan := func(env []string, args ...string) string {
		t.Helper()
		out, _ := f.hook("AGW_SUMMON_PREFLIGHT_ONLY", "", env, args...)
		return out
	}
	noGdbus := "AGW_TEST_GDBUS=/nonexistent/gdbus"
	for _, c := range []struct {
		name, want, not string
		helper          bool
		env, args       []string
	}{
		{"kept", "SUMMON=1 (kept", "SKIPPED", true, nil, []string{"--after-boot"}},
		{"kept, gdbus missing", "SUMMON=1: SKIPPED for this run", "SERVES SUMMON", true, []string{noGdbus}, []string{"--after-boot"}},
		{"SUMMON=1, no helper", "SUMMON=1: SKIPPED for this run", "SERVES SUMMON", false, []string{"SUMMON=1"}, []string{"--after-boot"}},
		{"install", "SUMMON=1: install summonhelper", "SKIPPED", false, []string{"SUMMON=1"}, []string{"./agw", "./camhelper", bin}},
		{"removal", "SUMMON=0: REMOVE the summonhelper service", "SKIPPED", true, []string{"SUMMON=0"}, []string{"--after-boot"}},
		{"removal, gdbus missing", "SUMMON=0: the removal is SKIPPED for this run", "REMOVE the summonhelper service", true,
			[]string{"SUMMON=0", noGdbus}, []string{"--after-boot"}},
		{"nothing", "SUMMON=0: no Summon", "SKIPPED", false, nil, nil},
	} {
		os.RemoveAll(f.path("gw"))
		if c.helper {
			f.installHelper()
		}
		if out := plan(c.env, c.args...); !strings.Contains(out, c.want) || strings.Contains(out, c.not) {
			t.Errorf("%s: want %q and not %q in the plan: %s", c.name, c.want, c.not, out)
		}
	}
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "SUMMON_OK=1") && (strings.Contains(l, "after a boot whose stop sequence failed") ||
			!strings.Contains(l, "skipped or failed")) {
			t.Errorf("the SUMMON_OK comment is stale: %s", l)
		}
	}
}

// ---- the authd pin (the camera session's ruling, the follow-up's ADDITION) ----

// authdPin runs the AGW_AUTHD_PIN_ONLY hook: the plan's sign-in line, the
// check against this car's authd (a full install's ask comes before its first
// write), then both helpers' run lines and the end note.
func authdPin(t *testing.T, f *fakes, stdin string, env []string, args ...string) (string, error) {
	t.Helper()
	return f.hook("AGW_AUTHD_PIN_ONLY", stdin, env, args...)
}

// installCamhelperRun writes camhelper's installed run script, with a pin or none.
func installCamhelperRun(t *testing.T, f *fakes, pin string) {
	t.Helper()
	line := "exec /var/lib/airgapp-camhelper/camhelper run -state /run/airgapp-camhelper-state -writer pw-cat"
	if pin != "" {
		line += " -authd-sha256 " + pin
	}
	must(t, os.WriteFile(f.path("camhelper-run"), []byte("#!/bin/sh\nexec 2>&1\n"+line+"\n"), 0o755))
}

// runLines are the two helpers' exec lines the hook prints (the first may
// follow an ask's prompt on the same line).
func runLines(out string) (camhelper, summonhelper string) {
	for _, l := range strings.Split(out, "\n") {
		if _, v, ok := strings.Cut(l, "camhelper run: "); ok && !strings.HasPrefix(l, "summonhelper run: ") {
			camhelper = v
		}
		if v, ok := strings.CutPrefix(l, "summonhelper run: "); ok {
			summonhelper = v
		}
	}
	return camhelper, summonhelper
}

// AUTHD_SHA256 unset inherits the installed camhelper run script's pin on a
// full install as after a boot; an explicit AUTHD_SHA256 wins; no installed
// pin means the compiled-in one (no -authd-sha256 at all). One resolve serves
// both helpers.
func TestAuthdPinInheritance(t *testing.T) {
	f := newFakes(t)
	must(t, os.WriteFile(f.path("authd"), []byte("authd"), 0o755))
	installed, given := strings.Repeat("cd", 32), strings.Repeat("ab", 32)
	for _, args := range [][]string{nil, {"--after-boot"}} {
		installCamhelperRun(t, f, installed)
		out, err := authdPin(t, f, "", []string{"FAKE_AUTHD_HASH=" + installed}, args...)
		c, h := runLines(out)
		if err != nil || !strings.HasSuffix(c, " -authd-sha256 "+installed) || !strings.HasSuffix(h, " run -authd-sha256 "+installed) {
			t.Fatalf("%v, inherited: %q %v", args, out, err)
		}
		out, err = authdPin(t, f, "", []string{"AUTHD_SHA256=" + given, "FAKE_AUTHD_HASH=" + given}, args...)
		c, h = runLines(out)
		if err != nil || !strings.HasSuffix(c, " -authd-sha256 "+given) || !strings.HasSuffix(h, " run -authd-sha256 "+given) {
			t.Fatalf("%v, an explicit AUTHD_SHA256 wins: %q %v", args, out, err)
		}
		installCamhelperRun(t, f, "")
		out, err = authdPin(t, f, "", []string{"FAKE_AUTHD_HASH=" + authority.DefaultAuthdSHA256}, args...)
		c, h = runLines(out)
		if err != nil || c == "" || h == "" || strings.Contains(c+h, "authd-sha256") {
			t.Fatalf("%v, no installed pin: compiled-in: %q %v", args, out, err)
		}
	}
}

// The plan line names the effective pin's first 8 hex digits and its source.
func TestAuthdPinPlanLine(t *testing.T) {
	f := newFakes(t)
	must(t, os.WriteFile(f.path("authd"), []byte("authd"), 0o755))
	given, installed := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	installCamhelperRun(t, f, installed)
	for _, c := range []struct {
		env  []string
		want string
	}{
		{[]string{"AUTHD_SHA256=" + given, "FAKE_AUTHD_HASH=" + given}, "authd pin abababab (given)\n"},
		{[]string{"FAKE_AUTHD_HASH=" + installed}, "authd pin cdcdcdcd (kept from the installed run script)\n"},
	} {
		if out, err := authdPin(t, f, "", c.env); err != nil || !strings.Contains(out, c.want) {
			t.Fatalf("want %q: %s %v", c.want, out, err)
		}
	}
	installCamhelperRun(t, f, "")
	if out, err := authdPin(t, f, "", []string{"FAKE_AUTHD_HASH=" + authority.DefaultAuthdSHA256}); err != nil ||
		!strings.Contains(out, "authd pin "+authority.DefaultAuthdSHA256[:8]+" (compiled-in)\n") {
		t.Fatalf("compiled-in: %s %v", out, err)
	}
}

// On a full install, a pin that does not match this car's authd, or an authd
// that cannot be hashed, asks before the first write: yes goes on, anything
// else stops with nothing changed (nothing generated). A match asks nothing.
func TestAuthdPinMismatchAsksOnAFullInstall(t *testing.T) {
	f := newFakes(t)
	pin, car := strings.Repeat("ab", 32), strings.Repeat("ef", 32)
	installCamhelperRun(t, f, pin)
	must(t, os.WriteFile(f.path("authd"), []byte("authd"), 0o755))
	for _, c := range []struct {
		name, what string
		env        []string
	}{
		{"mismatch", "does not match this car's " + f.path("authd") + " efefefef", []string{"FAKE_AUTHD_HASH=" + car}},
		{"unknown hash", "cannot hash " + f.path("authd"), []string{"FAKE_AUTHD_HASH="}},
		{"no authd", "cannot hash " + f.path("none"), []string{"FAKE_AUTHD_HASH=" + car, "AGW_TEST_AUTHD=" + f.path("none")}},
	} {
		ask := "camhelper's authd pin abababab (kept from the installed run script) " + c.what +
			": camera sign-in will fail closed until you re-run with a verified AUTHD_SHA256 (runbook step 2b; after an OTA, " +
			"its section \"After an OTA (gateway already installed)\"). Install anyway?"
		out, err := authdPin(t, f, "yes\n", c.env)
		if cl, hl := runLines(out); err != nil || !strings.Contains(out, ask) || cl == "" || hl == "" {
			t.Fatalf("%s, yes: %s %v", c.name, out, err)
		}
		for _, stdin := range []string{"no\n", ""} {
			out, err = authdPin(t, f, stdin, c.env)
			if cl, hl := runLines(out); exitCode(err) != 1 || !strings.Contains(out, ask) || cl != "" || hl != "" {
				t.Fatalf("%s, %q: must stop before any write: %s %v", c.name, stdin, out, err)
			}
		}
	}
	if out, err := authdPin(t, f, "", []string{"FAKE_AUTHD_HASH=" + pin}); err != nil || strings.Contains(out, "Install anyway?") {
		t.Fatalf("a match asks nothing: %s %v", out, err)
	}
}

// After a boot, a mismatch or an unknown hash is a loud note at the end that
// never blocks anything: no question, and the run scripts are generated as
// usual. The run's exit status is non-zero for it (the camera session's
// request: a stale pin means camera sign-in is dead, and --after-boot is run by
// hand). The note names the runbook's step 2b and, for an OTA, its "After an
// OTA" section. A pin that matches exits 0.
func TestAuthdPinAfterBootNote(t *testing.T) {
	f := newFakes(t)
	pin, car := strings.Repeat("ab", 32), strings.Repeat("ef", 32)
	installCamhelperRun(t, f, pin)
	must(t, os.WriteFile(f.path("authd"), []byte("authd"), 0o755))
	for _, c := range []struct{ name, what, hash string }{
		{"mismatch", "does not match this car's " + f.path("authd") + " efefefef", car},
		{"unknown hash", "cannot hash " + f.path("authd"), ""},
	} {
		out, err := authdPin(t, f, "", []string{"FAKE_AUTHD_HASH=" + c.hash}, "--after-boot")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		cl, hl := runLines(out)
		note := "camhelper's authd pin abababab (kept from the installed run script) " + c.what +
			": camera sign-in will fail closed until you re-run with a verified AUTHD_SHA256 (runbook step 2b; after an OTA, " +
			"its section \"After an OTA (gateway already installed)\")"
		if exitCode(err) != 1 || strings.Contains(out, "Install anyway?") || cl == "" || hl == "" ||
			!strings.Contains(out, "\n!!! NOTE: "+note+"\n") ||
			lines[len(lines)-1] != "install-car: the authd pin check failed (the NOTE above); every service was started" {
			t.Fatalf("%s after a boot: %s %v", c.name, out, err)
		}
	}
	if out, err := authdPin(t, f, "", []string{"FAKE_AUTHD_HASH=" + pin}, "--after-boot"); err != nil ||
		strings.Contains(out, "NOTE") {
		t.Fatalf("a matching pin after a boot: %s %v", out, err)
	}
}

// The car's authd hash is never adopted as the pin: with a mismatch, neither
// run script ever carries the car's hash, whatever the pin's source.
func TestAuthdPinNeverAdoptsTheCarsHash(t *testing.T) {
	f := newFakes(t)
	car := strings.Repeat("ef", 32)
	must(t, os.WriteFile(f.path("authd"), []byte("authd"), 0o755))
	for _, c := range []struct {
		installed string
		env, args []string
	}{
		{strings.Repeat("ab", 32), nil, nil},
		{strings.Repeat("ab", 32), nil, []string{"--after-boot"}},
		{"", nil, nil},
		{"", nil, []string{"--after-boot"}},
		{strings.Repeat("ab", 32), []string{"AUTHD_SHA256=" + strings.Repeat("cd", 32)}, nil},
	} {
		installCamhelperRun(t, f, c.installed)
		out, err := authdPin(t, f, "yes\n", append([]string{"FAKE_AUTHD_HASH=" + car}, c.env...), c.args...)
		cl, hl := runLines(out)
		want := 0 // a full install asked, and got yes
		if len(c.args) > 0 {
			want = 1 // after a boot, the mismatch's note exits non-zero
		}
		if exitCode(err) != want || cl == "" || hl == "" || strings.Contains(cl+hl, car) {
			t.Fatalf("%v %v: the car's hash must never become the pin: %s %v", c.env, c.args, out, err)
		}
	}
}

// The script's compiled-in pin is the helpers' own: the check compares
// against what camhelper and summonhelper really fall back to.
func TestAuthdCompiledPinMatchesAuthority(t *testing.T) {
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^AUTHD_COMPILED_PIN=([0-9a-f]{64})\b`).FindAllStringSubmatch(string(b), -1)
	if len(m) != 1 || m[0][1] != authority.DefaultAuthdSHA256 {
		t.Fatalf("AUTHD_COMPILED_PIN must be defined once and equal authority.DefaultAuthdSHA256 (%s): %v",
			authority.DefaultAuthdSHA256, m)
	}
}

// The script's own text matches what it does now (round 2): no comment still
// says a check failure stops a full install, or that only --after-boot keeps
// the installed authd pin.
func TestSummonScriptTextIsCurrent(t *testing.T) {
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Comments read as prose: their leading "#" dropped, whitespace joined.
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		lines = append(lines, strings.TrimPrefix(strings.TrimSpace(l), "#"))
	}
	s := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	for _, gone := range []string{
		"or on --after-boot the pin the installed camhelper run script names",
		"a failure stops a full install, and after a boot it is reported at the end",
	} {
		if strings.Contains(s, gone) {
			t.Errorf("install-car.sh still says: %s", gone)
		}
	}
}

// The README names summonhelper where the owner follows it by hand (m3), gives
// the name waits' real bound (m4), gives the helper's pin as the owner's own
// route, which keeps the VIN off argv (m5) and out of the recorded SSH session
// (SEC-2), and no longer promises that a later SUMMON=0 finishes a removal that
// cannot finish (I1). It says what the installer does now (the final fix
// wave): an unsupervised summonhelper skips Summon (DOC-1), the hand-back after
// a skip, the failed checks' report, the after-boot pin note's exit status,
// probe-summon's 500 ms, steps 0, 4b and 4c asked before step 1, the services
// with SUMMON=1, and the camera runbook's step 2b instead of the old Phase 1a.
func TestSummonReadme(t *testing.T) {
	b, err := os.ReadFile("../../deploy/README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(strings.Fields(string(b)), " ")
	for _, want := range []string{
		"an unsupervised `agw` or `camhelper` still running blocks the install (one of each, never two); an unsupervised " +
			"`summonhelper` skips Summon for the run instead (see \"Failures\").",
		"`sv down` the services (agw, camhelper, and summonhelper with `SUMMON=1`), then `umount",
		"`timeout -k 1 5`", "every 1 s, up to 10 times: up to about a minute",
		"waits for stock to register (10 checks, 1 s apart: up to about a minute)",
		"`P=$(mktemp) && chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper pin -vin-file /var/etc/vin > \"$P\" && " +
			"wc -c < \"$P\" && socat -u FILE:\"$P\" \"TCP:$LAPTOP:$PORT\"; rm -f \"$P\"`",
		"The pin holds the VIN and the car records SSH sessions, so the route never prints it",
		"The installer itself prints no pin and no VIN.",
		"`timeout -k 1 5 true; echo $?` must print `0`",
		"From step 1's `mkdir` on, a Summon failure or a declined Summon question never stops the install halfway",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("README.md must say: %s", want)
		}
	}
	for _, want := range []string{
		"a failing 4b or 4c command (the copy, the state directory, the keygen)",
		"an unsupervised `summonhelper` found before S3",
		"A failed check after S4 is different, on purpose: the helper stays linked and running",
		"the questions of steps 0, 4b and 4c",
		"the run hands `com.tesla.Webrtc` back to stock for this boot after S3, as the `SUMMON=0` removal does",
		"a name that has an owner starts nothing",
		"`Summon checks failed: …`",
		"A failed check is reported at the end too, but the helper stays linked and running",
		"the run then exits non-zero, though every service was started",
		"its OWNED record within 500 ms, exactly as agw's Open waits",
		"asks steps 0, 4b and 4c before step 1",
		"(and `airgapp-summonhelper` with `SUMMON=1`) linked there",
		"the read-only sign-in check is the camera runbook's step 2b",
		"its \"After an OTA (gateway already installed)\" section",
		// the final follow-up: agw's pin route in DONE, the exit path, the after-boot note's last line
		"agw's own pin goes the same way: the DONE text's step 3 gives its route, as uid 4950",
		"`P=$(mktemp) && chpst -u :4950:4950 /var/lib/airgapp-gw/agw pin -key /var/lib/airgapp-gw/state/key.pem > \"$P\" && " +
			"wc -c < \"$P\" && socat -u FILE:\"$P\" \"TCP:$LAPTOP:$PORT\"; rm -f \"$P\"`",
		"The same holds on the way out: any exit after S0b's stop sequence",
		// round 2: the exit path's decision, and SIGHUP ignored
		"our helper is linked (its `airgapp-summonhelper` link, which S3 makes) and supervised by a running runsv (its runsv " +
			"is up and has not been told to stop it or keep it down): it is the installed owner, so nothing is touched",
		"or its link has no running runsv (after a boot until S3 starts our runsvdir, since the link stays on /var/lib " +
			"from one boot to the next)",
		"checking every 1 s up to 10 times, once no `summonhelper` process runs and the name is free, stock is restarted and checked",
		"If a `summonhelper` process remains, a line names its pid; if the name stays owned (stock still letting go of it, " +
			"say), a line says so; either way stock is not restarted.",
		// round 2c: a runsv told to stop the helper, SIGPIPE ignored, and what a dead session ends
		"or its runsv was told to stop it or keep it down (S0b's `sv down`, in a run whose runsvdir was already up; the " +
			"helper may still be stopping)",
		"Neither a dropped SSH session nor a dead reader of the output can kill the run or its exit path: the run ignores " +
			"SIGHUP and SIGPIPE, and every command it starts inherits that.",
		"the run ends at its next question (\"no answer\"; S0b's question takes it as a no, a skip, and goes on) or at its " +
			"next write there that `set -e` checks (an I/O error on a terminal, a broken pipe otherwise), and the exit path runs.",
		"A failed write that `set -e` does not check (in a condition, or before `||` or `&&`) ends nothing: the next " +
			"question, checked write or the run's end does.",
		"Not covered: SIGKILL and the other signals the run does not trap, which end it without the exit path.",
		"Its last line is `install-car: the authd pin check failed (the NOTE above); every service was started`, or the " +
			"Summon report instead when there is one.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("README.md must say: %s", want)
		}
	}
	for _, gone := range []string{"pin -vin <VIN>", "next `SUMMON=0` run finishes", "`timeout 5`", "both services",
		"A full install still stops on a \"no\"", "a full install stops before anything for Summon is linked",
		"and a failed check.", "unset, the compiled-in pin applies", "A failure stops the install with the probe's last output",
		"`summonhelper` still running blocks the install", "Run it and add the pin to the phone the same way",
		"`chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper pin -vin-file /var/etc/vin`",
		"A failed check is reported the same way", "asks once more (step 0)", "/tmp/agw probe-auth -pub",
		"end that never blocks anything. Either way", "If a `summonhelper` process runs, it is left alone",
		"a signal (`Ctrl-C`, a dropped SSH session)", "an owned name starts nothing", "nothing is touched, whether it runs or not",
		"has not been told to keep it down", "its runsv was told to keep it down", "and so does every command it starts",
		"or its next failed write"} {
		if strings.Contains(s, gone) {
			t.Errorf("README.md must no longer say: %s", gone)
		}
	}
}

// gdbus and timeout are needed only with SUMMON=1 or to remove a helper: a
// SUMMON=0 car without one never needs them.
func TestSummonPreflightOnlyWithSummon(t *testing.T) {
	f := newFakes(t)
	noGdbus, noTimeout := "AGW_TEST_GDBUS=/nonexistent/gdbus", "AGW_TEST_TIMEOUT=/nonexistent/timeout"
	bin := f.path("summonhelper") // an explicit SUMMON=1 installs, so it needs one
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))
	pre := func(env ...string) (string, error) {
		t.Helper()
		return f.hook("AGW_SUMMON_PREFLIGHT_ONLY", "", env, "./agw", "./camhelper", bin)
	}
	if out, err := pre("SUMMON=0", noGdbus, noTimeout); err != nil {
		t.Fatalf("SUMMON=0, no helper: %s %v", out, err)
	}
	if out, err := pre("SUMMON=", noGdbus, noTimeout); err != nil {
		t.Fatalf("SUMMON unset, no helper: %s %v", out, err)
	}
	if out, err := pre("SUMMON=1", noGdbus); exitCode(err) != 1 || !strings.Contains(out, "gdbus not found") {
		t.Fatalf("SUMMON=1 without gdbus: %s %v", out, err)
	}
	if out, err := pre("SUMMON=1", noTimeout); exitCode(err) != 1 || !strings.Contains(out, "timeout not found") {
		t.Fatalf("SUMMON=1 without timeout: %s %v", out, err)
	}
	if out, err := pre("SUMMON=1"); err != nil {
		t.Fatalf("SUMMON=1 with both: %s %v", out, err)
	}
	if out, err := pre("SUMMON=yes"); exitCode(err) != 1 || !strings.Contains(out, "SUMMON must be 0 or 1") {
		t.Fatalf("SUMMON=yes: %s %v", out, err)
	}
	// Removing a helper needs them too.
	f.installHelper()
	if out, err := pre("SUMMON=0", noGdbus); exitCode(err) != 1 || !strings.Contains(out, "gdbus not found") {
		t.Fatalf("SUMMON=0 with a helper to remove, without gdbus: %s %v", out, err)
	}
}

// The checks retry each probe every 1 s, up to 15 times, deciding on its exit
// status only: a stale socket left on disk proves nothing, and the helper's
// first D2 stamp may still be due. The installer itself prints no pin and no
// VIN (m5): the DONE text gives the owner the helper's pin route, which never
// prints the pin (SEC-2): a fresh mktemp file, sent with socat, then removed.
// A failed check leaves the helper linked and running, so the report says the
// checks failed, never that Summon is not running (OPS-5), and nothing is
// handed back to stock.
func TestSummonChecksRetry(t *testing.T) {
	f := newFakes(t)
	gw := f.path("gw")
	must(t, os.MkdirAll(f.path("run", "airgapp-summon"), 0o755))
	sock := f.path("run", "airgapp-summon", "relay.sock")
	must(t, os.WriteFile(sock, nil, 0o644)) // stale
	must(t, os.WriteFile(f.path("vin"), []byte(" 5YJ3E1EA0JF000001 \n"), 0o644))
	links := f.installHelper() // SUMMON unset keeps it: the checks run, the helper up (FAKE_SV_STATUS=0)
	helperUp := "sv status " + f.helperSvc()
	pinRoute := "  P=$(mktemp) && chpst -u :1337:1337 " + gw + "/summonhelper pin -vin-file " + f.path("vin") +
		` > "$P" && wc -c < "$P" && socat -u FILE:"$P" "TCP:$LAPTOP:$PORT"; rm -f "$P"` + "\n"
	probes := func(summon, route []string) {
		t.Helper()
		for name, a := range map[string][]string{"probe-summon": summon, "probe-route": route} {
			os.Remove(f.path(name + ".n"))
			if err := os.WriteFile(f.path(name), []byte(strings.Join(a, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	checks := func(args ...string) ([]string, string, error) {
		t.Helper()
		f.calls()
		out, err := f.hook("AGW_SUMMON_CHECKS_ONLY", "", []string{"FAKE_SV_STATUS=0"}, args...)
		return f.calls(), out, err
	}
	summonProbe := "chpst -u :4950:4950 " + gw + "/agw probe-summon"
	routeProbe := "chpst -u :4950:4950 " + gw + "/agw probe-route"

	probes([]string{"fail", "fail", "ok"}, []string{"ok"})
	calls, out, err := checks()
	want := []string{helperUp, summonProbe, "sleep 1", summonProbe, "sleep 1", summonProbe, routeProbe}
	if err != nil || strings.Join(calls, "\n") != strings.Join(want, "\n") ||
		!strings.Contains(out, "summon relay: ok") || !strings.Contains(out, "route gate: ok") ||
		strings.Contains(out, "airgapp-gw:1:") || strings.Contains(out, "5YJ3E1EA0JF000001") ||
		!strings.Contains(out, pinRoute) || strings.Contains(out, "pin is not shown") ||
		!strings.Contains(out, "never print it here") || !strings.Contains(out, `docs/SUMMON.md, "The pins"`) {
		t.Fatalf("two failures, then ok (the DONE text gives the pin route):\n got %q\nwant %q\n%s %v", calls, want, out, err)
	}
	// No DONE line ends with the bare pin command, which would print the pin
	// into the recorded SSH session.
	for _, l := range strings.Split(out, "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), "pin -vin-file "+f.path("vin")) {
			t.Fatalf("the DONE text gives the bare pin command, which prints the pin: %q", l)
		}
	}
	// The owner's own pin route, from the real VIN file, never the VIN on argv.
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `printf '  P=$(mktemp) && chpst -u :%s:%s %s/summonhelper pin -vin-file %s > "$P" && wc -c < "$P" && socat -u FILE:"$P" "TCP:$LAPTOP:$PORT"; rm -f "$P"\n'`) ||
		strings.Contains(string(b), "pin -vin \"") || strings.Contains(string(b), "SUMMON_PIN") {
		t.Fatal("the DONE text must give the helper's mktemp pin route with -vin-file, and the installer must not run pin itself")
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the checks must not touch the socket: %v", err)
	}

	// 15 failures are reported at the end, non-zero, with the last output
	// verbatim. The helper stays linked and running on purpose (it is
	// harmless, and the owner may look at it): the DONE text shows no pin
	// command and points to the report instead.
	probes(repeat("fail", 15), []string{"ok"})
	calls, out, err = checks()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if exitCode(err) != 1 || count(calls, summonProbe) != 15 || count(calls, "sleep 1") != 14 || index(calls, routeProbe) >= 0 ||
		!strings.Contains(out, "summon relay: refused: a fake reason 15") ||
		strings.Contains(out, "pin -vin-file") || !strings.Contains(out, "summonhelper's pin is not shown: its checks failed") ||
		!strings.HasPrefix(lines[len(lines)-1], "install-car: Summon checks failed: the summon relay check failed 15 times") ||
		!strings.Contains(lines[len(lines)-1], "summonhelper stays linked and running") ||
		strings.Contains(out, "Summon is not running") {
		t.Fatalf("15 failures: %q %s %v", calls, out, err)
	}
	// The helper keeps the name: nothing is handed back to stock.
	if index(calls, "gdbus") >= 0 || index(calls, "sv up /etc/sv/webrtc-comms") >= 0 || index(calls, "sv -w 15 down /etc/sv/webrtc-comms") >= 0 {
		t.Fatalf("a failed check handed the name back: %q", calls)
	}
	if _, err := os.Lstat(links[0]); err != nil {
		t.Fatalf("a failed check leaves the helper linked: %v", err)
	}
	probes([]string{"ok"}, repeat("fail", 15))
	calls, out, err = checks()
	if exitCode(err) != 1 || count(calls, routeProbe) != 15 || index(calls, "chpst -u :1337:1337") >= 0 || strings.Contains(out, "pin -vin-file") ||
		!strings.Contains(out, "route gate: refused") || !strings.Contains(out, "agw probe-route: a fake reason 15") {
		t.Fatalf("probe-route, 15 failures: %q %s %v", calls, out, err)
	}
	// After a boot, the same failure is the report at the end, non-zero.
	probes(repeat("fail", 15), []string{"ok"})
	_, out, err = checks("--after-boot")
	lines = strings.Split(strings.TrimSpace(out), "\n")
	if last := lines[len(lines)-1]; exitCode(err) != 1 || !strings.HasPrefix(last, "install-car: Summon checks failed: ") ||
		!strings.Contains(last, "summon relay: refused: a fake reason 15") || strings.Contains(out, "Summon is not running") {
		t.Fatalf("after-boot, 15 failures: %s %v", out, err)
	}
}

// Every new car-state change asks first, after a boot too; without an answer
// nothing runs.
func TestSummonAsks(t *testing.T) {
	f := newFakes(t)
	installedRun := f.path("installed-summon-run")
	must(t, os.WriteFile(installedRun, []byte("#!/bin/sh\n"), 0o755))
	must(t, os.WriteFile(f.path("webrtc-comms.pid"), []byte("4242\n"), 0o644))
	must(t, os.MkdirAll(f.path("proc", "4242"), 0o755))
	must(t, os.WriteFile(f.path("proc", "4242", "comm"), []byte("minijail-init\n"), 0o644))
	links := f.installHelper()
	// Anything but a typed yes at S0b's stop sequence or at the SUMMON=0
	// removal's ask skips Summon for the run instead of stopping it: after a
	// boot (m2, 1b), and on a full install too, which has written by S0b (F1).
	// Nothing of the stop sequence or the removal runs; the helper is unlinked
	// (neither linked nor started) and its directory stays as it is; the
	// camera goes on to S3; the reason is reported at the end, non-zero.
	for _, c := range []struct {
		name, hook, report string
		env, args          []string
	}{
		{"stop", "AGW_STOP_WEBRTC_ONLY", "Summon is not running: skipped by this install: the S0b stop sequence was not confirmed",
			[]string{"SUMMON=1"}, nil},
		{"stop after boot", "AGW_STOP_WEBRTC_ONLY", "Summon is not running: skipped this boot: the S0b stop sequence was not confirmed",
			[]string{"AGW_TEST_SUMMON_RUN=" + installedRun}, []string{"--after-boot"}},
		{"removal", "AGW_SUMMON_REMOVE_ONLY", "SUMMON=0 removal skipped by this install: the removal was declined",
			[]string{"SUMMON=0"}, nil},
		{"removal after boot", "AGW_SUMMON_REMOVE_ONLY", "SUMMON=0 removal skipped this boot: the removal was declined",
			[]string{"SUMMON=0"}, []string{"--after-boot"}},
	} {
		for _, stdin := range []string{"", "no\n"} {
			f.answers("(false,)")
			f.calls()
			out, err := f.hook(c.hook, stdin, c.env, c.args...)
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if calls := f.calls(); exitCode(err) != 1 || len(calls) != 0 || !strings.Contains(out, "S3 services: agw camhelper\n") ||
				!strings.HasPrefix(lines[len(lines)-1], "install-car: "+c.report) {
				t.Fatalf("%s, answered %q: %q %s %v", c.name, stdin, calls, out, err)
			}
			if _, err := os.Lstat(links[0]); !os.IsNotExist(err) {
				t.Fatalf("%s, answered %q: the helper must be unlinked: %v", c.name, stdin, err)
			}
			for _, p := range []string{f.path("webrtc-comms.pid"), filepath.Join(f.helperSvc(), "run")} {
				if _, err := os.Lstat(p); err != nil {
					t.Fatalf("%s, answered %q: %s must be untouched: %v", c.name, stdin, p, err)
				}
			}
			must(t, os.Symlink(f.helperSvc(), links[0]))
		}
	}
	// The full install's copy and keygen are asked about together with the
	// other questions before the first write (summon_install_asks), and their
	// commands run later without a question (summon_install_files):
	// TestFullInstall4b4cAskedBeforeTheFirstWrite runs both.
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Both name waits are announced with their real bound: 10 checks, each up
	// to 6 s (timeout -k 1 5), 1 s apart (m4).
	for _, ask := range []string{"\"S0b. stop stock webrtc-comms", "\"S0b. SUMMON=0: remove the summonhelper service"} {
		i := strings.Index(string(b), ask)
		if i < 0 {
			t.Fatalf("no ask %s", ask)
		}
		if line := string(b)[i : i+strings.Index(string(b)[i:], "\n")]; !strings.Contains(line, "up to about a minute") {
			t.Errorf("the ask %s must say the name wait takes up to about a minute: %s", ask, line)
		}
	}
	body := func(name string) string {
		i := strings.Index(string(b), "\n"+name+"() {\n")
		if i < 0 {
			t.Fatalf("no function %s", name)
		}
		return string(b)[i : i+strings.Index(string(b)[i:], "\n}\n")]
	}
	if a := body("summon_install_asks"); !strings.Contains(a, "\task \"4b. ") || !strings.Contains(a, "\t\task \"4c. ") {
		t.Errorf("summon_install_asks must ask 4b and 4c: %s", a)
	}
	if f := body("summon_install_files"); strings.Contains(f, "ask ") || strings.Contains(f, "confirm ") {
		t.Errorf("summon_install_files must not ask: its questions come before the first write: %s", f)
	}
}

// Steps 4b and 4c's questions come before a full install's first write, with
// step 0's (F1, round 2): a "no" stops it with nothing written. Their commands
// run after step 4 (here, after the hook's model of the first write), each
// one checked; TestFullInstallSummonFailuresNeverHalfApply covers a failing cp
// or keygen.
func TestFullInstall4b4cAskedBeforeTheFirstWrite(t *testing.T) {
	f := newFakes(t)
	src := f.path("summonhelper-src")
	must(t, os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755))
	must(t, os.MkdirAll(f.path("gw"), 0o755)) // step 1 made it
	bin, state := f.path("gw", "summonhelper"), f.path("summon-home", "state")
	files := func(stdin string) (string, []string, error) {
		t.Helper()
		f.calls()
		out, err := f.hook("AGW_SUMMON_FILES_ONLY", stdin, []string{"SUMMON=1"}, "./agw", "./camhelper", src)
		return out, f.calls(), err
	}
	for _, stdin := range []string{"no\n", "yes\nno\n", "", "yes\n"} {
		out, calls, err := files(stdin)
		_, berr := os.Stat(bin)
		_, serr := os.Stat(f.path("summon-home"))
		if exitCode(err) != 1 || strings.Contains(out, "S3 services") || len(calls) != 0 || !os.IsNotExist(berr) ||
			!os.IsNotExist(serr) || !(strings.Contains(out, "not confirmed") || strings.Contains(out, "no answer")) {
			t.Fatalf("answered %q: must stop with nothing written: %q %s %v", stdin, calls, out, err)
		}
	}
	// Yes to both: the binary (0755), the state directory (0700), the key made
	// as the helper's uid, each step announced once done.
	out, calls, err := files("yes\nyes\n")
	bi, berr := os.Stat(bin)
	si, serr := os.Stat(state)
	if err != nil || !strings.Contains(out, "S3 services: agw camhelper summonhelper\n") || berr != nil || bi.Mode().Perm() != 0o755 ||
		serr != nil || si.Mode().Perm() != 0o700 ||
		index(calls, "chpst -u :1337:1337 "+bin+" keygen -key "+filepath.Join(state, "key.pem")) < 0 ||
		!strings.Contains(out, "4b. installed "+bin+" (root:root 0755) and "+state+" (1337:1337 0700)\n") ||
		!strings.Contains(out, "\n4c. generated the helper key "+filepath.Join(state, "key.pem")+" as uid 1337\n") {
		t.Fatalf("yes, yes: %q %s %v", calls, out, err)
	}
	// A key already there: no 4c question (one yes is enough), and no keygen.
	out, calls, err = files("yes\n")
	if err != nil || index(calls, "chpst") >= 0 || !strings.Contains(out, "the helper key already exists") ||
		!strings.Contains(out, "4b. installed ") || strings.Contains(out, "4c. generated") {
		t.Fatalf("a key already there: %q %s %v", calls, out, err)
	}
}

// The main flow's order (the after-boot binary check it replaces is gone with
// the old SUMMON path): every test hook before the root check; S0's exec
// mounts, then the Summon stage (stop or removal), before S2 writes a service
// script; S3 links; the helper goes up; the wait loop; S4; the checks after
// the last restart; the DONE text; the Summon report last.
func TestSummonMainFlowOrder(t *testing.T) {
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	root := strings.Index(s, "\n[ \"$(id -u)\" = 0 ] || die \"run as root\"\n")
	guard := strings.Index(s, "\nif env | grep -q '^AGW_TEST_'; then die \"AGW_TEST_* is set: test hooks only, refusing the real install\"; fi\n")
	if guard < 0 || root < 0 || guard > root || strings.Contains(s[guard:root], "\nif [ \"${AGW_") {
		t.Fatalf("the AGW_TEST_* guard must come after every hook and right before the root check")
	}
	for _, hook := range []string{"AGW_SUMMON_SCRIPTS_ONLY", "AGW_STOP_WEBRTC_ONLY", "AGW_SUMMON_REMOVE_ONLY",
		"AGW_SUMMON_PREFLIGHT_ONLY", "AGW_SUMMON_CHECKS_ONLY", "AGW_AUTHD_PIN_ONLY", "AGW_SUMMON_FILES_ONLY", "AGW_EXIT_PATH_ONLY"} {
		if i := strings.Index(s, "\nif [ \"${"+hook+":-}\" = 1 ]; then\n"); i < 0 || root < 0 || i > root || i > guard {
			t.Errorf("the %s hook must come before the guard and the root check", hook)
		}
	}
	inOrder(t, s[root:],
		"\nexit_trap_on\n", // every exit from here, a die included, runs exit_hand_back
		"\nsummon_mode\n",
		"\nsummon_preflight\n",
		"\nresolve_authd_pin\n",
		"\ncheck_authd_pin\n",       // the authd pin check is a pre-write check
		"\n\tsummon_install_asks\n", // 4b and 4c's questions, before the first write
		"\nask \"1. mkdir $DIR",
		"\nWRITTEN=1\nmkdir -p \"$DIR\" \"$STATE\"\n", // a full install's first write (F1)
		"\n\task \"4. generate $STATE/key.pem",
		"\n\tchpst -u \"$RUN_AS\" \"$DIR/agw\" keygen -key \"$STATE/key.pem\"\n",
		"\nif summon_installing; then summon_install_files; fi\n", // 4b and 4c's commands, after step 4
		"\nexec_mount \"$HELPER_DIR\"\n",
		"\nSVDIR=\n",
		"\nsummon_stage\n", // S0b: from its stop sequence on, STOCK_STOPPED=1
		"\nask \"S2. ",
		"\nif summon_on; then summon_unsupervised; fi\n",
		"link_into \"$",
		"\nif summon_on; then helper_up; fi\n",
		"\nwhile [ $_i -lt 15 ] && ! all_supervised; do",
		"\nif summon_on; then summon_up_check; fi\n",
		"\nstock_hand_back\n", // a skip after S0b's stop hands the name back to stock
		"\n\task \"S4. ",
		"\nif summon_on; then summon_checks; fi\n",
		"\ncat <<DONE\n",
		"\nsummon_done\n",
		"\nend_report\n",
	)
	if c := commands(s); c[len(c)-1] != "end_report" {
		t.Fatalf("end_report (the authd note, then the Summon report) must be the last command, got %q", c[len(c)-1])
	}
	end := func(name string) string {
		i := strings.Index(s, "\n"+name+"() {\n")
		if i < 0 {
			t.Fatalf("no function %s", name)
		}
		return s[i : i+strings.Index(s[i:], "\n}\n")]
	}
	inOrder(t, end("end_report"), "!!! NOTE: ", "\n\tsummon_report\n", "\n\t[ -z \"$AUTHD_NOTE\" ] || die ")
}

// ---- the hand-back after a skip (camera observation 4) ----

// stockCalls is the calls that touch stock webrtc-comms: the hand-back's down and up.
func stockCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "sv ") && strings.HasSuffix(c, " /etc/sv/webrtc-comms") {
			out = append(out, c)
		}
	}
	return out
}

// When Summon is skipped after S0b's stop sequence stopped stock webrtc-comms,
// the run hands com.tesla.Webrtc back to stock for this boot, the way the
// SUMMON=0 removal does (stock_hand_back): our helper unlinked, no
// summonhelper process left, then, only if the name is free, stock restarted
// and checked. An owned name starts nothing. A stock that does not register,
// or a summonhelper that stays, is reported at the end; neither stops the run.
func TestSkipAfterStopHandsTheNameBack(t *testing.T) {
	f := newFakes(t)
	// The helper is not running after S3 (FAKE_SV_STATUS=1): summon_up_check skips Summon, and the hand-back runs.
	upCheck := func(answers ...string) ([]string, string, error) {
		t.Helper()
		os.RemoveAll(f.path("gw"))
		os.RemoveAll(f.path("proc"))
		links := f.installHelper() // SUMMON unset keeps it
		f.answers(answers...)
		f.calls()
		out, err := f.hook("AGW_SUMMON_CHECKS_ONLY", "", nil)
		calls := f.calls()
		if _, lerr := os.Lstat(links[0]); !os.IsNotExist(lerr) {
			t.Fatalf("the helper must be unlinked: %v\n%s", lerr, out)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if exitCode(err) != 1 || !strings.Contains(out, "S4 services: agw camhelper\n") ||
			!strings.HasPrefix(lines[len(lines)-1], "install-car: Summon is not running: summonhelper is not running") {
			t.Fatalf("the skip's own report must come first, and the run must go on: %s %v", out, err)
		}
		return calls, out, err
	}

	// The name is free: stock is restarted (down, then up) and registers. The
	// helper's runsv does not run, so it is not brought down.
	calls, out, _ := upCheck("(false,)", "(true,)")
	if index(calls, "sv -w 15 down "+f.helperSvc()) >= 0 {
		t.Fatalf("a helper whose runsv does not run was brought down: %q", calls)
	}
	if got := stockCalls(calls); strings.Join(got, "\n") != "sv -w 15 down /etc/sv/webrtc-comms\nsv up /etc/sv/webrtc-comms" ||
		count(calls, nameHasOwner) != 2 || index(calls, nameHasOwner) > index(calls, "sv -w 15 down /etc/sv/webrtc-comms") ||
		!strings.Contains(out, "stock webrtc-comms owns com.tesla.Webrtc again\n") || strings.Contains(out, "did not register") {
		t.Fatalf("a free name: stock restarted and checked: %q %s", calls, out)
	}
	// The name has an owner: nothing is started.
	calls, out, _ = upCheck("(true,)")
	if got := stockCalls(calls); len(got) != 0 || count(calls, nameHasOwner) != 1 ||
		!strings.Contains(out, "com.tesla.Webrtc already has an owner (no summonhelper process runs): stock webrtc-comms is not restarted\n") {
		t.Fatalf("an owned name: nothing started: %q %s", calls, out)
	}
	// The helper's runsv runs while the helper is down (a start that keeps
	// failing): it is brought down before the wait, so it cannot start the
	// helper again while stock restarts.
	os.RemoveAll(f.path("gw"))
	os.RemoveAll(f.path("proc"))
	f.installHelper()
	f.answers("(false,)", "(true,)")
	f.calls()
	out, err := f.hook("AGW_SUMMON_CHECKS_ONLY", "", []string{"FAKE_SV_STATUS=down"})
	calls = f.calls()
	if exitCode(err) != 1 || index(calls, "sv -w 15 down "+f.helperSvc()) < 0 ||
		index(calls, "sv -w 15 down "+f.helperSvc()) > index(calls, nameHasOwner) ||
		index(calls, nameHasOwner) > index(calls, "sv up /etc/sv/webrtc-comms") ||
		!strings.Contains(out, "stock webrtc-comms owns com.tesla.Webrtc again\n") {
		t.Fatalf("a runsv that runs with the helper down: brought down, then stock restarted: %q %s %v", calls, out, err)
	}
	// Stock does not register: reported at the end, after the skip's own report, and never fatal.
	calls, out, _ = upCheck(append([]string{"(false,)"}, repeat("(false,)", 10)...)...)
	if got := stockCalls(calls); len(got) != 2 || count(calls, nameHasOwner) != 11 ||
		!strings.Contains(out, "; Summon was skipped after S0b stopped stock webrtc-comms, which was restarted, but stock "+
			"webrtc-comms did not register com.tesla.Webrtc after its restart (10 checks") {
		t.Fatalf("stock not registering: reported: %q %s", calls, out)
	}

	// After a boot, an unsupervised summonhelper found before S3 skips Summon
	// after the stop sequence ran. One that exits within the 10 s wait: then
	// the name is free, and stock is restarted.
	os.RemoveAll(f.path("gw"))
	os.RemoveAll(f.path("proc"))
	f.installHelper()
	stray := f.helperProc("4800", "serve")
	f.answers("(false,)", "(false,)", "(true,)") // the stop sequence's check, the hand-back's, stock registered
	f.calls()
	out, err = f.hook("AGW_STOP_WEBRTC_ONLY", "yes\n", []string{"FAKE_SLEEP_RM=" + stray, "FAKE_SLEEP_RM_AFTER=2"}, "--after-boot")
	calls = f.calls()
	if exitCode(err) != 1 || len(stockCalls(calls)) != 3 || stockCalls(calls)[0] != "sv down /etc/sv/webrtc-comms" ||
		count(calls, "sleep 1") != 2 || !strings.Contains(out, "stock webrtc-comms owns com.tesla.Webrtc again\n") ||
		!strings.Contains(out, "S3 services: agw camhelper\n") {
		t.Fatalf("a stray that exits: stock restarted: %q %s %v", calls, out, err)
	}
	// One that stays may hold the name: stock is left alone, and that is
	// reported at the end with the pid, never fatal.
	os.RemoveAll(f.path("gw"))
	os.RemoveAll(f.path("proc"))
	f.installHelper()
	f.helperProc("4800", "serve")
	f.answers("(false,)")
	f.calls()
	out, err = f.hook("AGW_STOP_WEBRTC_ONLY", "yes\n", nil, "--after-boot")
	calls = f.calls()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if exitCode(err) != 1 || len(stockCalls(calls)) != 1 || count(calls, nameHasOwner) != 1 || count(calls, "sleep 1") != 9 ||
		!strings.Contains(out, "S3 services: agw camhelper\n") ||
		!strings.Contains(lines[len(lines)-1], "; stock webrtc-comms stays stopped: a summonhelper process still runs (pid 4800) "+
			"and may hold com.tesla.Webrtc; kill it (kill 4800) or reboot the car") {
		t.Fatalf("a stray that stays: stock left alone, reported: %q %s %v", calls, out, err)
	}
}

// The hand-back runs only after S0b's stop sequence stopped stock: a skip
// before it (a "no" at S0b, a missing tool after a boot, SUMMON=1 with no
// helper after a boot, and a failing 4b/4c command, which comes before S0b)
// never stopped stock, so it starts nothing and asks nothing about the name.
// A failed check after S4 leaves the helper running: TestSummonChecksRetry.
func TestSkipBeforeStopHandsNothingBack(t *testing.T) {
	f := newFakes(t)
	for _, c := range []struct {
		name, stdin string
		env, args   []string
		helper      bool
	}{
		{"a no at S0b", "no\n", []string{"SUMMON=1"}, nil, true},
		{"a no at S0b after a boot", "no\n", nil, []string{"--after-boot"}, true},
		{"gdbus missing after a boot", "yes\n", []string{"AGW_TEST_GDBUS=/nonexistent/gdbus"}, []string{"--after-boot"}, true},
		{"SUMMON=1 without a helper after a boot", "yes\n", []string{"SUMMON=1"}, []string{"--after-boot"}, false},
	} {
		os.RemoveAll(f.path("gw"))
		os.RemoveAll(f.path("proc"))
		if c.helper {
			f.installHelper()
		}
		f.answers("(false,)", "(true,)")
		f.calls()
		out, err := f.hook("AGW_STOP_WEBRTC_ONLY", c.stdin, c.env, c.args...)
		calls := f.calls()
		if exitCode(err) != 1 || len(calls) != 0 || !strings.Contains(out, "S3 services: agw camhelper\n") ||
			strings.Contains(out, "webrtc-comms owns") || strings.Contains(out, "is not restarted") {
			t.Fatalf("%s: nothing handed back: %q %s %v", c.name, calls, out, err)
		}
	}
	// A failing 4b/4c command skips Summon before S0b, so S0b only unlinks.
	b, err := os.ReadFile("../../deploy/install-car.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	files := strings.Index(src, "\nif summon_installing; then summon_install_files; fi\n")
	stage := strings.Index(src, "\nsummon_stage\n")
	if files < 0 || stage < 0 || files > stage {
		t.Fatal("4b and 4c's commands must come before S0b (summon_stage)")
	}
	// Only S0b's confirmed stop sequence (and the checks hook, which models
	// the run after it) marks stock as stopped.
	var sets []string
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "STOCK_STOPPED=1") {
			sets = append(sets, strings.TrimSpace(l))
		}
	}
	stop := src[strings.Index(src, "\nsummon_stop_stage() {\n"):]
	stop = stop[:strings.Index(stop, "\n}\n")]
	if len(sets) != 2 || !strings.Contains(stop, "\tSTOCK_STOPPED=1 # from here stock may be down") ||
		strings.Index(stop, "STOCK_STOPPED=1") > strings.Index(stop, "if stop_webrtc; then") ||
		strings.Index(stop, "STOCK_STOPPED=1") < strings.Index(stop, "summon_skip \"the S0b stop sequence was not confirmed\"") {
		t.Fatalf("STOCK_STOPPED=1 must be set only once S0b's stop sequence was confirmed, right before it runs: %q", sets)
	}
}

// After a boot, an unsupervised summonhelper (one runsv does not supervise)
// skips Summon for the boot, the camera goes on to S3, and the end report names
// the pid, non-zero (the round-2 rule; a full install does the same:
// TestFullInstallSummonFailuresNeverHalfApply). Its run script stays for the
// next run.
func TestAfterBootUnsupervisedSummonhelperSkips(t *testing.T) {
	f := newFakes(t)
	for _, verb := range []string{"run", "serve"} {
		os.RemoveAll(f.path("gw"))
		os.RemoveAll(f.path("proc"))
		links := f.installHelper()
		stray := f.helperProc("4801", verb)
		f.answers("(false,)", "(true,)") // the stop sequence's check, then the hand-back's (the stray is gone by then)
		f.calls()
		out, err := f.hook("AGW_STOP_WEBRTC_ONLY", "yes\n", []string{"FAKE_SLEEP_RM=" + stray, "FAKE_SLEEP_RM_AFTER=1"}, "--after-boot")
		calls := f.calls()
		lines := strings.Split(strings.TrimSpace(out), "\n")
		_, lerr := os.Lstat(links[0])
		_, rerr := os.Stat(filepath.Join(f.helperSvc(), "run"))
		if exitCode(err) != 1 || index(calls, "sv up "+f.helperSvc()) >= 0 || !strings.Contains(out, "S3 services: agw camhelper\n") ||
			!strings.HasPrefix(lines[len(lines)-1], "install-car: Summon is not running: an unsupervised summonhelper is running (pid 4801): "+
				"kill it (kill 4801), then run install-car.sh again") || !os.IsNotExist(lerr) || rerr != nil {
			t.Fatalf("after a boot, an unsupervised summonhelper %s: %q %s %v", verb, calls, out, err)
		}
	}
}

// ---- the exit path: no exit after S0b's stop leaves stock stopped ----

// exitPath runs the AGW_EXIT_PATH_ONLY hook (the main flow's Summon steps under
// the real run's exit trap) on a fresh layout with the helper installed.
func exitPath(t *testing.T, f *fakes, stdin string, env []string, args ...string) ([]string, string, error) {
	t.Helper()
	f.calls()
	out, err := f.hook("AGW_EXIT_PATH_ONLY", stdin, env, args...)
	return f.calls(), out, err
}

// freshLayout empties the test layout and installs the helper (SUMMON unset
// keeps it). linked is whether our helper's link is in our runsvdir dir, as S3
// leaves it; an unlinked helper is a first install that stopped before S3, or
// one a skip unlinked.
func (f *fakes) freshLayout(linked bool) {
	f.t.Helper()
	os.RemoveAll(f.path("gw"))
	os.RemoveAll(f.path("proc"))
	for _, n := range []string{"sleeps", "held", "release"} {
		os.Remove(f.path(n))
	}
	links := f.installHelper()
	if !linked {
		for _, l := range links {
			must(f.t, os.Remove(l))
		}
	}
}

// Whatever ends the run after S0b's stop sequence stopped stock webrtc-comms,
// a die included, the exit path (exit_hand_back) decides:
//   - our helper is linked, the installed owner: nothing is touched, running or
//     not (after failed checks it stays linked and running on purpose);
//   - it is not linked: every 1 s, up to 10 times, it waits for no summonhelper
//     process and a free name, then restarts and checks stock (hand_back); a
//     process that remains, or a name that stays owned, gets a line and no
//     restart.
//
// It keeps the run's exit status, and never runs twice: not after the in-flow
// hand-back, not after the passing checks, not before S0b's stop.
func TestExitPathHandsStockBack(t *testing.T) {
	f := newFakes(t)
	dieAtS2 := "install-car: not confirmed, stopping (nothing further was changed)"
	restart := []string{"sv -w 15 down /etc/sv/webrtc-comms", "sv up /etc/sv/webrtc-comms"}
	onTheWayOut := "install-car: on the way out: S0b stopped stock webrtc-comms and nothing of ours serves com.tesla.Webrtc: restarting stock\n"

	// Linked and supervised by a running runsv: the installed owner is never
	// touched. A "no" at S2: nothing but S0b's own calls and the status read.
	f.freshLayout(true)
	f.answers("(false,)")
	calls, out, err := exitPath(t, f, "yes\nno\n", []string{"FAKE_SV_STATUS=0"})
	if exitCode(err) != 1 || !strings.Contains(out, dieAtS2) || len(stockCalls(calls)) != 1 || count(calls, nameHasOwner) != 1 ||
		count(calls, "sv -w 15 down "+f.helperSvc()) != 1 || strings.Contains(out, "on the way out") {
		t.Fatalf("linked and supervised, a no at S2: nothing on the way out: %q %s %v", calls, out, err)
	}
	// Linked and supervised, the checks failed and no helper process runs:
	// still nothing (DONE said it stays linked and running).
	f.freshLayout(true)
	f.answers("(false,)")
	for name, a := range map[string][]string{"probe-summon": repeat("fail", 15), "probe-route": {"ok"}} {
		os.Remove(f.path(name + ".n"))
		must(t, os.WriteFile(f.path(name), []byte(strings.Join(a, "\n")+"\n"), 0o644))
	}
	calls, out, err = exitPath(t, f, "yes\nyes\nyes\nyes\n", []string{"FAKE_SV_STATUS=0"})
	if exitCode(err) != 1 || count(calls, "sv -w 15 down "+f.helperSvc()) != 1 || len(stockCalls(calls)) != 1 ||
		count(calls, nameHasOwner) != 1 || strings.Contains(out, "on the way out") ||
		!strings.Contains(out, "install-car: Summon checks failed: the summon relay check failed 15 times") {
		t.Fatalf("linked and supervised, failed checks, no helper process: nothing on the way out: %q %s %v", calls, out, err)
	}

	// Not linked, a "no" at S2, nothing of ours running and the name free:
	// stock is restarted and checked, and the run still exits 1.
	f.freshLayout(false)
	f.answers("(false,)", "(false,)", "(true,)") // S0b's wait, the exit path's check, stock registered
	calls, out, err = exitPath(t, f, "yes\nno\n", nil)
	if exitCode(err) != 1 || !strings.Contains(out, dieAtS2) || strings.Join(stockCalls(calls)[1:], "\n") != strings.Join(restart, "\n") ||
		count(calls, nameHasOwner) != 3 || !strings.Contains(out, onTheWayOut) ||
		!strings.Contains(out, "stock webrtc-comms owns com.tesla.Webrtc again\n") || strings.Index(out, dieAtS2) > strings.Index(out, "on the way out") {
		t.Fatalf("unlinked, a no at S2, the name free: %q %s %v", calls, out, err)
	}
	// Not linked, the name owned at first (stock still letting go of it) and
	// free at the third check: the wait ends, and stock is restarted.
	f.freshLayout(false)
	f.answers("(false,)", "(true,)", "(true,)", "(false,)", "(true,)")
	calls, out, err = exitPath(t, f, "yes\nno\n", nil)
	if exitCode(err) != 1 || count(calls, nameHasOwner) != 5 || count(calls, "sleep 1") != 2 || len(stockCalls(calls)) != 3 ||
		!strings.Contains(out, onTheWayOut) || !strings.Contains(out, "stock webrtc-comms owns com.tesla.Webrtc again\n") {
		t.Fatalf("unlinked, the name freeing within the wait: %q %s %v", calls, out, err)
	}
	// Not linked, the name owned for all 10 checks: a line, and no restart.
	f.freshLayout(false)
	f.answers(append([]string{"(false,)"}, repeat("(true,)", 10)...)...)
	calls, out, err = exitPath(t, f, "yes\nno\n", nil)
	if exitCode(err) != 1 || count(calls, nameHasOwner) != 11 || count(calls, "sleep 1") != 9 || len(stockCalls(calls)) != 1 ||
		!strings.Contains(out, "install-car: on the way out: com.tesla.Webrtc is still not free after 10 checks, 1 s apart "+
			"(last NameHasOwner answer: (true,)), so stock webrtc-comms was not restarted\n") {
		t.Fatalf("unlinked, the name staying owned: %q %s %v", calls, out, err)
	}
	// Not linked, a stray summonhelper process that stays: a line naming it,
	// and no restart (it may hold the name).
	f.freshLayout(false)
	f.helperProc("4800", "serve")
	f.answers("(false,)")
	calls, out, err = exitPath(t, f, "yes\nno\n", nil)
	if exitCode(err) != 1 || count(calls, nameHasOwner) != 1 || count(calls, "sleep 1") != 9 || len(stockCalls(calls)) != 1 ||
		!strings.Contains(out, "install-car: on the way out: a summonhelper process still runs (pid 4800), so stock webrtc-comms "+
			"was not restarted\n") {
		t.Fatalf("unlinked, a stray summonhelper that stays: %q %s %v", calls, out, err)
	}
	// Not linked, a stock that does not register on the way out: a clear line,
	// the status kept.
	f.freshLayout(false)
	f.answers(append([]string{"(false,)", "(false,)"}, repeat("(false,)", 10)...)...)
	_, out, err = exitPath(t, f, "yes\nno\n", nil)
	if exitCode(err) != 1 || !strings.Contains(out, "install-car: stock webrtc-comms was restarted on the way out, but stock "+
		"webrtc-comms did not register com.tesla.Webrtc after its restart (10 checks, 1 s apart; last NameHasOwner answer: (false,))") {
		t.Fatalf("stock not registering on the way out: %s %v", out, err)
	}

	// Before S0b's stop sequence there is nothing to hand back: a die in the
	// tools check of a full install, and a "no" at S0b's question followed by a
	// "no" at S2.
	f.freshLayout(false)
	calls, out, err = exitPath(t, f, "yes\n", []string{"SUMMON=1", "AGW_TEST_GDBUS=/nonexistent/gdbus"})
	if exitCode(err) != 1 || len(calls) != 0 || strings.Contains(out, "on the way out") ||
		strings.TrimSpace(out) != "install-car: gdbus not found (the com.tesla.Webrtc name checks need it)" {
		t.Fatalf("a die before S0b: %q %s %v", calls, out, err)
	}
	f.freshLayout(false)
	calls, out, err = exitPath(t, f, "no\nno\n", []string{"SUMMON=1"})
	if exitCode(err) != 1 || len(calls) != 0 || strings.Contains(out, "on the way out") || !strings.Contains(out, dieAtS2) {
		t.Fatalf("S0b declined, then a no at S2: %q %s %v", calls, out, err)
	}

	// The normal success path: the checks pass, so our helper owns the name;
	// the run exits 0 and the exit path does nothing.
	f.freshLayout(true)
	f.answers("(false,)")
	for name, a := range map[string]string{"probe-summon": "ok", "probe-route": "ok"} {
		os.Remove(f.path(name + ".n"))
		must(t, os.WriteFile(f.path(name), []byte(a+"\n"), 0o644))
	}
	calls, out, err = exitPath(t, f, "yes\nyes\nyes\nyes\n", []string{"FAKE_SV_STATUS=0"})
	if err != nil || count(calls, nameHasOwner) != 1 || len(stockCalls(calls)) != 1 || strings.Contains(out, "on the way out") ||
		!strings.Contains(out, "S4 services: agw camhelper summonhelper\n") {
		t.Fatalf("the success path: %q %s %v", calls, out, err)
	}
	// A skip after S0b's stop: the in-flow hand-back restarts stock once, and
	// the exit path, after the end's report, does not do it again.
	f.freshLayout(true)
	stray := f.helperProc("4800", "serve")
	f.answers("(false,)", "(false,)", "(true,)")
	calls, out, err = exitPath(t, f, "yes\nyes\nyes\nyes\n", []string{"FAKE_SLEEP_RM=" + stray, "FAKE_SLEEP_RM_AFTER=2"})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if exitCode(err) != 1 || count(calls, "sv up /etc/sv/webrtc-comms") != 1 || count(calls, nameHasOwner) != 3 ||
		strings.Contains(out, "on the way out") ||
		!strings.HasPrefix(lines[len(lines)-1], "install-car: Summon is not running: an unsupervised summonhelper is running (pid 4800)") {
		t.Fatalf("no double hand-back: %q %s %v", calls, out, err)
	}
}

// Our helper is the installed owner only while it is linked AND supervised by
// a running runsv (its runsv up, not told to stop it or keep it down). A link
// with no such runsv starts nothing, so the exit path restores stock as for an
// unlinked helper: after a boot, before S3 starts our runsvdir (the link stays
// on /var/lib from the last install), and after S0b brought the helper down,
// or told it to stop while it still stops, in a run whose runsvdir was already
// up. Summon is installed (SUMMON=1) and the helper is linked in each case,
// except the last.
func TestExitPathLinkedHelper(t *testing.T) {
	f := newFakes(t)
	restored := func(what string, calls []string, out string, err error) {
		t.Helper()
		if exitCode(err) != 1 || count(calls, "sv up /etc/sv/webrtc-comms") != 1 || count(calls, nameHasOwner) != 3 ||
			!strings.Contains(out, "install-car: on the way out: S0b stopped stock webrtc-comms and nothing of ours serves "+
				"com.tesla.Webrtc: restarting stock\n") || !strings.Contains(out, "stock webrtc-comms owns com.tesla.Webrtc again\n") {
			t.Fatalf("%s: stock must be restored, exit 1: %q %s %v", what, calls, out, err)
		}
	}
	untouched := func(what string, calls []string, out string, err error) {
		t.Helper()
		if exitCode(err) != 1 || len(stockCalls(calls)) != 1 || count(calls, nameHasOwner) != 1 ||
			count(calls, "sv -w 15 down "+f.helperSvc()) > 1 || strings.Contains(out, "on the way out") {
			t.Fatalf("%s: nothing on the way out: %q %s %v", what, calls, out, err)
		}
	}

	// After a boot, no runsv for the helper yet (our runsvdir starts at S3): a
	// "no" at S2, or at S3, restores stock and exits 1.
	for _, c := range []struct{ name, stdin string }{{"a no at S2", "yes\nno\n"}, {"a no at S3", "yes\nyes\nno\n"}} {
		f.freshLayout(true)
		f.answers("(false,)", "(false,)", "(true,)")
		calls, out, err := exitPath(t, f, c.stdin, []string{"SUMMON=1"}, "--after-boot")
		restored("after a boot, linked, no runsv, "+c.name, calls, out, err)
	}
	// The helper's runsv up and running it, or restarting it (want up), or
	// only its log service told to stop: the installed owner; the exit path
	// takes no action.
	for _, status := range []string{"0", "down", "logdown"} {
		f.freshLayout(true)
		f.answers("(false,)")
		calls, out, err := exitPath(t, f, "yes\nno\n", []string{"SUMMON=1", "FAKE_SV_STATUS=" + status}, "--after-boot")
		untouched("linked, its runsv up (status "+status+")", calls, out, err)
	}
	// After a full install's S3, the helper supervised and running: a "no" at
	// S4 hands nothing back.
	f.freshLayout(true)
	f.helperProc("4810", "serve")
	f.answers("(false,)")
	calls, out, err := exitPath(t, f, "yes\nyes\nyes\nno\n", []string{"SUMMON=1", "FAKE_SV_STATUS=0"})
	if !strings.Contains(out, "S4. restart agw and camhelper") {
		t.Fatalf("the run did not reach S4: %s", out)
	}
	untouched("a full install's S3, supervised and running, a no at S4", calls, out, err)
	// Its runsv up but told to keep it down (S0b's sv down, in a run whose
	// runsvdir was already up): it starts nothing, so stock is restored.
	f.freshLayout(true)
	f.answers("(false,)", "(false,)", "(true,)")
	calls, out, err = exitPath(t, f, "yes\nno\n", []string{"SUMMON=1", "FAKE_SV_STATUS=wantdown"})
	restored("linked, its runsv told to keep it down", calls, out, err)
	// Its runsv told to stop it, the helper still stopping (S0b's sv -w 15 down
	// ran out of time: "run: …, want down", or its finish runs: "finish: …,
	// want down"): it starts nothing, so it is not the owner. The exit path
	// brings it down and waits: a helper that exits within the wait (after the
	// second 1 s check) has stock restored; one that stays gets a line naming
	// its pid, and no restart.
	for _, status := range []string{"rundown", "finishdown"} {
		f.freshLayout(true)
		stopping := f.helperProc("4810", "serve")
		f.answers("(false,)", "(false,)", "(true,)")
		calls, out, err = exitPath(t, f, "yes\nno\n", []string{"SUMMON=1", "FAKE_SV_STATUS=" + status,
			"FAKE_SLEEP_RM=" + stopping, "FAKE_SLEEP_RM_AFTER=2"})
		restored("linked, its runsv told to stop it ("+status+"), the helper exiting within the wait", calls, out, err)
		if count(calls, "sv -w 15 down "+f.helperSvc()) != 2 || count(calls, "sleep 1") != 2 {
			t.Fatalf("%s: S0b's and the exit path's sv down, then 2 checks with the helper still there: %q", status, calls)
		}
		f.freshLayout(true)
		f.helperProc("4810", "serve")
		f.answers("(false,)")
		calls, out, err = exitPath(t, f, "yes\nno\n", []string{"SUMMON=1", "FAKE_SV_STATUS=" + status})
		if exitCode(err) != 1 || count(calls, "sv -w 15 down "+f.helperSvc()) != 2 || count(calls, "sleep 1") != 9 ||
			len(stockCalls(calls)) != 1 || count(calls, nameHasOwner) != 1 ||
			!strings.Contains(out, "install-car: on the way out: a summonhelper process still runs (pid 4810), so stock "+
				"webrtc-comms was not restarted\n") {
			t.Fatalf("%s: a helper that stays: a line naming it, no restart, exit 1: %q %s %v", status, calls, out, err)
		}
	}
	// Not linked, its runsv still up and running it (a skip unlinked it): not
	// the installed owner, so its runsv is brought down and stock restored.
	f.freshLayout(false)
	f.answers("(false,)", "(false,)", "(true,)")
	calls, out, err = exitPath(t, f, "yes\nno\n", []string{"FAKE_SV_STATUS=0"})
	restored("unlinked, its runsv up", calls, out, err)
	if count(calls, "sv -w 15 down "+f.helperSvc()) != 2 {
		t.Fatalf("unlinked, its runsv up: S0b's and the exit path's sv down, want 2: %q", calls)
	}
}

// hookChild runs the AGW_EXIT_PATH_ONLY hook as this test's child, in a
// process group of its own (so a signal to that group reaches only the hook's
// shell and the commands it started, as a terminal's hangup reaches the
// foreground job), with stdin a pipe the test writes, and its output in
// hook.out (startHook) or on the caller's file (startHookOut).
type hookChild struct {
	t       *testing.T
	f       *fakes
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	exited  chan struct{} // closed once Wait returned
	waitErr error
}

func startHook(t *testing.T, f *fakes, shell string, env ...string) *hookChild {
	t.Helper()
	out, err := os.Create(f.path("hook.out"))
	must(t, err)
	t.Cleanup(func() { out.Close() })
	return startHookOut(t, f, shell, out, env...)
}

// startHookOut is startHook with the hook's stdout and stderr on out, which
// the caller owns (output then reads nothing).
func startHookOut(t *testing.T, f *fakes, shell string, out *os.File, env ...string) *hookChild {
	t.Helper()
	cmd := exec.Command(shell, "../../deploy/install-car.sh")
	cmd.Env = append(os.Environ(), f.env(append([]string{"AGW_EXIT_PATH_ONLY=1"}, env...)...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	must(t, err)
	cmd.Stdout, cmd.Stderr = out, out
	must(t, cmd.Start())
	h := &hookChild{t: t, f: f, cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	go func() {
		h.waitErr = cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() {
		stdin.Close()
		os.WriteFile(f.path("release"), nil, 0o644) // a held fake gdbus lets go
		select {
		case <-h.exited:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill() // the hook's own shell, this test's child (os/exec refuses pid 0 and -1)
			<-h.exited
		}
	})
	return h
}

// waitFor waits up to 10 s until cond holds.
func (h *hookChild) waitFor(what string, cond func() bool) {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(h.f.path("hook.out"))
			h.t.Fatalf("%s: not within 10 s\n%s", what, b)
		}
	}
}

// output is what the hook printed so far.
func (h *hookChild) output() string {
	b, _ := os.ReadFile(h.f.path("hook.out"))
	return string(b)
}

// alive reports whether the hook has not exited yet.
func (h *hookChild) alive() bool {
	select {
	case <-h.exited:
		return false
	default:
		return true
	}
}

// hangup sends SIGHUP to the hook's own process group, as a dropped SSH
// session's terminal does to its foreground job: the hook's shell and the
// commands it started, never anything else (the group is the child's own,
// made by Setpgid, and is checked first).
func (h *hookChild) hangup() {
	h.t.Helper()
	pid := h.cmd.Process.Pid
	if pgid, err := syscall.Getpgid(pid); err != nil || pid <= 1 || pgid != pid {
		h.t.Fatalf("the hook's process group: pid %d, pgid %d, %v: not the child's own, nothing sent", pid, pgid, err)
	}
	must(h.t, syscall.Kill(-pid, syscall.SIGHUP))
}

// result waits up to 10 s for the hook to exit, and returns its exit code.
func (h *hookChild) result() int {
	h.t.Helper()
	select {
	case <-h.exited:
		return exitCode(h.waitErr)
	case <-time.After(10 * time.Second):
		h.t.Fatalf("the hook did not exit within 10 s\n%s", h.output())
		return 0
	}
}

// A signal that ends the run after S0b's stop (TERM here; INT the same) runs
// the same exit path: stock is restarted and checked, and the run exits with
// the signal's usual status. stdin is closed right after the signal, so a
// signal that lands just before read(2) blocks cannot hang the test. The
// signal goes only to the hook's own shell, this test's child.
func TestExitPathOnASignal(t *testing.T) {
	shells := []string{"sh"}
	if _, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, "dash") // the car's BusyBox ash is dash's kin
	}
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			f := newFakes(t)
			f.freshLayout(false)
			f.answers("(false,)", "(false,)", "(true,)")
			f.calls()
			h := startHook(t, f, shell)
			io.WriteString(h.stdin, "yes\n") // S0b's question; S2's waits
			h.waitFor("the S2 question", func() bool { return strings.Contains(h.output(), "S2. write the service scripts") })
			must(t, h.cmd.Process.Signal(syscall.SIGTERM)) // the hook's own shell, this test's child
			h.stdin.Close()
			code := h.result()
			calls := f.calls()
			if code != 143 || count(calls, "sv up /etc/sv/webrtc-comms") != 1 || count(calls, nameHasOwner) != 3 ||
				!strings.Contains(h.output(), "stock webrtc-comms owns com.tesla.Webrtc again\n") {
				t.Fatalf("%s, SIGTERM at S2: exit %d, calls %q\n%s", shell, code, calls, h.output())
			}
		})
	}
}

// A dropped SSH session's SIGHUP never cuts the run or its exit path short:
// the run and every command it starts ignore it. Delivered while the exit path
// waits on a name check, the hand-back still completes; delivered at a
// question, the run goes on until its next read of the dead terminal (here
// stdin, closed after the hangup) ends it with "no answer", and then the exit
// path hands stock back. The hangup goes only to the hook's own process group.
func TestExitPathSurvivesAHangup(t *testing.T) {
	shells := []string{"sh"}
	if _, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, "dash")
	}
	for _, shell := range shells {
		t.Run(shell+"/during the exit path", func(t *testing.T) {
			f := newFakes(t)
			f.freshLayout(false)
			// S0b's wait, the exit path's check, then hand_back's wait, which holds until released.
			f.answers("(false,)", "(false,)", "hold:(true,)")
			f.calls()
			h := startHook(t, f, shell)
			io.WriteString(h.stdin, "yes\nno\n") // S0b yes, S2 no: the die runs the exit path
			h.stdin.Close()
			h.waitFor("the exit path's name check, held", func() bool { _, err := os.Stat(f.path("held")); return err == nil })
			h.hangup()
			time.Sleep(200 * time.Millisecond)
			if !h.alive() {
				t.Fatalf("the hangup ended the run during its exit path (exit %d)\n%s", exitCode(h.waitErr), h.output())
			}
			must(t, os.WriteFile(f.path("release"), nil, 0o644))
			code := h.result()
			calls := f.calls()
			if code != 1 || count(calls, "sv up /etc/sv/webrtc-comms") != 1 ||
				!strings.Contains(h.output(), "stock webrtc-comms owns com.tesla.Webrtc again\n") {
				t.Fatalf("%s, a hangup during the exit path: exit %d, calls %q\n%s", shell, code, calls, h.output())
			}
		})
		t.Run(shell+"/at a question", func(t *testing.T) {
			f := newFakes(t)
			f.freshLayout(false)
			f.answers("(false,)", "(false,)", "(true,)")
			f.calls()
			h := startHook(t, f, shell)
			io.WriteString(h.stdin, "yes\n")
			h.waitFor("the S2 question", func() bool { return strings.Contains(h.output(), "S2. write the service scripts") })
			h.hangup()
			time.Sleep(200 * time.Millisecond)
			if !h.alive() {
				t.Fatalf("the hangup ended the run at a question (exit %d)\n%s", exitCode(h.waitErr), h.output())
			}
			h.stdin.Close() // the dead terminal: the next read gets EOF
			code := h.result()
			calls := f.calls()
			if code != 1 || !strings.Contains(h.output(), "install-car: no answer (stdin closed)") ||
				count(calls, "sv up /etc/sv/webrtc-comms") != 1 || !strings.Contains(h.output(), "stock webrtc-comms owns com.tesla.Webrtc again\n") {
				t.Fatalf("%s, a hangup at a question: exit %d, calls %q\n%s", shell, code, calls, h.output())
			}
		})
	}
}

// A dead reader of the output (sshd's end of a session's pipe, once the
// session is gone) never kills the run or its exit path: the run ignores
// SIGPIPE, and every command it starts inherits that, so a write there fails
// instead (EPIPE), and set -e ends the run at its next checked one. Here the hook's output is a
// pipe whose reader the test closes at the S2 question; stdin then hits EOF,
// the die's line fails, and the exit path still restarts and checks stock
// (seen in the fakes' log: the output is gone) and exits 1. Nothing is sent to
// the hook: the closed pipe and the EOF alone.
//
// Under dash only, the car's BusyBox ash's kin: both drop a failed write's
// bytes (dash in its own output code, ash in glibc's stdio, the car's C
// library). macOS's /bin/sh, bash 3.2 on BSD stdio, keeps them in its stdout
// buffer and hands them to every later command substitution of a shell
// function, so there the exit path reads the die's line as a summonhelper pid.
func TestExitPathOnADeadPipe(t *testing.T) {
	if _, err := exec.LookPath("dash"); err != nil {
		t.Skip("dash not found (the car's BusyBox ash is its kin; macOS's /bin/sh keeps a failed write's bytes)")
	}
	f := newFakes(t)
	f.freshLayout(false)
	f.answers("(false,)", "(false,)", "(true,)")
	f.calls()
	r, w, err := os.Pipe()
	must(t, err)
	t.Cleanup(func() { r.Close() })
	h := startHookOut(t, f, "dash", w)
	w.Close()                        // the hook holds the only write end
	io.WriteString(h.stdin, "yes\n") // S0b's question; S2's waits
	var seen []byte
	buf := make([]byte, 4096)
	must(t, r.SetReadDeadline(time.Now().Add(10*time.Second)))
	for !strings.Contains(string(seen), "S2. write the service scripts") {
		n, err := r.Read(buf)
		seen = append(seen, buf[:n]...)
		if err != nil {
			t.Fatalf("the S2 question: %v\n%s", err, seen)
		}
	}
	must(t, r.Close()) // no reader left: every later write to the pipe fails
	h.stdin.Close()    // and the next read gets EOF
	code := h.result()
	calls := f.calls()
	if code != 1 || count(calls, "sv up /etc/sv/webrtc-comms") != 1 || count(calls, nameHasOwner) != 3 {
		t.Fatalf("the output's reader gone at S2: exit %d (want 1), calls %q\n%s", code, calls, seen)
	}
}

// The DONE text never prints a pin (each holds the VIN, and the car records
// SSH sessions): agw's step 3 gives the camera runbook's step-6 route, run as
// uid 4950 with agw's own key, from the installer's variables, rendered here
// as the real run renders the heredoc. summonhelper's route is
// TestSummonChecksRetry's.
func TestDoneTextNeverPrintsAPin(t *testing.T) {
	b, err := os.ReadFile("../../deploy/install-car.sh")
	must(t, err)
	s := string(b)
	i := strings.Index(s, "\ncat <<DONE\n")
	j := strings.Index(s, "\nDONE\n")
	if i < 0 || j < i {
		t.Fatal("no DONE heredoc")
	}
	heredoc := s[i+1 : j+len("\nDONE\n")]
	if !strings.Contains(heredoc, `  P=\$(mktemp) && chpst -u $RUN_AS $DIR/agw pin -key $STATE/key.pem > "\$P" && wc -c < "\$P" && `+
		`socat -u FILE:"\$P" "TCP:\$LAPTOP:\$PORT"; rm -f "\$P"`) {
		t.Fatalf("the DONE text must give agw's pin route from the installer's variables:\n%s", heredoc)
	}
	render := exec.Command("sh", "-c", "DIR=/var/lib/airgapp-gw; STATE=$DIR/state; UID_GW=4950; GID_GW=4950; "+
		"RUN_AS=:$UID_GW:$GID_GW; PI_IP=192.168.4.1; PI_PORT=17101; _done_svcs=svcs; _done_logs=logs\n"+heredoc)
	render.Env = []string{"PATH=" + os.Getenv("PATH")}
	got, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("rendering the DONE text: %v\n%s", err, got)
	}
	route := `  P=$(mktemp) && chpst -u :4950:4950 /var/lib/airgapp-gw/agw pin -key /var/lib/airgapp-gw/state/key.pem > "$P" && ` +
		`wc -c < "$P" && socat -u FILE:"$P" "TCP:$LAPTOP:$PORT"; rm -f "$P"` + "\n"
	if !strings.Contains(string(got), route) || !strings.Contains(string(got), "never\n   print it here (the car records SSH sessions)") ||
		!strings.Contains(string(got), "nc -l <port> > gwpin") || strings.Contains(string(got), "Print the pin") {
		t.Fatalf("the rendered DONE text must give agw's pin route:\n%s", got)
	}
	for _, l := range strings.Split(string(got), "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), "agw pin -key /var/lib/airgapp-gw/state/key.pem") {
			t.Fatalf("the DONE text gives the bare pin command, which prints the pin: %q", l)
		}
	}
}
