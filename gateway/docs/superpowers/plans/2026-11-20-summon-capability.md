# SUMMON Capability (Bench) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the SUMMON capability on the shared gateway — hold-to-move with shim-issued freshness ticks, a 300 ms deadman, one opening verb, ordered teardown, a crash-safe supervisor — and prove it on the bench against a mock QtCarServer. **No car, no motion.**

**Architecture:** The gateway core gains freshness ticks and a small cross-capability `Host`. A new `carlink` package owns `com.tesla.Webrtc` on the system bus (replacing stock `webrtc-comms` for the SUMMON session), builds every car message itself from typed fields, and parses the car's replies. `capability/summon` is the state machine. `agw supervise` runs the server as a child, SIGKILLs it on a missed 100 ms liveness tick, and runs the finish hook (`cmd_abort` + disconnect). The phone gets a native hold loop that echoes ticks without crossing the JS bridge.

**Tech Stack:** Go (existing `airgapp/gateway`), `github.com/godbus/dbus/v5`; Swift/CryptoKit (existing `modules/expo-gateway-client`); TypeScript.

**Spec:** `~/Work/tesla-dump/2026.8.6/re/bridge/PLAN-B2-summon-productionization-2026-11-19.md` (v1.3.7): §4.3–4.8, §5, §7, §9 R1, R6–R8, R11–R17, R19, R21, R22. Car message formats: `~/Work/tesla-dump/2026.8.6/re/bridge/SUMMON-PROTOCOL.md` §3–4. D-Bus contract (F7): `VERIFY-B2-v1.2-2026-11-19.md` lines 862–1090.

**Repos:** `GW` = `~/Work/airgapp/gateway` (Task 1 on `feat/gateway-core`; then branch `feat/summon` from it). `MOB` = `~/Work/airgapp/mobile` on `feat/dashcam-viewer` (owner's instruction; the camera session's uncommitted work is there — touch only gateway-client files).

## Global Constraints

- **Safety floor (§7):** this plan never runs anything on the car and never commands motion. Every test uses the mock car. The owner runs every motion test at the car later, under §7.
- **Window:** at most one SUMMON window; lifetime **W = 120 s** on BOOTTIME; **one opening verb** (`forward`, `reverse` or `find_me`), bound in the ARM.
- **Freshness:** ticks `{8-byte random id, 8-byte BOOTTIME ms}` at **20 Hz**; a HOLD/LOC is accepted only if its echoed tick was issued in this window and is ≤ **F = 150 ms** old; stale frames are dropped (never queued) and count as absence. Never use `car_timestamp`, a wall clock or `max_heartbeat_pingpong_ms`.
- **Deadman:** no fresh HOLD accepted for **T_shim = 300 ms** → teardown.
- **Link gate:** the opening verb is sent only after **10 consecutive** fresh HOLDs with tick RTT < **F/2 = 75 ms**.
- **Heartbeats:** exactly one `autopark:heartbeat_app_webrtc` per accepted fresh HOLD, after the verb; none from timers; none after teardown. HOLDs are accepted at most once per **80 ms**.
- **Exact car verb allowlist (built by the gateway, never passed through):** `autopark:heartbeat_app_webrtc`, `autopark:cmd_forward`, `autopark:cmd_reverse`, `autopark:cmd_find_me`, `autopark:cmd_abort`, `autopark:device_location`, `autopark:info`. Never emitted: `control:*`, `webrtc:*`, `homelink:*`, `autopark:set_webrtc` (as a datachannel message), `heartbeat_app`, `_request` wrappers.
- **D-Bus (F7):** own `com.tesla.Webrtc` at `/Webrtc` with DO_NOT_QUEUE, never ALLOW_REPLACEMENT; serve only while owning it; re-check at every arm. Methods: `RemoteToLocalSignaling(a{sv}) → u` (always reply 0, act on nothing), `CarserverToPeerCommsData(a{sv}) → a{sv}` (reply `{CarserverToPeerCommsData_Reply: int32 1}` on success, 0 on failure). Signals: `PeerCommsToCarserverData {uid: "test_phone", data: <JSON text>}`; `LocalToRemoteSignaling {data_channels_ready: true}` at session start; disconnect `{ice_state_change: true, msg_type: "autopark:set_webrtc", set_webrtc_value: false}` at teardown. Never export `com.tesla.Message.Process`.
- **Teardown order (idempotent, latched):** stop forwarding → `cmd_abort` → disconnect map → send nothing more → latch the re-arm cool-down.
- **Re-arm:** only after a **> 35 s** cool-down on BOOTTIME (the read-grant proof is a Phase-1 hook, default unavailable).
- **Goal and location:** the goal must be ≥ **3 m** from the car's own last reported GPS; `find_me` is refused until a car location has been received; device location must be within **65 m** of the car. Never log GPS or keys.
- **Arm gates (PreArm):** R1 (Tailscale `AdvertiseRoutes` contains no `192.168.90.0/24`, read from the LocalAPI; fail closed), `com.tesla.Webrtc` owned by this process, cool-down elapsed.
- **Revocation:** a `KeysUpdated` signal from `com.tesla.Auth` re-runs `KeyRole` for the open window's key; not 2/3 → teardown "revoked".
- **Supervisor (R17):** 100 ms liveness tick child → parent; a miss, child death or SIGTERM → SIGKILL child → finish hook acquires `com.tesla.Webrtc` and emits `cmd_abort` + disconnect.
- R22 stands: nothing in this plan may send to `192.168.90.0/24`.

