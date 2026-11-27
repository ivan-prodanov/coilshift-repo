# Car install

`install-car.sh` is run by the owner, as root, over SSH, with the car awake.
It is POSIX `sh` (BusyBox) and asks for a typed `yes` before every write.

    PI_IP=<pi hotspot ip> sh install-car.sh ./agw ./camhelper
    sh install-car.sh --after-boot   # after every car boot (PI_IP as installed)
    SUMMON=1 PI_IP=<pi hotspot ip> sh install-car.sh ./agw ./camhelper ./summonhelper   # serve SUMMON

The car dials out to the Pi. Its Wi-Fi module NATs the MCU and drops every
inbound packet (car-proven 2026-11-21: the MCU's `wlan0` is 192.168.21.2
behind the module, which the Pi's hotspot sees as 192.168.4.233), so agw runs
`serve -dial <pi-hotspot-ip>:17101` and nothing connects in. `PI_IP` is the
Pi's own address on its hotspot (192.168.4.1 by default). `CAR_IP` is no
longer used and is ignored.

If agw loses a connection to the Pi, it redials 1 s later and, while dials
fail, backs off, doubling from 1 s to 10 s. The car is pairable again at the
first dial that gets through:

- Only the Pi's forwarder restarts (netfilterd applying a new tailnet
  address): at the first redial, about 1 s after the loss.
- The Pi refuses the dials (nothing listens on 17101: netfilterd is down, or
  its forwarder could not listen yet): they fall about 1, 2, 4, 8, 16 and
  26 s after the loss, then every 10 s. Once the forwarder listens again,
  the car is pairable within about 2 s after an outage of up to about 4 s,
  about 3 s after a 5 s one (2.99 s measured on loopback, real agw and Pi
  forwarder, 2026-11-21) and up to 10 s after a longer one. When netfilterd
  itself comes back, the next case applies.
- A netfilterd restart (a crash, `RestartSec=5`, or `make install`), a Pi
  reboot or a hotspot drop: up to about 13 s after the car's Wi-Fi module is
  back on the hotspot and the Pi's port 17101 is open, as the car's dials can
  time out (3 s) instead of being refused. Every netfilterd start restarts
  hostapd, so the module must rejoin, and opens 17101 at its first Tailscale
  status read, right after it starts, or, if Tailscale is not up yet, up to a
  minute after it is.

If no close reached the car (a power cut, or a hotspot drop its connections
do not survive), it may notice only after 25 s with nothing heard, then
redials 1 s later.

Build the binaries with `make car` (static linux/amd64, `out/agw`,
`out/camhelper` and `out/summonhelper`). `HELPER_WRITER=aplay` selects the fallback talk writer (see
`docs/CAMERA-HELPER.md`, car visit item 1).

## What it does

1. Creates the layout below. The gateway runs as `chpst -u :4950:4950`. It
   gets no passwd entry and so no system bus access: camhelper (root) answers
   its sign-in questions on `/run/airgapp-camhelper/auth.sock`
   (`docs/CAMERA-HELPER.md`, "Sign-in socket").
2. Installs `agw` (root-owned, 0755).
3. Creates no key list: access is the car's own (below, "Who can sign in").
4. Generates the long-term key on the car by running `keygen` as 4950, so the
   key is owned by the gateway uid (`Load` checks this) and never leaves the
   car.

