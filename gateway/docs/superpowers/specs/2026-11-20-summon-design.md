# Summon on airgapp: design

Date: 2026-11-20, revised 2026-11-21 after review. Status: approved by the owner on 2026-11-21. Nothing here is
built yet.

Repos: gateway `~/Work/airgapp/gateway` (car side), mobile `~/Work/airgapp/mobile` (phone side).

**Appendix A** (`2026-11-20-summon-tesla-behaviour.md`, next to this file) is Tesla's Summon behaviour, extracted
read-only from the Tesla app (Android 4.58.0, iOS 4.57.5) and this car's firmware (2026.8.6), with evidence for
every row. It is normative for every behaviour this document does not override. Where the two differ, this document
wins; its header lists the overrides.

The owner settled the last two open decisions on 2026-11-21: the 3 m rule applies when a run starts (mid-run,
copy Tesla), and the camera strip copies Tesla's Grid (§3).

## 1. What this is, and why

The owner's car is airgapped: it has no connection to Tesla, so Tesla's app cannot summon it. Research on this car
(fw 2026.8.6, EU, autopark style `dead_man`) drove it from a laptop that acted as the phone's WebRTC peer to Tesla's
own on-car `webrtc-comms` (Come to Me drove on 2026-11-15). This project turns that research into a feature of the
owner's own app, airgapp:

- a Summon screen that looks and behaves like the Tesla app's Summon screen;
- a hardened path from the phone to the car. A small, locked-down helper on the car takes the place of
  `webrtc-comms`, and it is the only process the network can reach that can issue a motion command. It does so only
  for a phone key that the owner allowlisted and the car itself lists as owner or driver.

Replacing `webrtc-comms` has never been tried on the car: every research run kept Tesla's process as the owner of
`com.tesla.Webrtc`. The first car check (§8) settles whether QtCarServer accepts the helper.

**Success:** the owner, standing outside the car at a spot where the car allows Summon, opens Summon in airgapp,
holds Come to Me, and the car comes to them as it would with Tesla's app. Letting go stops it. Forward, Reverse and
Go to Target behave the same way. Nothing else on the network can make the car move.

**The owner's rule for every UX and behaviour decision:** "copy from tesla app, every decision". The only
deviations are the ones in §4. Tesla's cloud transport is replaced, never its behaviour (AIRGAPP-EQUIV).

## 2. Goals, non-goals, anti-goals

**Goals**
- G1. Tesla's Summon screen in look A (Tesla's own Summon colours): Come to Me, Go to Target, and the forward and
  reverse arrows, all hold-to-move.
- G2. Tesla's behaviour on the wire and on screen, decision by decision (Appendix A), plus the deviations in §4.
- G3. The locked-down helper (F2): the motion path is an end-to-end authenticated session between the phone and a
  helper on the car running as user `webrtc`. agw, which faces the network, only relays opaque bytes and cannot drive.
- G4. The owner's safety rules hold on every path (§5).

**Non-goals (v1)**
- Summon without a held control, from out of range, or without the phone key near the car.
- Autopark (self-park), HomeLink, and anything the car does not offer in this mode.
- Inferring "Summon complete" from gear changes. Tesla's app does not do it; it would be a new deviation.
- Android. Every gateway feature today is iOS-only (there is no Android gateway client); Android Summon waits for
  one, as a separate project.
- Any change to the car's root filesystem, D-Bus policy or firmware.
- Phones other than the owner's allowlisted phone keys.

**Anti-goals (a failure even if it "works")**
- The car moving while nobody holds a control, or seconds after a release.
- Any process the network can reach, other than the helper, being able to originate a motion command: agw, the Pi,
  Tailscale. (Root and user `tesla` on the car can already cause motion through the car's own data values, so the
  helper's exclusivity is against other users; camhelper, which runs as root, keeps its fixed list of data values
  with no motion value in it, pinned by a test.)
- UX that Tesla's app does not have, apart from §4. That includes setup or provisioning text on screens.
- Anything sent to 192.168.90.0/24 (R22).

## 3. Decisions already made

| Decision | By, when |
|---|---|
| Modes in v1: Forward, Reverse, Come to Me, Go to Target | owner, 2026-11-20 |
| Look A: Tesla's own Summon colours (from the mockups) | owner, 2026-11-20 |
| Architecture F2, the locked-down helper | owner, 2026-11-20 |
| "copy from tesla app, every decision" | owner, 2026-11-20 |
| Sign-in is helper-first; agw has no bus access and refuses to serve if it can reach the bus | owner, 2026-11-20 |
| 300 ms deadman on signal loss; the goal is never at the car's GPS (3 m floor); the assistant never commands motion; the operator procedure (§5.1) | owner, standing |
| Freshness F = 150 ms with the 300 ms deadman | owner-approved SUMMON plan, 2026-11-20 |
| The 3 m rule applies when a run starts (each press); during a run, copy Tesla's app | owner, 2026-11-21 |
| The camera strip copies Tesla's: the car's Grid view | owner, 2026-11-21 |

**Approaches considered for the architecture** (owner picked F2):
- **F1.** A helper running as `webrtc` only owns `com.tesla.Webrtc`; agw keeps the session crypto and the deadman.
  agw could then drive the car, so it is fit only for supervised tests.
- **F2 (chosen).** The helper terminates an inner phone-to-helper session with its own key and its own sign-in, and
  runs the whole Summon engine. agw relays opaque bytes.
- **E.** A D-Bus policy drop-in that lets agw's uid own `com.tesla.Webrtc`. Rejected outright.

**Dropped from the SUMMON capability plan** (`docs/superpowers/plans/2026-11-20-summon-capability.md`) as invented
policy, per the copy rule: one verb per window, the 120 s window, the 35 s re-arm cool-down, the verb fixed in the
ARM, a locked mode toggle, a goal frozen at the press, no device location before the verb, the 30 m accuracy cap,
the 65 m device range, the 15 s no-start rule, the reshaped STATUS/RESULT/CARLOC frames, one heartbeat per HOLD,
and the 10-HOLD link gate (its job now falls to the freshness rule on every motion frame, §5.2).

**Kept from that plan, reworked:**
- `internal/carlink`: the D-Bus contract, reworked from typed builders to schema-checked pass-through (§6.1).
- `internal/supervise`: the supervisor, plus a health gate and a name queue (§6.1).
- `internal/guards/route.go`: the R1 route gate, used by the relay.
- The mock QtCarServer, reworked to Tesla's flow (§8).
- The gateway core's freshness ticks (D2) and the bench-first approach.