## Deliberately not in this plan (need Phase 1 on the car, or are owner decisions)

- The read grant (`LocalDataValue.DataGetValueRequest`): re-arm proof via `CAPI_remoteAutopilotConnected`/`GUI_selfParkState`, the `VCSEC_keyWithinSummonRange*` proximity bit, the `GUI_selfParkState` teardown trigger, and the gear-based `T_shift` tolerance. Until then: the 35 s cool-down, and proximity is an **operator procedure** (§4.8, R19).
- `KeyInfo` formFactor ∈ {6, 7} (R8): waits for Phase 1b showing `KeyInfo` lists the phone key.
- Watching QtCarServer's own bus name for an owner change: the name isn't confirmed in the dump; Phase 1a.
- DERP refusal (§4.4): the car's link is the Pi's Wi-Fi; DERP can only occur on the phone↔Pi tailnet hop, which the gateway can't see. The link gate's RTT < 75 ms covers the intent; a Pi-side check can follow.
- The phone's Summon UI screen: a separate UI plan using the app's design tokens.

## Wire additions

| Dir | Type | Name | Body |
|---|---|---|---|
| out | 0x82 | TICK (core) | id (8) ‖ issued BOOTTIME ms (u64 BE) |
| in | 0x10 | HOLD | echoed tick id (8) |
| in | 0x11 | RELEASE | empty |
| in | 0x12 | LOC | echoed tick id (8) ‖ JSON `{"lat":…, "lon":…, "acc":…, "age_ms":…}` (≤ 256 bytes total). *Amended (final review I1):* `age_ms` = the fix's age at send time; the gateway refuses a fix with `age_ms` missing or > 10 000, or `acc` > 30 m (dropped before the verb, teardown `location` after); the 65 m check is distance + `acc` |
| out | 0x90 | STATUS | JSON (≤ 1024): `autopark_state`, `autopark_state_reason`, `smart_summon_state`, the four `smart_summon_last_*_reason` fields, `leash_travel_m`, `leash_proximity_m`, `phase` (`arming`, `gated`, `moving`, `dry`, `ended`). *Amended (final review):* the final STATUS (`phase: "ended"`) carries `end_reason` from SUMMON's fixed set (`release`, `deadman`, `lifetime`, `no-start`, `car-state`, `car-error`, `refused`, `revoked`, `name-lost`, `location`, `goal`, `overload`, `car-link`, `malformed`, `error`); the phone treats a missing one as unknown. `dry` is the `-summon-dry` phase |
| out | 0x91 | RESULT | JSON (≤ 256): `{"cmd":…, "ok":…, "reason":…}` from `cmd_result` / `error` |
| out | 0x92 | CARLOC | JSON (≤ 256): `{"lat":…, "lon":…, "heading":…}` |

