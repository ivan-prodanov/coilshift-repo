# Runbook: camera, talk and dashcam on the car gateway (first install)

> Moved here on 2026-11-26 from `tesla-dump/2026.8.6/re/bridge/camera/RUNBOOK-camera-gateway-install-2026-11-20.md`
> (that folder is frozen and will be deleted). Its scripts are `deploy/signin_precheck.sh` and `deploy/probe_media.sh`.

> **Since 2026-11-27 the car runs this branch in direct mode** (agw `serve -tailnet` on the car's own tailnet, no Pi in
> the path), installed and supervised by CoilShift's Direct Link and Direct Summon packages: see
> `tesla-dump/docs/direct-mode-no-pi-2026-11-27.md`. The steps below are the manual install over the Pi dial-out, kept
> for the record and for a re-pin after an OTA (step 2b's pre-check works the same in direct mode).

> **On 2026-11-26 the car runs camera and Summon from this branch, installed with `SUMMON=1`.** An update install
> stages all three binaries (agw, camhelper, summonhelper) and install-car.sh, runs step 2b, then installs with
> `SUMMON=1` as `docs/SUMMON.md`'s install section says; that section also lists the current build's hashes, and
> `deploy/signin_precheck.sh` pins agw's and camhelper's. Steps 0 to 6 below are the camera's first install
> (2026-11-25); steps 2, 2b, 3, 5 and 6 are what an update reuses. The camera's Summon strip needs summonhelper's
> live file (`docs/CAMERA-HELPER.md`, op `summon`), so camhelper and summonhelper always go in one install.

> **The car dials out to the Pi (owner's choice, 2026-11-21).** The Pi cannot reach the car over Wi-Fi. The MCU's
> `wlan0` (192.168.21.2, gw 192.168.21.1) is not a radio. The car's Wi-Fi module joins the Pi hotspot as
> 192.168.4.233 (lease `tesla`), NATs the MCU and drops every inbound packet: Pi → .233:13400/13401/17100 timed out,
> and tcpdump on the MCU's `wlan0` saw 0 packets. Car → Pi works. So agw runs `serve -dial 192.168.4.1:17101`, keeps
> idle connections open to the Pi, and the Pi pairs each phone connection on its tailnet :17100 with one of them.
> There is no INPUT rule on the car.
>
> **Ready (2026-11-21):** agw's `-dial` with the review's hardening (gateway `feat/agw-dial-out-hardening` `e04cd11`,
> fast-forwarded into `feat/camera-capabilities`) and the Pi's pairing build with the same hardening (rpi
> `feat/gateway-pairing-hardening` `8f04069`). Both come from the "Summon airgapped car findings" session (which owns
> the gateway core and rpi gwforward) and its follow-up session. Race tests pass three times on the merged branch.
> Hashes are in step 0.1. The branch tip is now `7cec32f` (2026-11-24): one test-only fix on top of `e04cd11` (the
> camera e2e test waits for the session log line), and the three car files build byte-identical.
>
> **Firmware (2026-11-22): the car is moving from 2026.8.6 to 2026.38.6.** Everything below was car-verified on 8.6 only.
> On 38.6 authd is a different build (`/usr/bin/authd` sha256 `e0c74323…`, 8.6's is `33d62d9e…`), so camhelper's
> compiled-in pin fails closed and nothing signs in. KeyInfo's signature is unchanged (`a(ayayuut)`, both dumps), so no
> rebuild: step 2b's pre-check finds the build, proves sign-in with the matching pin, and step 3 then passes
> `AUTHD_SHA256`. Also on 38.6 (dump diff): every DV the camera uses is still there, and PipeWire's webcam/megaphone/TDM
> routing is identical. But videod, pw-cat, pw-link, gst-launch, aplay and ffmpeg are new builds, and `/etc/camera.conf`
> no longer has the left/right pillar sections (ports 9893/9892). So step 1's probe and step 7's checks are the real
> verification there.
>
> **Already done on 2026-11-21, without an install:** bus cache cleared (root ReloadConfig; uid 4950 refused, rc 134),
> and sign-in as 4950 through a temporary camhelper passed (`owner=ok role=2 id=<key id> ff=6`; uid 4952 refused at
> the socket).

Topology: phone → (Tailscale) → Pi `:17100` dumb forwarder ← (Pi hotspot, car-initiated) ← car `agw` dialling
`192.168.4.1:17101` ↔ root `camhelper`. Code: `~/Work/airgapp/gateway-camera` (branch `feat/camera-capabilities` at `7cec32f`, `e04cd11` plus a test-only fix: the
camera work, the gateway core's helper client (`feat/agw-auth-helper` `5f6759f`, merged 2026-11-20) and agw's dial-out
with its hardening (`feat/agw-dial-out` then `feat/agw-dial-out-hardening`, merged 2026-11-21)). Phone app: already installed (Release build 2026-11-20 with GatewayClient, GatewayVideo, the
dashcam file sink and CATCHUP). The gateway core and Phase 0 belong to the "Summon airgapped car findings" session.
Its Phase 1a (`probe-auth`) is step 2b here and runs BEFORE anything is installed. **Sign-in is helper-first (owner's
choice 2026-11-20):** uid 4950 gets no account and no bus access; camhelper (root) answers its sign-in on
`/run/airgapp-camhelper/auth.sock`. agw has the helper client and no D-Bus path. The install needs a car reboot
after the 2026-11-20 overlay test first (step 0.5): dbus-daemon still remembers uid 4950 until then, and agw's
start-up self-test refuses to serve while it can reach the bus. (2026-11-21: a root ReloadConfig cleared it instead of
a reboot; step 0.5 re-checks.) **Phase 0 (the lockdown runbook)
is a separate visit:** it bars camera/talk/dashcam as verification, and its final policy has no phone → Pi :17100 grant
yet (Phase 4, N8). SUMMON stays off on this visit (no `SUMMON=1`).

## Rules for the whole visit

- **People and car state:** owner present, car parked (P), Sentry Mode armed, Live Camera access enabled on the car.
  Be ready before waking the car: it re-sleeps about 5 minutes after waking.
- **R22:** never send anything to 192.168.90.0/24.
- **Process kills:** never kill by name (the dashcam/Sentry recorders are gst-launch-1.0); stop our services with `sv`.
- **No file moves over SSH:** the session recorder fills and sshd locks us out (E22729), and the dev sshd has no
  scp/sftp. The car pulls files with `curl` (step 2).
- **Keep SSH output small.** Never print the VIN or a pin.
- Every install write asks for a typed `yes`.

## 0. Before the visit (laptop, no car contact)

1. Build: `cd ~/Work/airgapp/gateway-camera && make car && shasum -a 256 out/agw out/camhelper out/summonhelper deploy/install-car.sh`.
   Write the hashes down; they must match `docs/SUMMON.md`'s install section and, for agw and camhelper,
   `deploy/signin_precheck.sh`. agw is the gateway core's build with the helper client (it asks camhelper's sign-in
   socket), camhelper the build with the sign-in socket and typed error codes, install-car.sh the one without the
   account step (no `--account`) and with the dial-out (no INPUT rule). agw must have `serve -dial`.
   **History: the camera's first install (2026-11-21, `e04cd11`; reproducible, and the follow-up session's own build
   matches; `7cec32f` builds the same three files, rechecked 2026-11-24):**
   - agw `15318dead3bb7bc5e2266f96bcf2fd8204dd5fff3f7ee3f176b3f5c544ae1be7` (`a689828`'s `dadb3394…`, before the
     hardening, is superseded)
   - camhelper `bf3fce9f2afd2eb4ba58fad1c792f1965a603b797b33ef5eaa469b3160e2b40c` (unchanged since 2026-11-20)
   - install-car.sh `ca0e425442ab3b3987af6f85fdbd2cb53841ea700ce8c0a9f3796a78ea761058`

   The 2026-11-20 build (`67fa98d`: agw `072abc03…`, install-car.sh `5e9ae2d6…`) predates the dial-out and must not be
   installed.

   agw and camhelper go together. Recompute after any change.
