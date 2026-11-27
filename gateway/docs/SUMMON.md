# Summon, car side (`summonhelper` and agw's SUMMON relay)

Design: `docs/superpowers/specs/2026-11-20-summon-design.md` (approved by the owner on 2026-11-21) and its Appendix A,
`docs/superpowers/specs/2026-11-20-summon-tesla-behaviour.md`. Plan:
`docs/superpowers/plans/2026-11-21-summon-car-side.md`. Where this file differs from them, "Plan amendments" at the
end says why. Installing and removing: `deploy/README.md` ("SUMMON"). The wire: `docs/CAPABILITY-API.md` (the SUMMON
relay row and "Inner SUMMON capability").

**Status.** On the car since 2026-11-26: gateway `841d3df`, installed with `SUMMON=1`. The 2026-11-26 review fixes
(the live-file lock and the presence lease) replace its summonhelper at the next install: "Installing the 2026-11-26
review fixes" below. The phone side is built (mobile `feat/summon-phone`); its presence half (`feat/summon-presence`,
0df9858) goes on the iPhone only after that install.

**Safety floor.** Nothing here commands motion, and the assistant never does. The owner runs every car step. The only
car steps in this file are the installs and the no-motion checks below, and none of them sends a verb. `SUMMON=0`, the installer's
default, installs nothing for Summon. Nothing is ever sent to 192.168.90.0/24 (R22).

**Firmware.** Appendix A's firmware facts (H1's connect edge and the style it pushes, the 10 s heartbeat stop timer,
H4, H6, `cmd_result`) were reverse-engineered on 2026.8.6. The car is moving to 2026.38.6 (owner, 2026-11-22), whose
Summon binaries are decompiled but not deep-read. Check 4's probe is the first car step that would show a 38.6
difference, and it moves nothing. One difference already matters: 38.6's `/usr/bin/authd` is not 8.6's (the two dumps
hash differently), and the compiled-in authd pin is 8.6's. "2026.38.6: the authd pin" below says where the pin comes
from and how the installer keeps it.

## Architecture

### Processes and uids

| Process | uid | What it does | Code |
|---|---|---|---|
| `agw serve` | 4950 | Faces the network (it dials the Pi). Its SUMMON capability (0x01) relays the phone's inner session to the helper's socket as opaque bytes. It has no bus access and no helper key, so it cannot drive | `capability/summonrelay` |
| `summonhelper run` | 1337 (`webrtc`) | The supervisor. It runs `serve` as its child and queues for `com.tesla.Webrtc` behind it. When the child is gone, it takes the name and, while the session marker exists, sends `cmd_abort` and the disconnect. Then, on every branch, it removes the live file the child left, before it releases the name | `internal/summon/helper/takeover.go`, `internal/supervise` |
| `summonhelper serve` | 1337 | The helper. It owns `com.tesla.Webrtc` (DO_NOT_QUEUE) and serves the inner session with the gateway server core on the relay socket. It runs H1-H4, D2, D3 and the presence lease, and holds the live file, locked, while the engine of a session that opted into the presence lease is up. It is the only process the network can reach that can issue a motion command | `internal/summon/helper/helper.go`, `internal/summon/helper/live.go`, `internal/summon` |
| `summonhelper finish` | 1337 | runit's finish hook, after every exit of `run`. It first removes a live file left behind, whatever the marker says. If a session marker is left (a killed supervisor), it then sends `cmd_abort` and the confirmed disconnect; otherwise nothing more | `cmd/summonhelper/main.go` (`cmdFinish`), `helper.Finish` |

Around them:
- The helper's runit `run` script runs as root (`deploy/install-car.sh`, `gen_summon_scripts`). It makes
  `/run/airgapp-summon` (1337:4950, mode 2750, refusing a symlink), removes a stale `relay.sock`, a stale live file
  and any `.live.*` temp file, reads the VIN from `/var/etc/vin`, and refuses while `com.tesla.Webrtc` has an owner.
  Then it runs `SUMMONHELPER_VIN=<VIN> exec chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper run`, with no group
  4950.