ARM params: `{"verb": "forward"|"reverse"|"find_me", "goal": {"lat":…, "lon":…}}` — `goal` required for `find_me`, forbidden otherwise.

## Review Focus

1. **The phone stalls mid-hold (Wi-Fi blip, app frozen) and then flushes queued HOLDs:** the stale ones are dropped by tick age, the deadman has already fired, and nothing resumes. Test in Task 5.
2. **The car keeps "paused/resuming" after a release:** teardown still sends `cmd_abort` + disconnect, and a re-arm within 35 s is refused. Test in Task 5.
3. **The process is SIGKILLed or SIGSTOPped while moving:** the supervisor's finish hook still emits abort + disconnect within ~200 ms. Test in Task 6.
4. **Goal at (or near) the car's own position, or no car location yet:** `find_me` is refused before any verb is sent. Test in Task 4.
5. **QtCarServer calls `CarserverToPeerCommsData` while no window is open, or with junk:** reply 0, no crash, nothing forwarded. Test in Task 2.

---

### Task 1: Core ticks and the Host interface (on `feat/gateway-core`)

**Files:** Modify `GW/capability/capability.go`, `GW/internal/server/window.go`, `GW/internal/server/server.go`, `GW/capability/testkit/fakewindow.go`, `GW/docs/CAPABILITY-API.md`; Test `GW/internal/server/ticks_test.go`, `GW/capability/testkit/fakewindow_test.go`.

**Interfaces:**
- Produces: `Spec.TickHz int` (0 = none; SUMMON uses 20). Core sends TICK (0x82) on the priority lane at that rate, keeps the last 64 ticks per window.
- Produces: `Window.TickAge(id [8]byte) (age time.Duration, ok bool)` — `ok` only for a tick issued in this window; age from the injected Clock.
- Produces: `capability.Host` interface `{ SummonOpen() bool; SummonLinkDegraded() bool; SetSummonLinkDegraded(bool) }` and `Window.Host() capability.Host`. `SetSummonLinkDegraded` is a no-op unless called from the SUMMON (ID 0x01) window. `SummonOpen` is true while a SUMMON window holds its slot.
- testkit: `FakeWindow.IssueTick() [8]byte`, `SetTickAge(id, age, ok)`, and a settable fake Host.

- [ ] **Step 1: Failing tests:** `TestTicksAtRate` (TickHz 20 → 18–22 TICKs/s on the client, ids distinct, body 16 bytes); `TestNoTicksWhenZero`; `TestTickAge` (fresh tick → ok, age from the Fake clock; foreign id → !ok; a tick from another window → !ok); `TestTicksStopAtTeardown`; `TestHostSummonOpen` (true only while a CAPABILITY ID 0x01 window is open — use a test capability with ID 0x01); `TestSetLinkDegradedOnlySummon`.
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** `go test -race -count=3 ./internal/server ./capability/...` → PASS.
- [ ] **Step 5:** Document ticks and Host in `CAPABILITY-API.md` (camera: drop to keyframe-only while `SummonLinkDegraded()`; no TALK and no selectable camera while `SummonOpen()`). Commit `feat: freshness ticks and cross-capability Host`. Then `git switch -c feat/summon`.

### Task 2: `carlink` — the `com.tesla.Webrtc` service and typed car messages

**Files:** Create `GW/internal/carlink/messages.go`, `bus.go`, `service.go`, `fakebus.go`, `dbus_linux.go`, `carlink_test.go`.