2. Stop the laptop relay, which holds the same webcam DataValues:
   `kill -TERM $(pgrep -f "live_viewer_tcp.py")`. `out/relay.log` must end with `RELEASED` and `bye`.
3. Pi forwarder. As of 2026-11-21 the Pi (raspberrypi-1) runs rpi be3120a (the old Pi → car forwarder, which can
   never reach the car), with `gw_car_addr=192.168.4.233` and `gw_car_mac=<module-mac>` (the car's Wi-Fi module;
   its static lease is in place).
   - **Do this before waking the car.** Every netfilterd start restarts hostapd, so the install is a hotspot drop for
     the car (its Wi-Fi module must rejoin), not a short blip.
   - Keep a copy of the old binary for rollback first:
     `ssh pi@raspberrypi-1 'sudo cp /usr/local/bin/netfilterd /usr/local/bin/netfilterd.be3120a'`.
   - Deploy the hardened pairing build from the follow-up session's worktree (it keeps it until this deploy; the main
     `~/Work/airgapp/rpi` checkout is still on be3120a and `rpi-pairing` on the unhardened c88ec06, so run
     `make install` in neither):
     `cd ~/Work/airgapp/rpi-pairing-hardening && git log --oneline -1` must show `8f04069`, then
     `make install RPI=pi@raspberrypi-1` (arm64 build, copy, `systemctl restart netfilterd`).
   - netfilterd now logs `[GATEWAY] stopped accepting phones on …` and `… the car's connections on …` at every
     forwarder stop (a Reconfigure restart included), and on an Accept error `[GATEWAY] accepting … failed: …
     (retrying with backoff)`, then `… again after <n> failed accepts`. Its firewall opens 17100 and 17101 only while
     `gw_car_addr` is valid.
   - `gw_car_addr` must stay the module's hotspot lease, 192.168.4.233: the Pi sees the module's NATed source, not the
     MCU's 192.168.21.2. If it changes, the car is refused until the module takes the new lease.
   - Expected after the restart: phones on the Pi's tailnet :17100, cars on 192.168.4.1:17101, the latter only from
     `gw_car_addr`. Confirm with `sudo ss -ltn | grep -E ':1710[01] '`: only the pairing build listens on
     192.168.4.1:17101. netfilterd opens both at its first Tailscale status read (right after it starts, or up to a
     minute after Tailscale is up); while a status read is not Running, both stay closed until the next good one.
   - **Reconnects (agw redials on its own; nothing to do on the car).** agw redials 1 s after losing a connection and,
     while dials fail, backs off, doubling from 1 s to 10 s:
     - Only the Pi's forwarder restarts (netfilterd applying a new tailnet address): pairable again about 1 s later.
     - The Pi refuses the dials (nothing listens on 17101): the dials fall about 1, 2, 4, 8, 16 and 26 s after the
       loss, then every 10 s. Once 17101 listens again the car is back within about 2 s after an outage of up to
       about 4 s, about 3 s after a 5 s one, and up to 10 s after a longer one.
     - A netfilterd restart (a crash, or this step's `make install`), a Pi reboot or a hotspot drop: up to about 13 s
       after the car's module is back on the hotspot and 17101 is open.
     - If no close reached the car (a power cut, or a hotspot drop its connections do not survive), it may notice
       only after 25 s with nothing heard, then redials 1 s later. After an MCU suspend/resume expect up to about
       10 s (one keepalive interval; an estimate, not measured).

     A phone that connects before the car is back waits up to 3 s for a car connection, then is closed. agw logs the
     first connection lost before pairing (`… ended before pairing: … (any more are counted until the Pi keeps one)`)
     and then `agw: the Pi at … keeps a connection again, after <n> ended before pairing`; both are expected after a
     Pi restart.
   - Note the Pi's **tailnet** address (100.x.y.z): it is the phone's `host`.
4. Pull up the phone key. Open Live Camera on the phone once (the gateway is not provisioned yet). The app logs the
   public key without showing it on screen:
   `bash ~/Work/airgapp/mobile/scripts/godot-ios/pull-logs.sh` then grep for `device key 04…` (130 hex, `native=true`).
5. **The bus must refuse uid 4950.** The 2026-11-20 overlay test let dbus-daemon resolve uid 4950, and it keeps that
   until it restarts or reloads. A root `dbus-send --system --type=method_call --print-reply
   --dest=org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus.ReloadConfig` cleared it on 2026-11-21 (the
   owner's "reboot" had not restarted the MCU). Without it agw logs `BUS SELF-TEST FAILED` and never serves. Confirm as root:
   `chpst -u :4950:4950 dbus-send --system --print-reply --dest=org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus.GetId >/dev/null 2>&1; echo rc=$?`
   must print a non-zero `rc`. rc=0 means the bus still takes 4950: stop, nothing below will serve.

## 1. Read-only probe (car, root)

```bash
ssh -p 13400 root@<car> 'PI_IP=192.168.4.1 PI_PORT=17101 bash -s' < ~/Work/airgapp/gateway-camera/deploy/probe_media.sh
```
Since 2026-11-25 the probe keeps its temporary files (the talk writers' error output, the `--with-stream` watchdog)
in a fresh root-only `mktemp -d` folder, never at fixed `/tmp` names, and a signal ends it after the release; the
output is unchanged. If any port shows `NO_STREAM`, re-run with `'bash -s -- --with-stream'`. On 2026.38.6 the pillar ports (9892/9893)
may show `NO_STREAM` even then: its `/etc/camera.conf` dropped their sections. Note it; the pillar tiles would stay
empty, and nothing else depends on them. Decide from the output:

| Probe line | Means |
|---|---|
| `pw-cat stdin: rc=0` | default talk writer; else `aplay stdin: rc=0` → install with `HELPER_WRITER=aplay`; neither → no talk |
| `port …: helper>0` | uid 4951 captures work; `BLOCKED_FOR_NONROOT` → stop, captures need a change |
| PipeWire `helper>0` | uid 4951 can write talk audio |
| `vin readable by 4950: yes` | gateway can bind the pin to the car |
| `4950 -> 192.168.4.1:17101: rc=…` ok once the pairing build is in and 17101 is open (step 0.3); until then it times out (be3120a's input chain drops 17101), which says nothing about the car's Wi-Fi module | agw can dial the Pi through the car's Wi-Fi module |
| `runsvdir: …` | the runsvdirs running on the car, for the record: the install always uses its own runsvdir |

Also: `chpst -u :4951:4951:29:1021 head -c 8 /mnt/usb/TeslaCam/RecentClips/$(ls /mnt/usb/TeslaCam/RecentClips | head -1) | od -c | head -1`
(uid 4951 reads clips; if refused, cuts fail and the app falls back to whole files) and `df -h /run`.

## 2. Car pulls the files (no SSH transfer)

Laptop: `cd ~/Work/airgapp/gateway-camera && mkdir -p /private/tmp/agw-stage && cp out/agw out/camhelper out/summonhelper deploy/install-car.sh /private/tmp/agw-stage/ && cd /private/tmp/agw-stage && python3 -m http.server 17180 --bind <laptop-tailnet-ip>`

Car (one SSH line). The files go into a fresh root-owned folder, never to fixed names in `/tmp` itself: another uid
could plant a symlink or a file of its own there, and root would follow or reuse it. `mkdir` without `-p` fails if
anything is already in the way, and then the line stops:
```bash
rm -rf /tmp/airgapp-install && mkdir -m 0755 /tmp/airgapp-install && cd /tmp/airgapp-install || exit 1; for f in agw camhelper summonhelper install-car.sh; do curl -fsS -o $f http://<laptop-tailnet-ip>:17180/$f || echo FAIL $f; done; sha256sum agw camhelper summonhelper install-car.sh; chmod 0755 agw camhelper summonhelper
```
The hashes must match step 0.1 exactly. Then stop the laptop http.server (Ctrl-C).

## 2b. Sign-in pre-check (car, root, read-only; nothing installed)

Uses `agw` and `camhelper` from step 2 in `/tmp/airgapp-install`, and refuses if that folder is not root-owned and
writable only by root. It reads `/usr/bin/authd`'s hash and picks the pin, refuses an unknown authd build, runs a
temporary camhelper as root with that pin, and asks it as uid 4950. The temporary camhelper has its own state and
sockets (`/run/airgapp-signin-precheck`, `/run/airgapp-signin-precheck-sock`), so an installed camhelper is left alone
and the check also works after an OTA (see "After an OTA"). Then it kills the temporary camhelper by its exact pid and
removes its files. Capture to a file (never pipe through `head`):
```bash
ssh -p 13400 -o LogLevel=ERROR root@<car> "PHONE=$PHONE sh -s" < ~/Work/airgapp/gateway-camera/deploy/signin_precheck.sh > /private/tmp/agw-stage/precheck.txt 2>/dev/null; cat /private/tmp/agw-stage/precheck.txt
```
Must show `hashes: ok` and the authd line:
- `2026.8.6 build`: step 3 runs without `AUTHD_SHA256`.
- `2026.38.6 build`: the pin is `e0c74323da8f638b2d21d823cd989e0f6b43dfcf24b9db3e68aa9520057c1c0f`. A first install
  passes it as `AUTHD_SHA256`. An update on a car whose camhelper already runs with this pin may leave
  `AUTHD_SHA256` unset: the installer keeps the pin from the installed run script, which is what `docs/SUMMON.md`'s
  install section does. Pass it only when the pin changes (after an OTA, below).

Then, as uid 4950, `owner=ok role=2 fp=<fp> id=… ff=6 secs=0` with `rc=0`, then `permission denied` for
uid 4952, then `cleanup: done (… gone: yes)`. On 8.6 this passed on 2026-11-21 (id=<key id>). On 38.6 it is the first
proof that KeyInfo means the same. Any other result (an unknown authd build, an error class such as `key_unknown` or
`shape`, or rc≠0): **stop**, install nothing, and report it. Nothing can arm, the camera included.

## 3. Install (car, root, interactive)

```bash
ssh -t -p 13400 root@<car> 'cd /tmp/airgapp-install && SUMMON=1 PI_IP=192.168.4.1 sh install-car.sh ./agw ./camhelper ./summonhelper'
```
On a first install on 2026.38.6 (step 2b said so) put
`AUTHD_SHA256=e0c74323da8f638b2d21d823cd989e0f6b43dfcf24b9db3e68aa9520057c1c0f` before `PI_IP`; the installed
camhelper (and summonhelper) run scripts keep it, so an update leaves it unset, and `--after-boot` reuses it. Add `HELPER_WRITER=aplay` before `sh` if
step 1 said so. `SUMMON=1`: the car serves Summon since 2026-11-26, and its part of this install (the stop of stock
webrtc-comms, the checks) is `docs/SUMMON.md`'s install section, owner-only. The camera's first install (2026-11-25)
ran without `SUMMON` and without summonhelper; `SUMMON=0` would remove Summon. Answer `yes` to each step. S3 always
uses OUR runsvdir on `/var/lib/airgapp-gw/svdir` (with setsid), never another one. Logs: svlogd, 3 x 1 MB, in `/var/lib/airgapp-gw/svlog/agw` and
`/var/lib/airgapp-camhelper/svlog`. Afterwards:
`sv status /var/lib/airgapp-gw/sv/agw /var/lib/airgapp-gw/sv/camhelper` → both `run:`, and
`grep -c "bus self-test passed" /var/lib/airgapp-gw/svlog/agw/current` → 1 or more. A `BUS SELF-TEST FAILED` line
instead means agw waits 60 s and exits, over and over: go back to step 0.5.
`grep -c "dialing the Pi at 192.168.4.1:17101" /var/lib/airgapp-gw/svlog/agw/current` → 1 or more, and on the Pi
`sudo ss -tn state established '( sport = :17101 )'` lists 3 connections from 192.168.4.233 while idle. None there and
`cannot reach the Pi` in agw's log: check step 0.3's `ss -ltn` and `gw_car_addr`.
Keep the installer for the per-boot step (it does not copy itself):
`install -m 0755 -o root -g root /tmp/airgapp-install/install-car.sh /var/lib/airgapp-gw/install-car.sh && rm -rf /tmp/airgapp-install`.

## 4. (No allow step)

There is no allowlist and nothing to add here: access is the car's own. A phone signs in when it proves possession of
its key and the car's authd lists that key as OWNER or DRIVER (paired in the Tesla app or with the key card). Removing
the key from the car ends its open sessions at the next authority re-check (agw: at once on authd's key change, else
within 30 s, or 10 s while the key watch is down; summonhelper: within 10 s). An `/var/lib/airgapp-gw/allow` left by an
older install is no longer read.

## 5. probe-auth again, through the installed camhelper (read-only)

```bash
chpst -u :4950:4950 /var/lib/airgapp-gw/agw probe-auth -pub <phone key hex from step 0.4>
```
This goes through camhelper's sign-in socket. Must print `owner=ok role=2` (or 3), as in step 2b. Any error: **stop**
and roll back. The error names its class: `key_unknown` (the phone key is not paired with the car), `auth_owner`
(com.tesla.Auth is not authd), `authd_changed` (an OTA changed authd: re-verify KeyInfo), `bus`, `shape`, `timeout`.
uid 4950's bus refusal was checked in step 0.5 and by agw's self-test in step 3.

## 6. Provision the phone

The pin holds the VIN: never print it. Pull it the reverse way, with no SSH transfer. First, on the laptop:
`nc -l 17181 > /private/tmp/agw-stage/gwpin`. Then on the car, one line. `mktemp` makes a new 0600 file that
nobody else can have planted, and the line deletes it whatever happens. `agw pin` runs as uid 4950: the key is
4950's, and agw refuses to load a key it does not own (as root it would print nothing):
```bash
P=$(mktemp) && chpst -u :4950:4950 /var/lib/airgapp-gw/agw pin -key /var/lib/airgapp-gw/state/key.pem > "$P" && wc -c < "$P" && socat -u FILE:"$P" TCP:<laptop-tailnet-ip>:17181; rm -f "$P"
```

Build the link on the laptop and send it to the phone (AirDrop or Notes):
`airgapp://gw-pin?pin=<url-encoded pin>&host=<pi-tailnet-ip>&port=17100`

Tap it on the phone and confirm. Delete `/private/tmp/agw-stage/gwpin` afterwards.

## 7. Silent checks (phone + car read-only)

On 2026.38.6 these are the first proof that videod (a new build) still behaves as found on 8.6, so run all of them.
That means the exterior selectable lever, the interior hold, and talk reaching `TDM-playback` 9/10.

1. **Live Camera:** Front is the windshield ULTRAWIDE (selectable 3), undistorted, with no camera housing along the
   bottom. If it shows the bumper (a round fisheye at bumper height), the build predates the Front change. Fascia is
   the bumper (port 9901). Each exterior camera switch should be near-instant (catch-up burst). Grid fills all tiles
   at once.
2. **Interior:** the picture appears. Back to Front, then on the car:
   `/usr/local/bin/get-local-dv CarServer GUI_selectableCameraRequest` → `3` (Front's ultrawide is held while parked).
   After leaving Live Camera it must read `0`. The session log has the interior hold event.
3. **Talk armed, mic NOT pressed:** with Live Camera open on the car,
   `pw-link -l | grep -A2 "webcam:monitor"` must show `TDM-playback:playback_9` and `playback_10`.
4. **Dashcam:** the list loads, a SavedClips event plays, and Save downloads the minute.
5. **Drills:**
   - `kill -9 $(cat /var/lib/airgapp-gw/sv/agw/supervise/pid)`. Within about 2 s,
     `get-local-dv CarServer WEBCAM_webcamSessionActive` must read false. runit restarts agw and the phone reconnects.
   - `kill -9 $(cat /var/lib/airgapp-gw/sv/camhelper/supervise/pid)`. The runit finish (`camhelper sweep`) must clear
     the same DVs, and `ls /run/airgapp-camhelper-state` must show no `ra`/`sa`/`sel`/`gain` markers.
6. **Load while parked** (warm captures are unmeasured): `top -b -n 1 | head -20`. Note agw, camhelper and the
   gst-launch children owned by uid 4951 (one per port, shared, at most 10).
7. **Latch readback:** after leaving Live Camera,
   `grep -c "LATCH NOT CLEARED" /var/lib/airgapp-camhelper/svlog/current` must be 0 and `ls /run/airgapp-camhelper-state`
   must show no markers. If the readback format differs from `(true, 'false')` / `(true, 'None')`, the helper keeps
   retrying: report it.

## 8. One ear test (owner by the car)

Hold the mic in Live Camera and say a sentence. It plays from the external speaker at the push-to-talk level (0 dB, above Tesla's own −6 maximum; owner's choice). Listen for distortion; if it rattles, rebuild with −6.

## Rollback (any step)

```bash
sv down /var/lib/airgapp-gw/sv/agw /var/lib/airgapp-gw/sv/camhelper; /var/lib/airgapp-camhelper/camhelper sweep
rm -f /var/lib/airgapp-gw/svdir/airgapp-agw /var/lib/airgapp-gw/svdir/airgapp-camhelper
for t in iptables ip6tables; do $t -D OUTPUT -m owner --uid-owner 4950 -j AIRGAPP_GW_OUT; $t -F AIRGAPP_GW_OUT; $t -X AIRGAPP_GW_OUT; done
```
Our idle runsvdir stops supervising the removed links within 5 s and is gone at the next boot.
Pi, if the pairing build misbehaves: `ssh pi@raspberrypi-1 'sudo cp /usr/local/bin/netfilterd.be3120a
/usr/local/bin/netfilterd && sudo systemctl restart netfilterd'` (the copy from step 0.3).
Then restart the laptop relay (`bash run_relay.sh front`) to get the old Live Camera back.

## After every car boot

`sh /var/lib/airgapp-gw/install-car.sh --after-boot` (the copy kept in step 3). It reads `PI_IP` back from the
installed agw run script (put `PI_IP=<ip>` in front only to change it). /run is tmpfs, and neither iptables rules (the
egress lock) nor mounts (the exec mounts) are saved. It keeps the installed SUMMON mode.

Since 2026-11-26 the car runs camera and Summon from this branch with `SUMMON=1`, and plain `--after-boot` works: the
kept installer always uses our own runsvdir and needs no `RUN_ROOT` (a `RUN_ROOT` or `PROCFS` in the environment is
only noted).

## After an OTA (gateway already installed)

An OTA can bring a new `/usr/bin/authd`. camhelper then fails sign-in closed: step 5's `probe-auth` says
`authd_changed` and no phone can arm. Installers from the Summon session's follow-up on also print a `NOTE: camhelper's
authd pin … does not match this car's /usr/bin/authd` at the end of `--after-boot`. They never adopt the car's hash
as the pin. Re-pin only after a sign-in proof:

1. Laptop: dump the new firmware and check that authd's KeyInfo is still `a(ayayuut)` (the 38.6 check is in
   memory `camera-on-38-6`). Add the new authd hash to `signin_precheck.sh` as a known build, because the
   pre-check refuses unknown builds.
2. Steps 2 and 2b, with the build that is on the car (gateway feat/camera-capabilities; its hashes are in
   `docs/SUMMON.md`'s install section, and agw's and camhelper's in `signin_precheck.sh`, which refuses any other).
   The pre-check runs its own camhelper on its own sockets, next to the installed one, which keeps running untouched.
3. Step 3 again, from `/tmp/airgapp-install`, with `AUTHD_SHA256=<the new hash>` in front (step 3 already sets
   `SUMMON=1`; its Summon part follows `docs/SUMMON.md`). This is the full install on an installed car: the keys
   stay, camhelper's and summonhelper's run scripts get the new pin, and S4 restarts them.
   `--after-boot` then keeps the new pin.
4. Step 5 must print `owner=ok role=2` again.