- QtCarServer, user `tesla` (uid 1111 in both dumps' `/etc/passwd`), is the car's end. It calls
  `CarserverToPeerCommsData` and `RemoteToLocalSignaling` on `/Webrtc` and listens to the helper's signals.
- `summonhelper probe` (uid 1337) is the owner's no-motion tool. It runs only while the service and stock
  `webrtc-comms` are down.
- agw's build cannot reach the code that drives the car: `TestAgwCannotReachTheCar` (`cmd/agw/deps_test.go`) fails
  if `go list -deps` of agw, for linux/amd64 or darwin, ever lists `internal/carlink`, `internal/summon` (the engine),
  `internal/summon/helper`, `internal/summon/probe` or `client`.

Paths: the socket `/run/airgapp-summon/relay.sock` (0660, group 4950 from the setgid directory), the session marker
`/run/airgapp-summon/session`, the live file `/run/airgapp-summon/live` (next), and the helper's state
`/var/lib/airgapp-summon/state` (1337:1337 0700, holding `key.pem` and the session log `log`). There is no
allowlist: neither agw nor the helper keeps a key list (access is the car's own, next to "Sign-in" below).

**The live file** (`internal/summon/helper/live.go`) is camhelper's trust signal. camhelper's SUMMON mode waives its
gear, Sentry and camera-access gates for the Summon camera, and it is granted only while this file's lock is held
(`docs/CAMERA-HELPER.md`, the `summon` op). The session marker cannot serve there: it stays after an unclean end, for
the takeover's abort. The path is `proto.SummonLivePath` (`camhelper/proto`), whatever `-marker` says.
- **Only a session that opted into the presence lease has one** (ruling F1, 2026-11-26's final review). agw can keep
  a session that did not opt in up about 20 s per frame it held back ("The presence lease" below), so such a session
  must not vouch for the camera: it keeps Summon, the 20 s lease and H4, makes no live file, and gets no SUMMON mode.
  The Summon camera therefore needs a phone build that sends `{"presence":1}` in its inner ARM params. The helper
  logs which a session is, once, as it starts: `summon: presence lease on for this session` or
  `summon: presence lease off for this session (the phone did not opt in; no Summon camera)` ("Logs").
- **On.** Once the Start of a session that opted in has succeeded, `serve` creates a temp file `.live.<16 hex digits>`
  in the directory (`O_CREATE|O_EXCL|O_NOFOLLOW`, mode 0600; group 4950 from the setgid directory), takes
  `flock(LOCK_EX)` on it, writes its pid and renames it to `live`. It keeps the file open, and so locked, for the
  session's life. If any of that fails, the session goes on without the Summon camera.
- **Off.** When `serve` ends a session, for whatever reason, the file goes before H4's Stop. `serve` unlinks `live`
  only while that name is still this session's file (the same device and inode), then closes it. The close drops the
  lock even when the unlink failed, so a file left that way proves nothing. A `serve` that dies or is killed runs no
  off: the kernel drops its lock, and the removals below take the file.
- **What camhelper counts.** A regular file in a real directory, both owned by 1337 and not group- or
  other-writable, whose lock is held: camhelper's `flock(LOCK_SH|LOCK_NB)` must fail with `EWOULDBLOCK`. The kernel
  drops the lock when `serve` exits, so a file a dead helper left counts for nothing, and agw cannot open the 0600
  file to hold a lock itself.
- **Stale files are removed** wherever a helper's end is handled, whatever the marker says, and the removal never
  changes the result of Finish or the takeover:
  - `serve`, before it asks for the name;
  - the supervisor's takeover, on every branch, after any abort and before it releases the name;
  - `Finish` (`summonhelper finish`, and the supervisor's own first step), before its no-marker return;
  - the runit `run` script, beside `relay.sock`, with any `.live.*` temp file;
  - the installer's `SUMMON=0` removal, once no `summonhelper` process runs.

  The removals matter despite the lock: a SIGKILLed `serve` stuck in D state keeps its lock until it exits, and the
  takeover's removal is then what ends SUMMON mode.

### Two sessions, one inside the other

```
phone ──outer session (agw's pin)──▶ Pi ──▶ agw serve (4950), SUMMON relay 0x01
   └──inner session (the helper's pin), carried as INNER bytes──▶ relay.sock ──▶ summonhelper serve (1337)
                                                                       ──D-Bus com.tesla.Webrtc──▶ QtCarServer
```

- **Outer:** the gateway session agw serves. The phone ARMs capability SUMMON (0x01), agw's relay:
  `Spec{Name: "SUMMON", ID: 0x01, In: {0x10: 16384}, Out: {0x90: 16384}, LeaseMS: 20000, TickHz: 0}`. Both types use
  the priority lane, and their payloads are chunks of the inner session's byte stream, which agw never parses.
- **Inner:** a second gateway session, phone to helper, in the outer window's bytes. It is the same protocol (hello,
  HPKE, ARM, frames, lanes, lease, freshness ticks) with the helper's own key and pin: `summonhelper pin` prints
  `airgapp-gw:1:<key hex>:<VIN>`, agw's format. agw never has that key, so, once the phone has pinned the helper's
  own pin (sent the non-printing way, "The pins" below), it cannot terminate the inner session: it can neither read,
  forge, reorder nor replay its frames. Frames open only in order, so if agw drops one, the session ends: whatever it
  passes next fails to open or to parse, and if it passes nothing, a lease ends it. What agw can do is delay frames
  and close the session, which ends it with H4. Delay can stretch a session: each frame agw held back renews the 20 s
  lease when it arrives, so a phone that has not opted into the presence lease can be kept up about 20 s per held
  frame after it left. One that has is kept up at most 10 s past its last fresh tick echo, or, before any echo, 10 s
  plus up to 2 s past its ARM ("The presence lease" below). Only a session that opted in holds the live file, so the
  longer stretch never reaches the camera's SUMMON mode ("The live file" above).
  Sign-in: the phone proves possession of its key (the handshake), and authd's KeyInfo must list that key as OWNER
  or DRIVER (the car's own keys, managed in the Tesla app or with the key card), asked directly on the bus with the
  authd pin. No key list is involved, and nothing is provisioned over SSH. A key that is not OWNER or DRIVER is
  refused at its hello, with no challenge (no VIN binding, no helper key), and authd is asked again after the ARM
  (the gate) before the session slot's state is told. The re-checks' authd connection is warmed at startup with the
  sign-ins' (step 7), so the first re-check, about 10 s into a session, never pays for the connection's setup. The inner capability is
  `Spec{Name: "SUMMON", ID: 0x01, In: {0x14: 8, 0x15: 1036, 0x16: 524, 0x17: 2048, 0x18: 8}, Out: {0x91: 16384}, LeaseMS: 20000, TickHz: 20}`.

### Inner frame types

| Type | Name | Body | Way |
|---|---|---|---|
| 0x14 | LIVE | tick id (8 bytes) | phone → helper |
| 0x15 | VERB | tick id (8) ‖ press id (u32 BE) ‖ Tesla's `cmd_find_me`, `cmd_forward` or `cmd_reverse` | phone → helper |
| 0x16 | HB | tick id (8) ‖ press id (u32 BE) ‖ Tesla's `heartbeat_app_webrtc` | phone → helper |
| 0x17 | MSG | Tesla's `cmd_abort`, `device_location`, `info` or `control:keepalive` | phone → helper |
| 0x18 | PRESENCE | tick id (8 bytes) | phone → helper |
| 0x91 | CAR | channel (1 byte: 0 `dcTcp`, 1 `dcUdp`) ‖ the car's JSON, verbatim | helper → phone |

The relay's own types are 0x10 and 0x90. The inner ones differ, so a frame sent to the wrong session is refused as
grammar instead of being misread (plan call 4). The helper parses press ids and logs them; no rule applies to them.
PRESENCE serves only the presence lease (below): it reaches neither the engine nor the car, and it is never a D2
liveness instant.

### Relay records

On `relay.sock`, both ways, a record is type (u8) ‖ length (u16 BE) ‖ payload (`internal/summon/relay/record.go`):

| Record | Payload |
|---|---|
| `0x01 INNER` | 1..16384 bytes of the inner stream, either way |
| `0x02 LINK` | helper to agw, 1 byte, 0 or 1: the link flag |
| `0x03 OWNED` | helper to agw, empty: the first record of a connection, exactly once |

Anything else closes the connection: an unknown type, a length of 0 on INNER, an oversize record, a LINK payload
other than 0 or 1, or OWNED anywhere but first.
- The helper serves one relay connection at a time, only from uid 4950 (SO_PEERCRED). It writes OWNED only while it
  owns `com.tesla.Webrtc` and is healthy, and hands the connection to its core at the first INNER record, which must
  come within 10 s (plan calls 1 and 5).
- agw's Open requires the socket's server to be uid 1337 and OWNED within 500 ms; `agw probe-summon` replays that
  check exactly, with the same 500 ms. agw writes through a queue of 64 bodies, with a 1 s deadline per write.

### What the helper originates

Only:
- `{"data_channels_ready": true}` (H1), at session start;
- `{"msg_type":"autopark:cmd_abort"}`, from D2's trip, H4, the supervisor's takeover and the finish hook;
- the disconnect map `{"ice_state_change": true, "msg_type": "autopark:set_webrtc", "set_webrtc_value": false}`;
- refusal `cmd_result`s, to the phone on `dcTcp`:
  `{"cmd_type":<the request's msg_type>,"msg_type":"autopark:cmd_result","reason":<reason>,"result":false,"timestamp":<ms>}`.
  The timestamp is max(the wall clock in Unix ms, the highest `cmd_result` timestamp seen in the session). The
  reasons are `action_invalid` (a stale verb, or a verb while one is active), `location_params_invalid` (D3: too
  close, or out of range) and `invalid_vehicle_location` (D3: no car position, or a stale one).

It never sends a heartbeat, a motion verb or a `device_location` of its own. It does not forward the `cmd_result` of
an abort it originated: it swallows one per injected abort. Everything else it sends the car is the phone's own
message, checked against Tesla's shape and re-encoded (H2). The probe sends only `data_channels_ready`,
`heartbeat_app_webrtc`, `control:keepalive` and the disconnect map, and its package has no code path for a verb, an
abort or a `device_location`.

### D2, the deadman, and its bounds

- **Freshness.** LIVE, VERB and HB carry the newest tick id the phone has. A frame is fresh when its tick was issued
  in this session at most F = 150 ms before it arrived (`Window.TickAge`, on the helper's BOOTTIME clock).
- **Liveness instants** are fresh LIVE and HB frames and forwarded verbs. **The trip:** while a verb is active and
  more than 300 ms have passed since the latest one, the helper injects `cmd_abort`. It swallows that abort's
  `cmd_result`, keeps the session, and forwards no HB until the next forwarded verb. The D2 loop runs every 10 ms.
  So on a lost or stalled link the abort goes out about 300 ms after the last fresh frame arrived: at most
  F + 300 ms plus one 10 ms loop period (about 460 ms) after that frame's tick was issued. Design §5.2 rounds this to
  450 ms; the bench measured trips 301-310 ms after the last liveness instant.
- **Refusals:** a stale verb, or a verb while one is active, gets `action_invalid`.
- **The link flag:** 3 LIVE tick ages in a row over 100 ms set it; the end of a hold, or 2 s of ages at or under
  75 ms, clear it. agw passes it to `Host().SetSummonLinkDegraded`, and CAMERA then sends keyframes only.
- **Health:** the helper is unhealthy when the D2 loop has not stamped for over 250 ms, when the name is lost, or
  when an emit failed. An emit that overruns 100 ms latches the bus dead and exits with code 3 (plan call 3). A Fatal
  (a panic in an engine's Stop or Tick, a code bug) exits with code 4. Both exits are immediate, so the takeover's
  abort never waits, and the panic's own log line is usually lost with the log queue: the supervisor's
  `supervise: child pid <pid> exited on its own: exit status 3` or `… 4` line says which it was.
- **The supervisor:** the child writes its liveness pipe every 50 ms while healthy, and the parent SIGKILLs it after
  100 ms of silence (5 s of grace at start). It then waits up to 1 s for the name and, while the marker exists,
  sends `cmd_abort` and the disconnect. The parent also SIGKILLs the child when its queue request for the name fails
  or gets the name at once (the child had it and lost it; the parent keeps it and takes over), and when its own bus
  connection closes; each of these ends `run` non-zero, so runit's finish runs too.
- **The hang bound (R25):** a hang is caught within 250 ms of no progress plus 100 ms of liveness silence, so the
  abort lands about 0.4 s after a hang. A crash is caught at once.
- **H4, the session's end:** the live file goes first, then `cmd_abort` if a verb is active, then the disconnect map
  exactly once, confirmed by a round trip on the same bus connection. The marker is removed only after that
  confirmation, and never after any failed send in the session. No authenticated frame for the 20 s lease ends the
  session, and so does the car's `RemoteToLocalSignaling` type 9, which it sends 60 s after a disconnect (Appendix A
  §1.2, H4). For a phone that opted in, so does the presence lease (next).
- **The presence lease** (review F5r; `internal/summon/capability.go`, `frames.go`). The 20 s lease counts arrivals:
  a frame that opens in sequence renews it however long agw held it back. An idle phone sends a HEARTBEAT every 5 s,
  a keepalive every 15 s and a `device_location` on each fix, so agw could hold those back and release one every
  < 20 s after the phone has left, each buying about 20 s of session. The presence lease bounds that for a phone
  that opts in. Only such a session turns the live file on (ruling F1), so the camera's SUMMON mode is always under
  the presence lease's bound.
  - **Opt-in:** only through the authenticated inner ARM params, which agw can neither read nor change: an object
    whose `presence` is a JSON number equal to 1 (`{"presence":1}`; `1.0` and `1e0` count too). No params, `null`,
    `{}` or an object without `presence` leave it off, and other keys are ignored. Any other `presence` (`true`,
    `"1"`, `0`, `2`, `null`), a value that is not an object, or malformed JSON refuses the session:
    `summon: the session did not start: bad params`, and the core closes the connection with no message.
  - **Stamps:** a PRESENCE, LIVE, VERB or HB whose tick the window knows (`TickAge` ok) and is at most 3 s old
    (`PresenceFresh`) stamps that tick's issue time: now less its age, rounded up to the ms. Stamps only move
    forward. VERB and HB stamp before their JSON is parsed, so a fresh echo counts even when its message is dropped.
    Every session takes stamps; only the close depends on the opt-in.
  - **Expiry:** in a session that opted in, every D2 loop pass (every 10 ms) checks, after the engine's Tick, whether
    more than 10 s (`PresenceAfter`) have passed since the stamp: 10000 ms stays open, 10001 ms closes. A clock
    behind the stamp counts as expired. The close is logged and is the normal teardown with the reason `lease`: the
    live file goes, then H4 (`cmd_abort` if a verb is active, the confirmed disconnect, the marker), then BYE
    `lease`, as at the 20 s lease.
  - **The bound:** Open seeds the stamp with its own time. agw can hold the inner ARM back until the core's 2 s
    pre-auth deadline, so at a session's start the bound is 10 s plus up to 2 s. Once a fresh echo has stamped, it is
    10 s from the issue time of the newest tick stamped: an echo agw held more than 3 s stamps nothing, and one it
    released sooner stamps its tick's issue time, not its arrival, so holding frames back gains agw nothing more. A
    phone that has not opted in can still be kept up about 20 s per held frame, but its session has no live file, so
    that stretch never reaches SUMMON mode. Neither moves the car: every verb still needs a tick echo at most 150 ms
    old.
  - **An idle phone stays up:** the phone sends PRESENCE about once a second whether or not a control is held, so it
    is dropped only once more than 10 s have passed since the issue of the newest tick it echoed at most 3 s old.
    After its last such echo, that is 7 to 10 s (10 s less that echo's age) in which every echo arrived more than
    3 s late. PRESENCE makes no engine call and touches neither D2, D3, the link flag nor the car.
  - **Rollout:** the helper must be on the car before any phone build sends 0x18. An older helper's Spec has no 0x18,
    so it answers PRESENCE with BYE `grammar`. The phone's half (mobile `feat/summon-presence`, 0df9858) is held back
    until then. This helper accepts PRESENCE from a phone that has not opted in: it stamps it and enforces nothing.
    Until the phone's half is on the iPhone, every session is opted out: Summon works, but there is no live file and
    no Summon camera. The order and the checks are in "Installing the 2026-11-26 review fixes". Making the opt-in
    mandatory for Summon itself is a later change (the 2026-11-26 design's non-goals).
  - **Tests:** `TestPresence*`, `TestLiveNeedsTheOptIn` (ruling F1) and `TestInnerSpec` (`internal/summon`), and the
    held-back-frames attack, end to end on the real `Serve` (`internal/summon/helper`):
    `TestHeldBackFramesCannotStretchSession`, its opted-out control `TestHeldBackFramesStretchAnOptedOutSession` (no
    live file throughout), and `TestSilentAgwEndsSessionAtLease`.

### D3, the 3 m floor

At each verb (`cmd_find_me`, `cmd_forward`, `cmd_reverse`), in this order (`internal/summon/engine.go`,
`floorLocked`):
- no car position, or a stale one: `invalid_vehicle_location`;
- the phone position or the goal outside −90 ≤ lat ≤ 90 and −180 ≤ lon ≤ 180, a NaN included:
  `location_params_invalid` (R28);
- the phone position or the goal closer than 3.0 m to the car: `location_params_invalid`. The goal is
  `goal_latitude`/`goal_longitude` when both are present, else the phone position, as the car derives it. The arrows
  carry no goal, so their goal is the phone.

The car position is the latest `vehicle_data:location` relayed in the session. `device_location` is never checked: it
goes to the car as in Tesla's app (owner, 2026-11-21).

**Staleness** (plan call 2, as amended; `staleLocked`'s doc in `internal/summon/engine.go`). It runs only while a verb
is active, and quiet hold time is summed across holds (the carry rule). Any car frame resets the no-frame window's
sum; only a location frame resets the motion window's.
- A position goes stale after more than 1 s with no car frame of any kind.
- It also goes stale after more than 1 s with no location frame:
  - while the last location showed a speed other than 0, summed over holds; or
  - while it showed gear D or R at speed 0, within one hold.
- Unknown gears (Invalid, SNA) do not count as motion.
- Each window counts from the later of its last frame and the verb's forwarding, so silence before a press never
  counts. The rules are checked at every 10 ms tick of the D2 loop and at the phone's release.

Once stale, the position stays stale until a newer location frame arrives. A stationary car sends location only
when it changes, so in practice every later press in that session is refused with `invalid_vehicle_location` until
the session restarts (H1 makes the car push a location). Tesla's app would resume there: this is the owner's 3 m
floor rule bending Tesla's behaviour.

**The 3 m boundary, for the phone plan.** The helper uses haversine with R = 6371 km (`guards.DistanceM`) on the
coordinates as received, and refuses when d < 3.0 m. A phone that pushes a Go to Target point out to exactly 3 m
(MapKit's geodesic is ellipsoidal) sits on that boundary, so the phone plan must choose a margin. Task 7's review
suggested pushing targets to at least 3.05 m and enabling the controls only from 3.05 m, measured with the same
haversine.

### Callers, revocation and keys

- **Car-frame callers.** The helper accepts car frames from any caller. It logs the first caller of each session and
  every change of caller (R25): `summon: the session's car frames come from "<sender>" (uid "<uid>")`, then
  `summon: the session's car frames now come from …`. The uid there is the frame's own `uid` key. Check 4's
  `callers` line identifies QtCarServer's connection, so pinning to it can follow later.
- **Revocation.** A key the car stops listing as OWNER or DRIVER (removed in the Tesla app or with the key card) is
  revoked by the authority re-check: the helper re-checks an open session's key through KeyInfo every 10 s, on an
  authd connection of its own (its sign-ins use another), and agw re-checks its outer window at once on authd's key
  change (camhelper's keys_changed). Whichever acts first, the other session ends with it, and H4 sends one abort and
  one disconnect.
- **KeysUpdated.** The helper does not subscribe to authd's KeysUpdated (plan call 7). Check 4 only measures whether
  uid 1337 receives it, for a later decision (design §6.1).

### Logs

summonhelper's only logger is a queue of 1024 lines that one goroutine writes to stderr; when it is full, lines are
dropped and counted (`internal/summon/logq`). runit's svlogd keeps stderr in
`/var/lib/airgapp-gw/svlog/summonhelper/current` (3 files of 1 MB); the session log is
`/var/lib/airgapp-summon/state/log`. Car and phone messages are logged by `msg_type` and size. No log line holds GPS
coordinates, a key (fingerprints only) or the VIN.

The core logs a refused or failed SUMMON arm only as `agw: preauth: SUMMON PreArm refused: error` or `… Open failed:
error`, so each side logs its reason itself (`capability/summonrelay`, `internal/summon/capability.go`):
- agw: `summonrelay: PreArm refused: <reason>` (the route gate refused; no relay socket; not a socket; the socket's
  owner), `summonrelay: Open failed: <reason>` (the socket's server uid; no OWNED within 500ms; closed before OWNED:
  the helper refused this uid, does not own the bus name, or is unhealthy), and once per window
  `summonrelay: the window ends: <cause>` (the helper's records ended, the phone's priority lane refused a 0x90 (full,
  or the window was closing), or a failed write). A
  window the core ends itself (the lease, the phone's CLOSE, a revocation) gets no such line.
- the helper: `summon: PreArm refused: the car link is not owned and healthy` and
  `summon: the session did not start: <reason>` (a failed start, a session already up, or `bad params`: inner ARM
  params the presence lease's opt-in cannot read, which are never logged themselves);
- the helper, once for each session that started (a refused or failed one logs only its own line):
  `summon: presence lease on for this session` when the phone opted in, so the presence lease is enforced and the
  session holds the live file, or
  `summon: presence lease off for this session (the phone did not opt in; no Summon camera)`: the 20 s lease alone,
  and no live file ("The live file"). Neither line holds the params. The install's checks count the first
  ("Installing the 2026-11-26 review fixes", step 7);
- the helper's presence lease: `summon: no fresh presence for <ms> ms: ending the session (lease)`, once for the
  session it closes. A clock behind the stamp prints the wrapped difference (a number near 18446744073709551615), as
  D2's trip line does. Rarely, the line comes for a window that was already closing for another reason; its BYE then
  keeps that reason.

None of these lines holds tailscaled's prefs, a key, the VIN or anything of the inner stream.

The helper also logs its live file ("The live file" above; `internal/summon/helper`):
- `summonhelper: live file /run/airgapp-summon/live`, once at each `serve` start.
- `summonhelper: removed a stale live file (an earlier session did not end cleanly)` when `serve`'s start found one,
  or `summonhelper: removing a stale live file: <error> (unlocked, it proves nothing to camhelper; still locked by a
  dying process, it counts until that process exits)` when it could not remove it. Under runit it seldom fires: the
  run script and the supervisor's Finish remove such a file first.
- **A hand-run `summonhelper finish` during a session** removes that session's live file, if it has one: its
  removal comes first, whatever the marker says. Then it cannot get the name, which the session's `serve` holds, so
  it sends nothing. It logs `summonhelper: finish removed the live file`, then
  `summonhelper finish: com.tesla.Webrtc not acquired within 1s (carlink: com.tesla.Webrtc has another owner); the session marker stays`.
  That fails closed: the session goes on without the Summon camera. A hand-run `summonhelper run` does the same, but
  only with a valid VIN in `SUMMONHELPER_VIN`; without one it refuses before anything else. A hand-run
  `summonhelper serve` is refused before its removal and touches nothing: it needs a valid `SUMMONHELPER_VIN` and
  `AGW_LIVENESS_FD`, which only the supervisor sets, and logs `summonhelper serve: refusing to serve: …`.
- `summonhelper: takeover removed the live file` or `summonhelper: finish removed the live file`: a session was up
  when the child or the supervisor died, so the session's own off never ran. That includes an `sv down` or restart
  during a session, since the supervisor stops its child with SIGKILL. `… could not remove the live file: <error>
  (…)` in place of `removed the live file` means the file stays; it counts only while a dying process still holds
  its lock.
- `summonhelper: live file not created: <error> (no Summon camera for this session)`: the on failed, and the session
  goes on without the Summon camera. `summonhelper: live file not removed: <error> (its lock is released, so it
  proves nothing)`: the off's unlink failed.
- `summon: the live signal failed: <panic>`: a panic in the on or the off, recovered there. Open goes on, and so does
  the teardown's Stop (H4).

### R1, the route gate

agw's PreArm refuses when tailscaled advertises any subnet route other than the exit-node pair `0.0.0.0/0` and
`::/0` (`guards.RouteOff`, over tailscaled's LocalAPI). That is stricter than "not 192.168.90.0/24": a car that
advertises any other subnet never arms Summon. `agw probe-route`, which the installer runs as uid 4950, prints
`route gate: ok` or `route gate: refused`. If the car is a tailnet exit node, the tailnet ACLs decide whether
exit-node users can reach 192.168.90.0/24: prove it from the car's state and the ACLs (R22), never by probing.

### Build and platform notes

- `make car` builds `out/agw`, `out/camhelper` and `out/summonhelper` (static linux/amd64).
- The static agw grows from about 8.5 MB to about 11.7 MB, because `guards.RouteOff` brings in `net/http`.
- The Linux-only paths have not run on a Linux host: SO_PEERCRED, Pdeathsig, the Linux unix-socket buffers, and the
  dump settings (prctl `PR_SET_DUMPABLE` 0 and `RLIMIT_CORE` 0). The bench ran on darwin, so the car is their first
  Linux run. The installer's `agw probe-summon` exercises SO_PEERCRED both ways, step 6 below the dump settings, and
  step 7 Pdeathsig. The socket buffers wait for the phone link.

### For the phone plan

- **Messages.** Each phone message is checked against Tesla's exact shape for its `msg_type`
  (`internal/summon/schema.go` has the decision table), then re-encoded; only those bytes reach the car.
  - Every key Tesla always sends is required, with its type, and an unknown key makes a message non-conforming
    (plan call 9). Every message except `control:keepalive` and `cmd_abort` needs `created:timestamp`.
  - `client_version` must be the number 2, `summon_type` `find_me` or `pin_drop`, and `info`'s `txid` `<none>`.
  - **No nulls.** Values must be finite numbers or strings, as listed. Omit `heartbeat_app_webrtc`'s
    `car_timestamp` while there is none (Tesla omits it while it is 0 or null). Send numbers for `device_location`'s
    `accuracy`, `altitude` and `altitude_accuracy`; Tesla sends −1 when unknown. Otherwise every such heartbeat or
    `device_location` is dropped.
- **Frames.** Verbs go only in VERB, the heartbeat only in HB, everything else in MSG. `Parse`'s table
  (`internal/summon/schema.go`) decides each case. "Fatal" ends the session with H4: the abort if a verb is active,
  then the disconnect. "Drop" skips the message and logs it, and the session goes on.

  | Frame | Case | Result |
  |---|---|---|
  | any | a JSON object followed by more than whitespace | Fatal |
  | any | a top-level `_request` key, even on a `cmd_abort` | Fatal |
  | VERB | not a JSON object; no string `msg_type`; a `msg_type` that is not a verb (`cmd_abort` included) | Fatal |
  | VERB | a verb whose keys or values do not match Tesla's shape | Fatal |
  | HB | not a JSON object, or no string `msg_type` | Drop |
  | HB | any `msg_type` but `autopark:heartbeat_app_webrtc` (`cmd_abort` and unknown types included) | Fatal |
  | HB | a heartbeat whose keys or values do not match | Drop |
  | MSG | not a JSON object, or no string `msg_type` | Drop |
  | MSG | a verb or the heartbeat | Fatal |
  | MSG | a `msg_type` not in the table | Drop |
  | MSG | `autopark:cmd_abort` | accepted, whatever else it carries, and sent as `{"msg_type":"autopark:cmd_abort"}` |
  | MSG | a `device_location`, `info` or keepalive that does not match | Drop |

  So a `cmd_abort` in a MSG frame is always accepted; in a VERB or HB frame, or with `_request`, it ends the session.
- **Results.** Every verb gets a `cmd_result`: the car's, forwarded, or the helper's refusal, whose timestamp never
  goes below one already seen.
- **Presence** ("The presence lease" above; review F5r).
  - On every 20th tick of an up connection, about once a second, held or not, send PRESENCE (0x18) with the newest
    tick id. Its body is exactly the 8-byte id, as in LIVE; any other length ends the session.
  - Opt in with the inner leg's ARM params `{"presence":1}`: the JSON number 1, never `true`. The outer leg's params
    do not carry it (agw's relay ignores its params). Inner params the helper cannot read refuse the session: the
    connection closes with no message. The opt-in also decides the Summon camera: only a session that opted in
    turns the live file on, so one that did not gets no SUMMON mode (ruling F1, "The live file").
  - Handle a `lease` end (BYE `lease`, from the presence lease or the 20 s lease) as any drop that is not `revoked`.
  - **The helper ships before the phone.** An older helper answers 0x18 with BYE `grammar`, about a second after
    each session comes up. The phone's half is built (mobile `feat/summon-presence`, 0df9858) and is held back until
    a presence-aware helper, this one, runs on the car ("Installing the 2026-11-26 review fixes").
- **Reconnecting.** The helper takes the next relay connection only after the previous session's H4 went out, and
  agw waits 500 ms for OWNED. A hello right after a drop can therefore be refused once: retry with backoff
  (`docs/CAPABILITY-API.md`, "Reconnecting").

## The bench (no car)

Everything runs in one process on the Mac: the phone (`client`), a fake Pi (`dialouttest`), agw with the real SUMMON
relay and sign-in, the helper (`helper.Serve`), and the mock QtCarServer (`internal/carlink/mockcar`). Nothing leaves
loopback and unix sockets.

```sh
go test ./cmd/agw/ -run TestSummon -count=1 -v      # the bench, end to end (about 25 s)
go test -race -count=3 ./cmd/agw/ -run TestSummon    # what each task ran
```

It covers the happy path (on the in-memory bus, and on a private dbus-daemon in `TestSummonRealBus`), D2 (lost
frames, a stalled link, links late by 2 s and by 220 ms, a delayed verb, a stall then a flush, a verb while one is
active), D3 (the floor for each verb and for Go to Target's phone and goal apart, a stale position,
`device_location` passing through), the schema, and revocation (the car removes the key: both authority re-checks
fail; then each side's re-check alone, which forces the helper-first and the agw-first order).

The packages that start a private dbus-daemon (`internal/carlink/bustest`) are the bench's `TestSummonRealBus`,
`internal/carlink`, `internal/carlink/mockcar`, `internal/summon/helper`, `internal/summon/probe` and
`cmd/summonhelper`:

```sh
go test -count=1 ./internal/carlink/... ./internal/summon/... ./cmd/summonhelper/ ./cmd/agw/
go test -count=1 -v ./internal/carlink/... ./internal/summon/... ./cmd/summonhelper/ ./cmd/agw/ 2>&1 | grep -- '--- SKIP' | grep -v TestDialThroughRealPi
```

The second command must print nothing. `bustest.Start` skips a test when `dbus-daemon` is not on PATH
(`brew install dbus` on the Mac). The Summon bench skips when run as root: it passes its own uid as the helper's, and
the relay reads a helper uid of 0 as 1337. `TestDialThroughRealPi` (cmd/agw) is not a Summon test: it always skips
unless `AGW_E2E_PI_PHONE` and `AGW_E2E_PI_CAR` name a running gwforward-lab.

## 2026.38.6: the authd pin

camhelper's sign-in (agw's), the helper's sign-in and the probe's `-keyinfo` ask authd's KeyInfo only after
`/usr/bin/authd` hashes to their pin. A mismatch fails every check closed before any bus call
(`internal/authority/authd.go`, `checkPin`). The compiled-in pin is the 2026.8.6 build's, and 38.6's authd is
another build. So on 38.6:

- **The pin comes only from the camera runbook's step 2b pre-check** (`docs/CAMERA-INSTALL.md`,
  step 2b, which runs `deploy/signin_precheck.sh`). It hashes the car's `/usr/bin/authd`, picks the pin from the builds
  whose KeyInfo was verified, refuses an unknown build, and proves KeyInfo on the car: `owner=ok role=2` as uid 4950,
  through a temporary camhelper that it runs from the runbook's root-owned `/tmp/airgapp-install`, on its own state
  and sockets (`/run/airgapp-signin-precheck`, `/run/airgapp-signin-precheck-sock`), and removes again. So it also
  works next to an installed camhelper, after an OTA for example (the runbook's "After an OTA (gateway already
  installed)"). For 38.6 it prints `authd: 2026.38.6 build (install WITH AUTHD_SHA256=<pin>)`. Run it as that runbook
  says, with its own step-2 files.
- **The first full install on 38.6 carries that pin:** `AUTHD_SHA256=<step 2b's pin>` in front of
  `sh install-car.sh`, `SUMMON=1` included. The installer writes it into both helpers' run scripts. **Later installs
  keep it, unless the pin check flags a mismatch:** with `AUTHD_SHA256` unset, a full install, like `--after-boot`,
  keeps the installed camhelper run script's pin (`deploy/install-car.sh`, `resolve_authd_pin`). An explicit value
  wins, and with no pin installed the compiled-in one applies.
- **The pin check.** The plan's sign-in line shows the pin's first 8 hex digits and its source:
  `authd pin <8 hex> (given)`, `(kept from the installed run script)` or `(compiled-in)` (`authd_pin_line`). The
  installer compares the pin with `sha256sum /usr/bin/authd` (`check_authd_pin`). On a full install, a mismatch, or an
  authd it cannot hash, asks before anything is written:
  `camhelper's authd pin <8 hex> (<source>) does not match this car's /usr/bin/authd <8 hex>: camera sign-in will fail closed until you re-run with a verified AUTHD_SHA256 (runbook step 2b; after an OTA, its section "After an OTA (gateway already installed)"). Install anyway?`
  Anything but `yes` stops it with nothing changed. After a boot the same text is a `NOTE:` that blocks nothing and
  is repeated at the end as `!!! NOTE:`, and the run exits non-zero (the camera session's request: `--after-boot` is
  run by hand, so its status must show a stale pin). Its last line is then
  `install-car: the authd pin check failed (the NOTE above); every service was started`, or, when there is one, the
  Summon report instead (`install-car: Summon is not running: …`, `install-car: Summon checks failed: …` or
  `install-car: SUMMON=0 removal …`; `end_report`). The installer never adopts the car's hash as the pin.
- **Never copy a hash from an error or a note into `AUTHD_SHA256`.** The hash that the pin check, or a
  `role error: … authd changed` line, prints is the car's own build, not proof. Only step 2b's result counts.
- **Before check 4's step 2**, the helper's run script must carry the pin:
  `grep -c -e '-authd-sha256 [0-9a-f]\{64\}' /var/lib/airgapp-gw/sv/summonhelper/run` prints `1`, and
  `grep -o -e '-authd-sha256 [0-9a-f]\{8\}' /var/lib/airgapp-gw/sv/summonhelper/run` shows the same first 8 hex digits
  as step 2b's pin. `0` means no pin: on 38.6 the helper's sign-ins and the probe's roles then fail closed, and check
  4 cannot settle KeyInfo as uid 1337.

## Installing and removing

See `deploy/README.md`: "SUMMON", "Failures" and "After every boot". In short:
- **Before the first `SUMMON=1` install**, check on the car, read-only, that its `timeout` (a BusyBox applet) takes
  `-k`, which the name checks use: `timeout -k 1 5 true; echo $?` must print `0` (`deploy/README.md`). The firmware's
  own `/etc/runit/common:177` runs `timeout -k 21 20`, on 2026.8.6 and 2026.38.6 alike. On 38.6, bring step 2b's
  `AUTHD_SHA256` to that install ("2026.38.6: the authd pin").
- `SUMMON=1` on a full install copies `summonhelper` and makes its key. Its stop sequence (S0b) stops stock
  `webrtc-comms` at install and after every boot. The installer then checks, as uid 4950, `agw probe-summon` (expect
  `summon relay: ok`; a refusal prints `summon relay: refused: <reason>`; it waits 500 ms for OWNED, exactly as
  agw's Open) and `agw probe-route` (expect `route gate: ok`), each retried every 1 s up to 15 times.
- `SUMMON` unset keeps what is installed, the summonhelper binary included: it copies none. Only an explicit
  `SUMMON=0` removes the helper and hands the name back to stock. To replace the binary, pass `SUMMON=1` and the new
  `summonhelper`, as the next section does.
- **camhelper's live-file check and summonhelper's lock ship in one install.** Since the 2026-11-26 review fixes
  (camhelper from the camera branch's eed5773 on), camhelper counts the live file only while summonhelper holds its
  flock, and an older summonhelper (`841d3df`'s) writes it without one. With that pair every `summon on` is refused,
  and agw logs `camera: SUMMON mode refused (summon_unverified); asking again every …` while a CAMERA window is open
  during a Summon session; camhelper logs nothing. That agw line alone does not prove a mismatch: agw logs it too for
  any session whose phone did not opt into the presence lease (summonhelper then logs `summon: presence lease off for
  this session (…)`). The helper's `summonhelper: live file /run/airgapp-summon/live` line, which only this build
  logs, tells the two apart. So a camera install that keeps an older summonhelper (`SUMMON`
  unset, or a camera-only staging without summonhelper) leaves the Summon camera off: pass `SUMMON=1` and the new
  summonhelper in that install.
- **Refresh the kept installer after every install.** `--after-boot` runs `/var/lib/airgapp-gw/install-car.sh` and
  rewrites every service script from that copy's own templates, so an older copy puts its older scripts back at the
  next boot (for `841d3df`'s, a summonhelper run script without the live-file removal). After each install, as root:
  `install -m 0755 -o root -g root /tmp/airgapp-install/install-car.sh /var/lib/airgapp-gw/install-car.sh`.
- **Failures** (`deploy/install-car.sh`, `summon_soft`; `deploy/README.md`, "Failures"). A full install's checks
  before its first write stop it with nothing changed: the tools, the binaries, the `SUMMON` value, the authd pin
  check and the questions before step 1, which include steps 0, 4b and 4c. From step 1's `mkdir` on (`WRITTEN=1`), a
  Summon or removal failure, or a declined Summon question, skips Summon for the run: the helper is neither linked
  nor started, agw and camhelper still finish S2-S4, and the run ends non-zero with the Summon report
  (`Summon is not running: …`). That covers a failing 4b or 4c command (the copy, the state directory, the keygen), a
  name that stays taken at S0b, anything but `yes` at S0b's or the removal's question, a removal held up by a
  lingering `summonhelper` process, an unsupervised `summonhelper` found before S3, and a helper not running after S3.
- **The hand-back after a skip** (`stock_hand_back`). When S0b's stop sequence had already stopped stock
  `webrtc-comms` (a name that stays taken, an unsupervised `summonhelper`, a helper not running after S3), the run
  gives `com.tesla.Webrtc` back to stock for this boot after S3, as the `SUMMON=0` removal does: our helper is
  unlinked and, if its runsv runs, brought down (`sv -w 15 down`); once no `summonhelper` process runs (up to 10 s),
  and only if the name is free, stock is restarted and checked (`hand_back`). A name that has an owner starts
  nothing. A `summonhelper` that does not go away, or a stock that does not register, is reported at the end and
  never stops the run. A skip before the stop sequence (a decline, a missing tool, a failing 4b or 4c command, which
  all come before S0b) never stopped stock, so nothing is handed back.
- **On the way out** (`exit_hand_back`, the run's EXIT trap). It runs at any exit after S0b's stop sequence, before a
  hand-back settled it or the helper's checks passed: a die (a "no" at S2, S3 or S4, an unsupervised agw or
  camhelper, agw or camhelper not running), an error, `Ctrl-C` or TERM (exit 130 or 143), or the end. It decides:
  - our helper is linked (`helper_linked`: its `airgapp-summonhelper` link, which S3 makes) and supervised by a
    running runsv (`helper_supervised`: its runsv is up and has not been told to stop it or keep it down): it is the
    installed owner, so nothing is touched. After failed checks it stays linked and running, as the DONE text says;
  - otherwise it starts nothing: it is not linked (a first install that stopped before S3, or a skip that unlinked
    it), or its link has no running runsv (after a boot until S3 starts our runsvdir, since the link stays on
    /var/lib from one boot to the next), or its runsv was told to stop it or keep it down (S0b's `sv down`, in a run
    whose runsvdir was already up; the helper may still be stopping). Its runsv, if one still runs, is brought down;
    then, checking every 1 s up to 10 times, once no `summonhelper` process runs and the name is free, stock is
    restarted and checked (`install-car: on the way out: … restarting stock`). A `summonhelper` process that remains
    gets a line naming its pid, and a name that stays owned (stock still letting go of it, say) a line too; either
    way stock is not restarted.

  It never dies and keeps the run's exit status. Neither a dropped SSH session nor a dead reader of the output can
  kill the run or its exit path: the run ignores SIGHUP and SIGPIPE, and every command it starts inherits that. Over
  the dead session, the run ends at its next question (`no answer (stdin closed)`; S0b's question takes it as a no,
  a skip, and goes on) or at its next write there that `set -e` checks (an I/O error on a terminal, a broken pipe
  otherwise), and the exit path runs. A failed write that `set -e` does not check (in a condition, or before `||` or
  `&&`) ends nothing: the next question, checked write or the run's end does. A run whose answers and output do not
  go through the session goes on to its end. Not covered: SIGKILL and the other signals the run does not trap, which
  end it without the exit path.
- **A failed check after S4** (`probe-summon` or `probe-route`, 15 times) is reported at the end as
  `Summon checks failed: …`, non-zero. The helper stays linked and running on purpose (it is harmless, and you may
  look at it) and keeps the name, so nothing is handed back, and the DONE text shows no pin route for it.
- **After a boot** the same holds, and a missing `gdbus` or `timeout`, or `SUMMON=1` with no helper installed
  ("install with SUMMON=1 first"), skips Summon too. A `SUMMON` other than 0 or 1 still stops the run before anything
  (`SUMMON must be 0 or 1`), on a full install and after a boot.
- **Test hooks never reach the car.** The real install refuses to run while any `AGW_TEST_*` variable is set, even an
  empty one: `install-car: AGW_TEST_* is set: test hooks only, refusing the real install`.
- **The pins.** The installer prints no pin. Each pin holds the VIN, and the car records SSH sessions, so neither
  is ever printed: each goes to the laptop through a fresh file that is removed at once. The DONE text gives both
  routes: agw's in its step 3 (the camera runbook's step 6, run as uid 4950 with agw's own key:
  `P=$(mktemp) && chpst -u :4950:4950 /var/lib/airgapp-gw/agw pin -key /var/lib/airgapp-gw/state/key.pem > "$P" && wc -c < "$P" && socat -u FILE:"$P" "TCP:$LAPTOP:$PORT"; rm -f "$P"`,
  with `nc -l <port> > gwpin` on the laptop), and the helper's once its checks passed. For the helper's pin, first
  start `nc -l <port> > sumpin` on the laptop, and set `LAPTOP` to the laptop's tailnet address and `PORT` to that
  port in the car's shell. Then, as root, one line:

  ```sh
  P=$(mktemp) && chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper pin -vin-file /var/etc/vin > "$P" && wc -c < "$P" && socat -u FILE:"$P" "TCP:$LAPTOP:$PORT"; rm -f "$P"
  ```

  - `mktemp` makes a new file, mode 0600, with O_EXCL, so a file or FIFO that another uid planted in `/tmp` is never
    opened. That matters on this car: `/tmp` is a mode-1777 tmpfs, and the firmware's `sysctl.conf` sets only
    `fs.protected_symlinks` and `fs.protected_hardlinks`, so a fixed name there could be pre-planted (by a
    compromised agw, for example) to read or swap the pin. Nothing is sent unless the pin was written, and the line
    removes the file at its end. If you interrupt it (`Ctrl-C`), run `rm -f "$P"` before anything else.
  - The line sets `P`, which the probe's signals below also use: after a pin route, set `P=$(helper_pids probe)`
    again before signalling the probe.
  - `wc -c` must print `162`: `airgapp-gw:1:` (13 bytes), the key's 65-byte point in hex (130), `:` (1), the VIN (17)
    and a newline (`keys.PinString`, printed with a newline by `summonhelper pin`). Any other count: delete the
    laptop's `sumpin`, and start again.
  - The car's own scripts use `mktemp` (`/usr/local/bin/paniclog`, `/etc/runit/3`). If `command -v mktemp` prints
    nothing, use `P=/root/sumpin` instead of `P=$(mktemp)`: `/root` is a root-only tmpfs (mode 0700, nosymfollow;
    the firmware's `etc/fstab`), where no other uid can plant a file.
  - Optionally, check that the key arrived intact, comparing only the key field, never the VIN. On the car,
    `chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper pin -vin-file /var/etc/vin | cut -d: -f3 | sha256sum | cut -c1-16`;
    on the laptop, `cut -d: -f3 sumpin | shasum -a 256 | cut -c1-16`. Both print the same 16 hex digits, or delete
    `sumpin` and start again.

  Build the phone link on the laptop, never on the car.

## Installing the 2026-11-26 review fixes

The car has run `841d3df` with `SUMMON=1` since 2026-11-26. This install replaces all four of its files: summonhelper
(the live-file lock and the presence lease), camhelper and agw (the camera branch's work since `841d3df`, camhelper's
flock-based live-file check included), and `install-car.sh`. The owner runs it, and nothing in it sends a verb.
"Rules for the whole visit" (check 4) apply: be in position before waking the car, and keep every output small
(`grep -c`, `ls -ln`). Define the functions of "Once per SSH login" first.

**The order:** the car, then a check of its helper's build, then the presence phone build, then the checks of the
live file, its lock and SUMMON mode.
- The phone build waits for the car: `841d3df`'s helper answers PRESENCE with BYE `grammar` about a second into every
  session.
- The last checks wait for the phone: only a session that opted into the presence lease has a live file (ruling F1).
- Rolling the car back to an older helper needs the phone rolled back first.
- camhelper and summonhelper go in the same install ("Installing and removing"). With this camhelper and
  `841d3df`'s summonhelper, every `summon on` is refused, and agw logs
  `camera: SUMMON mode refused (summon_unverified); asking again every …` while a CAMERA window is open during a
  Summon session. agw logs the same line for any session that did not opt into the presence lease, so on its own it
  does not prove a mismatch: steps 5 and 7 tell the two apart.

1. **Build** (laptop), on the merged head (`feat/camera-capabilities` with these fixes): `make car`, then
   `shasum -a 256 out/agw out/camhelper out/summonhelper deploy/install-car.sh`, and write the four hashes down. All
   four differ from `841d3df`'s (agw `07cf4a48…`, camhelper `d8cc4662…`, summonhelper `83977dc1…`, install-car.sh
   `d39c6823…`). With go1.26.2, the code of these fixes builds the hashes below. A documentation change leaves them
   as they are; another Go toolchain, or any later code change, does not:
   - agw `f4ef404bc954f301984ecaff70875a0963375d8660d866c667fda301a206423e`
   - camhelper `6c24978b153ade7fbc1733cf5aceab28969661df01e945e04833d77912ed9d6e`
   - summonhelper `8c16d153b27059ea5687049c089331394adc89743cdb5c7b6da6c097b29c7c83`
   - install-car.sh `3e3df3f861f806d7d327027c935278f63dfafe28e634bdbf41f1d103bc777ea9`
2. **Stage all four**, summonhelper included, as the camera runbook's step 2 does (`docs/CAMERA-INSTALL.md`): the
   laptop's `cp` takes `out/summonhelper` too, and the car's fetch, `sha256sum` and `chmod` take `summonhelper` too.
   On the car, `sha256sum agw camhelper summonhelper install-car.sh` must print step 1's four hashes. Run the camera
   runbook's step 2b as it says: its `deploy/signin_precheck.sh` must pin this build's agw and camhelper first, or it
   stops at `HASH MISMATCH`.
3. **Install** (car, root, interactive):

   ```sh
   cd /tmp/airgapp-install && SUMMON=1 PI_IP=192.168.4.1 sh install-car.sh ./agw ./camhelper ./summonhelper
   ```

   - Put `HELPER_WRITER=aplay` in front only if
     `grep -c -e '-writer aplay' /var/lib/airgapp-gw/sv/camhelper/run` prints `1`: the installer writes `pw-cat`
     otherwise.
   - Leave `AUTHD_SHA256` unset: the installed camhelper run script's pin is kept, and the plan's sign-in line says
     `(kept from the installed run script)`. Step 4c leaves the helper's key, so its pin and the phone's pairing
     stay: no pin route is needed.
   - The plan must say `SUMMON=1: install summonhelper (/var/lib/airgapp-gw/summonhelper, uid 1337)`. A plan that
     says `SUMMON=1 (kept: summonhelper is installed; …)` and `no new binary` would keep the old helper: answer
     anything but `yes` at the first question, which stops the install before its first write, and fix the command.
   - Then the camera runbook's step 3 checks for agw and camhelper, and the installer's own `summon relay: ok` and
     `route gate: ok`.
4. **Refresh the kept installer** (car, root). `--after-boot` runs it and rewrites every service script from its own
   templates, so the old copy would put `841d3df`'s run script back at the next boot:

   ```sh
   install -m 0755 -o root -g root /tmp/airgapp-install/install-car.sh /var/lib/airgapp-gw/install-car.sh
   ```

5. **Check the build on the car** (car, root; no phone needed):

   ```sh
   sha256sum /var/lib/airgapp-gw/agw /var/lib/airgapp-camhelper/camhelper /var/lib/airgapp-gw/summonhelper /var/lib/airgapp-gw/install-car.sh
   grep -c 'summonhelper: live file /run/airgapp-summon/live' /var/lib/airgapp-gw/svlog/summonhelper/current
   grep -c 'airgapp-summon"/.live' /var/lib/airgapp-gw/sv/summonhelper/run
   ```

   Expect step 1's four hashes; then a count of at least `1`, since only this build logs that line (once at each
   `serve` start); then `1`, since only this installer's run script removes the live file and its `.live.*` temp
   files. A `0` from either `grep` means the old helper or the old run script is still in place: stop there, and do
   not install the phone build. Once all of it passes, `rm -rf /tmp/airgapp-install`.
6. **Install the presence phone build** (mobile `feat/summon-presence`) on the iPhone. Only now.
7. **Check the live file, its lock and SUMMON mode** (car, root): the car parked, the Summon screen open on the
   iPhone, no press.

   ```sh
   grep -c 'summon: presence lease on for this session' /var/lib/airgapp-gw/svlog/summonhelper/current
   ls -ln /run/airgapp-summon/
   s=$(helper_pids serve); echo "serve=$s"; grep -c " FLOCK .*WRITE $s " /proc/locks
   ```

   Expect:
   - a count of at least `1`: the session opted in. If the log has only `summon: presence lease off for this
     session (…)` lines, the phone build does not opt in: no live file, and no Summon camera;
   - `live` listed as `-rw------- 1 1337 4950`. The old helper's file looked the same, so this alone proves nothing;
   - `serve=` and one pid, then `1`: `serve` holds the file's lock.

   Then open a camera view during the session.
   `grep -c "SUMMON mode on (summonhelper pid $s)" /var/lib/airgapp-camhelper/svlog/current` prints at least `1`.
   If it prints `0`, and `grep -c 'camera: SUMMON mode refused (summon_unverified)' /var/lib/airgapp-gw/svlog/agw/current`
   is above `0`, camhelper found no locked live file.
8. **Close the Summon screen.** `ls -ln /run/airgapp-summon/` lists no `live`, and
   `grep -e 'SUMMON mode off' -e 'SUMMON mode ended' /var/lib/airgapp-camhelper/svlog/current | tail -n 1` shows a line
   from just now.

## Check 4: the no-motion checks (the owner, at the car)

Design §8 check 4, as plan call 10 splits it. These are the only car steps in this plan, and none sends a verb. The
probe (step 2) covers QtCarServer's acceptance, the caller, the stream freeze, KeyInfo as uid 1337 (on 38.6 only
with step 2b's pin) and KeysUpdated. Steps 3-7 cover the pre-prime, the runtime config, R79 and the keepalive, the
gear probe, the VIN the run script read, and the finish hook.

### Rules for the whole visit

- **Be in position before waking the car:** it re-sleeps about 5 minutes after waking. W below is the wake.
- **Time.** Steps 1-4 fit one wake: SSH and the helpers take about 20 s, step 1 up to about a minute, then the 60 s
  wait and step 2's 155 s, about 4.5 minutes in all. Steps 5-7 do not fit in the same wake. Either seat the occupant
  before about W+4 min, since an occupied car stays awake, or plan a second wake. A second wake starts as every wake
  after a doze does (the next rule), and then `name_owner`: with the helper down, `(true,)` means stock
  `webrtc-comms` is back (a run-mode switch restarts it), so do step 1 and its 60 s wait again.
- **After any doze or dropped SSH session**, a new login has none of the functions of "Once per SSH login" below:
  define them first. Then run `P=$(helper_pids probe); echo "probe=$P"`. If it prints a pid, a probe still runs:
  wait for its `probe exit` line, or end it with the checked `kill -INT` below. Run `finish` only when no probe runs
  and the marker is present. A probe that a doze or a dropped session cut into does not settle row 4: rerun step 2
  on the next wake.
- **Never stream bulk data over SSH.** The car records every SSH session, and when the record spool fills, sshd
  refuses every login (E22729) until a reboot. Read a probe's file only with `show_report` (below): its size, its
  last 8 KB and a count of dropped car calls, never a bare `cat`. Read logs with `grep -c` and `tail -n`, never a
  follow.
- **R22:** nothing is ever sent to 192.168.90.0/24, and none of our commands touches the network.
- **Never print the VIN or a pin.** Step 6 compares them inside the shell.
- **Never kill by name.** Stop our services with `sv`. Every kill here checks its pid first: step 7's, and every
  signal to the probe, which goes to `$P` only while `/proc/$P/cmdline` is still the probe's (below).
- **The person in the driver's seat never types while the car is in D.** In step 5 they only hold the brake and
  shift; a second person, if there is one, may run the SSH reads.
- **If the car moves** (nothing here sends a verb, so it should not), stop it physically (the brake, or open a door:
  the VCSEC abort) and end the probe, whichever comes to hand first, with the checked `kill -INT`. If you ran a pin
  route in this shell since the launch, it set `P`: set `P=$(helper_pids probe)` again first.

  ```sh
  tr '\0' ' ' 2>/dev/null </proc/"$P"/cmdline | grep -q '^/var/lib/airgapp-gw/summonhelper probe' && kill -INT "$P" || echo "nothing sent: P=$P is not the probe"
  ```

  `$P` is the pid recorded at the probe's launch. The line sends nothing unless that pid is still the probe: an empty
  `$P`, or a pid that ended and was reused, gets no signal, and the line says so. The probe's disconnect ends the
  session, a terminal abort on the car.

### Once per SSH login (root)

```sh
helper_pids() { for p in /proc/[0-9]*; do case "$(tr '\0' ' ' 2>/dev/null <"$p/cmdline")" in "/var/lib/airgapp-gw/summonhelper $1"*) echo "${p#/proc/}" ;; esac; done; }
name_owner() { timeout -k 1 5 gdbus call --system --dest org.freedesktop.DBus --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.NameHasOwner com.tesla.Webrtc; }
dv() { for d in "$@"; do printf '%s ' "$d"; /usr/local/bin/get-local-dv CarServer "$d"; done; }
show_report() { wc -c "$1"; tail -c 8192 "$1"; printf 'carlink drops: '; grep -c 'carlink: dropped' "$1"; }
```

- `helper_pids serve`, `helper_pids run` and `helper_pids probe` print the pids of those summonhelper commands.
  `helper_pids ''` prints every summonhelper process: the installer's own test (`running_pids`).
- `name_owner` is the installer's name check: `(false,)` when `com.tesla.Webrtc` is free, `(true,)` when it has an
  owner.
- `dv` reads DataValues as the camera runbook does, for example `VAPI_shiftState (true, 'P')`.
- `show_report <file>` prints a probe file's size, its last 8 KB, which hold the report and the `probe exit` line,
  and how many `carlink: dropped …` lines it has. Every car call the bus drops logs one (`internal/carlink/dbus.go`),
  so expect `0`. A count above 0 means car calls did not have the expected shape (a 38.6 change, for example):
  write it down, and `grep -m 3 'carlink: dropped' <file>` shows the first reasons.

### Step 1: summonhelper down, stock webrtc-comms stopped

This is the installer's stop sequence (S0b, steps 1-4), by hand. It settles nothing itself: it is step 2's
precondition.

```sh
sv -w 15 down /var/lib/airgapp-gw/sv/summonhelper
sv status /var/lib/airgapp-gw/sv/summonhelper
helper_pids ''
[ -e /run/airgapp-summon/session ] && echo 'marker: PRESENT' || echo 'marker: none'
sv down /etc/sv/webrtc-comms || echo 'note: webrtc-comms is not supervised now'
p=$(tr -dc '0-9' 2>/dev/null </run/service/webrtc-comms/webrtc-comms.pid); if [ -n "$p" ] && [ "$p" -gt 1 ] && [ "$(cat "/proc/$p/comm" 2>/dev/null)" = minijail-init ]; then kill -KILL "$p"; echo "killed the orphaned minijail-init $p"; fi; rm -f /run/service/webrtc-comms/webrtc-comms.pid
name_owner; date
```

Expect:
- `sv status`: a line starting with `down: /var/lib/airgapp-gw/sv/summonhelper:` (its log service may still read
  `run: log: …`);
- `helper_pids ''` prints nothing;
- `marker: none`. `marker: PRESENT` means a session may be open. Once `name_owner` prints `(false,)` (finish needs
  the name), run `chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper finish`. Expect
  `summonhelper: finish: sent cmd_abort and the disconnect (confirmed) and removed the session marker`, then check
  again;
- `name_owner` prints `(false,)`. If it prints `(true,)`, try again every second for about 10 s, as the installer
  does. If it stays taken, find the owner and stop there:
  `timeout -k 1 5 gdbus call --system --dest org.freedesktop.DBus --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.GetConnectionUnixProcessID com.tesla.Webrtc`
  prints `(uint32 <pid>,)`, and `cat /proc/<pid>/comm` names it.

Note the time: step 2 starts at least 60 s later. Do step 4's config read meanwhile.

### Step 2: the probe

It settles:
- whether QtCarServer accepts the helper: car frames arrive after H1 (design §8 check 4's first item; risk 1);
- the caller of the car frames (`callers`; design §5.5);
- the stream freeze (expect about 1200 echoes; Appendix A §9 row 4; risk 2);
- KeyInfo as uid 1337 (`roles`; design §6.1), on 38.6 only with step 2b's pin (precondition 5);
- KeysUpdated delivery to uid 1337 (`keys updated`; design §6.1, plan call 7).

**Preconditions.** The first four are required. The fifth decides whether the roles can settle KeyInfo.
1. The car is in P.
2. Nobody is using Summon (the Tesla app, research tools or anything else).
3. At least 60 s have passed since step 1 stopped stock `webrtc-comms` (past H6's ~45 s, by which a run left open has
   ended).
4. `/run/airgapp-summon` exists: `ls -lnd /run/airgapp-summon` shows `drwxr-s--- … 1337 4950`. The helper's run
   script makes it at every start. After a reboot with the helper down, make it as root:
   `install -d -o 1337 -g 4950 -m 2750 /run/airgapp-summon`.
5. On 38.6, the helper's run script carries step 2b's pin ("2026.38.6: the authd pin"):
   `grep -c -e '-authd-sha256 [0-9a-f]\{64\}' /var/lib/airgapp-gw/sv/summonhelper/run` prints `1`, and its first 8 hex
   digits are step 2b's. Without it the probe still settles everything else, but not KeyInfo.

**What the probe does** (`internal/summon/probe`; `summonhelper probe [-duration 120s] [-keyinfo -pub hex[,hex...]
[-authd-sha256 hex]]`, and no `-marker` flag: it always uses the helper's marker, which `finish` and the supervisor
check):
1. It refuses, sending nothing and exiting 1, while the marker exists, while `com.tesla.Webrtc` has an owner, or
   without `/run/airgapp-summon`. Each refusal says which.
2. It sets the marker and sends H1.
3. **The style gate.** It sends no heartbeat or keepalive until the car's connect-edge `autopark:style` arrives,
   within 2 s of H1. Without one, it sends only H1 and the disconnect, says so, and exits 1. That means the car made
   no fresh session: either one was already open, or the car did not accept the helper.
4. With the style: a keepalive and a heartbeat at once, then Tesla's `heartbeat_app_webrtc` every 100 ms (with the
   last echo's `car_timestamp`) and `control:keepalive` every 15 s, until 120 s after H1.
5. The disconnect, confirmed. The marker is removed only after the confirmation. Then it releases the name and
   prints its report, once, at the end.

SIGINT, SIGTERM, SIGHUP and SIGQUIT end it early, with the disconnect (exit 0 once confirmed). It ignores SIGPIPE.
Under `setsid` a dropped SSH session sends it no SIGHUP, so it runs its full duration. It runs detached, so `Ctrl-C`
and `Ctrl-\` at the terminal do not reach it: send the same signals to the pid recorded at launch, only while that pid
is still the probe (after a pin route in this shell, set `P=$(helper_pids probe)` again first). The `Ctrl-C` key's
signal (SIGINT) is the checked `kill -INT` of the rules above:

```sh
tr '\0' ' ' 2>/dev/null </proc/"$P"/cmdline | grep -q '^/var/lib/airgapp-gw/summonhelper probe' && kill -INT "$P" || echo "nothing sent: P=$P is not the probe"
```

For the `Ctrl-\` key's signal (SIGQUIT), the same line with `kill -QUIT "$P"`. Either sends nothing, and says so,
when `$P` is empty or no longer the probe.
SIGKILL, or exit 3 (an emit overran 100 ms), leaves the marker: run
`chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper finish` before anything else, once `helper_pids probe` prints
nothing.

**Run it.** The root shell opens the report file before chpst drops to uid 1337. The report goes to a file, never a
pipe, so it survives a dropped session. `$A` passes the helper's own `-authd-sha256` pin, from its run script, so the
probe's roles use the helper's pin: on 38.6 step 2b's (precondition 5); with none, the compiled-in pin applies. The
last line records the probe's pid in `$P`, and looks once more a second later if the first look found none (a slow
start): expect `probe=` and one pid. A `probe=` that is still empty means the probe ended or never started (a
refusal, for example): read its report.

```sh
grep -c 'handleCarserverToPeerCommsDataReply error' /var/log/qtcar-carserver/current
dv GUI_selfParkStandbyMode GUI_selfParkState GUI_selfParkActivation GUI_remoteAccPowerRequest
A=$(grep -o -e '-authd-sha256 [0-9a-f]*' /var/lib/airgapp-gw/sv/summonhelper/run)
K=<the phone key hex, as in the camera runbook's step 0.4>
A="$A -pub $K" setsid sh -c 'chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper probe -duration 120s -keyinfo $A >/var/lib/airgapp-summon/probe.out 2>&1; echo "probe exit $?" >>/var/lib/airgapp-summon/probe.out' </dev/null &
sleep 1; P=$(helper_pids probe); [ -n "$P" ] || { sleep 1; P=$(helper_pids probe); }; echo "probe=$P"
```

Then, counting from the launch:

| When | Do |
|---|---|
| about 10 s | `dv GUI_selfParkState GUI_selfParkActivation` (step 3) |
| about 20 s | `dv GUI_remoteAccPowerRequest` (step 4) |
| about 30 s | make authd emit KeysUpdated (below). The roles are in by then: they are asked at H1, within 10 s |
| about 45 s and 100 s | `dv GUI_selfParkState GUI_selfParkActivation` (step 3) |
| after 125 s | `show_report /var/lib/airgapp-summon/probe.out`, then the marker test from step 1, then the `grep -c` line above again |
| about 155 s | `dv GUI_remoteAccPowerRequest` (step 4) |

**KeysUpdated.** authd emits it only when it starts or a key changes, so do one of two things at about 30 s:
- change a key in the car's key list; or
- restart authd, then read its status twice, about 5 s apart:

  ```sh
  sv restart /etc/sv/authd; sv status /etc/sv/authd; sleep 5; sv status /etc/sv/authd
  ```

  This has not been run on this car yet. authd is the car's phone-key authority (the Virtual VCSEC), and a restart
  re-runs its start script as a boot does: it deletes its `keys.sqlite3`, may regenerate its keys, stops itself
  (`sv once`) if it cannot load the VIN, and exits (`die`) on other failures, which runsv restarts every second (the
  38.6 dump's `etc/sv/authd/run`). Both reads must say `run:` with the same pid, the second uptime about 5 s higher:
  a fresh start. An uptime that stays at 0-1 s (a new pid each time) is a restart loop, and `down:` means it stopped
  itself: either way, stop the visit and reboot the car.

**Expect** (values are examples):

```
probe: H1 sent; waiting up to 2s for the car's autopark:style
probe: the car's autopark:style arrived: a fresh session; sending heartbeats and keepalives
summonhelper probe: held the session 2m0s after H1; ended: the duration ran out
car frames after H1, by msg_type (the largest gap includes the tail to the end):
  msg_type                         count  first  largest gap
  "autopark:style"                 1      12ms   1m59.988s
  "autopark:status"                1      13ms   1m59.987s
  "vehicle_data:location"          3      15ms   1m40s
  "autopark:heartbeat_car_webrtc"  1199   40ms   140ms
heartbeats: 1199 sent, 1199 echoed
callers:
  uid=1111 pid=2345 sender=:1.42
keys updated: 1
roles:
  entry 1: role 2
disconnect: confirmed; the session marker is cleared
probe: ended (the duration ran out) 2m0s after H1; disconnect confirmed; the session marker is cleared
probe exit 0
```

The log lines (`probe: …`) and the report share the file, so their order can vary.

| Line | Healthy | Otherwise |
|---|---|---|
| `no autopark:style within 2s of H1: …` | absent | The car made no fresh session (one was open, or the car did not accept the helper). Only H1 and the disconnect went out, and the exit is 1. Run the probe once more after 60 s; the same again means the car does not accept the helper: stop there |
| `<n> car frame(s) before H1: a session may have been open` | absent | A session was open when the probe took the name |
| `car frames after H1` | `autopark:style`, `autopark:status` and `vehicle_data:location` (Appendix A §1.2, H1), then the echoes | `none`: QtCarServer did not accept the helper (risk 1). Nothing else matters until that is understood |
| `heartbeats: N sent, M echoed` | M close to N, both about 1200, and `heartbeat_car_webrtc`'s largest gap well under 1 s | M stops short, and its largest gap runs to the end: the stream froze (risk 2). But if the car dozed or SSH dropped during the probe, the counts settle nothing: rerun step 2 on the next wake |
| `callers:` | one line, `uid=1111` (user `tesla`), and `cat /proc/<pid>/comm` prints `QtCarServer` | More lines: other connections called too. Write them all down for the pinning decision |
| `keys updated:` | 1 or more after the authd restart or key change | `0`: none reached uid 1337 in this run (the other of the two ways, in another run, is the second way to ask). `not watched: …`: the subscription was refused |
| `roles:` | `entry <i>: role 2` (owner) or `role 3` (driver) for each `-pub` key, in `-pub` order | `entry <i>: role error: …` names the cause. `authd changed, re-verify KeyInfo: /usr/bin/authd has sha256 …` means the pin did not match, so KeyInfo was never asked (no bus call): KeyInfo as uid 1337 is NOT settled. Never copy the hash that line prints into `AUTHD_SHA256`; only step 2b's pin counts. The other causes did reach the bus: `authd changed …: KeyInfo SecondsLeft …` or `… role … unknown` (KeyInfo answered in an unverified shape), `key not in authd's KeyInfo list`, or a bus refusal of uid 1337 |
| `disconnect:` | `confirmed; the session marker is cleared` | `not confirmed (…)`: run `finish` before anything else |
| `probe exit` | `0` | `1`: an error, named on the `summonhelper probe:` line. `2`: a typo in the command. `3`: an emit overran: run `finish`. `137`: SIGKILLed: run `finish` |

The `grep -c` count must not grow over the probe: no `webrtc handleCarserverToPeerCommsDataReply error on dbus` in
QtCarServer's log (Appendix A §9 row 4, which also notes that the car's 10 s stop timer then writes CAPI=3 in Idle,
which is inert). The log path follows the firmware's `runit-setup-log qtcar-carserver` and Phase 0's runbook; it has
not been read on this car yet, so a missing file means only that this line cannot be checked.

### Step 3: the connect pre-prime (Appendix A §9 row 5)

Read over SSH during step 2's probe, a few values only (the times are in step 2's table): `GUI_selfParkState` and
`GUI_selfParkActivation`. Step 2's first `dv` line read them before H1, with `GUI_selfParkStandbyMode`.

Appendix A §1.2 predicts this. When its conditions hold (among them `GUI_selfParkStandbyMode` set,
`GTW_chargeState` 0 and `GUI_selfParkInhibited` 0), `GUI_selfParkActivation` becomes 4 (StandbyMode) after H1.
`GUI_selfParkState` then runs the prep states and returns to Idle once the prep timeouts run out (35 s pre-primed,
20 s primed, then NotStartedTimeout, then Idle 1 s later). The activation values are 0 None, 1 Keyfob,
2 RemoteAutopark, 3 AutoSummon, 4 StandbyMode and 5 Factory. Write each value down with its time: they are the D1
normalizer's inputs.

### Step 4: the runtime config (row 15), R79 (row 14) and the keepalive (row 16)

The config and R79, read-only, at any time (Appendix A §8.8 has the defaults):

```sh
/usr/bin/is-fused; echo "is-fused rc=$? (0 = fused)"; ls /var/run/is-fused
F=/home/tesla/.Tesla/car/settings.conf; if [ -e "$F" ]; then grep -n -E 'smart_summon_worst_accuracy|remote_request_radius_m|user_paused_timeout_ms|default_stop_timeout|default_pause_timeout|force_defer_summon_checks_to_ap|stub_ready_state|ignore_server_config' "$F" || echo 'no Summon keys'; else echo 'settings.conf: absent'; fi
dv GUI_applyEceR79 VAPI_R79BehaviorOverride FEATURE_disableR79Restrictions
```

The `dv` line settles row 14's R79 DataValues (design §9 gives them to check 4's DataValue reads): write the three
values down. An error for one of them means that DataValue is not on CarServer.

Expect `is-fused rc=0` and `/var/run/is-fused`, then `settings.conf: absent` or `no Summon keys`. The compiled-in
defaults then hold:
- `capi/smart_summon_worst_accuracy` 200;
- `carapi/remote_request_radius_m` 300;
- `selfpark/user_paused_timeout_ms` 35000;
- `autopark/default_stop_timeout` 10000 and `autopark/default_pause_timeout` 0;
- `vapi/force_defer_summon_checks_to_ap` false;
- `autopark/stub_ready_state` false.

If a key is listed, write the line down for the design (it is a value, not personal data). Phase 0's reading of
ConfigValue on 8.6 is that QtCarServer ignores settings.conf's `autopark/*` keys while `/var/run/is-fused` exists.
Never write to settings.conf.

The keepalive (Appendix A §8.7: `control:keepalive` sets `GUI_remoteAccPowerRequest` true). Step 2's table reads it
before the probe (its first `dv` line), at about 20 s into it, and about 30 s after it ends. Expect `true` during the
probe. The probe sends its first keepalive right after the style, then one every 15 s.

Whether the keepalive also holds off the ~5 min doze shows only in a longer session. That run is optional, on
another wake, with nobody in the car. Do step 1, meet step 2's preconditions (60 s included), then:

```sh
setsid sh -c 'chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper probe -duration 420s >/var/lib/airgapp-summon/probe-doze.out 2>&1; echo "probe exit $?" >>/var/lib/airgapp-summon/probe-doze.out' </dev/null &
sleep 1; P=$(helper_pids probe); [ -n "$P" ] || { sleep 1; P=$(helper_pids probe); }; echo "probe=$P"
```

About 6 minutes after the wake, see whether SSH still answers. If the car dozed during the run, then at the next wake,
before anything else, define the functions of "Once per SSH login" and run `P=$(helper_pids probe); echo "probe=$P"`.
If it prints a pid, the probe still runs (a doze
can pause it with the name and the marker held): wait for its `probe exit` line, or end it with the checked
`kill -INT` ("Rules for the whole visit"). Only then
`show_report /var/lib/airgapp-summon/probe-doze.out` and do the marker test; run `finish` if the marker is there and
no probe runs.

### Step 5: the gear probe (Appendix A §9 row 8)

P to D to P, with an occupant in the driver's seat. No verb is sent. The occupant only holds the brake and shifts,
keeping the brake held from leaving P until P is back. **The person in the driver's seat never types while the car is
in D:** an owner alone at the car confirms D on the touchscreen instead of the SSH read, and a second person, if there
is one, may run the SSH read while in D. Start when
step 2's probe has ended with `disconnect: confirmed`, with the car in P and nobody using Summon.

```sh
setsid sh -c 'chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper probe -duration 60s >/var/lib/airgapp-summon/probe-gear.out 2>&1; echo "probe exit $?" >>/var/lib/airgapp-summon/probe-gear.out' </dev/null &
sleep 1; P=$(helper_pids probe); [ -n "$P" ] || { sleep 1; P=$(helper_pids probe); }; echo "probe=$P"
```

1. At about 10 s, in P: `dv VAPI_shiftState` reads P. Then hands off the keyboard.
2. The occupant presses and holds the brake, shifts to D, sees D on the touchscreen, waits about 5 s, and shifts back
   to P. Only a second person may type now: `dv VAPI_shiftState` reads D.
3. Back in P, the brake may be released. `dv VAPI_shiftState` reads P.
4. After 65 s: `show_report /var/lib/airgapp-summon/probe-gear.out`, then the marker test.

Expect:
- the car does not move;
- `vehicle_data:location` has at least 3 frames: the connect frame, then one for each shift, since the car resends
  its location on a gear change;
- the echoes keep up with the heartbeats across the shifts;
- `disconnect: confirmed; the session marker is cleared` and `probe exit 0`.

This settles the no-motion half of row 8: a gear change reaches the session as a location frame, and the session
survives it. The probe prints no field values, so the `shift_state` string on the wire and the speed units are left
to the phone link and the first supervised run.

### Step 6: the VIN the run script read (design §8, check 4) and the dump settings

Bring the helper back first:

```sh
sv up /var/lib/airgapp-gw/sv/summonhelper; sleep 5; sv status /var/lib/airgapp-gw/sv/summonhelper
chpst -u :4950:4950 /var/lib/airgapp-gw/agw probe-summon
```

Expect `run: /var/lib/airgapp-gw/sv/summonhelper: (pid …)` and `summon relay: ok`. Retry `probe-summon` every second
for up to 15 s, as the installer does: a stale socket and the helper's first D2 stamp both need the retry. Each probe
leaves a `relay: closed a connection: …` line in the helper's log, which is its close after OWNED. A refusal that
stays, `summon relay: refused: closed before OWNED: the helper refused this uid, does not own the bus name, or is
unhealthy`, has its cause in that same line, read with
`grep 'relay: closed a connection' /var/lib/airgapp-gw/svlog/summonhelper/current | tail -n 3`: `peer uid <n> refused`
(SO_PEERCRED did not see uid 4950), or `not ready (the name is not owned, or the helper is unhealthy)`.

Then, as root:

```sh
s=$(helper_pids serve); echo "serve=$s"
v=$(tr '\0' '\n' <"/proc/$s/environ" | sed -n 's/^SUMMONHELPER_VIN=//p'); p=$(chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper pin -vin-file /var/etc/vin); if [ -z "$v" ]; then echo 'unreadable: serve environ'; elif [ -z "$p" ]; then echo 'unreadable: pin (see the error above)'; elif [ "$v" = "${p##*:}" ]; then echo match; else echo mismatch; fi; unset v p
ls -ln "/proc/$s/environ"; grep 'Max core file size' "/proc/$s/limits"
```

Expect:
- `serve=` and exactly one pid;
- `match`. This compares the VIN the root run script passed to the running `serve` (its `SUMMONHELPER_VIN`) with
  the VIN in the helper's pin, which is what the phone gets from the owner's pin command (the installer prints no
  pin: "Plan amendments"). One of four words is printed, never the VIN:
  - `match`: the phone's pin and the serving helper agree on the VIN;
  - `mismatch`: they differ, so the phone would refuse the helper's challenge, which binds the VIN: stop, and check
    `/var/etc/vin`;
  - `unreadable: serve environ`: root could not read `/proc/<serve pid>/environ` (the shell's error above says why,
    for example a wrong pid or the car's security module): the VIN is not checked;
  - `unreadable: pin (see the error above)`: the pin command failed as uid 1337 (its own error line names the key or
    the VIN file): the VIN is not checked;
- `ls -ln` shows owner `0`: a process that is not dumpable has root-owned `/proc` files (serve's prctl
  `PR_SET_DUMPABLE` 0);
- `Max core file size` reads `0` and `0` (`RLIMIT_CORE` 0).

### Step 7: the finish hook covers a killed supervisor (Linux, no session open)

A phone session can be open now: close the Summon screen on the iPhone first. The marker test confirms that none is.
The kills run only when the marker is absent, `run` is our supervisor (`supervise/pid` is the one `summonhelper run`)
and it is `serve`'s parent.
Both kills go on one line: the supervisor kills a silent child itself after 100 ms, and this step needs the
supervisor dead before that.

```sh
[ -e /run/airgapp-summon/session ] && echo 'marker: PRESENT: stop' || echo 'marker: none'
L=/var/lib/airgapp-gw/svlog/summonhelper/current; r=$(cat /var/lib/airgapp-gw/sv/summonhelper/supervise/pid); s=$(helper_pids serve); t=$(grep -c 'summonhelper: takeover:' "$L"); echo "run=$r serve=$s takeover lines=$t"
if [ -n "$r" ] && [ -n "$s" ] && [ ! -e /run/airgapp-summon/session ] && [ "$(helper_pids run)" = "$r" ] && [ "$(cut -d' ' -f4 "/proc/$s/stat" 2>/dev/null)" = "$r" ]; then kill -STOP "$s"; kill -KILL "$r"; sleep 1; if tr '\0' ' ' 2>/dev/null <"/proc/$s/cmdline" | grep -q summonhelper; then echo "serve $s: still there"; else echo "serve $s: gone"; fi; else echo 'stop: a marker is present, or run and serve are not our supervisor and its child'; fi
timeout -k 1 5 gdbus call --system --dest org.freedesktop.DBus --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.GetConnectionUnixProcessID com.tesla.Webrtc; echo "serve now: $(helper_pids serve)"
echo "liveness-missed lines for $s: $(grep -c "liveness missed .*child pid $s\$" "$L")"; echo "takeover lines: before $t, now $(grep -c 'summonhelper: takeover:' "$L")"; grep 'summonhelper finish: no session marker: nothing to send' "$L" | tail -n 1
```

Expect:
- `marker: none`, then `run=`, `serve=` (one pid each) and the takeover line count. If the guard fails, the third line
  kills nothing and prints `stop: a marker is present, or run and serve are not our supervisor and its child`: go
  back to the marker test and step 6's `sv status`;
- `serve <pid>: gone` within the 1 s. The kernel SIGKILLed the stopped child when its parent died (Pdeathsig);
- `com.tesla.Webrtc` has no owner (the gdbus call fails with `NameHasNoOwner`), or it belongs to the restarted
  helper: `(uint32 <pid>,)` with `<pid>` the new `serve now:` pid, never the old one;
- `liveness-missed lines for <pid>: 0`, and the takeover line count unchanged. A `supervise: liveness missed (…):
  SIGKILLed child pid <pid>` line, or a new `summonhelper: takeover: …` line, means the supervisor, not Pdeathsig,
  killed the child: the step has not proved Pdeathsig;
- the svlog's newest `summonhelper finish: no session marker: nothing to send` line is from just now: runit's finish
  ran and found no marker.

Within about 15 s, `sv status /var/lib/airgapp-gw/sv/summonhelper` reads `run:` again, and `probe-summon` (step 6)
prints `summon relay: ok`.

`serve <pid>: still there` means Pdeathsig did not fire. The stopped child still holds `com.tesla.Webrtc`, so the
restarted run script refuses to start (`summonhelper run: com.tesla.Webrtc is not free …`). Run `kill -KILL <pid>`,
and record the failure: the supervisor's death must not leave its child.

### After the visit

The helper runs and owns `com.tesla.Webrtc`. Stock `webrtc-comms` stays down until the next boot or run-mode switch,
and `--after-boot` stops it again. Once the reports are read and noted, remove them:
`rm -f /var/lib/airgapp-summon/probe*.out`.

## What check 4 cannot settle yet

- **The round trip with the camera running** (design §8, check 4; Appendix A §9 row 6; plan call 10). It needs a
  signed-in phone on the real path, so it runs once the phone link exists (the phone plan). Until then nothing
  measures the phone-to-helper round trip against F = 150 ms.
- **The swallow assumption.** After D2's trip the helper swallows one car `cmd_result` per injected abort. That
  assumes QtCarServer answers every `cmd_abort` with exactly one `cmd_result` (Appendix A §8.2), which has not been
  seen on this car; check 4 sends no abort. If it does not, the phone's next abort result could be swallowed: a UX
  hang (Tesla's app waits for the command response), not a safety problem, since the car got the abort.
- **Caller pinning.** The helper accepts car frames from any caller until pinning is built. Step 2 identifies
  QtCarServer's connection for it.
- **KeysUpdated.** Step 2 measures delivery. Wiring the watch is a later decision; until then the helper's own
  revocation is the 10 s KeyInfo re-check (agw's outer window is re-checked at once on camhelper's keys_changed).
- **The rest of row 8:** the `shift_state` string on the wire and the speed units (the phone link, then the first
  supervised run).
- **The relay under a real session:** the Linux unix-socket buffers, the 1 s write deadline and agw's 64-body queue
  (the phone link).
- **`requireEuBLECheck`.** Design §9 gives it, with R79, to check 4's DataValue reads, but it is a cloud setting, not a
  CarServer DataValue (Appendix A, "EU BLE check"), so nothing at the car settles it. It decides only the Bluetooth
  gate's texts (Appendix A §9 row 14); step 4 reads only R79's three DataValues. The phone plan must not assume it was
  checked here.
- **KeyInfo as uid 1337 on 38.6 without a verified pin.** With no pin from step 2b in the helper's run script, every
  role line reads `authd changed, re-verify KeyInfo: /usr/bin/authd has sha256 …`: KeyInfo was never asked, and the
  item stays open until a run with step 2b's pin ("2026.38.6: the authd pin").
- **Motion:** release-to-standstill (R23), pause and resume, Come to Me, Go to Target and the arrows are the owner's
  supervised tests (design §8, step 5).

## Plan amendments

Each line is a ruling that changed the approved design's or the plan's text, with its reason.

- **D3's carry rule** (plan call 2; Task 7). Call 2's per-hold windows became the carry rule. Quiet hold time sums
  across holds until a car frame resets it (for the motion rule, a location frame). Gear D or R at speed 0 keeps the
  per-hold window, "speed above 0" reads as any speed other than 0, and staleness is checked at the phone's release
  too. Why: per-hold windows let short holds hide a frozen car stream forever (fail-open).
- **R1's strict reading** (design §6.2; Task 12). The design says Tailscale must not advertise 192.168.90.0/24.
  `guards.RouteOff` refuses any advertised route except the exit-node pair. Why: it was built so before this plan,
  it fails closed, and design §6.2's intent is that agw never fronts the vehicle LAN. Narrowing it to the vehicle
  LAN is a small later change, and `probe-route` shows a refusal at install.
- **The relay's Frame after a failed write** (plan Task 12; R13). After a failed write to the helper socket, 0x10
  bodies are discarded (Frame returns nil) until the window closes "drop", within 250 ms. The plan's Frame error
  would close the window "error" first. Why: the helper's last records, the inner BYE, then reach the phone first;
  there is no safety effect.
- **The probe's style gate, preconditions and flags** (plan Task 14, rules 5-6; its review). The probe sends no
  heartbeat or keepalive until the car's connect-edge `autopark:style` arrives within 2 s of H1, and without it sends
  only H1 and the disconnect (exit 1). Step 2 requires the car in P, no Summon in use, 60 s after stopping
  `webrtc-comms`, and `/run/airgapp-summon`. The `-marker` flag is gone, and the probe also maps SIGQUIT and ignores
  SIGPIPE. Why: heartbeats fed into a session another client left open re-arm the car's 10 s stop timer and could
  keep a run going. A typed marker path would hide the probe's marker from `finish` (plan call 12). Without the
  SIGQUIT mapping, Go's default handler dumps the goroutines and exits before the teardown. A dead output pipe must
  not kill the probe before its disconnect.
- **The SUMMON=0 removal** (design §6.6, plan call 11; Task 15). It decides on our helper's processes, not on the
  name. With the name free it restarts stock `webrtc-comms` (down, its orphaned jail killed, up) and checks that
  stock registers the name, where design §6.6 said `sv up webrtc-comms`. Why: after any reboot stock owns the name
  legitimately, which made SUMMON=0 impossible. Stock registers the name only at its start, so `sv up` on a running,
  nameless stock changes nothing.
- **SUMMON unset keeps an installed helper on a full install** (plan Task 15). The plan inferred SUMMON only on
  `--after-boot`; a full install now infers it from the installed run script too, and only an explicit `SUMMON=0`
  removes. Why: a camera-only re-install must never threaten Summon, and removal needs explicit intent. It keeps the
  installed binary too, so since the 2026-11-26 review fixes it no longer suffices on its own: camhelper's flock
  check and summonhelper's lock ship in one install, which passes `SUMMON=1` and the new summonhelper. With a
  mismatched pair, agw logs `camera: SUMMON mode refused (summon_unverified)` ("Installing and removing"), as it
  does for any session that did not opt into the presence lease.
- **The helper's pin comes from the owner's own command** (plan Task 15). The installer no longer runs
  `summonhelper pin -vin "$VIN"` or prints its output; its DONE text gives the owner's non-printing route for
  `chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper pin -vin-file /var/etc/vin`: a fresh `mktemp` file, sent to
  the laptop with `socat`, then removed ("Installing and removing", "The pins"). Why: the pin carries the VIN, so the
  VIN left the installer's output and the SSH session (the car records SSH sessions) and the command line (`-vin`
  put it in `/proc/<pid>/cmdline`).
- **`timeout -k 1 5`** (plan Task 15). The name check was `timeout 5`. Why: a gdbus that ignores SIGTERM would hang
  the installer and the run script. Each check can now take about 6 s, so the 10-check waits say "up to about a
  minute".
- **A Summon problem skips Summon, never the camera: after a boot, and on a full install once it has written
  anything** (plan Task 15; the camera session's review, and the installer's follow-up rounds). The plan kept the
  camera up only after a boot whose stop sequence failed. Now these skip Summon for the run: after a boot, a missing
  `gdbus` or `timeout`, or `SUMMON=1` with no helper installed; after a boot and from a full install's first write
  (step 1's `mkdir`), a failing step 4b or 4c command (the copy, the state directory, the keygen), anything but `yes`
  at S0b's or the removal's question, a name that stays taken, a removal held up by a lingering `summonhelper`
  process, an unsupervised `summonhelper` found before S3 (the plan's unsupervised-process check stopped the install
  for it, as it still does for agw and camhelper), or a helper not running after S3. The helper is neither linked
  nor started, agw and camhelper finish S2-S4, and the run ends with the Summon report and a non-zero exit. Steps 4b
  and 4c are asked before step 1, with step 0, so a "no" stops the install with nothing changed; their commands still
  run after step 4. A failed `probe-summon` or `probe-route` check after S4 no longer stops the install (the plan's
  "Either failing makes the installer die"): it is reported at the end as `Summon checks failed: …`, non-zero, the
  helper stays linked and running, and the DONE text omits its pin route. S4 never restarts summonhelper, and its
  change detection leaves it out: S0b brought it down and step 5 starts it fresh on every run. What still stops a
  run: `SUMMON must be 0 or 1`, before anything, in both modes, and a full install's checks before its first write,
  with nothing changed. Why: a Summon problem must never keep the camera down, and a full install must never stop
  halfway, with new binaries under old run scripts.
- **The authd pin is kept on a full install too, and checked against the car's authd** (plan Task 15; the camera
  session's request). With `AUTHD_SHA256` unset, a full install also keeps the installed camhelper run script's pin;
  the plan's Task 15 kept it only on `--after-boot`. An explicit value wins, and with none installed the compiled-in
  pin applies. The effective pin is compared with `sha256sum /usr/bin/authd`: on a full install a mismatch, or an
  authd that cannot be hashed, asks before the first write; after a boot it is a note that blocks nothing and makes
  the run exit non-zero. The car's hash is never adopted as the pin. Why: a re-install that forgets `AUTHD_SHA256`
  must not silently revert a verified 38.6 pin to the compiled-in 8.6 one, and a stale pin must show, on either path.
- **A skip after S0b's stop sequence hands the name back to stock** (plan Task 15; the camera session's review). The
  plan left stock `webrtc-comms` stopped for the rest of the boot when Summon was skipped after S0b had stopped it.
  Now the run hands `com.tesla.Webrtc` back after S3 (`stock_hand_back`, "Installing and removing"): our helper
  unlinked and brought down, no `summonhelper` process left, then, only if the name is free, stock restarted and
  checked, as the `SUMMON=0` removal does. Any other exit after the stop sequence, before that hand-back or the
  passing checks (a die, an error, `Ctrl-C` or TERM), does the same on the way out (`exit_hand_back`), unless our
  helper is the installed owner: linked and supervised by a running runsv that has not been told to stop it or keep
  it down, which is never touched. A link with no such runsv (after a boot before S3, or after S0b's `sv down`)
  starts nothing, so stock is restored then too. The run ignores SIGHUP and SIGPIPE, so neither a dropped SSH session
  nor a dead reader of its output can end it without its exit path. Why: no run may leave `com.tesla.Webrtc` without
  an owner for the Tesla app's WebRTC features. The cost: an extra stock restart on a failed install.
- **The hang bound and caller logging** (design §5.2, §5.5; R25). The design gives about 200 ms for a helper crash or
  hang, and logs every car-frame call. A hang is caught in about 0.4 s (above), and the helper logs the first caller
  of each session and every change. Why: the design's figure assumed the 100 ms tick alone, and logging every call
  at about 14 Hz floods the log.
- **The range floor** (D3; R28, Task 7). D3 also refuses with `location_params_invalid` unless −90 ≤ lat ≤ 90 and
  −180 ≤ lon ≤ 180 (a NaN is refused too), for the phone position and every verb's goal. Why: such values could wrap
  onto the car's own position in the car's encoding, and Tesla's app never presses without a fix.
- **The supervisor's at-once name** (plan Task 11; its review). When the queue request gets `com.tesla.Webrtc` at
  once, the child had the name and lost it. The parent keeps the name, SIGKILLs the child, runs the normal takeover
  (if the marker is there: the abort and the confirmed disconnect, then the clear; then the release), and exits
  non-zero. The plan said to release the name first. Why: releasing it left an open session with no parent abort.
- **Data after a JSON object is Fatal in every frame** (plan Task 3; design H2). The plan dropped an HB or MSG whose
  body was not a JSON object; an object followed by data is now a framing error that ends the session (H4), in every
  frame kind. Trailing whitespace is still accepted. Why: design H2 says a framing error ends the session, so a
  corrupted `cmd_abort` aborts at once instead of waiting for D2's trip.
- **A Fatal exits 4** (plan Task 10, `Serve` step 3: "calls `Exit(3)`"; the final review). Fatal (a panic in an
  engine's Stop or Tick) exits with code 4, `helper.ExitFatal`, no longer the emit overrun's 3. Both exits stay
  immediate. Why: the panic's log line is usually lost with the log queue, and the supervisor's exit-status line must
  tell a code bug from an emit overrun.
- **`probe-summon` waits 500 ms for OWNED** (plan Task 12: "reads OWNED within 1 s"; the final review). It waits
  `summonrelay.OwnedWait`, Open's own bound. Why: an OWNED that comes after 500 ms fails every real Open, so the
  installer's cross-uid check must fail it too; the installer retries the probe 15 times anyway.
- **The live-file lock and the presence lease** (2026-11-26: the design
  `docs/superpowers/specs/2026-11-26-summon-review-fixes-design.md`, approved by the owner, and its plan
  `docs/superpowers/plans/2026-11-26-summon-review-fixes.md`; review findings F5r and L1-L5). The plan's inner Spec
  gained PRESENCE (`0x18: 8`), and a phone that sends `{"presence":1}` in the inner ARM params gets the presence
  lease. The live file is held under an flock for the session, and a stale one is removed wherever a helper's end is
  handled ("The live file"). Why: by holding an idle phone's frames back, agw could keep a session, and so the
  Summon camera's waived gates, up about 20 s per held frame after the phone left; and an unclean end left a live
  file that camhelper's old pid check could take for a live session. The final review then ruled (F1) that only a
  session that opted into the presence lease turns the live file on, where the plan had every session whose Start
  succeeded do so. Why: an opted-out session can still be stretched about 20 s per held frame, so its vouching kept
  F5r open for the camera until every phone opted in; the cost is no Summon camera for a phone without the presence
  build, which fails closed.