**Interfaces:**
- Produces: message builders returning `Msg` (an opaque type whose only constructors are these): `Heartbeat(bootMS uint64)`, `Forward(lat, lon float64)`, `Reverse(lat, lon float64)`, `FindMe(lat, lon, acc, goalLat, goalLon float64)` (`summon_type: "find_me"`, `client_version: 2`), `Abort()`, `DeviceLocation(lat, lon, acc, goalLat, goalLon float64, summonType string)` (`altitude: -1`, `altitude_accuracy: -1`), `Info(keyID string)` (`txid: "<none>"`, `x_tesla_user_agent`/`tag`: `"airgapp-gw/1"`, `model: "iPhone"`, `name: "airgapp"`, `public_key_hash: keyID`). Every message is a flat JSON object with `msg_type` and `created:timestamp` (BOOTTIME ms; never wall clock). `(Msg).JSON() []byte`.
- Produces: `Bus` interface `{ RequestName(name string) (owned bool, err error); ReleaseName(name string) error; Emit(member string, body map[string]any) error; ServeMethods(h MethodHandler) error; SubscribeKeysUpdated(func()) error; OnNameLost(name string, f func()) error; Close() error }`; `NewDBusBus() (Bus, error)` (linux); `NewFakeBus() *FakeBus` (records emissions in order; `CallCarserverToPeer(map[string]any) map[string]any`; `CallRemoteToLocal(map[string]any) uint32`; `FireKeysUpdated()`; `LoseName(name)`).
- `CarserverToPeerCommsData` replies immediately (R21): the sink is called synchronously but must not block — it may only hand the data to a bounded, non-blocking queue (drop when full; car→phone STATUS/CARLOC use `TrySend`).
- Produces: `Service` with `NewService(b Bus) *Service`; `Acquire() error` (DO_NOT_QUEUE; error if not owned); `Owned() bool`; `Connect() error` (emits `{data_channels_ready: true}`); `Send(m Msg) error` (emits `PeerCommsToCarserverData {uid:"test_phone", data:<json>}`; refuses after `Disconnect`); `Disconnect() error` (emits the exact disconnect map; idempotent; latches); `SetSink(func(json []byte))` — receives `data` from `CarserverToPeerCommsData` while a sink is set (reply 1), otherwise reply 0.

- [ ] **Step 1: Failing tests:** `TestMessageGolden` (each builder → exact JSON key set and values; field order irrelevant; `created:timestamp` present; no `car_timestamp`); `TestNoGenericConstructor` (compile-time: `Msg` has no exported fields; documented in the test comment); `TestRemoteToLocalRejected` (returns 0, nothing emitted); `TestCarserverToPeerNoSink` (reply 0) and `…WithSink` (reply 1, sink got `data`); `TestCarserverJunk` (missing/ non-string `data`, 1 MiB `data` → reply 0, no panic) — Review Focus 5; `TestDisconnectMapExact` and idempotent; `TestSendAfterDisconnectRefused`; `TestAcquireNotOwned` (FakeBus configured as taken → error).
- [ ] **Step 2–4:** RED → implement → `go test -race ./internal/carlink` and `GOOS=linux go vet ./internal/carlink` → PASS.
- [ ] **Step 5:** Commit `feat: carlink Webrtc service and typed car messages`.

### Task 3: Car-reply parser and the mock QtCarServer

**Files:** Create `GW/internal/carlink/parse.go`, `GW/internal/carlink/mockcar.go`, `GW/internal/carlink/parse_test.go`, `GW/internal/carlink/mockcar_test.go`, `GW/internal/carlink/testdata/*.json` (fixtures from SUMMON-PROTOCOL §4).