**Retired:** `capability/summon`, the geo and cool-down guards, `agw supervise` and `agw summon-finish` (the finish
hook moves into the helper), and the phone's old SUMMON API (mobile `src/services/gateway/summon.ts`, the native
hold loop in commits ba19a3e, 624c4c9 and c4a3c98).

## 4. Deviations from Tesla

- **D1. Standby normalizer.** This car always reports `autopark_state` "standby" and `smart_summon_state`
  "initializing" (the Actually Smart Summon short-circuit in the firmware). Copied literally, Tesla's app would
  disable every control forever and abort any run on its first status. A phone-side normalizer sits in front of
  Tesla's unchanged status handling and reports what the same firmware reports when the short-circuit does not
  apply. Exact rules: Appendix A §4.5.
- **D2. The owner's 300 ms deadman**, enforced in the helper with freshness (§5.2). What it adds that Tesla lacks:
  - motion frames older than F are not forwarded;
  - a stale or concurrent verb is refused;
  - the helper injects an abort when fresh frames stop;
  - after such an abort it stops forwarding heartbeats until the next press, so Tesla's own "Reducing speed due to
    poor connectivity" appears;
  - camera keyframes only when the link runs late (§6.2).
- **D3. The 3 m floor, at each press** (§5.3). The helper refuses a verb whose phone position or goal is within
  3 m of the car's GPS, and all motion controls are disabled on the phone within 3 m. During a run, location
  updates flow as in Tesla's app (owner, 2026-11-21).