| Path | Owner | Mode |
|---|---|---|
| `/var/lib/airgapp-gw` | root:root | 0755 |
| `/var/lib/airgapp-gw/agw` | root:root | 0755 |
| `/var/lib/airgapp-gw/state/` | 4950:4950 | 0700 |
| `state/key.pem`, `state/log` | 4950:4950 | 0400 / session log |
| `/var/lib/airgapp-gw/summonhelper` (`SUMMON=1`) | root:root | 0755 |
| `/var/lib/airgapp-summon` (`SUMMON=1`) | root:root | 0755 |
| `/var/lib/airgapp-summon/state/` | 1337:1337 | 0700 |
| `/var/lib/airgapp-summon/state/key.pem`, `log` | 1337:1337 | 0400 / session log |
| `/var/lib/airgapp-gw/sv/summonhelper/{run,finish}` | root:root | 0755 |
| `/run/airgapp-summon/` (made by the run script at every start) | 1337:4950 | 2750 |
| `/run/airgapp-summon/relay.sock` (made by the helper) | 1337:4950 | 0660 |
| `/run/airgapp-summon/session` (the helper's session marker) | 1337:4950 | 0600 |
| `/run/airgapp-summon/live` (the helper's live file, under its flock while the engine of a session that opted into the presence lease is up; `docs/SUMMON.md`) | 1337:4950 | 0600 |

The gateway uid cannot change its own binary (spec 4.2).

### Who can sign in

Access is the car's own: there is no allowlist, no `agw allow` command and
nothing to provision over SSH. A phone gets a session when it proves
possession of its key (the HPKE handshake) and the car's authd lists that key
as OWNER (2) or DRIVER (3): the keys the owner adds or removes in the Tesla app
or with the key card. agw asks through camhelper's sign-in socket; summonhelper
asks authd on the bus itself. A key that is not OWNER or DRIVER is refused at
its hello, with no challenge (so it never gets the VIN binding or the gateway's
key), and authd is asked again after the ARM, which is the real gate; a key
learns whether a window is open only after it. `agw serve` still accepts an
old run script's `-allow` for one release, ignores it and logs so.

A key the car stops listing loses its open session in both agw and
summonhelper through the authority re-check (BYE `revoked`): agw re-checks at
once on authd's key change (the sign-in helper's keys_changed), every 30 s
while that watch is up and every 10 s while it is down; summonhelper re-checks
every 10 s on its own authd connection. An `allow` file left by an older
install is no longer read (the installer notes it and leaves it alone).

5. (Removed: there is no INPUT rule. agw dials out, and replies to its own
   connections come back like any other outbound TCP's.)
6. The camera root helper section (S1-S4) installs `camhelper`
   (`/var/lib/airgapp-camhelper/camhelper`, root:root 0755), writes the runit
   services `/var/lib/airgapp-gw/sv/agw` and `/var/lib/airgapp-gw/sv/camhelper`
   (`run` + `finish`) and starts both under runit:
   - `agw`'s `run` is always `chpst -u :4950:4950 agw serve -dial
     <pi-hotspot-ip>:17101 …` (stderr to svlogd) and its `finish` is a no-op;
     with `SUMMON=1` a third service, `summonhelper`, serves Summon (see
     below);
   - `camhelper`'s `run` is `camhelper run` as root; its `finish` is
     `camhelper sweep`, which clears whatever the helper's markers record
     (requestAudio 0, sessionActive 0, selectable 0, gain -200 only if marked);
   - the services always go into **our own runsvdir**, never another one: the
     firmware's runsvdir watches `/service`, which resolves to
     `/etc/sv/run-modes/<level>` on the rootfs, and every runsvdir under `/run`
     on the car belongs to another supervisor (coilshift's `/run/ssp/services`,
     2026-11-25) or to whoever started it. So it is
     `setsid runsvdir -P /var/lib/airgapp-gw/svdir`, with
     `airgapp-agw` and `airgapp-camhelper` (and `airgapp-summonhelper` with
     `SUMMON=1`) linked there;
   - the services log through svlogd (`log/run`, 3 files of 1 MB):
     `/var/lib/airgapp-gw/svlog/agw/current` and
     `/var/lib/airgapp-camhelper/svlog/current` (agw's session log
     `state/log` is unchanged), and with `SUMMON=1`
     `/var/lib/airgapp-gw/svlog/summonhelper/current`;
   - an unsupervised `agw` or `camhelper` still running blocks the install
     (one of each, never two); an unsupervised `summonhelper` skips Summon for
     the run instead (see "Failures"). There is no bare nohup `agw` any more.

The script refuses loopback, the vehicle LAN and the tailnet range for either
address.

**noexec /var.** The car mounts `/var` (and `/home`) `noexec,nosymfollow`, so a
binary, a runit `run` script or a runsvdir symlink under `/var/lib` cannot run
(`deploy/probe_media.sh` prints the mount line; first seen on the car 2026-11-20). Step 1b
and S0 bind-mount `/var/lib/airgapp-gw` and `/var/lib/airgapp-camhelper` onto
themselves (`remount,bind,nosuid,nodev,exec,symfollow`: the car's util-linux mount only clears a flag that is named), the way the car's own mcu-root-utils runs files
kept on `/var`, and verify the result (the script stops if `noexec` or
`nosymfollow` is still set). Bind mounts do not survive a reboot: `--after-boot`
redoes them before runsvdir starts. Rollback: `sv down` the services (agw,
camhelper, and summonhelper with `SUMMON=1`), then
`umount /var/lib/airgapp-gw /var/lib/airgapp-camhelper`.

**No account for uid 4950.** dbus-daemon refuses a uid it cannot resolve, and
the installer keeps it that way: on this car any connected uid may call
`com.tesla.DisplayManager` and `com.tesla.Videod` (RE car check 6,
2026-11-20). An earlier per-boot `/etc/passwd` overlay (0c5315d) was removed
for that reason (helper-first, the owner's choice). The helper's authd pin is
`AUTHD_SHA256` (camhelper `-authd-sha256`; summonhelper gets the same one).
Unset keeps the installed camhelper run script's pin, on a full install as
after a boot, and with none installed the compiled-in 2026.8.6 pin applies.
A value that is not 64 lowercase hex digits stops the script. The plan line
shows the effective pin's first 8 hex digits and where it came from (given,
kept from the installed run script, or compiled-in). The script compares that
pin with `sha256sum /usr/bin/authd | cut -d' ' -f1`. If they differ, or authd
cannot be hashed, a full install asks before it writes anything, and anything
but `yes` stops it with nothing changed. After a boot it is a loud note at the
end that never blocks anything, and the run then exits non-zero, though every
service was started. Either way camera sign-in fails closed until the pin is
re-verified (the camera runbook's step 2b; after an OTA, its "After an OTA
(gateway already installed)" section). The script never adopts the car's
hash as the pin: a pin is only ever given after step 2b proved sign-in on that
build. dbus-daemon caches a uid it once
resolved until it restarts, so after the 2026-11-20 overlay test uid 4950 can
still reach the bus until the car reboots. agw's bus self-test serves only
when the bus definitely refuses it twice in a row (no socket, `REJECTED`, or a
disconnect right after `BEGIN`; one such disconnect can also be dbus-daemon's
connection limit on a busy bus). If the bus takes the connection, or does not
refuse it twice in a row (a busy bus may still let it in), agw logs
`BUS SELF-TEST FAILED`, waits
60 s and exits, so runit retries about once a minute until the car reboots. If an
old overlay is still mounted in the current boot, the per-boot section asks to
`umount /etc/passwd` (then reboot).

**The egress lock (step 5c).** agw only answers the Pi on its listening port:
the camera capture runs in the helper's children (uid 4951), and the bus and
the helper are unix sockets. The car's OUTPUT policy is ACCEPT, with an owner
chain per Tesla service and none for 4950 (seen on the car 2026-11-20), so
without a lock a compromised agw could reach the vehicle LAN and every
loopback service. Step 5c adds an owner chain the way Tesla does for its own
services: `AIRGAPP_GW_OUT` = [`-o wlan0 -d PI_IP -p tcp --dport 17101 -j
ACCEPT`, `-j DROP`] (agw's own connections to the Pi and nothing else), jumped to from the first OUTPUT rule
(`-m owner --uid-owner 4950`), and for ip6tables a chain that only drops. It
is idempotent: a chain that is not exactly that is rebuilt behind a temporary
DROP (fails closed), and the jump is put first before any duplicate is
removed. Rules are not saved, so `--after-boot` redoes it, and agw's runit
`run` script (as root, before `chpst`) refuses to start, logging "egress lock
is missing", while the jump or the DROP is absent. Known gap: if the firmware
ever flushed OUTPUT while agw ran, the lock would be gone until the next
start. Rollback, after agw is down, for iptables and ip6tables:
`iptables -D OUTPUT -m owner --uid-owner 4950 -j AIRGAPP_GW_OUT;
iptables -F AIRGAPP_GW_OUT; iptables -X AIRGAPP_GW_OUT`.

## SUMMON (`SUMMON=1`)

Summon is served by `summonhelper`, its own runit service, never by agw:
agw always runs plain `serve` and only relays the inner session to the
helper's socket (design §6.1, §6.6; the architecture and the owner's car
checks are in `docs/SUMMON.md`).

**Which mode.** The rule is the same for a full install and `--after-boot`:
- `SUMMON` unset keeps what is installed. If summonhelper's `run` is
  installed, the helper stays: S0b stops it, it is started fresh, and the
  checks run, as with `SUMMON=1`, but no new binary or key is installed. If it
  is not, nothing is done for Summon. A camera-only re-install that forgets
  `SUMMON` therefore neither offers to remove the helper nor needs a
  summonhelper binary.
- **camhelper and summonhelper ship in one install** (the 2026-11-26 review
  fixes). camhelper counts summonhelper's live file only while summonhelper
  holds its flock, and an older summonhelper (`841d3df`'s) holds none. So
  the install that brings that camhelper keeps no older summonhelper: it
  passes `SUMMON=1` and the new `summonhelper`. With a mismatched pair every
  Summon camera request is refused, and agw logs `camera: SUMMON mode
  refused (summon_unverified)` while a CAMERA window is open during a Summon
  session. That line alone does not prove a mismatch: agw logs it too for any
  session whose phone did not opt into the presence lease (summonhelper then
  logs `summon: presence lease off for this session (…)`). The install checks
  tell the two apart (`docs/SUMMON.md`, "Installing the 2026-11-26 review
  fixes").
- `SUMMON=0` removes an installed helper (below).
- `SUMMON=1` installs it on a full install (steps 0, 4b and 4c, then the
  rest), or keeps it after a boot, where it needs one installed.

An explicit `SUMMON=1` on a full install asks steps 0, 4b and 4c before step 1
(a "no" stops it with nothing changed), then:

- **4b, 4c.** Copies the third argument (default `./summonhelper`) to
  `/var/lib/airgapp-gw/summonhelper` (root 0755, in the exec-mounted dir),
  makes `/var/lib/airgapp-summon` (root 0755) and its `state/` (1337:1337
  0700), and, if it is missing, generates the helper's own key there as uid
  1337 (`summonhelper keygen`, 0400). agw never has this key.
- **S0b, the stop sequence**, after S0 and before the helper starts, at
  install and after every boot. Stock `webrtc-comms` holds `com.tesla.Webrtc`
  in the infotainment and sentry run modes, under minijail:
  1. if our helper's runsv runs, `sv -w 15 down` it (its takeover and finish
     run, and it lets go of the name);
  2. `sv down /etc/sv/webrtc-comms` (outside those run modes it has no runsv:
     noted, not fatal);
  3. SIGKILL the pid in `/run/service/webrtc-comms/webrtc-comms.pid` only if
     its `comm` is `minijail-init` (the firmware's own
     `KillOrphanedMinijailProcessComm`), then remove the pidfile;
  4. poll `NameHasOwner com.tesla.Webrtc` (gdbus, under `timeout -k 1 5`)
     every 1 s, up to 10 times: up to about a minute, as each check may take
     6 s;
  5. once S3 has linked the helper, `sv up` it.

  If the name stays taken, Summon is skipped for the run (see "Failures"
  below). `sv down` survives neither a reboot nor a run-mode switch, so the
  helper's `run` checks the name again at every start.
- **The service** `/var/lib/airgapp-gw/sv/summonhelper`, linked as
  `airgapp-summonhelper` next to agw and camhelper, logging through svlogd to
  `/var/lib/airgapp-gw/svlog/summonhelper`:
  - `run` (as root): makes `/run/airgapp-summon` (1337:4950, 2750, so the
    socket gets group 4950; a symlink or a non-directory there is refused),
    removes a stale `relay.sock`, a stale live file `live` and any `.live.*`
    temp file, reads the VIN from `/var/etc/vin` (whitespace stripped; empty
    is refused), refuses while `com.tesla.Webrtc` has an owner, then
    `SUMMONHELPER_VIN=<VIN> exec chpst -u :1337:1337
    summonhelper run` (the supervisor). Each refusal logs its reason, waits
    10 s and exits 1, like agw's. `AUTHD_SHA256` gives `-authd-sha256`, as for
    camhelper: unset keeps camhelper's installed pin (a full install as after
    a boot), and with none installed the compiled-in pin applies (see "No
    account for uid 4950" for the pin check).
  - `finish`: `chpst -u :1337:1337 summonhelper finish`, which first removes
    a stale live file, whatever the marker says, then sends `cmd_abort` and
    the disconnect if a killed supervisor left a session marker, and nothing
    more otherwise.
- **Checks** after the last restart, as uid 4950, each retried every 1 s for
  up to 15 s (a stale socket and the helper's first D2 stamp need it):
  `agw probe-summon` (the cross-uid connect test: the helper's socket, its
  uid and its OWNED record within 500 ms, exactly as agw's Open waits) and
  `agw probe-route` (the route gate). A failure is reported at the end with
  the probe's last output (`Summon checks failed: …`), and the run exits
  non-zero; the helper stays linked and running (see "Failures"). Once the
  checks pass, the DONE text gives the helper's pin route. The pin holds the
  VIN and the car records SSH sessions, so the route never prints it: it
  writes the pin to a fresh `mktemp` file, sends it to the laptop, then
  removes it (`docs/SUMMON.md`, "The pins", has the laptop's side):
  `P=$(mktemp) && chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper pin -vin-file /var/etc/vin > "$P" && wc -c < "$P" && socat -u FILE:"$P" "TCP:$LAPTOP:$PORT"; rm -f "$P"`.
  agw's own pin goes the same way: the DONE text's step 3 gives its route, as
  uid 4950 (the camera runbook's step 6):
  `P=$(mktemp) && chpst -u :4950:4950 /var/lib/airgapp-gw/agw pin -key /var/lib/airgapp-gw/state/key.pem > "$P" && wc -c < "$P" && socat -u FILE:"$P" "TCP:$LAPTOP:$PORT"; rm -f "$P"`.
  The installer itself prints no pin and no VIN.

The script needs `gdbus` and `timeout` with `SUMMON=1`, or to remove a helper.
After a boot, a missing one skips Summon for that boot instead (below).
Before the first `SUMMON=1` install, check on the car (read-only) that its
`timeout`, a BusyBox applet, takes `-k`, which the name checks use:
`timeout -k 1 5 true; echo $?` must print `0`. The firmware's own
`/etc/runit/common` runs `timeout -k 21 20`.

**Failures.** What comes before a full install's first write (step 1's
`mkdir`) stops it with nothing changed:
- the checks of the tools, the summonhelper binary, the `SUMMON` value and
  the authd pin;
- the questions of steps 0, 4b and 4c (the summonhelper copy, its state
  directory and key), which are asked before step 1 and done after step 4.

From step 1's `mkdir` on, a Summon failure or a declined Summon question
never stops the install halfway, which would leave new agw and camhelper
binaries under old run scripts. It skips Summon for the run instead, the way
`--after-boot` does: the helper is neither linked nor started (its link is
removed; its directory stays), agw and camhelper are set up as usual
(S2-S4), and the run ends with the report and a non-zero exit. That covers:
- a failing 4b or 4c command (the copy, the state directory, the keygen);
- a name that stays taken at S0b, or a "no" to S0b's questions;
- a removal held up by a lingering `summonhelper` process;
- an unsupervised `summonhelper` found before S3;
- a helper that is not running after S3.

When S0b's stop sequence had already stopped stock webrtc-comms (a name that
stays taken, an unsupervised `summonhelper`, a helper not running after S3),
the run hands `com.tesla.Webrtc` back to stock for this boot after S3, as the
`SUMMON=0` removal does: once no `summonhelper` process runs (up to 10 s),
and only if the name is free, stock is restarted and checked; a name that
has an owner starts nothing. A `summonhelper` that does not go away, or a
stock that does not register, is reported at the end; neither stops the run.
A skip before the stop sequence (a "no" at S0b, a missing tool, a failing 4b
or 4c command) never stopped stock, so nothing is handed back.

The same holds on the way out: any exit after S0b's stop sequence, before that
hand-back or the helper's passing checks, runs the run's EXIT trap
(`exit_hand_back`). That is a die (a "no" at S2, S3 or S4, an unsupervised agw
or camhelper, agw or camhelper not running), an error, `Ctrl-C` or TERM (exit
130 or 143), or the end. It decides:
- our helper is linked (its `airgapp-summonhelper` link, which S3 makes) and
  supervised by a running runsv (its runsv is up and has not been told to stop
  it or keep it down): it is the installed owner, so nothing is touched. After
  failed checks it stays linked and running, as the DONE text says;
- otherwise it starts nothing: it is not linked (a first install that stopped
  before S3, or a skip that unlinked it), or its link has no running runsv
  (after a boot until S3 starts our runsvdir, since the link stays on /var/lib
  from one boot to the next), or its runsv was told to stop it or keep it down
  (S0b's `sv down`, in a run whose runsvdir was already up; the helper may
  still be stopping). Then its runsv, if one still runs, is brought down;
  then, checking every 1 s up to 10 times, once no `summonhelper` process runs
  and the name is free, stock is restarted and checked (`install-car: on the
  way out: … restarting stock`). If a `summonhelper` process remains, a line
  names its pid; if the name stays owned (stock still letting go of it, say),
  a line says so; either way stock is not restarted.

It never dies and keeps the run's exit status. Neither a dropped SSH session
nor a dead reader of the output can kill the run or its exit path: the run
ignores SIGHUP and SIGPIPE, and every command it starts inherits that. Over
the dead session, the run ends at its next question ("no answer"; S0b's
question takes it as a no, a skip, and goes on) or at its next write there
that `set -e` checks (an I/O error on a terminal, a broken pipe otherwise),
and the exit path runs. A failed write that `set -e` does not check (in a
condition, or before `||` or `&&`) ends nothing: the next question, checked
write or the run's end does. A run whose answers and output do not go through
the session goes on to its end. Not covered: SIGKILL and the other signals
the run does not trap, which end it without the exit path.

A failed check after S4 is different, on purpose: the helper stays linked and
running, because it is harmless and you may want to look at it, and it keeps
the name. The DONE text then shows no pin route for it and points to the
report, and the run ends with the report (`Summon checks failed: …`) and a
non-zero exit. An unsupervised agw or camhelper still stops the install: that
is the camera's rule.

**`SUMMON=0`** installs nothing for Summon. On a car that has the helper it
removes it and hands the name back to stock webrtc-comms (asked). The
removal decides on our helper's processes, never on the name: after a reboot
stock owns the name and our helper does not run, which is the end state the
removal wants. In order:
1. the `airgapp-summonhelper` link and the helper's `run` go first, so no
   later `--after-boot` can bring it back;
2. `sv -w 15 down` the helper, then wait, 1 s apart for up to 10 s, until no
   `summonhelper` process runs. If one lingers, the removal stops there:
   webrtc-comms is left alone, the service directory stays without its `run`,
   and the message names the pid. Kill it or reboot, then run `SUMMON=0`
   again. agw and camhelper are still set up, and the run ends with the
   report, non-zero. Once none runs, a stale `/run/airgapp-summon/live` is
   removed: no summonhelper holds its lock then, and camhelper's SUMMON mode
   needs that lock held;
3. if stock already owns `com.tesla.Webrtc`, nothing is started;
4. if the name is free, webrtc-comms is restarted, not only brought up:
   `sv -w 15 down`, its orphaned jail killed as in S0b, then `sv up`. Stock
   registers the name once, at its start, and a stock that a run-mode switch
   restarted while our helper held the name runs without it, which `sv up`
   alone would not change. Then it waits for stock to register (10 checks,
   1 s apart: up to about a minute). Without a runsv (a loadshed or lowpower
   run mode) it notes that stock registers at its next infotainment or
   sentry start. If stock does not register, that never keeps agw or
   camhelper down: the install goes on, and it is reported at the end with a
   non-zero exit;
5. the service directory is deleted. The binary and the helper's key stay.

**The check-4 tool.** `summonhelper probe` is the owner's no-motion car check
(`docs/SUMMON.md` has the runbook; run it only as written there). It needs
`/run/airgapp-summon` to exist, has no `-marker` flag (it always uses the
marker `finish` and the supervisor check), and sends no heartbeat until the
car's connect-edge `autopark:style` arrives.

## After every boot: `--after-boot`

iptables rules do not survive a reboot, and `/run` (where the car's runsvdir
directory and the helpers' state live) is tmpfs. After every car boot run
`install-car.sh --after-boot`: it lays the egress lock again (5c), redoes the
exec mounts (S0), runs the stop sequence again when summonhelper is installed
(S0b), rewrites each service script that differs from what its own templates
make, with `PI_IP` and the authd pin read back from the installed scripts
unless given in front (`PI_IP=<ip>`, `AUTHD_SHA256`), and starts our own
runsvdir again (its links on /var/lib persist). It installs no binary and no
key.

Because it writes its own templates, run the installer that made the last
install: **refresh the kept `/var/lib/airgapp-gw/install-car.sh` after every
install**, as root, with `install -m 0755 -o root -g root
/tmp/airgapp-install/install-car.sh /var/lib/airgapp-gw/install-car.sh`. An
older copy puts its older scripts back at the next boot (for `841d3df`'s, a
summonhelper run script without the live-file removal).

- `SUMMON` unset follows the installed `sv/summonhelper/run`, as on a full
  install. `SUMMON=0` runs the removal above. `SUMMON=1` with no helper
  installed skips Summon for that boot ("install with SUMMON=1 first"); a
  `SUMMON` that is neither 0 nor 1 stops before anything.
- A Summon problem after a boot never keeps the camera down. If the name
  stays taken, if `gdbus` or `timeout` is missing, or if S0b gets anything
  but a typed `yes` (no stdin too), Summon is skipped for that boot: agw and
  camhelper start as usual, the helper is neither linked nor started (its
  link is removed until the next run), and the run ends with the Summon
  report and a non-zero exit; if S0b had stopped stock, it gets the name back
  ("Failures"). A failed check is reported at the end too, but the helper
  stays linked and running ("Failures"). With
  `SUMMON=0`, anything but a typed `yes` at the removal's ask skips the
  removal for that boot the same way: the helper is unlinked and not
  started, its directory stays as it is, and the report says the removal
  was declined. A full install does the same once it has written anything
  ("Failures" above).
- An authd pin that does not match the car's `/usr/bin/authd` (or an authd it
  cannot hash) is a `NOTE:` that blocks nothing, repeated at the end as
  `!!! NOTE:`; the run then exits non-zero, though every service was started
  ("No account for uid 4950"). Its last line is `install-car: the authd pin
  check failed (the NOTE above); every service was started`, or the Summon
  report instead when there is one.
- An agw `run` left by the old `SUMMON=1` (agw under its own supervisor) is
  rewritten as plain `serve`.

## Phase 1a: the sign-in pre-check (owner present, read-only, no motion)

Before installing, the read-only sign-in check is the camera runbook's step 2b
(`docs/CAMERA-INSTALL.md`, "2b. Sign-in pre-check"). As
root it runs `deploy/signin_precheck.sh` from the root-owned `/tmp/airgapp-install`
of its step 2, with a temporary camhelper on its own state and sockets, so it
also works next to an installed camhelper (after an OTA, for example). It
picks the authd pin from the car's `/usr/bin/authd` hash, refuses an unknown
build, and asks as uid 4950: `owner=ok role=2 fp=<key fp> id=<authd key
Id> ff=6 secs=0` (role 2 OWNER or 3 DRIVER). Sign-in calls only
`AuthOwnerIsAuthd` and the `KeyInfo` lookup, never `KeyRole` (the bus policy
grants that only to `command-router` and root). "hashing authd" or "authd
changed" means the authd pin failed. The Phase 1a this section used to give,
agw's old direct-bus probe-auth (verified on the car 2026-11-20, agw
5f43e293…), is gone: agw's `probe-auth` now asks camhelper only (its
`-authd-sha256` is gone), so it cannot run before camhelper is installed, and
step 2b's pre-check, which brings its own temporary camhelper, replaces it.

After installing, uid 4950 asks through camhelper's sign-in socket about a
phone key (its hex, 65-byte uncompressed P-256; comma-separate several):

    chpst -u :4950:4950 /var/lib/airgapp-gw/agw probe-auth -pub <hex>

`role=2` (OWNER) or `role=3` (DRIVER) signs in; anything else is refused.

## R22

The gateway and this script never send anything to `192.168.90.0/24`. Do not
add routes, rules or tests that do.

## Camera root helper

The marked section at the end of the script
(`# ==== CAMERA ROOT HELPER (owned by the camera session) ====`) owns both
runit services and the per-boot step; only the camera session edits it.
Helper layout, protocol, release order and the car-visit checklist:
`docs/CAMERA-HELPER.md`.

| Path | Owner | Mode |
|---|---|---|
| `/var/lib/airgapp-camhelper/camhelper` | root:root | 0755 |
| `/var/lib/airgapp-gw/sv/{agw,camhelper}/{run,finish}` | root:root | 0755 |
| `/run/airgapp-camhelper/` (socket dir, made by the helper) | root:4950 | 0750 |
| `/run/airgapp-camhelper/camhelper.sock` | root:4950 | 0660 |
| `/run/airgapp-camhelper-state/` (markers, lock) | root:root | 0711 |
| `/run/airgapp-camhelper-state/cut/` | 4951:4951 | 0700 |