**Interfaces:**
- Produces: `Parse(data []byte) (Event, error)` with `Event` one of `Status{AutoparkState, AutoparkReason, SmartState, LastUnavailable, LastAbort, LastWait, LastSafety string; LeashTravelM, LeashProximityM float64}`, `CmdResult{Cmd string; OK bool; Reason string}`, `CarError{Type string}`, `CarLocation{Lat, Lon, Heading float64}`, `Style{}`, `Ignored{MsgType string}` (heartbeat_car_webrtc, smart_summon_viz, occupancy grid, unknown). Car→phone JSON is limited to 64 KiB before parsing.
- Produces: `MockCar` driving a `FakeBus` as QtCarServer would: on `data_channels_ready` → sends `status{standby}` + `vehicle_data:location`; on a verb → `cmd_result{ok}` then `status{active_forward|active_reverse}` (find_me: `smart_summon_state` 4); tracks heartbeats (`Heartbeats() int`); on `cmd_abort` → `status{aborting}` then `{standby}` after 200 ms; on disconnect → `Connected() == false`; configurable `PausedAfterAbort bool` (stays `paused` for 35 s) and car GPS. `Log() []string` returns the ordered list of received `msg_type`s and signals.

- [ ] **Step 1: Failing tests:** `TestParseFixtures` (each fixture → expected Event); `TestParseOversize`; `TestParseUnknownIgnored`; `TestMockCarHappyPath` (connect → status standby; forward → active_forward; abort → aborting → standby); `TestMockCarPausedAfterAbort`.
- [ ] **Step 2–4:** RED → implement → `go test -race ./internal/carlink` → PASS. **Step 5:** Commit `feat: car reply parser and mock QtCarServer`.

### Task 4: Arm guards — route gate, geo checks, cool-down

**Files:** Create `GW/internal/guards/route.go`, `geo.go`, `cooldown.go`, `guards_test.go`.

**Interfaces:**
- Produces: `RouteOff(ctx context.Context, sock string) error` — `GET http://local-tailscaled.sock/localapi/v0/prefs` over the unix socket (default `/var/run/tailscale/tailscaled.sock`); error unless `AdvertiseRoutes` contains no prefix other than `0.0.0.0/0` and `::/0`; any I/O, status or JSON error → error (fail closed); 500 ms budget.
- Produces: `DistanceM(lat1, lon1, lat2, lon2 float64) float64` (haversine, R = 6371000); `CheckGoal(goal, car LatLon, carKnown bool) error` (refuse if `!carKnown` or distance < 3 m); `CheckDevice(dev, car LatLon) error` (refuse if > 65 m); `ValidLatLon(lat, lon float64) bool` (finite, |lat| ≤ 90, |lon| ≤ 180, not both 0).
- Produces: `Cooldown` with `NewCooldown(c clock.Clock, ms uint64)`; `Start()`; `Ready() bool` (35000 ms default); `Remaining() uint64`.

- [ ] **Step 1: Failing tests:** `TestRouteOffClean` (prefs with `[]`, and with only the /0 pair → nil); `TestRouteOnRefused` (`192.168.90.0/24` present → error); `TestRouteOtherPrefixRefused` (any other prefix → error); `TestRouteSocketMissing` / `BadJSON` / `HTTP500` / `Slow` (600 ms → error) — all with an `httptest` unix-socket server; `TestGoalTooClose` (2.9 m → error, 3.1 m → nil); `TestGoalNoCarLocation` — Review Focus 4; `TestDeviceTooFar` (65.1 m → error); `TestValidLatLon` (NaN, Inf, 91, 0/0 → false); `TestCooldown` (Fake clock: not ready at 34 999 ms, ready at 35 001 ms).
- [ ] **Step 2–4:** RED → implement → `go test ./internal/guards` → PASS. **Step 5:** Commit `feat: summon arm guards`.

### Task 5: The SUMMON capability state machine