**Known differences forced by the platform** (not choices):
- iOS location has no interval setting (Tesla's 1 s interval is an Android setting).
- The car never reports completion or car-side aborts (§7).

**AIRGAPP-EQUIV (transport only):**
- Tesla's cloud WebRTC session becomes the helper session through agw.
- Tesla's keepalive becomes `control:keepalive` in the session every 15 s. This is [INFERRED]: the firmware's
  cloud-side handler takes the same message (`ControlCommandHandler::handleMessage`, QtCarServer.c:1174135-1174300),
  and its effect on this car is measured in §8.
- Tesla's "Pair phone key" entry check becomes "no helper pin for this car".
- Tesla's secure-connection alert becomes the helper refusing sign-in (§6.4).
- Tesla's poor-connectivity RTT becomes the phone-to-helper round trip.
- Tesla's `autopark:info` fields keep Tesla's keys, filled from airgapp's equivalents (§6.4).

## 5. Safety

### 5.1 Operator procedure (physical; unchanged)

- The car is empty. The operator is outside and clear of the path from the car to the goal. The physical kill is
  opening a door (VCSEC abort).
- The owner runs every motion test at the car. Nothing in this project commands motion by itself, and the assistant
  never does.
- Off by default: the helper is installed and served only with `SUMMON=1` at install time.

### 5.2 D2: the deadman

**Frames from the phone** (inner session, §6.1). Every motion-relevant frame carries the id of the latest freshness
tick the phone has received. The ticks are the gateway core's existing ones: 20 Hz, random ids, BOOTTIME,
`Window.TickAge`, issued by the helper's core inside the inner session.

| Frame | Sent by | Content | Helper |
|---|---|---|---|
| `LIVE` | native, on every tick received while a motion control is physically held | tick id | liveness only; never forwarded |
| `VERB` | JavaScript (Tesla's press logic), passed through native | tick id, press id, Tesla's `cmd_find_me` / `cmd_forward` / `cmd_reverse` | forwarded only if fresh and D3 passes, else refused |
| `HB` | JavaScript (Tesla's heartbeat timing), passed through native | tick id, press id, Tesla's `heartbeat_app_webrtc` | forwarded only if fresh; never after a trip until the next forwarded verb |
| `MSG` | JavaScript or native | Tesla's `cmd_abort`, `device_location`, `info`, `control:keepalive` | `cmd_abort` always forwarded; the rest per §5.3 and §6.1 |
| `PRESENCE` (added 2026-11-26) | native, on every 20th tick of an up connection (about 1 Hz), held or not | tick id | the presence lease only (below); never forwarded, never a liveness instant |

- **Fresh** means the frame's tick was issued in this session no more than **F = 150 ms** before the frame arrived.
  Because `LIVE` echoes each tick as it arrives, its tick age is about one round trip.
- **Native gate.** Native sends `VERB` and `HB` only while the press that produced them (its press id) is still
  physically held; otherwise it drops them. It never sends `heartbeat_app_webrtc` or a motion verb as `MSG`, and the
  helper rejects them there.
- **verbActive** is set when the helper forwards a verb. It is cleared only when the helper forwards or injects
  `cmd_abort`, or when the session ends.
- **Liveness instants:** the arrival of each fresh `LIVE`, `HB` or forwarded `VERB` frame.
- **Presence** (added 2026-11-26 for the review's F5r; `2026-11-26-summon-review-fixes-design.md`). A `PRESENCE`,
  `LIVE`, `HB` or `VERB` whose tick is at most 3 s old stamps that tick's issue time. When the phone opts in, with the
  inner ARM params `{"presence":1}`, the session ends once more than 10 s have passed since the newest stamp (H4,
  then BYE `lease`). Presence never counts for D2: a `PRESENCE` is no liveness instant, and nothing of it reaches the
  car.
- **Trip:** if verbActive and more than **300 ms** have passed since the latest liveness instant, the helper injects
  `{"msg_type":"autopark:cmd_abort"}`, the same bytes as Tesla's release.
  - It swallows that abort's `cmd_result` and keeps the session.
  - It forwards no `HB` until the next forwarded verb. The car's heartbeat echoes therefore stop, and Tesla's own
    path runs on the phone unchanged: "Reducing speed due to poor connectivity" after 2 s, and a disconnect and
    reconnect after 10 s.
  - The car pauses: UserPaused if it was in Primed through Active, otherwise a terminal abort. Release and press
    again to resume.
- **Refusals.** The helper answers a refused verb with
  `cmd_result {cmd_type: <the request's msg_type>, result: false, reason, timestamp}`, with a timestamp no earlier
  than the highest `cmd_result` timestamp seen in the session. Tesla's app shows its alert and sends its own abort.
  - A stale verb is refused with reason `action_invalid` ("Something went wrong").
  - A verb that arrives while verbActive is refused with reason `action_invalid`. Tesla's app never sends one: its
    controls take one press at a time.
  - D3 refusals are in §5.3.

**Why freshness, not arrival alone.** Arrival alone cannot tell a live finger from a link that delivers everything
late. With a few seconds of queueing (Wi-Fi bufferbloat, a Tailscale relay server, a hostile hop), frames keep
arriving at their usual spacing while the release is still seconds behind them. A verb queued that way could start
or resume the car after the finger is up. With ticks on every motion frame, a late link stops counting as a held
control within F + 300 ms, and a late verb is never forwarded.

**Operating condition.** Holds work while the phone-to-helper round trip stays under about 150 ms: the phone on the
Pi's Wi-Fi, or on a direct Tailscale path. On a relayed path every hold is refused or pauses within about 0.3 s,
which is the safe failure. §8 measures the real path with the camera running, before any motion test.

**The phone side: Tesla's release rules, enforced in native code.** Tesla's hold button and arrows are React
Native touchables (Pressability): a release is the finger lifting, the touch being cancelled, or the finger sliding
out of the control's frame expanded by 20 pt left, right and top and 30 pt below. Sliding back in is a new press.
airgapp's three motion controls are native views that reproduce exactly those rules, and they own the safety-critical
side:
- **One press at a time.** At most one motion control holds a touch. Other touches are ignored, not treated as a
  release. A touch-down on a control that JavaScript has marked disabled raises no event. The held control keeps its
  touch when it is restyled to Tesla's "disabled while held" look.
- **Press:** native assigns a press id, starts `LIVE`, and tells JavaScript, which runs Tesla's press logic and
  sends the verb.
- **Release**, any of:
  - the touch ends or is cancelled;
  - the touch leaves the expanded frame;
  - the view leaves its window, is unmounted, recycled or hidden;
  - the app resigns active or goes to the background.

  Native then immediately stops `LIVE`, sends Tesla's `cmd_abort` (Tesla's release always sends one on this car,
  whether or not a verb went out), and tells JavaScript. JavaScript runs Tesla's release logic; native drops that
  logic's abort for the same press id, because it has already been sent.
- **Binding.** Each press is bound to its `UITouch`. On every tick native re-checks that the touch and its view are
  still live. No ancestor gesture recognizer may delay touch ends.
- **Ordering.** Press and release events reach JavaScript on the same ordered native event stream as car frames.
  Tesla's machine therefore always sees the release before the abort's `cmd_result`.

A JavaScript stall can never extend a hold. The car gets heartbeats and verbs only while a finger is on the control,
and every release aborts at once.

**Worst cases.**
- Normal release: the abort reaches the car after one phone-to-car trip.
- Abort lost, or link lost: the helper injects the abort at most F + 300 = 450 ms after the last fresh frame.
- No authenticated frame within the session lease (20 s): the session ends (H4: abort, then disconnect, a terminal
  abort on the car). *Corrected 2026-11-26 (review F5r):* that lease counts arrivals, so a relay that holds frames
  back and releases one every < 20 s keeps the session up after the phone has left, about 20 s per held frame. When
  the phone opts in, the presence lease ends the session at most 10 s after the issue time of its newest fresh tick
  echo, or, before any echo, 10 s plus up to 2 s after its ARM (the core's pre-auth deadline).
- Helper crash or hang: the finish hook sends abort and disconnect within about 200 ms (§6.1).
- These bounds end when the abort reaches QtCarServer. The time from there to standstill (QtCar UI process, then
  APE) is unmeasured. The first supervised test measures release-to-standstill on both the release path and the
  injected-abort path before any walking test (§8, R23).

### 5.3 D3: the 3 m floor

**Origin.** The owner's rule: "the device_location/goal must never equal the car's own GPS". In research a phone
position at the car's exact GPS made the car abort while arming the drive rail (2026-11-15, on a Forward run); 3 to
6 m off the car worked.

**The car's position**, as the helper knows it:
- It is the latest `vehicle_data:location` relayed in this session, in arrival order (made reliable as in §6.1).
- While verbActive, the position goes **stale** if no car frame of any kind arrives for more than 1 s. It also goes
  stale if no location frame arrives for more than 1 s after one that showed speed above 0 or gear D or R.
- A stale position stays stale until a newer location frame arrives.
- No position, or a stale one, counts as "no car GPS".

**The goal** is derived as the car derives it: both `goal_latitude` and `goal_longitude` if present, else the phone
position (Appendix A §3.5).

**Rules in the helper, at each press.** For each `cmd_find_me`, `cmd_forward` and `cmd_reverse`:
- If its phone position (`latitude`/`longitude`) or its goal is within 3.0 m of the car (haversine, R = 6371 km),
  it is not forwarded. The helper refuses it with `location_params_invalid`; Tesla's app shows "Summon Failed /
  Invalid location / Close", then sends its own abort.
- With no car GPS (none yet, or stale), it is refused with `invalid_vehicle_location` ("Something went wrong").

This covers the arrows too, because the research self-abort was on a Forward run.

**During a run, and between runs,** `device_location` goes to the car as in Tesla's app, with no 3 m check (owner,
2026-11-21). The car's planner decides where the car stops.

**On the phone (Tesla's look, no text):**
- Every motion control is disabled while the phone fix is within 3 m of the car.
- Go to Target pushes a target that would land closer than 3 m out to 3 m on the same bearing, and moves the map
  back the way Tesla's outer clamp does, so the crosshair sits on the real target. Tesla's own clamp caps the target
  at 64 m.

### 5.4 What the helper may originate

Only:
- `data_channels_ready` at session start;
- `cmd_abort` (D2's trip, session end);
- the disconnect map at session end;
- refusals (§5.2, §5.3).

Never a heartbeat, a motion verb or a device location. It does not forward the `cmd_result` of an abort it
originated itself. The probe tool for car checks (§8) is a separate subcommand, not the service, and sends only
heartbeats and keepalives.

### 5.5 Threats

- **agw, the Pi or Tailscale compromised.** They can delay, drop or close traffic. D2 turns that into a stop, and a
  delayed verb is refused. They cannot read, forge, reorder or replay inner frames (no helper key, no phone key):
  frames open only in order, so a dropped frame ends the session. They cannot own `com.tesla.Webrtc` (the bus policy
  allows only user `webrtc`). *Corrected 2026-11-26 (review F5r):* the session lease bounds them only per frame. It
  counts arrivals, so by holding the phone's frames back and releasing one every < 20 s they keep a session up about
  20 s per held frame after the phone has left. For a phone that opts in, the presence lease (§5.2) caps that at
  10 s after its newest fresh tick echo, or, before any echo, 10 s plus up to 2 s after its ARM. Only a session that
  opted in holds the live file (§6.1; ruling F1 of the 2026-11-26 final review), so the camera's SUMMON mode (§6.5)
  is never stretched past the presence lease. Neither moves the car: every verb still needs a tick echo at most
  150 ms old.
- **Another process on the car.**
  - It cannot connect to the helper's socket (peer uid 4950 only). Even with a connection it cannot sign in without
    an allowlisted phone key that authd lists as owner or driver.
  - Root (camhelper included) and user `tesla` can already cause motion through the car's own data values. User
    `tesla` may also call any method on `com.tesla.Webrtc`, so it could forge car frames to the helper.
  - The helper logs the caller of every car-frame call. Once a car check shows QtCarServer's connection, it accepts
    car frames only from that caller.
- **A stolen phone.** As with Tesla, the phone key is the credential.
  - Revocation through the owner's allowlist is immediate: the helper watches the same file as agw.
  - Revocation through authd's key list goes by KeyInfo, re-checked every 10 s, and can lag about 10 minutes on
    authd's side.
- **Car-side interlocks.** The car's key-in-range check (`keyfob_not_present`) is empirically supported, not
  firmware-proven: the MCU's `keyfobOk` is skipped on the remote path, and keyless runs ended in
  `AbortedDriveAuthTimeExceeded`. The VCSEC door abort stays.

## 6. Architecture

```
 phone (airgapp, iOS)               Pi        car MCU
 ┌──────────────────────────┐  Tailscale  ┌─────┐ Wi-Fi ┌─ agw (uid 4950) ──────────┐ unix   ┌─ summonhelper (uid webrtc) ──┐ D-Bus      ┌─────────────┐
 │ Summon screen (JS):      │────────────▶│ fwd │──────▶│ SUMMON relay (cap 0x01):  │──────▶│ gateway core on one socket:  │──────────▶│ QtCarServer │
 │  Tesla's state machine   │             └─────┘       │  opaque bytes both ways   │ socket │  sign-in (allowlist+KeyInfo)│ com.tesla. │             │
 │  + D1 normalizer         │                           │ CAMERA (strip, §6.5)      │        │  SUMMON engine: H1-H7,D2,D3 │ Webrtc     │             │
 │ native SummonLink:       │                           └───────────────────────────┘        │  supervisor + finish hook    │            └─────────────┘
 │  outer + inner sessions, │                                                                 └──────────────────────────────┘
 │  LIVE/HB/VERB gating,    │
 │  native motion controls  │
 └──────────────────────────┘
```

The inner session is the existing gateway protocol (hello, HPKE, ARM, frames, lanes, lease, freshness ticks),
nested inside the outer SUMMON window. Both ends reuse reviewed code: the helper runs the gateway server core, and
the phone runs a second `GatewaySession` whose transport is the outer window (§6.3).

**The car opens the car-to-Pi hop.** On 2026-11-21 the camera session proved on the car that the car's Wi-Fi module
NATs the MCU and drops every inbound packet, so the Pi cannot connect to agw. The owner chose the fix the same day:
agw dials out to the Pi (`agw serve -dial 192.168.4.1:17101`) and keeps three idle connections open there, and the
Pi pairs each phone's connection with one of them. It is built and merged into feat/camera-capabilities (e04cd11),
but not yet on the Pi or the car. The diagram's arrows show which way requests flow, not which end connects.

Summon rides on the gateway session, so nothing above the transport changes. What Summon needs from the hop is a
phone-to-helper round trip under about 150 ms (§5.2). The car-to-Pi leg is the local Wi-Fi, which keeps that by
construction; the phone's leg is covered in §5.2, and check 4 measures the whole round trip.

### 6.1 Car: `summonhelper` (new, gateway repo)

**Process and files.**
- A new binary (the name is the builder's call). Its runit run script starts as root and:
  - creates `/run/airgapp-summon` at every start, owner 1337, group 4950, mode 2750 (setgid, so the socket inherits
    group 4950). `/run` is a root-owned tmpfs, so the helper itself cannot.
  - reads the VIN from `/var/etc/vin` and passes it in;
  - refuses to start while `com.tesla.Webrtc` has an owner (§6.6);
  - then `chpst -u :1337:1337` (user `webrtc`, no extra groups; not group 4950, which would also open
    `camhelper.sock`).
- State, including the helper's private key, lives in a 0700 directory outside `/tmp`. No network listener, no
  abstract sockets.
- The socket is `/run/airgapp-summon/relay.sock`, mode 0660. The helper accepts connections only from uid 4950
  (SO_PEERCRED), one at a time.
- **Its own key and pin.** `summonhelper pin` prints `airgapp-gw:1:<key>:<VIN>`, the same format as `agw pin`. The
  owner adds it to the phone the same way as the gateway pin. agw never has this key, which is what keeps agw from
  terminating the inner session itself.

**Sign-in.**
- The allowlist is agw's own file, `/var/lib/airgapp-gw/allow` (root-owned, 0644 in a 0755 directory, so `webrtc`
  can read it), loaded and watched like agw does. A key the owner removes is revoked at the helper at once, even if
  agw is compromised. One list governs both the camera and Summon, as Tesla lets any owner or driver key summon.
- The authority is the direct D-Bus KeyInfo oracle (`internal/authority`, with the authd pin), warmed with one call
  before the helper accepts connections. The camera session read the car's live `com.tesla.Auth` policy on
  2026-11-20: the default context may call KeyInfo, and user `webrtc` has an account. KeyInfo as uid 1337 is still
  checked on the car (§8).
- Open sessions are re-checked every 10 s. If user `webrtc` receives authd's KeysUpdated signal (checked in §8), a
  watch is wired to `server.KeysChanged`/`SetWatchUp`, and the cadence becomes the core's 30 s.
- A failed or unreachable check ends the session (H4, a terminal abort). That is the safe outcome; the owner presses
  again.

**Capability.** The inner SUMMON engine: TickHz 20, LeaseMS 20000. In: `LIVE`, `VERB`, `HB`, `MSG` (§5.2), and
`PRESENCE` (0x18, the 8-byte tick id; added 2026-11-26 for the presence lease, §5.2). Out:
`CAR` (channel, then the car's JSON). Size limits are the builder's call, generous enough for every car frame seen
(Appendix A §8.6).

**D-Bus** (`internal/carlink`, reworked; contract in Appendix A §1.2):
- Owns `com.tesla.Webrtc` at `/Webrtc` with DO_NOT_QUEUE, never ALLOW_REPLACEMENT, and serves only while it owns
  the name. On NameLost or a failed emit the engine latches unhealthy (below).
- **H1.** On session up (inner ARM accepted) it emits `LocalToRemoteSignaling {data_channels_ready: true}` once.
  The car then pushes style, status and location.
- **H2.** Phone to car: each `VERB`, `HB` and `MSG` is parsed against Tesla's exact message shape for its
  `msg_type` (Appendix A §2.1), and re-encoded:
  - every key Tesla always sends is required with its exact type, and `summon_type` is `find_me` or `pin_drop`;
  - the result is emitted as `PeerCommsToCarserverData {uid: "test_phone", data}`;
  - the allowlist is `autopark:cmd_find_me`, `cmd_forward`, `cmd_reverse` (as `VERB`), `heartbeat_app_webrtc` (as
    `HB`), and `cmd_abort`, `device_location`, `info` and `control:keepalive` (as `MSG`);
  - a non-conforming `device_location`, `info`, keepalive or heartbeat is dropped and logged (Appendix A's
    behaviour-neutral rule);
  - a non-conforming verb, a wrong frame type for a message, a `_request` wrapper, or a framing or oversize error
    ends the session (H4);
  - a `cmd_abort` is always forwarded, as the canonical payload-less abort.
- **H3.** Car to phone: `CarserverToPeerCommsData` is answered at once, before the phone link is touched (the
  research stream froze after about 150 messages; an unanswered call is the leading suspect). The car's call order
  is captured on godbus's read goroutine with `dbus.WithIncomingInterceptor`, because godbus otherwise runs each
  call on its own goroutine. One goroutine forwards the frames in that order with their channel (`dcTcp`/`dcUdp`).
  The caller of each call is logged (§5.5).
- **H4.** Session end, in order: `cmd_abort` if verbActive; then the disconnect map
  `{ice_state_change: true, msg_type: "autopark:set_webrtc", set_webrtc_value: false}`, exactly once. After that,
  phone frames are dropped until the next session.
- **H5.** Never copy `webrtc-comms`' ICE mirroring.
- **H6.** The finish hook is mandatory (QtCarServer does not watch the name's owner).
- The car's own `RemoteToLocalSignaling {type: 9}`, sent 60 s after a disconnect, is answered and closes any
  session.
- carlink changes from feat/summon:
  - pass-through with the schema in place of typed builders;
  - `car_timestamp` in heartbeats, and `pin_drop`;
  - the `control:` prefix for keepalive;
  - the channel on car frames;
  - type 9 handling;
  - the interceptor.

**Supervisor and finish hook** (`internal/supervise`, plus what feat/summon kept in cmd/agw):
- A parent and child with a 100 ms liveness tick. The tick is sent only while the child is **healthy**:
  - D2's timer loop stamped progress within 250 ms, stamped before it takes any lock;
  - no injected or session-end abort or disconnect emit has failed (godbus reports most write errors only as
    ErrClosed, so ErrClosed, NameLost and an emit that overruns its deadline all count as failures);
  - the name is still owned.
- On a missed tick, child death or SIGTERM, the parent SIGKILLs the child.
- The parent (same uid) queues for `com.tesla.Webrtc` without DO_NOT_QUEUE once the child owns it. The bus then hands
  it the name atomically when the child's connection drops; it emits `cmd_abort` and the disconnect map, and
  releases the name.
- The runit finish script runs the same finish hook as uid `webrtc`, which covers a killed supervisor. Its marker
  lives in `/run/airgapp-summon`.
- **The live file** (added 2026-11-26, review L1-L5). camhelper grants its SUMMON mode (§6.5) only while the helper
  holds `flock(LOCK_EX)` on `/run/airgapp-summon/live`. The helper takes it for each session that opted into the
  presence lease (§5.2; ruling F1, 2026-11-26), once its start succeeded, before the file is visible, and keeps it
  until the session ends, whatever ends it; the kernel drops it when the helper exits. A session that did not opt in
  makes none, so it gets no SUMMON mode. A file left behind is removed by the parent's takeover (on every branch,
  before it releases the name) and by the finish hook (first, whatever the marker says), as by the helper's start,
  the runit run script and the installer's `SUMMON=0` removal (`docs/SUMMON.md`, "The live file").

**`summonhelper probe`** (for the no-motion car checks, §8). It runs only with the service and stock
`webrtc-comms` down. It owns the name, emits `data_channels_ready`, and sends Tesla's `heartbeat_app_webrtc` at
10 Hz and `control:keepalive` every 15 s for a set time. It logs the car frames by type, count and timing (never
GPS, keys or the VIN), then disconnects. It has no code path for a verb or a device location.

### 6.2 Car: agw's SUMMON relay (gateway repo)

- Capability SUMMON, ID `0x01`, kept so that `Host().SummonOpen()` still drives CAMERA's rules.
  - LeaseMS 20000, TickHz 0.
  - In `0x10 INNER`, Out `0x90 INNER` (opaque bytes of the inner stream), on the priority lane.
- **PreArm:** the R1 route gate (Tailscale must not advertise 192.168.90.0/24; fail closed), and the helper's socket
  exists and is owned by uid 1337.
- **Open:** dial the relay socket and check that the server uid is 1337. Writes to the socket go through a goroutine
  with a bounded queue and a write deadline, so `Frame` never blocks. The socket is closed when the window is done,
  not only in Teardown. The helper sees EOF and runs H4.
- **Relay framing on the socket:** small records with a type byte (inner bytes, or the link flag). agw never parses
  the inner bytes.
- **Link flag (part of D2).** The helper sets it when `LIVE` tick ages exceed 100 ms on 3 frames in a row, and clears
  it when a hold ends or after 2 s of ages at or under 75 ms. agw passes it to `Host().SetSummonLinkDegraded`, and
  CAMERA sends keyframes only (about one every 0.75 s) while it is set. That is the stand-in for WebRTC's congestion
  control, which protects the deadman's bandwidth.
- agw keeps no D-Bus Summon code and always runs `serve`.

### 6.3 Phone: native SummonLink (`modules/expo-gateway-client`, Swift)

- `GatewaySession` gets a transport abstraction, with two implementations: the existing NWConnection, and an
  adapter over the outer window's `INNER` frames with buffered framed reads.
- SummonLink opens the outer SUMMON window to agw, then an inner `GatewaySession` to the helper over it using the
  helper's pin, and ARMs the inner SUMMON capability.
- The old cap-0x01 code is deleted: HOLD, RELEASE, LOC, `setHold`, `sendLoc`, the LOC timer and the cap-0x01
  app-state observers. The tick bookkeeping moves into SummonLink. Coordinate with `expo-gateway-video`, which
  consumes `GatewaySession`.
- **API to JavaScript:**
  - `sendVerb(json, pressId)`, `sendHeartbeat(json, pressId)` and `send(json)` (abort, `device_location`, `info`,
    keepalive);
  - one ordered event stream carrying press and release events, car frames `{channel, data}`, session state and the
    round trip.
- **The three motion controls** (hold button, up arrow, down arrow) are native views with the rules in §5.2,
  rendered in Tesla's look. JavaScript sets each control's style, label and enabled flag.
- **Reconnect:** Tesla's timing: 1000 ms after a loss while the screen is up, and 500 ms after a heartbeat-critical
  or round-trip disconnect.

### 6.4 Phone: the Summon screen (JavaScript, mobile repo)

- **Route and entry.**
  - A `summon` route, reached from a "Summon" row on Home. The camera session's uncommitted Home edits replaced an
    earlier Summon row with a test button; the row comes back.
  - The entry copies Tesla's Home-row path: location permission, then precise location. Precise location is iOS
    temporary full accuracy with purpose key `SummonPreciseLocationPurposeKey`; expo-location has no API for it, so
    it needs a small native call plus the `NSLocationTemporaryUsageDescriptionDictionary` Info.plist entry. Then it
    navigates.
  - Tesla's "Pair phone key" alert shows when the car has no helper pin.
- **Tesla's state machine,** ported with its quirks (Appendix A §1 to §4): connecting, waiting for initial status,
  ready, not ready, waiting for command response, summon in progress, not connected.
  - The D1 normalizer sits in front of status handling.
  - Verbs go out through `sendVerb` and heartbeats through `sendHeartbeat`, with the press id from the press event.
  - The implementation style (reducer, a state library) is the builder's call. The transitions, actions and quirks
    must match Appendix A.
- **Secure-connection alert** (Tesla's alert 350, "Close" goes back) shows when:
  - the inner session ends with BYE `revoked`; or
  - the inner sign-in closes before READY while the outer window is READY, twice in a row (which rules out the
    busy-slot race on a fast reconnect).

  Outer-window failures (R1, `SUMMON=0`, helper absent) stay Tesla's "Cannot connect to vehicle" with Tesla's
  reconnect timing.
- **Location.** A small native location watch with Tesla's active config: BestForNavigation, distance filter 0,
  `pausesLocationUpdatesAutomatically` false, activity type other, foreground only. The first fix uses a 5000 ms
  race. A `device_location` goes out for each changed fix with no accuracy or age filter; stored fixes older than
  60 s are not sent.
- **Map** (`MapSurface`, Apple Maps):
  - HYBRID by default, with the globe toggle persisted.
  - The car marker `vehicle_mapmarker_online` / `_stale` (282 px shown at 62 pt) is an image child rotated with
    `rotateZ(heading)`; map rotation is off, as in Tesla's app.
  - The leash circle is 65 m, and the path line comes from `smart_summon_viz` (probably never drawn on this car).
    The seam gains Circle and Polyline (react-native-maps on iOS).
  - Go to Target: a fixed crosshair with the map panning under it, the 64 m clamp and the 3 m push-out (D3), and a
    pin marker while held.
  - Tesla's camera framing rules.
- **Band** (#222222):
  - the status line (Tesla's priority list and texts);
  - the hold button (Come to Me or Go to Target);
  - the arrows;
  - the spinner, the first-use caution, alerts, the panic overlay and haptics.
- **Header:**
  - the back button;
  - the summon badge while executing;
  - "Parked · 12 m"-style drive status;
  - the poor-connectivity icon (round trip of 500 ms or more, sampled every 2 s);
  - the blinkers.
- **Right column:** the mode toggle, the globe, and the sentry button (which hides or shows the camera strip).
- **`autopark:info`:** Tesla's exact keys, with values from airgapp's equivalents of Tesla's sources:
  `x_tesla_user_agent` and `tag` are the app's user agent, `model` the device model, `name` the device name, and
  `public_key_hash` the phone key's id. The format of that id matches Tesla's once decompiled (§9); the car stores it,
  but it does not gate motion.
- **Strings:** Tesla's English texts, verbatim (Appendix A §4.2, §4.3, §6). Every size, colour and timing comes from
  Appendix A §6 and the look-A mockup.
- **Unverified rows.** The UI and status rows of Appendix A (§2.3, §4, §5, §6, §7) were never checked by a verifier.
  A read-only verification pass over them runs before the screen is built.

### 6.5 Camera strip

**What Tesla does** (decompiled; this settles Appendix A §9 #10). For `webcam:signal` with context `summon`,
QtCarServer skips its normal webcam checks and calls `startWebcam(summon=true)`. That:
- selects the APE's **Grid** camera (selectable 0xb) for the webcam;
- holds `WEBCAM_webcamSessionActive`;
- runs `summon-webcam`, which captures port 9895 at 456×342, 10 fps, about 495 kbps.

Evidence: QtCarServer.c:1376760-1376866 and 1378449-1378535; the dump's `etc/sv/summon-webcam/run`. Tesla's strip
therefore shows the car's camera grid.

**What airgapp's CAMERA does today** while SUMMON is open: it releases the hold and the selectable and shows only
the front stream on port 9898. That stream is dead while parked (camera research), and the car is parked before
every press, during a pause between presses, and after arrival. So, as built, the strip would be dark most of the
time.

**airgapp copies Tesla (owner, 2026-11-21).**
- While SUMMON is open, CAMERA selects Grid on 9895 and holds the webcam session, as Tesla's `startWebcam(summon=true)`
  does.
- This reverses the camera work's rule never to select Grid. While Grid is selected, the dashcam's "wide" recording
  captures the grid, exactly as with stock Summon.
- It needs SUMMON exceptions in camhelper's gear and Sentry gates, mirroring Tesla's skipped checks, and a car check
  that the hold survives leaving P. That is camera-session code, agreed with that session.
- Grid is selected only while SUMMON is open, and the selection goes back to 0 when it closes.

**Also:**
- the rest is Tesla's rules for visibility, size, fade, the spinner, the 5 s hide and retries (Appendix A §6);
- the sentry button toggles the persisted hide preference;
- the link flag (§6.2) drops CAMERA to keyframes when the deadman's link runs late;
- the strip is built after the motion controls are proven, and §8 measures the round trip with it running.

### 6.6 Install and provisioning

`install-car.sh` on feat/camera-capabilities implements `SUMMON=1` as the old design: `agw supervise`,
`agw summon-finish`, and a uid-4950 marker in `/run/airgapp-gw`. This work rewrites that path and shows the change
to the camera session for review before it lands:
- **agw's run script is always plain `serve`.** `agw_has_summon` and the `/run/airgapp-gw` marker go.
- **A new runit service for the helper,** linked into the same runsvdir, with an exec mount for its binary. `/var` is
  noexec. Its run script is in §6.1, and its finish script runs the finish hook.
- **Stock `webrtc-comms`.** It is a firmware runit service, linked in the infotainment and sentry run modes, running
  under minijail with its own PID and mount namespaces. At install and in `--after-boot`, as root, before the helper
  starts:
  - `sv down webrtc-comms`;
  - SIGKILL the pid in `/run/service/webrtc-comms/webrtc-comms.pid` only if `/proc/<pid>/comm` is `minijail-init`
    (as the firmware's own StopSandbox does), and remove the pidfile;
  - wait until `com.tesla.Webrtc` has no owner.

  `sv down` does not survive a reboot or a run-mode switch, so the helper's run script and the relay's PreArm both
  re-check that the name is free or the helper's. Sharing uid 1337 with a surviving stock process is acceptable:
  that process is PID- and mount-namespaced, so it can only contend for the name, and DO_NOT_QUEUE makes that fail
  closed. Uninstall runs `sv up webrtc-comms`, whose own run script clears leftovers.
- **Checks.** The installer's checks include a cross-uid connect test (uid 4950 to the helper's socket).
- **`SUMMON=0`** (the default) installs nothing for Summon, and agw's relay then refuses PreArm.
- **Pins** are read on the car over SSH and added to the phone. They never appear on a screen.

## 7. Behaviour summary (copy; full detail in Appendix A)

- **Opening the screen** starts the session: "Connecting" with a spinner. The car pushes its style and status, and D1
  makes this car read as ready. Opening the session also pre-primes the car (it wakes its buses and turns the drive
  rail on), as with Tesla's app. On the first visit per app launch, Tesla's caution text fades in and out, then the
  idle hint shows: "Press and hold controls to move vehicle."
- **Holding Come to Me** sends one `cmd_find_me` with goal = the phone.
  - Once the car accepts it, Tesla's heartbeats go out every 100 ms.
  - A `device_location` with goal = the phone goes out for every new fix, so the car follows the phone.
  - The status line reads "Preparing", then "Release to stop".
- **Releasing**, which includes sliding the finger off the control, sends `cmd_abort`.
  - From Primed through Active the car pauses (UserPaused) for up to 35 s. Pressing again within that window resumes:
    there is no resume message, just a fresh `cmd_find_me`.
  - A release before Primed is a terminal abort, and the next press starts a new run.
  - Pause and resume are unverified on this car (§9).
- **Go to Target:** the map pans under a fixed crosshair, and the target is clamped to 64 m from the car (and pushed
  to at least 3 m). A press sends `cmd_find_me` with `summon_type` "pin_drop" and the target as goal.
- **Arrows:** hold to creep forward or reverse; a release pauses, as above.
- **The mode toggle stays live during a hold** (Tesla), so the summon type can change mid-run.
- **No phone-side timers beyond Tesla's:**
  - "Reducing speed due to poor connectivity" after 2 s without car heartbeats, which D2 also triggers after a trip;
  - a disconnect and reconnect after 10 s, with no abort.
- **The helper's own timers** are D2's (F, the 300 ms trip, the link flag), the 20 s lease, the 10 s sign-in
  re-check and the 100 ms supervisor tick, and, for a phone that opts in, the presence lease's (an echo at most 3 s
  old stamps; 10 s after the newest stamp the session ends; added 2026-11-26).
- **Residual differences on this car:**
  - The car never reports completion or car-side aborts, so "Release to stop" stays until the finger lifts. The
    header shows "Parked" once the gear reads P.
  - If the car's stream freezes before the first `cmd_result` reaches the phone, Tesla's app waits forever in
    "waiting for command response" (Tesla has no timer there). The car is safe, because D2 aborts it.
- **Leaving the screen** aborts if a run is active. Going to the background leaves the screen and closes the
  session (Tesla).
- **Location:** no accuracy gate on the phone. More than 300 m from the car shows "Move closer" with the controls
  disabled. The car itself refuses `find_me` above 200 m accuracy or 300 m distance.

## 8. Testing and the path to the car

1. **Bench (laptop, no car).**
   - The mock QtCarServer from feat/summon is reworked to Tesla's flow: style, status and location pushed on
     connect, heartbeat echoes, `cmd_result`, 250 ms location sampling, keepalive, and the standby quirk.
   - End to end: phone session, agw relay, helper, mock car.
   - D2 tests:
     - lost frames;
     - a stalled link;
     - a link delaying everything by seconds (D2 must trip, and a queued verb must be refused);
     - a stall followed by a flush (nothing resumes);
     - a verb while verbActive.
   - D3 tests: each verb type, a stale car position, and `device_location` passing through.
   - H4, the finish hook and the name queue under SIGKILL and SIGSTOP of the child and of the supervisor.
   - Schema accept, drop and reject, revocation through the watched allowlist, and both peer-uid checks.
2. **Device (no car).**
   - The release rules: lift, cancel, slide off by 30 pt, slide back in, a second finger, and a remount or recycle
     mid-hold (mode toggle, status change).
   - Background and resign-active.
   - A quick tap during a JavaScript stall.
   - The time from touch-up to the last `LIVE` frame.
   - Heartbeat jitter during map pans and the fix rate of a stationary iPhone.
   - The look against the mockup and a screen recording of Tesla's app.
3. **Install.** Phase 0 runs first, as its runbook says. The dbus uid cache left by the 2026-11-20 overlay test was
   cleared on 2026-11-21 (a root ReloadConfig; uid 4950 is now refused by the bus), so no reboot is needed for it.
   Then install with `SUMMON=1`, once the car-initiated hop (§6) is on the Pi and the car.
4. **No-motion car checks** (the owner at the car, with `summonhelper probe`; no verb is ever sent):
   - **Does QtCarServer accept the helper?** With a non-stock owner of `com.tesla.Webrtc`, `data_channels_ready`
     should bring style, status and location, and car frames should arrive. The caller of those frames is
     identified. This is first, because nothing else matters if it fails.
   - **The stream freeze:** 10 Hz heartbeats for 120 s, expecting about 1,200 echoes.
   - KeyInfo as uid 1337; whether user `webrtc` receives KeysUpdated; the VIN read by the root run script.
   - The connect pre-prime (`GUI_selfParkState`, `GUI_selfParkActivation`).
   - The phone-to-helper round trip and its jitter on the path the owner will use, with the camera strip running,
     against F.
   - Runtime config reads, the keepalive effect, and a gear probe.
5. **Supervised motion tests** (the owner only, under §5.1), in this order:
   - **Release-to-standstill (R23)** on the release path and on the injected-abort path, watching
     `GUI_selfParkState` and speed. It sets the operator's clearance before any walking test.
   - Pause and resume (UserPaused, then Resume), with `DAS_setSpeed` and `DI_autoparkState` during the pause.
   - A static Come to Me.
   - A walking Come to Me.
   - A short Go to Target target.
   - The arrows.

**Biggest risks:**
1. QtCarServer may not accept a non-stock owner of `com.tesla.Webrtc`. It never has had one.
2. The car stream may still freeze after about 150 messages. If it freezes before the first `cmd_result`, the
   phone waits forever; after it, Tesla's logic disconnects after 10 s.

Check 4 settles both before any motion.

## 9. Open items and how each is settled

| Item | Settled by |
|---|---|
| The car-to-Pi transport | settled 2026-11-21: the car dials out to the Pi (§6); its round trip is measured in check 4 |
| QtCarServer accepts a non-stock owner of `com.tesla.Webrtc`; who calls `CarserverToPeerCommsData` | no-motion check 4 (first) |
| The stream freeze with the helper | no-motion check 4 |
| KeyInfo and KeysUpdated for uid 1337 | no-motion check 4 |
| The connect pre-prime and prep timeouts | no-motion check 4 |
| Round trip and jitter against F, with the camera running | no-motion check 4 |
| Speed units and gear on the wire during motion | gear probe, then the first supervised run |
| Release-to-standstill time | supervised test 5, first |
| Does the APE hold the car in UserPaused and resume on a re-press? Pause limits (35 s; the APE's own caps) | supervised test 5 |
| A moving Come to Me goal; Go to Target (never run on this car); stop distance | supervised test 5 |
| Whether the Grid hold survives leaving P | supervised test 5 |
| `public_key_hash` format in `info` | decompile (fidelity only) |
| `requireEuBLECheck` and R79 on this car | no-motion check 4 (DV reads) |
| Appendix A's UI and status rows | read-only verification pass before the screen is built |
| Held-button look, crosshair offset, target icon size | screen recording of Tesla's app |
| iOS parity of the status/enable selectors | decompile of the iOS bundle |

## 10. Left to the builder

- Names (binary, socket paths, frame type numbers), the relay record format and size limits.
- How the Tesla state machine is implemented, as long as it reproduces Appendix A, quirks included.
- Code layout and test structure.
- **Build order:**
  1. In feat/summon, merge feat/camera-capabilities (e04cd11 on 2026-11-21, with the car dial-out), taking the
     camera side for agw. In the same step, delete `capability/summon`, the geo and cool-down guards,
     `agw supervise`, `agw summon-finish` and their tests; keep `internal/carlink`, `internal/supervise` and
     `internal/guards/route.go`. A trial merge on 2026-11-21 conflicts in `cmd/agw/main.go` (7 hunks),
     `README.md` (2) and `cmd/agw/main_test.go` (1).
  2. The helper, the relay, the probe and the bench.
  3. The installer rewrite (§6.6).
  4. The no-motion checks can run as soon as 2 and 3 exist, before any phone work.
  5. The phone link and motion controls.
  6. The screen, after the Appendix A verification pass, with a fake link until the bench path is end to end.
  7. The camera strip.
  8. The supervised tests.
- **Coordination.**
  - The mobile work starts from the camera session's committed mobile changes (gateway provisioning, the camera
    client, the Home screen, Info.plist), so those need committing first.
  - The installer rewrite and the camhelper gate exceptions (§6.5) go to the camera session for review.

## 11. Calls made in this design (for the owner to check)

1. **D2 counts only fresh frames, and every motion frame carries a tick.** Late heartbeats are not forwarded, a late
   verb is refused, and after a trip the heartbeats stop so Tesla's poor-connectivity text shows. F = 150 ms is the
   value from the approved SUMMON plan. Holds need a round trip under about 150 ms: the Pi's Wi-Fi or a direct
   Tailscale path, measured in check 4.
2. **Releases are enforced natively with Tesla's exact rules,** including slide-off and one press at a time. Every
   release aborts, as in Tesla's app.
3. **D3 applies to every verb at each press,** the arrows included (the research self-abort was on Forward). A
   stale car position also refuses the press. All motion controls are disabled within 3 m.
4. **The helper reads agw's own allowlist file:** one list for camera and Summon, revoked at once.
5. **Non-conforming messages are dropped** if harmless, and end the session if they are verbs.
6. **`summonhelper probe`** is the no-motion test tool. It sends only heartbeats and keepalives, so no key leaves
   the phone and the app gets no hidden controls.
7. **iOS only in v1.**
8. **This work rewrites the installer's `SUMMON=1` path,** reviewed by the camera session.
