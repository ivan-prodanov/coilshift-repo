# airgapp gateway

Car-side gateway `agw`: one authenticated entry point on the car for the phone's
non-BLE features (camera, talk, dashcam, and the relay to Summon's helper).
Static linux/amd64 binary for the Tesla MCU. Design: `tesla-dump/2026.8.6/re/bridge/PLAN-B2-summon-productionization-2026-11-19.md`
(v1.3.7, §4 and §4.9). Plan: `docs/superpowers/plans/2026-11-20-shared-gateway-core.md`.

Path: phone → (tailnet TCP) → Pi `:17100` (dumb forwarder, airgapp/rpi) ←
car agw, which dials the Pi's hotspot address on `:17101` (the car's Wi-Fi
module NATs the MCU and drops all inbound, car-proven 2026-11-21); the Pi
pairs each phone connection with one idle car connection and pipes bytes.

## Build and test

- `make test` runs the Go tests.
- `make car` builds `out/agw`, `out/camhelper` and `out/summonhelper` (static).
- `make interop` builds the Swift/CryptoKit interop CLI and checks it against
  the Go side (macOS 14+).

## Done (bench-verified, not yet on the car)

- Shared login, no allowlist: signed challenge → HPKE Auth arm (possession of the phone key)
  → authd role ∈ {OWNER, DRIVER} (the car's own keys: Tesla app / key card),
  read from `com.tesla.Auth.KeyInfo` with authd pinned by SHA-256, asked through
  camhelper's sign-in socket (plan amendments 2026-11-20). Open windows are
  re-checked when keys change and every 30 s (10 s while the helper's key watch is
  down); a failed check revokes the window.
- Window manager: one window per capability, pre-auth limits, grammar and size
  limits, 5 s / 20 s heartbeat lease, two-lane outbound, ordered teardown, session log.
- Capability API for other features: `docs/CAPABILITY-API.md`, `capability/testkit`.
- Bench client: `client.Dial`; bench server: `agw serve -bench -fake-role 2 -listen 127.0.0.1:17100 …` (or `-dial <pi-ip>[:17101]`: the car connects out to the Pi).
- Direct mode (2026-11-27): `agw serve -tailnet` listens on the car's own tailnet address (the one IPv4 on
  `tailscale0`, port 17100), so the phone reaches the car over Tailscale with no Pi in between. Inbound on
  `tailscale0` passes the car's firewall (`ts-input` accepts it); the Wi-Fi module's NAT does not apply to the tunnel.
  `-listen` and `-dial` keep refusing 100.64.0.0/10: `-tailnet` is the only way to bind a tailnet address.
- Pi forwarder: airgapp/rpi branch `feat/gateway-pairing`.
- Phone client: airgapp/mobile `modules/expo-gateway-client` + `src/services/gateway`
  (branch `feat/dashcam-viewer`). The real Swift session was verified end to end
  against `agw` on loopback (ECHO round trip).

- CAMERA (live views, phone-tiled grid, interior/fascia) with TALK, and
  DASHCAM (the laptop viewer's listing, chunked/cut clip reads), through the
  camera root helper `camhelper` (root, compile-time allowlist, §4.9 release
  order, marker sweep): `docs/CAMERA-PROTOCOL.md`, `docs/CAMERA-HELPER.md`.
  Bench-tested end to end against a fake car (`internal/camhelper/e2e_test.go`).
- Supervision: runit services for `agw` and `camhelper`, and `summonhelper`
  with `SUMMON=1` (`deploy/install-car.sh`, `--after-boot` after each boot).

## Summon, car side (bench-verified, not yet on the car)

- `summonhelper` (uid 1337, user `webrtc`) owns `com.tesla.Webrtc` and serves
  the phone's inner SUMMON session with its own key: Tesla's message shapes,
  the 300 ms deadman on freshness ticks (D2), the 3 m floor (D3) and the
  session end (H4). A supervisor queues for the bus name and aborts if the
  helper dies, and runit's finish hook covers a dead supervisor.
- agw's SUMMON capability (0x01) only relays the inner session's bytes to the
  helper's socket: agw cannot drive the car.
- `summonhelper probe` is the owner's no-motion car check.
- `deploy/install-car.sh` installs it only with `SUMMON=1` and stops stock
  `webrtc-comms`. The camera session reviews that change before it reaches
  `feat/camera-capabilities`.
- Architecture, bench and the owner's car checks: `docs/SUMMON.md`.

Next step: the owner's no-motion checks at the car (design §8 check 4, in
`docs/SUMMON.md`), after Phase 0 and a `SUMMON=1` install. On 2026.38.6 the
first full install carries the verified `AUTHD_SHA256` from the camera
runbook's step 2b, and later installs keep it unless the installer's pin check
flags a mismatch (`docs/SUMMON.md`, "2026.38.6: the authd pin"). The checks are
the helper's first run on the car, and they move nothing. Then the phone plan:
the SummonLink, the Summon screen, and the round trip with the camera running.

## Still needs the car or the network

- The app's `pod install` + Release build (needs network).
- Owner visit, read-only, after installing: `probe-auth` as uid 4950, through
  the helper, and `deploy/probe_media.sh`:

      chpst -u :4950:4950 /var/lib/airgapp-gw/agw probe-auth -pub <phone key hex>

  Expect `owner=ok role=2 fp=<fp> id=<key id> ff=6 secs=0` (fp is
  the key's fingerprint as the logs show it, id is authd's key Id). After an OTA, an "authd
  changed" error means: re-verify KeyInfo for the new authd, then pass its hash
  to camhelper (`-authd-sha256`; `AUTHD_SHA256=<hex>` for the camera installer).
- agw has no system bus access. uid 4950 has no account, so dbus-daemon refuses
  it, and agw only asks camhelper (root) on `/run/airgapp-camhelper/auth.sock`.
  On this car any connected uid may call `com.tesla.DisplayManager` and
  `com.tesla.Videod` (RE car check 6, 2026-11-20), which is why agw stays off
  the bus (helper-first, see the plan amendments).
- Bus self-test: before serving, agw tries a system bus connection (2 s each,
  up to 4 tries). Only a definite refusal passes, and only twice in a row: no
  bus socket, `REJECTED`, or dbus-daemon disconnecting agw right after `BEGIN`
  (how it refuses a uid it cannot resolve, but also what its connection limit
  does to a busy bus, hence the second try). If the bus takes the connection,
  or no two tries in a row are refused (a busy bus may still let the uid in),
  agw logs `BUS SELF-TEST FAILED`, waits 60 s and exits (runit retries). dbus-daemon
  caches a uid it once resolved, so after the 2026-11-20 overlay test uid 4950
  can reach the bus until dbus-daemon restarts or reloads: a root
  `org.freedesktop.DBus.ReloadConfig` cleared it on 2026-11-21 (uid 4950 refused
  since, root unaffected), as does a reboot.
- Install with `deploy/install-car.sh` (asks before every write; runit
  supervision; per-boot uid 4950 egress lock (TCP to the Pi's :17101 only, no INPUT rule); `--after-boot`
  after each boot).

## Next

- CAMERA / TALK / DASHCAM: the car visit, then the first silent session
  (`docs/CAMERA-HELPER.md`).
- SUMMON: the owner's no-motion checks at the car (`docs/SUMMON.md`), after
  Phase 0 and a `SUMMON=1` install (on 2026.38.6 with step 2b's verified
  `AUTHD_SHA256`: `docs/SUMMON.md`, "2026.38.6: the authd pin"); then the
  phone plan.