**Files:** Create `GW/capability/summon/summon.go`, `GW/capability/summon/window.go`, `GW/capability/summon/summon_test.go`.

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces: `summon.New(cfg Config) capability.Capability` with `Config{Service *carlink.Service; Bus carlink.Bus; Oracle authority.Oracle; RouteSock string; Clock clock.Clock; Cooldown *guards.Cooldown; SkipRouteGate bool /* bench only */}`. Spec: ID 0x01, `In {0x10: 8, 0x11: 0, 0x12: 256}`, `Out {0x90: 1024, 0x91: 256, 0x92: 256}`, `LeaseMS 0`, `TickHz 20`, `Bulk {}`.
- PreArm: route gate (unless `SkipRouteGate`), `Service.Acquire`/`Owned`, `Cooldown.Ready()`.
- Open: validate params (verb; goal rules; `ValidLatLon`); `Service.SetSink(parser)`; `Send(Info(keyID))` (keyID = first 4 bytes of SHA-1(pkS), hex — the KeyInfo id); `Connect()`; start W = 120 s timer; STATUS `phase: "arming"`.
- Frame HOLD: drop unless `TickAge` ok and ≤ 150 ms; drop if < 80 ms since the last accepted HOLD; accepted → reset the 300 ms deadman. Before the verb: count consecutive accepted HOLDs with tick age < 75 ms (any slower one resets the count); at 10 → geo check (`find_me`: `CheckGoal` against the last `CarLocation`; all verbs: a LOC must have been received and `CheckDevice` pass) → send the one verb (`forward`/`reverse` use the last device location; `find_me` uses device + goal) → STATUS `phase: "moving"`. After the verb: each accepted HOLD → exactly one `Heartbeat`. For the whole window, set `Host.SetSummonLinkDegraded(true)` whenever a HOLD's tick age ≥ 75 ms (F/2), false otherwise; false at teardown.
- Frame LOC: same tick freshness (stale → dropped); `ValidLatLon` + `CheckDevice` (fail → teardown "location"); after the verb send `DeviceLocation` (`summon_type`: `"find_me"` for find_me, else omitted); LOC does not feed the deadman.
- Frame RELEASE → teardown "release".
- Car events (from the sink, parsed): `CmdResult`/`CarError` → RESULT frame; `CarLocation` → remember + CARLOC frame; `Status` → STATUS frame; after an `active_*` state (or `smart_summon_state` 4) has been seen, a status in {paused, resuming, aborting, panic, disabled, opening_gate} → teardown "car-state"; no active state within **T_start = 15 s** of the verb → teardown "no-start"; `Style` → `Send(Info)` again.
- `KeysUpdated` → `Oracle.KeyRole(pkS)`; not 2/3 or error → teardown "revoked".
- `com.tesla.Webrtc` lost (`OnNameLost`) → teardown "name-lost" (abort/disconnect best effort; the name is gone).
- Teardown (once, from any trigger incl. the core's `Teardown`): stop forwarding; `Send(Abort())`; `Disconnect()`; clear the sink; `Cooldown.Start()`; STATUS `phase: "ended"` (best effort); never send anything after.

- [ ] **Step 1: Failing tests** (testkit FakeWindow + FakeBus + MockCar + Fake clock): `TestArmGates` (route on → PreArm error; name not owned → error; cooldown not ready → error); `TestParamsValidation`; `TestHappyForward` (10 fast HOLDs → exactly one `cmd_forward`, then one heartbeat per accepted HOLD; MockCar log order: data_channels_ready, info, cmd_forward, heartbeats… (info after the channel is up)); `TestStaleHoldDropped` (age 151 ms → no heartbeat, deadman not reset); `TestHoldRateLimit` (HOLDs 20 ms apart → one accepted per 80 ms); `TestLinkGateSlowLink` (HOLDs at 80 ms tick age → never a verb; Host degraded true); `TestDeadman` (no HOLD for 300 ms after the verb → MockCar sees `cmd_abort` then disconnect, in that order, within 350 ms); `TestStallThenFlush` — Review Focus 1 (stop HOLDs 400 ms, then deliver 5 HOLDs echoing old ticks → all dropped, teardown already happened, no heartbeat after abort); `TestReleaseTeardownOrder`; `TestPausedAfterReleaseNoRearm` — Review Focus 2 (MockCar PausedAfterAbort; re-arm at +20 s → PreArm error; at +36 s → ok); `TestOneVerbOnly` (continued HOLDs never produce a second verb); `TestFindMeNeedsCarLocation`; `TestGoalAtCar` (goal 1 m from car GPS → teardown before any verb); `TestCarStateTriggers` (after active_forward, `paused` → teardown); `TestNoStart` (no active within 15 s → teardown); `TestWindowLifetime` (120 s → teardown); `TestRevocation` (FireKeysUpdated with Oracle role 8 → teardown "revoked"); `TestNameLost`; `TestLinkDegradedAfterVerb` (slow HOLD after the verb → Host degraded true; teardown → false); `TestSinkNeverBlocks` (a full phone lane + 100 car messages → every `CarserverToPeerCommsData` replies within 10 ms); `TestNoHeartbeatAfterTeardown`; `TestNeverEmitted` (scan every MockCar-received `msg_type` across all tests' logs ⊆ the allowlist).
- [ ] **Step 2–4:** RED → implement → `go test -race -count=3 ./capability/summon` → PASS.
- [ ] **Step 5:** Commit `feat: summon state machine`.

### Task 6: Supervisor, liveness watchdog and finish hook

**Files:** Create `GW/internal/supervise/supervise.go`, `GW/internal/supervise/supervise_test.go`; Modify `GW/cmd/agw/main.go` (subcommands `supervise` and `summon-finish`; `serve` writes a liveness byte every 50 ms to fd 3 when `AGW_LIVENESS_FD=3`).

**Interfaces:**
- Produces: `supervise.Run(ctx, argv []string, finish func() error, opts Options) error` — starts the child with a pipe as fd 3; if no byte arrives for **100 ms**, or the child exits, or the parent gets SIGTERM/SIGINT → SIGKILL the child (if alive), wait, run `finish` once, return. `Options{LivenessTimeout time.Duration /* 100ms */}`.
- Produces: `agw summon-finish` — `carlink.NewDBusBus` → `Acquire` (retry up to 500 ms while the dead child's name is released) → `Send(Abort())` → `Disconnect()` → release; exit 0 even if the bus is unavailable (logged), never blocks > 1 s.
- `agw supervise -- serve …` passes the remaining args to a child `agw serve`.

- [ ] **Step 1: Failing tests** (helper child = the test binary re-exec'd via `os.Args[0]` with a mode env var): `TestFinishOnChildExit`; `TestKillOnMissedLiveness` (child stops writing → SIGKILLed within 100–200 ms, finish ran once); `TestSIGSTOPChild` — Review Focus 3 (child SIGSTOPped → treated as a liveness miss); `TestParentSIGTERM` (finish runs, child gone); `TestFinishOnce`; `TestSummonFinishWithFakeBus` (with an injected FakeBus: Abort then disconnect map, in order).
- [ ] **Step 2–4:** RED → implement → `go test -race ./internal/supervise ./cmd/agw` → PASS. **Step 5:** Commit `feat: supervisor, liveness watchdog, summon finish hook`.

### Task 7: Wire SUMMON into `agw`, bench mode, and the end-to-end bench tests

**Files:** Modify `GW/cmd/agw/main.go`, `GW/client/client.go` (tick support), `GW/docs/CAPABILITY-API.md`, `GW/README.md`; Create `GW/cmd/agw/summon_e2e_test.go`, `GW/docs/SUMMON.md`.

**Interfaces:**
- `capTable`: real SUMMON (real D-Bus bus, `RouteSock`) — never with `-fake-role`; bench SUMMON (`benchOnly: true`) selected only with `-bench -fake-car`, wired to `FakeBus` + `MockCar` and `SkipRouteGate: true`. `-fake-car` without `-bench` → error. The `KeysUpdated` subscription and `Service` are created once per process.
- `client.Session` gains `NewestTick() (id [8]byte, age time.Duration, ok bool)`, `Hold() error` (sends HOLD echoing the newest tick; error if none or ≥ 150 ms old), `Release() error`, `Loc(lat, lon, acc float64) error`.

- [ ] **Step 1: Failing e2e tests** (in-process `run()` with `-bench -fake-role 2 -fake-car`, the Go client): `TestSummonBenchHappyPath` (arm forward → 10 HOLDs at 100 ms → MockCar log `[data_channels_ready, info, cmd_forward, heartbeat…]` → `Release` → `[cmd_abort, disconnect]`); `TestSummonBenchDeadman` (stop HOLDs → BYE within 400 ms; MockCar ends with `[cmd_abort, disconnect]`); `TestSummonRearmCooldown`; `TestRealSummonNeverWithFakeRole` (`selectCaps` and flags); `TestFakeCarRequiresBench`.
- [ ] **Step 2–4:** RED → implement → `go test -race ./...` → PASS; `make car` still static.
- [ ] **Step 5:** `docs/SUMMON.md`: the wire additions, the timing constants, the teardown order, the bench recipe, and the Phase-3 no-motion checklist for the owner (all read-only or no-motion): webrtc-comms stopped for the test window and restored after; uid 4950 can own `com.tesla.Webrtc` and emit both signals; `KeysUpdated` signal name confirmed; `agw summon-finish` emits abort + disconnect with no window open (car stays parked); the supervisor's runit `run` uses `agw supervise -- serve …`. Commit `feat: wire summon into agw with bench mode`.

### Task 8: Phone — native hold loop and SUMMON API (MOB)

**Files:** Modify `MOB/modules/expo-gateway-client/ios/GatewaySession.swift`, `GatewayClientModule.swift`; `MOB/src/services/gateway/client.ts`; Create `MOB/src/services/gateway/summon.ts`, `summon.test.ts`.

**Interfaces:**
- Swift `GatewaySession`: consumes TICK (0x82) natively — keeps the newest `(id, receivedAt)`; TICK frames are never forwarded to JS. `func setHold(_ down: Bool)`: while down, every **100 ms** on the session queue send HOLD echoing the newest tick **only if** it was received < 150 ms ago (else skip that beat); on `false` send RELEASE at once and stop the loop; also stops on close. `func sendLoc(lat: Double, lon: Double, acc: Double)`: LOC echoing the newest tick (skip if none < 150 ms). Never queues more than one unsent HOLD (drop if the connection's send is still pending).
- JS: `summonHold(id: string, down: boolean): boolean`, `summonLoc(id, lat, lon, acc): boolean` (false before READY / after close), `openSummon({host, port, pin, verb, goal?}): Promise<string>` (builds ARM params per the wire section; validates lat/lon like `ValidLatLon`).
- [ ] **Step 1: Failing TS tests** (`node:test`, native mocked): param validation (find_me needs goal; forward forbids it; NaN/0,0 rejected); `summonHold` false before READY; calls pass through.
- [ ] **Step 2–4:** RED → implement → `pnpm test` (gateway subset + full once) and `tsc --noEmit` → PASS. Swift: standalone `swiftc` compile of the module files at iOS 16.4 with strict availability (as before).
- [ ] **Step 5:** Commit only these files on `feat/dashcam-viewer` (stage by name). No pod install, no device deploy.

### Task 9: End-to-end with the real Swift session (controller) and handoff

- [ ] **Step 1:** Extend the macOS harness (real `GatewaySession.swift` + `GatewayCrypto.swift`, bench key double) to arm `forward` against `agw serve -bench -fake-role 2 -fake-car`, hold for 2 s, release; expect the MockCar log `[data_channels_ready, info, cmd_forward, heartbeat×≈11, cmd_abort, disconnect]`. Then a run that stops the hold loop without RELEASE → deadman teardown.
- [ ] **Step 2:** Tell the camera session: Host API (degraded/summon-open rules), `agw supervise -- serve …` for the runit `run` script, the finish hook is now real.
- [ ] **Step 3:** Update `README.md` status.
