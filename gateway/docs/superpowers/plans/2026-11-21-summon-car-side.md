# Summon Car Side Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the car side of airgapp Summon. That is `summonhelper` (the only process the network can reach that can
issue a motion command), agw's SUMMON relay, a bench that proves both against a mock QtCarServer, and the installer's
`SUMMON=1` path. The plan ends with `summonhelper probe` ready for the owner's no-motion car checks.

**Architecture:**
- `summonhelper` runs as user `webrtc` (uid 1337) and owns `com.tesla.Webrtc`. It runs the unchanged gateway server
  core on a unix socket, with one inner capability: the SUMMON engine.
  - The engine checks every phone message against Tesla's exact shapes (H2).
  - It enforces the deadman (D2) and the 3 m floor (D3).
  - It bridges to QtCarServer through a reworked `internal/carlink`.
- agw's capability 0x01 becomes a relay of opaque bytes between the phone's outer session and the helper's socket.
- A supervisor parent queues for the bus name and sends the abort and disconnect if the child dies. The runit finish
  script covers a dead supervisor.
- The phone side (mobile repo) is the next plan. Here, a Go test phone nests the inner session inside the outer one.

**Tech Stack:** Go, module `airgapp/gateway` (go 1.26.2); `github.com/godbus/dbus/v5` v5.1.0 (already in go.mod); no
new dependencies. POSIX `sh` for the installer; runit on the car. A private `dbus-daemon` for bus tests (present on
this Mac at `/opt/homebrew/bin/dbus-daemon`).

**Spec:** `~/Work/airgapp/gateway/docs/superpowers/specs/2026-11-20-summon-design.md` ("the design",
approved 2026-11-21) and its Appendix A, `2026-11-20-summon-tesla-behaviour.md` ("Appendix A"), at feat/summon
fd6996b. The design wins where they differ. Read the design's §5, §6.1, §6.2, §6.6 and §8, and Appendix A §1.2,
§2.1, §2.6, §3.4, §3.5 and §8, before any task.

## Global Constraints

**Where the work lands.** Worktree `~/Work/airgapp/gateway-summon`, new branch `feat/summon-car`, cut from
`feat/summon` at this plan's commit. Nothing is pushed.
- Never touch the other worktrees: `gateway-camera`, `gateway-dialout`, `gateway-dialout-hardening`, `rpi*`.
- Nothing contacts the car or the Pi.
- The camera session reviews the installer change (Task 15) before it reaches `feat/camera-capabilities` (design §6.6,
  §10). The phone, CAMERA's Grid and keyframe exceptions, and camhelper's gates are out of scope here (camera-session
  code, design §6.5).

**Safety (the owner's standing rules):**
- Nothing in this plan commands motion, and no task runs anything on the car. The owner runs every car check.
- The helper originates only:
  - `data_channels_ready` (H1);
  - `cmd_abort` (D2's trip, H4, the supervisor's takeover, the finish hook);
  - the disconnect map;
  - refusal `cmd_result`s.

  It never sends a heartbeat, a motion verb or a `device_location` of its own. It does not forward the `cmd_result` of
  an abort it originated.
- The probe sends only `data_channels_ready`, `heartbeat_app_webrtc`, `control:keepalive` and the disconnect map. Its
  package has no code path for a verb, an abort or a `device_location`.
- Nothing is ever sent to 192.168.90.0/24 (R22).
- `SUMMON=0`, the default, installs nothing for Summon.
- Logs never contain GPS coordinates, keys (fingerprints only) or the VIN. Car and phone messages are logged by
  `msg_type` and size only.
- **Logging never blocks.**
  - `summonhelper`'s only logger is a bounded queue of 1024 lines, drained by one writer goroutine. When the queue is
    full, lines are dropped and counted (`internal/summon/logq`, Task 10).
  - Nothing on these paths writes to stderr directly: the D2 loop, the engine lock, the car-frame forwarder, godbus's
    read goroutine, an emit-overrun exit, the takeover, and the finish hook.
  - The supervisor sends its abort and disconnect before it writes any log line about a child's death.
- **godbus reports success for a failed write.** So "sent" means "emitted, then confirmed by a round trip on the same
  connection" (`Link.Disconnect`, Task 4). The session marker is cleared only after that confirmation.

**D-Bus contract (exact; Appendix A §1.2):**
- The name is `com.tesla.Webrtc`, at path `/Webrtc`, interface `com.tesla.Webrtc`.
- The helper and the probe request it with DO_NOT_QUEUE only, never ALLOW_REPLACEMENT or REPLACE_EXISTING. Only the
  supervisor parent queues for it (flags 0).
- Methods served:
  - `CarserverToPeerCommsData(a{sv}) → a{sv}`, which replies `{"CarserverToPeerCommsData_Reply": int32}`: 1 while a
    session is up, else 0.
  - `RemoteToLocalSignaling(a{sv}) → u`, which replies 0.
- The car's frame map keys are `uid`, `data` and `channel` (`dcTcp` | `dcUdp`), from QtCarServer.c:1155195-1155518.
- Signals emitted:
  - `LocalToRemoteSignaling`. H1 is `{"data_channels_ready": true}`. H4 is
    `{"ice_state_change": true, "msg_type": "autopark:set_webrtc", "set_webrtc_value": false}`.
  - `PeerCommsToCarserverData` `{"uid": "test_phone", "data": <JSON text>}`.
- The abort's data is exactly `{"msg_type":"autopark:cmd_abort"}`.
- A refusal is sent to the phone on channel `dcTcp` with these keys, in Go's sorted map order (QtCarServer builds
  `cmd_result` from a QVariantMap, so its keys arrive sorted too):
  `{"cmd_type":<the request's msg_type>,"msg_type":"autopark:cmd_result","reason":<reason>,"result":false,"timestamp":<ms>}`.
  - `timestamp` = max(the wall clock in Unix ms, the highest `cmd_result` timestamp seen in this session).
  - The reasons are `action_invalid` (a stale verb, or a verb while verbActive), `location_params_invalid` (within
    3 m) and `invalid_vehicle_location` (no car position, or a stale one).

**Inner SUMMON capability (served by the helper's core):**
- `Spec{Name: "SUMMON", ID: 0x01, In: {0x14: 8, 0x15: 1036, 0x16: 524, 0x17: 2048}, Out: {0x91: 16384}, LeaseMS: 20000, TickHz: 20}`.
- Frame bodies:
  - LIVE 0x14 = tick id (8 bytes);
  - VERB 0x15 and HB 0x16 = tick id (8) ‖ press id (u32 BE) ‖ Tesla's JSON;
  - MSG 0x17 = Tesla's JSON;
  - CAR 0x91 = channel (1 byte: 0 `dcTcp`, 1 `dcUdp`) ‖ the car's JSON, verbatim.
- The helper parses press ids and logs them. It applies no rule to them: the design gives none.

*Amended 2026-11-26 (the Summon review fixes, F5r):* the inner Spec's `In` also has `0x18: 8`, PRESENCE (the 8-byte
tick id), for the presence lease a phone opts into with the inner ARM params `{"presence":1}`. Nothing else in the
Spec changed. Design: `docs/superpowers/specs/2026-11-26-summon-review-fixes-design.md`; the wire as built:
`docs/SUMMON.md` and `docs/CAPABILITY-API.md`.

**Outer relay (agw):**
- `Spec{Name: "SUMMON", ID: 0x01, In: {0x10: 16384}, Out: {0x90: 16384}, LeaseMS: 20000, TickHz: 0}`.
- Both types use the priority lane.
- The payloads are chunks of the inner session's byte stream.

**Relay records** (on `relay.sock`, both ways): type (u8) ‖ length (u16 BE) ‖ payload.
- `0x01 INNER`: 1..16384 bytes of the inner stream, either way.
- `0x02 LINK`: helper to agw, 1 byte, 0 or 1.
- `0x03 OWNED`: helper to agw, empty, the first record of a connection, exactly once.
- Anything else closes the connection: an unknown type, a length of 0 on INNER, an oversize record, a LINK payload
  other than 0 or 1, or OWNED anywhere but first.

**Timing values:**

| Value | Meaning |
|---|---|
| F = 150 ms | A tick age of at most 150 ms is fresh (`Window.TickAge`). |
| 300 ms | The trip fires when verbActive and more than 300 ms have passed since the latest liveness instant. |
| 10 ms | Period of the D2 loop, which stamps its progress before it takes any lock. |
| 250 ms | The helper is unhealthy if the D2 loop has not stamped for longer than this. |
| 100 ms | Emit deadline. An overrun latches the bus dead and exits with code 3. |
| 50 ms / 100 ms / 5 s | Liveness: the child writes every 50 ms while healthy, and the parent SIGKILLs it after 100 ms of silence (`internal/supervise` defaults). The child gets a startup grace of 5 s, which covers the oracle warm-up. |
| 1 s | After the child dies, the parent waits this long for the name. The finish hook retries the name every 20 ms for this long. |
| 100 ms ×3 / 75 ms / 2 s | The link flag is set when 3 LIVE tick ages in a row exceed 100 ms. It is cleared when a hold ends, or after 2 s of ages at or under 75 ms. |
| 3.0 m | D3 floor: haversine distance, R = 6371 km. |
| 1 s | D3 staleness (§5.3, and call 2 below). |
| 10 s | Sign-in re-check period. watchUp stays false: the KeysUpdated watch is not wired (call 7). |
| 500 ms | How long agw's Open waits for OWNED. |
| 10 s | How long the helper waits for the first INNER record before it closes a relay connection. |
| 64 records / 1 s | agw's write queue to the socket, and its write deadline. |
| 1024 frames | The helper's ordered car-frame queue. |
| 16383 bytes | Largest car JSON forwarded. Larger frames are dropped and logged by type and size. |

**Paths and uids:**
- `/run/airgapp-summon`: the root run script creates it at every start, owner 1337, group 4950, mode 2750.
- `/run/airgapp-summon/relay.sock`: mode 0660. The helper creates it while already running as uid 1337, so
  SO_PEERCRED shows 1337. The run script removes any stale socket first.
- `/run/airgapp-summon/session`: the session marker.
- `/var/lib/airgapp-summon/state`: 1337:1337, mode 0700. It holds `key.pem` (0400) and `log`.
- The allowlist `/var/lib/airgapp-gw/allow` is read and watched, never written.
- The run script reads the VIN from `/var/etc/vin` as root and passes it in the environment as `SUMMONHELPER_VIN`.
- The helper runs under `chpst -u :1337:1337`, with no group 4950.
- The helper accepts only peer uid 4950, and agw's relay accepts only server uid 1337. Tests set both to
  `os.Getuid()` through seams.

**Checks.** Each task runs its own packages; Task 16 runs everything.
- `gofmt -l .` prints nothing.
- `go vet ./...`, plus `GOOS=linux GOARCH=amd64 go vet ./...`.
- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`.
- `go test -race -count=1 <packages>`, then `-count=3 ./...` in Task 16.
- Unix socket paths in tests use `os.MkdirTemp("", "<short>")`, because darwin caps them at 104 bytes.

**Out of scope:**
- The phone (the next plan).
- CAMERA selecting Grid, and its keyframe and gate exceptions (camera session).
- Wiring the KeysUpdated watch (after check 4).
- Phase 0.
- Deploying to the car.

## Calls made in this plan (for the owner to check)

1. **The relay's name check moves from PreArm to the helper's OWNED record.** The design (§6.6) says agw's PreArm
   re-checks that `com.tesla.Webrtc` is free or the helper's, but agw has no bus access (§3). So:
   - The helper accepts relay connections only while it owns the name and is healthy, and says so in an OWNED record.
   - agw's Open requires OWNED within 500 ms.
   - PreArm keeps the R1 gate and the socket check.
2. **D3's stale-position rules run only while verbActive.** Both 1 s windows are measured from the later of the last
   frame and the moment the current verb was forwarded, so silence before a press never counts. Outside a run, the
   last position stays valid. A position that went stale stays stale until a newer location frame arrives.
   - The firmware resends `vehicle_data:location` only when latitude, longitude, heading, speed, blinker or gear
     change. `VechicleLocation::operator==`, QtCarServer.c:1152842, does not compare the timestamp.
   - One case remains. If the car stands still in D or R for over 1 s after a press, a re-press after the release is
     refused (`invalid_vehicle_location`, "Something went wrong") until the car's next location change. Tesla's app
     would resume there.
   - This plan builds what the design says. The owner may relax it later.
3. **An emit that overruns 100 ms latches the bus dead and exits the process.** A stuck godbus write blocks every
   other send on that connection, and `Conn.Close` deadlocks behind it (verified against godbus v5.1.0). The parent's
   takeover is then the only way to deliver the abort.
4. **The inner frame types (0x14-0x17, 0x91) differ from the relay's (0x10, 0x90).** A frame sent to the wrong session
   is refused as grammar instead of being misread.
5. **The helper hands a relay connection to the core only when its first INNER record arrives.** So the core's 2 s
   pre-auth clock starts at the phone's inner hello, not at agw's dial.
6. **Car frames:**
   - A car frame whose JSON is over 16383 bytes is dropped and logged by type and size.
   - If the phone link cannot take a frame (the inner priority lane is full), the session ends (H4). A frame is never
     dropped silently: Tesla's app has no command-ack timer.
7. **The KeysUpdated watch is not wired.** Open sessions are re-checked every 10 s. The probe reports whether uid 1337
   receives a sender-pinned KeysUpdated, so the owner can decide (design §6.1).
8. **Two new agw commands, `probe-route` and `probe-summon`.**
   - R1 needs uid 4950 to read tailscaled's LocalAPI, which is unverified on the car. The installer runs
     `probe-route` as uid 4950.
   - The installer's cross-uid connect test is `probe-summon`, run as uid 4950: it reads OWNED and closes.
9. **Unknown keys make a phone message non-conforming.**
   - Duplicate keys need no rule, because the helper re-encodes what it checked.
   - A `cmd_abort` is always accepted and sent as the canonical abort.
10. **Check 4's round-trip item needs a signed-in phone,** so it runs once the phone link exists (the phone plan).
    - The probe covers QtCarServer's acceptance, the caller, the stream freeze, KeyInfo as uid 1337 and KeysUpdated.
    - Task 16's owner steps cover the VIN the run script read, the pre-prime, the config reads, the keepalive and the
      gear probe.
11. **Installing with `SUMMON=0` over a `SUMMON=1` install** removes the helper service and runs
    `sv up /etc/sv/webrtc-comms`. This is the design's "uninstall"; the installer has no other uninstall.
12. **The probe uses the session marker.** It refuses while the marker exists. It sets the marker before H1, and
    clears it only after a confirmed disconnect. So a probe that died after H1 is cleaned up by
    `summonhelper finish`, the same backstop the helper uses. Otherwise the car would stay "connected", and the next
    H1 would bring no frames.

## Review Focus

These are the five likeliest failure modes that the design implies but none of its listed tests exercise. Each has a
test in its owning task.

1. **The phone link stalls while the car keeps sending:** heartbeat echoes at 10 Hz, plus location frames.
   - Expected: every `CarserverToPeerCommsData` is still answered at once, and nothing buffers without bound.
   - When the inner lane is full, the session ends (H4) instead of dropping a frame.
   - Test: Task 8, `TestCarFramesWhilePhoneStalled`.
2. **agw re-arms while the previous relay connection's H4 is still emitting.**
   - Expected: the new connection gets OWNED only after the previous abort and disconnect went out, so the car sees
     abort, disconnect, then `data_channels_ready`.
   - Test: Task 10, `TestRearmWaitsForTeardown`.
3. **A trip, then the phone's own release.**
   - Expected: only the injected abort's `cmd_result` is swallowed. The phone's abort and its result go through.
   - Test: Task 6, `TestSwallowOnlyInjectedAbortResult`.
4. **The car's `cmd_result` timestamps run ahead of the helper's clock.**
   - Expected: a refusal's timestamp is never below the last forwarded one. Tesla's app drops stale results while it
     waits for a command response.
   - Test: Task 6, `TestRefusalTimestampNeverGoesBack`.
5. **A car frame arrives between sessions,** for example a late `cmd_result` after H4.
   - Expected: it is dropped, and the next session starts clean: no swallow count, no highest timestamp, no car
     position, no link flag.
   - Test: Task 8, `TestFramesBetweenSessionsDropped`.

## File structure

| Path | Responsibility | Task |
|---|---|---|
| `client/` | The retired API cut; `Handshake` over a `net.Conn`; `InnerConn` | 1, 2 |
| `internal/summon/schema.go` | Tesla's phone-to-car shapes: parse, classify, canonical re-encode | 3 |
| `internal/carlink/{bus,dbus,link,fakebus}.go` | The bus seam, godbus on any OS, the deadline-bounded `Link` | 4 |
| `internal/carlink/bustest/` | A private `dbus-daemon` for tests | 4 |
| `internal/carlink/mockcar/` | The mock QtCarServer, in Tesla's flow | 5 |
| `internal/summon/engine.go`, `geo.go` | D2 (with the link flag), D3, H4 sequencing and refusals; pure, with an injected clock | 6, 7 |
| `internal/summon/frames.go`, `capability.go` | The inner capability: frames, the window binding, the car-frame queue, the D2 loop | 8 |
| `internal/peercred/` | Peer uid of a unix connection (Linux SO_PEERCRED, darwin LOCAL_PEERCRED) | 9 |
| `internal/summon/relay/` | The record codec, the helper's relay listener, the relay connection | 9 |
| `internal/summon/helper/` | The helper process: run, serve, takeover, finish | 10, 11 |
| `cmd/summonhelper/` | The CLI: `run`, `serve`, `finish`, `keygen`, `pin`, `probe` | 10, 11, 14 |
| `internal/supervise/` | The parent and child (exists); an exported first-beat hook | 11 |
| `capability/summonrelay/` | agw's cap 0x01 relay | 12 |
| `cmd/agw/` | The relay row, a test hook, `probe-route`, `probe-summon`, the bench e2e | 12, 13 |
| `internal/summon/probe/` | The no-motion probe | 14 |
| `deploy/install-car.sh`, `cmd/camhelper/install_test.go`, `Makefile`, `deploy/README.md` | `SUMMON=1` | 15 |
| `docs/SUMMON.md`, `docs/CAPABILITY-API.md`, `README.md` | The architecture, the bench and the no-motion checks | 16 |

---

### Task 1: Merge the camera branch and retire the old SUMMON code

**Files:**
- Merge commit on `feat/summon-car`: `git merge --no-ff feat/camera-capabilities` (its tip was e04cd11 when this plan
  was written).
- Take the camera side whole: `cmd/agw/main.go`, `cmd/agw/main_test.go`, `README.md`.
- Delete: `cmd/agw/supervise_test.go`, `cmd/agw/summon_e2e_test.go`, `capability/summon/` (5 files),
  `internal/guards/cooldown.go`, `docs/SUMMON.md` (Task 16 rewrites it).
- Modify:
  - `internal/guards/geo.go`: keep only `DistanceM` and `ValidLatLon`;
  - `internal/guards/guards_test.go`: keep the route tests and the `DistanceM`/`ValidLatLon` tests;
  - `internal/guards/route.go:1-2`: the package comment;
  - `client/client.go`, `client/tick_test.go`;
  - `docs/CAPABILITY-API.md`.

**Interfaces:**
- Consumes: `feat/camera-capabilities`.
- Produces a green tree with:
  - `guards.RouteOff(ctx context.Context, sock string) error`;
  - `guards.DefaultTailscaleSock`;
  - `guards.DistanceM(lat1, lon1, lat2, lon2 float64) float64`;
  - `guards.ValidLatLon(lat, lon float64) bool`;
  - `client.Session.NewestTick() (id [8]byte, age time.Duration, ok bool)`;
  - `client.TickMaxAge`.

  `internal/carlink` and `internal/supervise` are still present but unused.

- [ ] **Step 1: Create the worktree**

```bash
git -C ~/Work/airgapp/gateway worktree add -b feat/summon-car ~/Work/airgapp/gateway-summon feat/summon
```

- [ ] **Step 2: Merge, resolving as one green merge commit**

```bash
cd ~/Work/airgapp/gateway-summon
git merge --no-ff --no-commit feat/camera-capabilities
git checkout feat/camera-capabilities -- cmd/agw/main.go cmd/agw/main_test.go README.md
git rm -q cmd/agw/supervise_test.go cmd/agw/summon_e2e_test.go internal/guards/cooldown.go docs/SUMMON.md
git rm -rq capability/summon
```

Then trim `geo.go` and `guards_test.go` as listed: drop `CheckGoal`, `CheckDevice` and the cool-down tests, plus the
imports they leave unused. Fix the route.go package comment.
- The expected conflicts are exactly README.md, cmd/agw/main.go (7 hunks) and cmd/agw/main_test.go (1 hunk).
- If other files conflict: take the camera side for files the camera branch owns, and feat/summon's side for files
  only feat/summon changed. List them in the report.

- [ ] **Step 3: Check that the merge tree builds and passes**

Run: `git add -A internal/guards && git status --short`
Expected: no line with an unstaged change (a second-column letter). Then run
`go build ./... && go vet ./... && go test -count=1 ./...`.
Expected: PASS. Commit with `git commit -m "merge: feat/camera-capabilities into feat/summon-car; retire the agw-side SUMMON capability"`.

- [ ] **Step 4: Cut the retired phone API from `client/`**

- Delete `Hold`, `Release`, `Loc`, `TypeHold`, `TypeRelease`, `TypeLoc`, `LocMaxAge` and `LocMaxAccM`, and
  `freshTick` if nothing else uses it.
- Keep `Recv`'s TICK intake, `NewestTick` and `TickMaxAge`.
- Rewrite `client/tick_test.go` as `TestTicksConsumed`. It uses a TickHz-20 probe capability with ID
  `proto.CapSummon` and no inbound types, and asserts:
  - `Recv` never returns type 0x82 over 300 ms;
  - `NewestTick` reports `ok == true` and `age < TickMaxAge` after the first tick.
- In `docs/CAPABILITY-API.md`, replace the auto-merged capEntry and SUMMON-BENCH text with the camera branch's
  `capTable`. The SUMMON row says only "0x01: Summon relay (In 0x10 INNER, Out 0x90 INNER); see docs/SUMMON.md".

- [ ] **Step 5: Verify that nothing retired survives**

Run:

```bash
git grep -n -E 'capability/summon([^a-z]|$)|summon-finish|cmdSupervise|CheckGoal|CheckDevice|Cooldown|TypeHold|TypeLoc|LocMaxAge' -- client cmd/agw capability internal/guards docs/CAPABILITY-API.md README.md
```

Expected: no output. This covers only the paths Task 1 owns. `summon-finish` stays in the installer files (Task 15)
and in `internal/supervise`'s package comment (Task 11) until those tasks, and Task 16 greps the whole tree.

Run: `go vet ./... && GOOS=linux GOARCH=amd64 go vet ./... && go test -race -count=1 ./client/ ./internal/guards/ ./cmd/agw/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add -A client docs/CAPABILITY-API.md
git status --short   # expect no unstaged changes
git commit -m "chore(client): cut the retired HOLD/RELEASE/LOC API; keep the tick intake"
```

---

### Task 2: The Go test phone can nest a session

**Files:**
- Create: `client/nest.go`, `client/nest_test.go`

**Interfaces:**
- Consumes: the unexported `client.handshake`; `client.Session.Send/Recv`.
- Produces:

```go
// Handshake runs the full sign-in over conn (hello, challenge, HPKE ARM, READY) and returns the session.
func Handshake(ctx context.Context, conn net.Conn, cap byte, params any, pinned *ecdsa.PublicKey, vin string,
	device *ecdh.PrivateKey) (*Session, error)

// InnerConn turns an open outer session into a byte stream: writes go out as frames of type in (split at 16384
// bytes), frames of type out are read back, and other types are dropped. It owns outer.Recv from now on. Closing
// the conn closes outer.
func InnerConn(outer *Session, in, out byte) net.Conn
```

`InnerConn` is a `net.Pipe` end, pumped by two goroutines. That gives `handshake` the deadlines it calls.

- [ ] **Step 1: Write the failing tests**

`nest_test.go`, inside package `client`. An in-process outer `server.New` carries a test relay capability (ID
`proto.CapSummon`, In 0x10, Out 0x90) that pipes bytes into a second, inner `server.New`. The inner server has a
TickHz-20 capability (ID `proto.CapSummon`). Both use `authority.Fake{Role: 2, Owner: true}`.
- `TestHandshakeOverConn`.
- `TestInnerConnNestedSession`:
  - the inner session comes up, and `NewestTick()` turns fresh (`ok`, age < 150 ms) within 500 ms;
  - a `Send` of 16384 bytes on an inner type whose In limit is 16384 arrives whole. Its sealed frame is about 16405
    bytes, so it spans two 0x10 frames;
  - an inner Out type with limit 40000, sent from the inner capability's `Open`, arrives whole at the phone.

  The core caps In limits at 16384 and panics above that.
- `TestInnerConnCloseClosesOuter`.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./client/ -run 'Test(Handshake|InnerConn)' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement `client/nest.go`**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./client/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add client/nest.go client/nest_test.go
git commit -m "feat(client): Handshake over a net.Conn and InnerConn, for a session nested in another"
```

---

### Task 3: Tesla's phone-to-car message shapes

**Files:**
- Create: `internal/summon/schema.go`, `internal/summon/schema_test.go`

**Interfaces:**
- Produces:

```go
package summon

type Kind uint8

const (
	KindVerb Kind = iota + 1 // VERB frames
	KindHeartbeat            // HB frames
	KindMsg                  // MSG frames
)

const AbortJSON = `{"msg_type":"autopark:cmd_abort"}`

var (
	ErrDrop  = errors.New("summon: non-conforming message, dropped")
	ErrFatal = errors.New("summon: non-conforming message, session ends")
)

type Message struct {
	Type             string  // msg_type
	Canonical        []byte  // exactly what the helper sends to the car
	Lat, Lon         float64 // verbs and device_location
	GoalLat, GoalLon float64
	HasGoal          bool    // both goal_latitude and goal_longitude present
}

func Parse(k Kind, body []byte) (Message, error) // errors wrap ErrDrop or ErrFatal
```

**Shapes** (Appendix A §2.1). Every listed key is required unless marked optional, and no other key is allowed.
Numbers must be JSON numbers (decode with `UseNumber`) whose `Float64()` is finite.

| msg_type | Frame | Keys |
|---|---|---|
| `autopark:cmd_find_me` | VERB | `latitude`, `longitude`, `accuracy`, `goal_latitude`, `goal_longitude` (numbers); `client_version` (exactly 2); `summon_type` (`find_me` or `pin_drop`); `created:timestamp` |
| `autopark:cmd_forward`, `autopark:cmd_reverse` | VERB | `latitude`, `longitude`; `client_version` (exactly 2); `created:timestamp` |
| `autopark:heartbeat_app_webrtc` | HB | `timestamp`; `car_timestamp` (optional); `created:timestamp` |
| `autopark:cmd_abort` | MSG | anything; always accepted and sent as `AbortJSON` |
| `autopark:device_location` | MSG | `latitude`, `longitude`, `accuracy`, `altitude`, `altitude_accuracy`, `goal_latitude`, `goal_longitude` (numbers); `summon_type` (`find_me` or `pin_drop`); `created:timestamp` |
| `autopark:info` | MSG | `txid` (exactly `<none>`); `x_tesla_user_agent`, `model`, `name`, `tag`, `public_key_hash` (strings); `created:timestamp` |
| `control:keepalive` | MSG | `created:timestamp` (optional) |

**What Parse returns:**
- Fatal:
  - a top-level `_request` key, in any frame;
  - a VERB body that is not a JSON object;
  - a VERB or HB whose `msg_type` belongs to another frame type, or is not in the table;
  - a verb with a shape error;
  - a MSG whose `msg_type` is a verb or `autopark:heartbeat_app_webrtc`.
- Drop:
  - an HB or MSG body that is not a JSON object, or has no string `msg_type`;
  - a MSG whose `msg_type` is not in the table;
  - a heartbeat, `device_location`, `info` or keepalive with a shape error.
- `Canonical` is the parsed object re-marshalled compact, with keys sorted and every number kept as its original
  `json.Number` text. For `cmd_abort` it is exactly `AbortJSON`.

- [ ] **Step 1: Write the failing tests**

`schema_test.go`, table-driven, using Appendix A §2.1's examples:
- `TestParseAcceptsTeslaShapes`: one case per table row. It asserts `err == nil`, `m.Type`, and that `m.Canonical`
  decodes back to the same keys and values. For `cmd_find_me` with `summon_type` `pin_drop`, it asserts
  `m.HasGoal == true` and the goal values. For `cmd_forward`, `m.HasGoal == false`.
- `TestParseCanonicalKeepsNumbers`: `"accuracy":-1` stays `-1`, `"client_version":2` stays `2`, and
  `"created:timestamp":1700000000123` keeps all 13 digits.
- `TestParseAbortIsCanonical`: `{"msg_type":"autopark:cmd_abort","created:timestamp":5}` gives exactly `AbortJSON`.
- `TestParseFatal` (each wraps `ErrFatal`):
  - VERB `cmd_find_me` without `accuracy`;
  - VERB with `summon_type` `"auto"`;
  - VERB with `client_version` 3;
  - VERB carrying `device_location`;
  - VERB that is not JSON;
  - HB carrying `cmd_forward`;
  - MSG carrying `cmd_reverse`;
  - MSG carrying `heartbeat_app_webrtc`;
  - `{"_request":{...}}` in MSG.
- `TestParseDrop` (each wraps `ErrDrop`):
  - `device_location` without `goal_latitude`;
  - `info` with `txid` `"x"`;
  - keepalive with an extra key;
  - heartbeat with `car_timestamp` as a string;
  - MSG `{"msg_type":"autopark:cmd_pause"}`;
  - MSG that is not JSON.
- `TestParseExtraKeyOnVerbIsFatal`: `cmd_forward` with `"speed":1`.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/summon/ -run TestParse -count=1`
Expected: FAIL (the package does not exist).

- [ ] **Step 3: Implement `Parse` in `internal/summon/schema.go`**

Decode with `json.Decoder.UseNumber()` into `map[string]any`. Validate against a per-`msg_type` key table, then build
`Canonical` with `json.Marshal` of the validated map. Go sorts the keys, and `json.Number` keeps the digits.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race ./internal/summon/ -run TestParse -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/summon/schema.go internal/summon/schema_test.go
git commit -m "feat(summon): Tesla's phone-to-car message shapes, with drop/fatal classes and canonical re-encoding"
```

---

### Task 4: carlink, the helper's D-Bus link

**Files:**
- Modify: `internal/carlink/bus.go`, `internal/carlink/fakebus.go`
- Create:
  - `internal/carlink/dbus.go`, from `dbus_linux.go`, with no build tag;
  - `internal/carlink/link.go`;
  - `internal/carlink/dbus_test.go`, from `dbus_linux_test.go`, portable;
  - `internal/carlink/link_test.go`;
  - `internal/carlink/bustest/bustest.go`.
- Delete:
  - `internal/carlink/{service.go,messages.go,parse.go,dbus_linux.go,dbus_other.go,mockcar.go}`;
  - their tests (`carlink_test.go`, `parse_test.go`, `mockcar_test.go`, `dbus_linux_test.go`);
  - `internal/carlink/testdata/`.

  Task 5 rebuilds the mock car in its own package.

**Interfaces:**
- Produces:

```go
package carlink

const (
	WebrtcName      = "com.tesla.Webrtc"
	WebrtcPath      = "/Webrtc"
	WebrtcIface     = "com.tesla.Webrtc"
	PhoneUID        = "test_phone"
	ReplyKey        = "CarserverToPeerCommsData_Reply"
	MaxCarData      = 65536
	ChannelTCP      = "dcTcp"
	ChannelUDP      = "dcUdp"
	EmitDeadline    = 100 * time.Millisecond
	ExitEmitOverrun = 3
)

type CarFrame struct{ Channel, UID, Sender string; Data []byte }

type Handlers struct {
	CarFrame  func(CarFrame)             // in the car's call order; must not block
	Signaling func(body map[string]any)  // RemoteToLocalSignaling, in order; must not block
	Reply     func() int32               // the CarserverToPeerCommsData_Reply value
}

type Bus interface {
	RequestName(name string, queue bool) (owned bool, err error) // queue=false: DO_NOT_QUEUE; true: flags 0
	ReleaseName(name string) error
	Emit(member string, body map[string]any) error
	Serve(h Handlers) error // exports both methods on /Webrtc once
	OnNameLost(name string, f func()) error
	OnNameAcquired(name string, f func()) error
	NameOwner(ctx context.Context, name string) (string, error) // org.freedesktop.DBus.GetNameOwner
	UniqueName() string
	Close() error
}

func DialBus(addr string) (*DBusBus, error) // a private bus: tests and the bench
func SystemBus() (*DBusBus, error)          // the car's system bus
// CallerInfo returns the caller's uid, and its pid when the bus knows it (0 otherwise; Homebrew's dbus-daemon on
// darwin cannot tell). A missing pid is not an error.
func (b *DBusBus) CallerInfo(ctx context.Context, sender string) (uid, pid uint32, err error)
func (b *DBusBus) SubscribeKeysUpdated(f func()) error

var (
	ErrNameTaken = errors.New("carlink: com.tesla.Webrtc has another owner")
	ErrBusDead   = errors.New("carlink: bus latched dead after an emit overran")
)

type LinkOption func(*Link)
func WithExit(f func(code int)) LinkOption // default os.Exit
func WithLogf(f func(format string, args ...any)) LinkOption
func NewLink(b Bus, opts ...LinkOption) *Link
func (l *Link) Acquire() error           // RequestName(WebrtcName, false); ErrNameTaken if not owned
func (l *Link) Owned() bool
func (l *Link) Healthy() bool            // owned, not dead, and no emit has failed
func (l *Link) Connect() error           // H1 body
func (l *Link) Send(data []byte) error   // PeerCommsToCarserverData {uid: PhoneUID, data: string(data)}
// Disconnect emits the H4 disconnect body, then confirms it: within EmitDeadline, NameOwner(WebrtcName) must return
// this connection's unique name while Healthy() is still true. The daemon handles one connection's messages in
// order, so that reply proves the signal left while the name was ours. Anything else is an error.
func (l *Link) Disconnect() error
func (l *Link) OnUnhealthy(f func())     // called once, on its own goroutine, when Healthy turns false
func (l *Link) Release() error

package bustest

type Bus struct{ /* ... */ }
func Start(t testing.TB) *Bus // t.Skip("no dbus-daemon") only when dbus-daemon is not on PATH
func (b *Bus) Addr() string
func (b *Bus) Conn(t testing.TB) *dbus.Conn // Dial + Auth + Hello, closed at cleanup
```

**Rules:**
- **Ordering (H3).** `DBusBus` builds its connection with
  `dbus.WithIncomingInterceptor(...)` and `dbus.WithSignalHandler(dbus.NewSequentialSignalHandler())`.
  - The interceptor runs on godbus's read goroutine. It recognizes method calls to `CarserverToPeerCommsData` and
    `RemoteToLocalSignaling` on `/Webrtc`, decodes `body[0]` as `map[string]dbus.Variant`, and calls
    `Handlers.CarFrame` or `Handlers.Signaling`.
  - `Sender` is the message's sender header. Data over `MaxCarData`, or a malformed map, is dropped and logged by
    size.
  - The exported methods only reply.
- **Name ownership.**
  - NameLost and NameAcquired come from `org.freedesktop.DBus` only.
  - A closed connection fires every NameLost callback once (as feat/summon's carlink does today).
- **`SubscribeKeysUpdated`** adds the sender-pinned match
  `sender='com.tesla.Auth',path='/Auth',interface='com.tesla.Auth',member='KeysUpdated'`.
- **The emit deadline lives in `Link`, so it covers every `Bus`.**
  - Each emit runs on its own goroutine with a timer of `EmitDeadline`.
  - On overrun, `Link` latches dead: later emits return `ErrBusDead` at once, without a goroutine. It then logs and
    calls `exit(ExitEmitOverrun)`.
  - Any emit error latches `Healthy` to false. So does NameLost.
  - `Link` never calls `Bus.Close` on these paths, because `Close` deadlocks behind a stuck write.
  - `Link`'s emit methods do not check ownership; only `Healthy` does. The supervisor's takeover (Task 11) emits
    through a `Link` whose name it got by queueing, not through `Acquire`. Its ownership comes from
    `OnNameAcquired`.
  - `Link` logs through the injected `logf`, which must never block (Global Constraints). The overrun path calls
    `exit` without waiting for its log line.
  - **The confirmation round trip.** godbus v5.1.0's `Emit` returns nil even when the write failed, so
    `Disconnect`'s confirmation is what makes it trustworthy. The godbus read goroutine must not be blocked by any lock
    held during H4, so the reply can arrive. `FakeBus.NameOwner` returns its unique name only while it owns the name.
- **`FakeBus`** implements `Bus` for a single connection. It keeps `TakeName`, `FreeName`, `LoseName`,
  `DeliverNameLost`, `SetEmitErr`, `Emissions` and `OnEmit`, and adds:
  - `StallEmits(release <-chan struct{})`: `Emit` blocks until `release` is closed;
  - `CallCarserverToPeer(in map[string]any) map[string]any`: runs `Handlers.CarFrame` synchronously, then replies;
  - `CallRemoteToLocal(in map[string]any) uint32`.

  `RequestName(name, true)` returns an error: queueing is tested on a real bus.

- [ ] **Step 1: Write the failing tests**

- `bustest` has no tests of its own; it is exercised below.
- `dbus_test.go` (bustest; every test passes on darwin):
  - `TestRequestNameDoNotQueue`: A owns. B with queue=false gets `owned == false` and no error.
  - `TestQueuedOwnerAcquiresOnOwnerDeath`: A owns, and B gets InQueue with queue=true. After `A.Close()`, B's
    `OnNameAcquired` fires within 1 s.
  - `TestNameLostOnClose`.
  - `TestCarCallsDeliveredInOrder`. A second connection plays the car and fires 500
    `CarserverToPeerCommsData` calls with `Go` (no waiting), each `{uid:"test_phone", channel:"dcTcp"|"dcUdp",
    data:"<n>"}`. It asserts:
    - `Handlers.CarFrame` sees n = 1..500 in order, with the right channel and `UID == "test_phone"`;
    - `Sender` is the car connection's unique name;
    - every reply is `{ReplyKey: int32(1)}` while `Reply` returns 1.
  - `TestSignalingDelivered`: `{type: int32(9), uid: "test_phone", session_id: ""}` reaches `Handlers.Signaling`,
    and the reply is 0.
  - `TestKeysUpdatedSubscriptionIsSenderPinned`. A connection that owns `com.tesla.Auth` emits `KeysUpdated` on
    `/Auth`: the callback fires once. A non-owner emits the same signal: no callback.
  - `TestCallerInfo`: an in-process caller gives `uid == os.Getuid()` and no error. Only on Linux does it also
    assert `pid == os.Getpid()`.
- `link_test.go`:
  - `TestLinkEmitBodies` (FakeBus): `Connect`, `Send` and `Disconnect` give exactly the bodies in Global
    Constraints, with members `LocalToRemoteSignaling` and `PeerCommsToCarserverData`.
  - `TestLinkAcquireTaken`: `Acquire` returns `ErrNameTaken`.
  - `TestLinkHealthyFalseAfterNameLost`.
  - `TestLinkFailedEmitLatchesUnhealthy`.
  - `TestLinkDisconnectConfirms`: `Disconnect` returns nil while the name is owned. After `LoseName` it returns an
    error, even though `FakeBus.Emit` succeeded.
  - `TestLinkOnUnhealthyOnce`: `LoseName`, then a failed emit, call the callback exactly once.
  - `TestLinkEmitOverrunExits`. `StallEmits` makes `Send` stall. Asserts:
    - the exit hook is called once, with 3, within 100-200 ms;
    - `Healthy()` is false;
    - the next `Send` returns `ErrBusDead` within 5 ms;
    - `runtime.NumGoroutine()` does not grow across 50 more sends.
  - `TestLinkEmitOverrunRealGodbus`: `dbus.NewConn` over a `socketpair` whose peer never reads, with a 4 MiB body.
    The exit hook fires.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/carlink/... -count=1`
Expected: FAIL (compile errors: the new API is missing).

- [ ] **Step 3: Implement**

Write `bus.go`, `dbus.go`, `link.go`, `fakebus.go` and `bustest/bustest.go` as specified.
- Copy `startBus`'s allow-all config and `--nofork --print-address` from
  `feat/camera-capabilities:internal/authsrv/watch_test.go:29-69`.
- `SystemBus` uses `dbus.ConnectSystemBus`.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./internal/carlink/... && GOOS=linux GOARCH=amd64 go vet ./internal/carlink/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -A internal/carlink
git commit -m "feat(carlink): portable godbus link with ordered car frames, a 100 ms emit deadline and a private-bus harness"
```

---

### Task 5: The mock QtCarServer

**Files:**
- Create: `internal/carlink/mockcar/mockcar.go`, `mockcar_test.go`

**Interfaces:**
- Consumes: `carlink.FakeBus`, `carlink.Handlers` (Task 4), `bustest` (Task 4).
- Produces:

```go
package mockcar

type Port interface {
	CallCarserverToPeer(channel string, data []byte) error
	CallRemoteToLocal(body map[string]any) error
}

// New attaches the Car to its Port (an unexported attach method), so the port can deliver signals to it.
func FakePort(b *carlink.FakeBus) Port // delivers b's emissions to the attached Car
// DBusPort plays QtCarServer on a real bus. It matches signals with
// sender='com.tesla.Webrtc',path='/Webrtc',interface='com.tesla.Webrtc' (the daemon resolves the well-known name to
// its current owner), and calls methods on destination com.tesla.Webrtc, as QtCarServer's interface does.
func DBusPort(conn *dbus.Conn) Port

type Config struct {
	Port        Port
	Now         func() time.Time // the car's epoch clock (timestamps)
	SampleEvery time.Duration    // default 250 ms
	Type9After  time.Duration    // default 60 s
	Logf        func(format string, args ...any)
}

type Received struct {
	Member, MsgType string
	Data            []byte
	At              time.Time
}

func New(cfg Config) *Car
func (c *Car) Signal(member string, body map[string]any) // input: what the helper emitted
func (c *Car) Run(ctx context.Context)                   // real time: sampling and the type 9 timer
func (c *Car) Step(now time.Time)                        // deterministic, for unit tests
func (c *Car) SetPose(lat, lon, heading, speed float64, blinker, shift string)
func (c *Car) RefuseNext(reason string)
func (c *Car) SetEcho(on bool)
func (c *Car) FreezeAfter(n int) // sends nothing after n more frames (the research freeze)
func (c *Car) Received() []Received
func (c *Car) Connected() bool
func (c *Car) Violations() []string
func (c *Car) WaitFor(pred func([]Received) bool, timeout time.Duration) bool
```

**Tesla's flow** (Appendix A §1.2 and §8). Outbound JSON uses Go's sorted keys, as QtCarServer's maps do.
- **On `data_channels_ready`** (a false-to-true edge), send in this order, all on `dcTcp`:
  1. `{"autopark_style":"dead_man","msg_type":"autopark:style"}`;
  2. the status `{"autopark_state":"standby","autopark_state_reason":"","msg_type":"autopark:status",
     "smart_summon_last_abort_reason":"","smart_summon_last_unavailable_reason":"",
     "smart_summon_leash_proximity_meters":0,"smart_summon_leash_travel_meters":0,
     "smart_summon_state":"initializing","timestamp":<ms>}`;
  3. one location frame.
- **Location:** `{"blinker_state","heading","latitude","longitude","msg_type":"vehicle_data:location","shift_state",
  "speed","timestamp"}`, sampled every `SampleEvery`. It is sent only when latitude, longitude, heading, speed,
  blinker or gear changed (the firmware's `operator==`). The status is never re-sent during motion (the standby quirk).
- **Heartbeat:** `heartbeat_app_webrtc` gets `{"car_timestamp":<ms>,"msg_type":"autopark:heartbeat_car_webrtc",
  "timestamp":<the echoed timestamp>}` on `dcUdp`, while echo is on (default on).
- **Verbs:** a verb gets `{"cmd_type":<its msg_type>,"msg_type":"autopark:cmd_result","reason":""|<refusal>,
  "result":<bool>,"timestamp":<ms>}` on `dcTcp`.
- **One CAPI value, as the firmware keeps it** (Appendix A §8.2).
  - A verb sets the value to 1, 2 or 6, which resets after 20 s. An abort sets 3, which resets after 5 s.
  - `already_set` is returned only when the request's value equals the current one. So a release, then a re-press of
    the same verb within 20 s, gets `result: true`, as on the car.
  - Otherwise the result is `true`, and the value is set.
- **No reply:** `device_location`, `info` and `control:keepalive` are recorded and get no reply.
- **The disconnect map** sets the car disconnected. After `Type9After` the car calls `RemoteToLocalSignaling`
  `{type: int32(9), uid: "test_phone", session_id: ""}`.
- **Violations:**
  - a `msg_type` outside the helper's allowed set;
  - an abort whose data is not exactly `{"msg_type":"autopark:cmd_abort"}`;
  - `uid` ≠ `test_phone`;
  - data while disconnected;
  - a second connect without a disconnect;
  - a disconnect without a connect.

- [ ] **Step 1: Write the failing tests**

Using `FakePort` and `Step`:
- `TestConnectPushesStyleStatusLocation`: exact order, exact keys, all on `dcTcp`.
- `TestHeartbeatEcho`: `dcUdp`, the echoed timestamp, and `car_timestamp` taken from `Now`.
- `TestVerbResultShape`:
  - `cmd_find_me` with `summon_type` `pin_drop` answers `cmd_type` `autopark:cmd_find_me`;
  - `cmd_reverse` answers `autopark:cmd_reverse`;
  - `RefuseNext("plugged_in")` gives `result:false`, `reason:"plugged_in"`.
- `TestAbortResultAndAlreadySet`: a second abort within 5 s gets `already_set`.
- `TestReleaseThenRepressAccepted`: a verb, an abort, then the same verb within 20 s gets `result: true`.
- `TestLocationOnChangeOnly`:
  - no change for 2 s sends no frame;
  - a speed change sends one frame within one sample;
  - a gear change P to D sends one frame.
- `TestDisconnectThenType9`, with `Type9After` 50 ms.
- `TestViolations`: one case per rule above.
- `TestFreezeAfter`.
- `TestDBusPortIgnoresNonOwner` (bustest): a `LocalToRemoteSignaling` emitted by a connection that does not own
  `com.tesla.Webrtc` is not received.
- `TestDBusPortRoundTrip` (bustest). A `carlink.Link` on `DialBus` owns the name. The car connection gets
  `data_channels_ready` and answers through the helper's methods: the style, status and location arrive at the
  link's `Handlers.CarFrame`, in order.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/carlink/mockcar/ -count=1`
Expected: FAIL (the package does not exist).

- [ ] **Step 3: Implement the mock in `internal/carlink/mockcar/mockcar.go`**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./internal/carlink/mockcar/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/carlink/mockcar
git commit -m "feat(mockcar): a mock QtCarServer in Tesla's flow, on a fake or a real bus"
```

---

### Task 6: The engine's deadman (D2) and session end (H4)

**Files:**
- Create: `internal/summon/engine.go`, `internal/summon/engine_test.go`

**Interfaces:**
- Consumes: `summon.Message`, `summon.AbortJSON` (Task 3); `clock.Clock`.
- Produces:

```go
package summon

const (
	Fresh        = 150 * time.Millisecond
	TripAfter    = 300 * time.Millisecond
	ChanTCP      = byte(0) // CAR channel byte for dcTcp
	ChanUDP      = byte(1) // CAR channel byte for dcUdp
	LinkSetAge   = 100 * time.Millisecond
	LinkSetRun   = 3
	LinkClearAge = 75 * time.Millisecond
	LinkClearFor = 2 * time.Second
)

type Car interface { // carlink.Link implements it
	Connect() error
	Send(data []byte) error
	Disconnect() error
}

type Phone interface { // the capability adapter implements it
	Car(channel byte, data []byte) bool // false: the phone link cannot take it
	SetLink(degraded bool)
	End(reason string) // called by the engine only after it releases its lock
}

type Marker interface{ Set() error; Clear() error }

type Config struct {
	Car    Car
	Phone  Phone
	Marker Marker
	Clock  clock.Clock      // BOOTTIME ms: liveness, staleness, the trip
	Now    func() time.Time // wall clock: refusal timestamps
	Logf   func(format string, args ...any) // never blocks (Global Constraints)
	Fatal  func(err error) // a panic in Stop or Tick; the helper latches the link dead and exits
}

func NewEngine(cfg Config) *Engine
func (e *Engine) Start() error // Marker.Set, then Car.Connect (H1); an error refuses the session
func (e *Engine) Stop(reason string) // H4, exactly once; later calls do nothing
// age and ok are Window.TickAge's answer for the frame's tick id. Fresh means ok && age <= Fresh.
func (e *Engine) Live(age time.Duration, ok bool)
func (e *Engine) Verb(m Message, age time.Duration, ok bool, pressID uint32)
func (e *Engine) Heartbeat(m Message, age time.Duration, ok bool, pressID uint32)
func (e *Engine) Msg(m Message)
func (e *Engine) CarFrame(channel string, data []byte)
func (e *Engine) Tick() // called every 10 ms by the D2 loop
func (e *Engine) VerbActive() bool
```

**D2 rules** (design §5.2; they replace Appendix A §2.6 where the two differ):
- **Liveness instants:** the arrival of a fresh LIVE or HB frame, and the forwarding of a verb.
- **A verb**, in this order:
  1. While verbActive, refuse with `action_invalid`.
  2. If it is not fresh, refuse with `action_invalid`.
  3. D3 (Task 7).
  4. Otherwise forward `m.Canonical`, set verbActive, clear `tripped` and stamp liveness.
- **A heartbeat** is forwarded only if it is fresh and not `tripped`.
- **`cmd_abort`** is always forwarded as `AbortJSON` and clears verbActive. `device_location`, `info` and keepalive are
  forwarded.
- **Trip:** in `Tick`, if verbActive and BootMS − the last liveness instant > 300 ms:
  - `Car.Send(AbortJSON)`;
  - `swallow++`;
  - verbActive = false;
  - `tripped` = true, which means no HB is forwarded until the next forwarded verb.

  The session continues.
- **Car frames:**
  - Every `autopark:cmd_result` raises `highestTS` to its `timestamp`.
  - One whose `cmd_type` is `autopark:cmd_abort` while `swallow > 0` is dropped, and `swallow--`.
  - A car refusal (`result: false`) does NOT clear verbActive (design override).
  - Forwarding: `Phone.Car(channel, data)`, with `dcTcp` as 0 and `dcUdp` as 1. Another channel is dropped and
    logged. A `false` return ends the session.
- **A refusal** is `Phone.Car(ChanTCP, json)`, with the exact keys in Global Constraints, and timestamp =
  max(`Now().UnixMilli()`, `highestTS`).
- **The link flag** (design §6.2; part of D2) comes from LIVE tick ages.
  - `LinkSetRun` LIVE frames in a row whose age exceeds `LinkSetAge` set it. A LIVE whose tick is unknown
    (`ok == false`) counts as over.
  - It clears when a hold ends (the engine forwards or injects `cmd_abort`), or after `LinkClearFor` of LIVE ages at or
    under `LinkClearAge`. An age above `LinkClearAge` restarts that period.
  - `Phone.SetLink` is called only when the flag changes. `Stop` clears it.
- **A `Car.Send` error** on any path ends the session through `Phone.End`.
- **H4 (`Stop`):**
  1. If verbActive, `Car.Send(AbortJSON)`.
  2. `Car.Disconnect()`, exactly once. It returns nil only after Link's confirmation (Task 4).
  3. `Marker.Clear()`, but only if the abort (when one was sent) and `Disconnect` both returned nil.

  Once stopped, every phone and car input is ignored.
- **Panics.** `Stop` and `Tick` recover a panic and pass it to `Fatal`. The core would otherwise recover it in
  `Teardown`, and a half-done H4 would go unnoticed.

- [ ] **Step 1: Write the failing tests**

`engine_test.go` uses fake `Car`, `Phone` and `Marker` that record every call, with `clock.NewFake()` and a fixed
`Now`.
- Its setup helper feeds one `vehicle_data:location` car frame right after `Start`: the car at 52.52, 13.405, gear P,
  speed 0.
- Every verb is placed 10 m from the car.

So these tests stay valid once Task 7 adds the 3 m floor, and Task 7 must leave them unchanged and passing.
- `TestStartSetsMarkerThenConnects`: order Marker.Set, then Connect.
- `TestStartRefusedWhenMarkerFails`: no Connect.
- `TestVerbForwardedWhenFresh`: `Car.Send` gets the canonical bytes, and `VerbActive()` is true.
- `TestStaleVerbRefused`: age 151 ms, and separately `ok == false`, each give no Send and one refusal with reason
  `action_invalid` and `cmd_type` = the verb's `msg_type`. Age 150 ms is forwarded.
- `TestVerbWhileActiveRefused`.
- `TestTripAfter300ms`:
  - after a verb, advance 300 ms and `Tick`: no abort;
  - advance 1 ms and `Tick`: `Car.Send(AbortJSON)` exactly once, and `VerbActive()` is false.
- `TestFreshLiveKeepsAlive`: a fresh LIVE every 50 ms for 5 s gives no trip.
- `TestStaleLiveDoesNotKeepAlive`: LIVE with age 160 ms every 50 ms trips at 301 ms. So does `Live(0, false)` (an
  expired tick).
- `TestTripSwallowsItsAbortResult`: after a trip, the car's `cmd_result` for `cmd_abort` is not passed to `Phone.Car`.
- `TestSwallowOnlyInjectedAbortResult`: a trip, then the phone's `cmd_abort`, then two abort results from the car
  (`result: true`, then `already_set`). Exactly the second result reaches `Phone.Car`, and the phone's abort reached
  `Car.Send`.
- `TestNoHeartbeatAfterTripUntilNextVerb`.
- `TestStaleHeartbeatNotForwarded`: age 151 ms, and `Heartbeat(m, 0, false, id)`. Neither is forwarded, and neither
  stamps liveness.
- `TestCarSendErrorEndsSession`: a `Car.Send` that returns an error calls `Phone.End`.
- `TestStopPanicCallsFatal`: a `Car.Disconnect` that panics calls `Fatal`, and the marker stays.
- `TestHeartbeatBeforeAnyVerbForwarded`.
- `TestAbortAlwaysForwardedCanonical`.
- `TestCarRefusalKeepsVerbActive`: a car `cmd_result` with `result: false` leaves `VerbActive()` true, and a trip
  still fires 301 ms after the last liveness instant.
- `TestRefusalShape`: the bytes decode to exactly the five keys.
- `TestRefusalTimestampNeverGoesBack`:
  - with `Now` = 1000 ms and a forwarded car `cmd_result` timestamp of 5000, a refusal has `timestamp` 5000;
  - with `Now` = 9000, it has 9000.
- `TestStopRunsH4Once`:
  - verbActive gives abort, then Disconnect, then Marker.Clear;
  - a second `Stop` does nothing;
  - inputs after `Stop` reach neither side.
- `TestStopWithoutVerbOnlyDisconnects`.
- `TestStopKeepsMarkerWhenDisconnectFails`.
- `TestPhoneEndCalledOutsideLock`. The fake `Phone.End` calls `VerbActive()`. That deadlocks, and fails the test by
  timeout, if `End` is called under the lock.
- `TestLinkFlag`:
  - three LIVE ages of 110 ms in a row call `SetLink(true)` once;
  - 110, 50, 110 do not;
  - an unknown tick counts as over;
  - after the phone's abort it is `SetLink(false)`;
  - 2 s of ages ≤ 75 ms clears it, and one age of 90 ms restarts that 2 s;
  - `Stop` clears it.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/summon/ -run 'Test(Start|Verb|Stale|Trip|Fresh|Swallow|NoHeartbeat|Heartbeat|Abort|CarRefusal|CarSend|Refusal|Stop|PhoneEnd|LinkFlag)' -count=1`
Expected: FAIL (`NewEngine` is undefined).

- [ ] **Step 3: Implement `Engine` in `internal/summon/engine.go`**

One mutex guards the engine. `Car.Send` is called under it; Link's 100 ms deadline bounds that. `Phone.End` is
collected under the lock and called after unlocking.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./internal/summon/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/summon/engine.go internal/summon/engine_test.go
git commit -m "feat(summon): the D2 deadman, the link flag, refusals and H4 in a pure engine"
```

---

### Task 7: The 3 m floor (D3)

**Files:**
- Modify: `internal/summon/engine.go`
- Create: `internal/summon/geo.go`, `internal/summon/d3_test.go`

**Interfaces:**
- Consumes: `guards.DistanceM` (Task 1); `Message.Lat/Lon/GoalLat/GoalLon/HasGoal` (Task 3).
- Produces:
  - `const FloorM = 3.0`;
  - `const StaleAfter = time.Second`;
  - `func goalOf(m Message) (lat, lon float64)`, which is the goal if `HasGoal`, else the phone position. That is the
    car's own rule (Appendix A §3.5).

**Rules** (design §5.3, and call 2):
- **The car position** is the latest `vehicle_data:location` the engine forwarded: latitude, longitude, speed and
  `shift_state`, with `lastLocAt`. `lastCarAt` is the arrival of any car frame.
- **Staleness, only while verbActive.** `verbAt` is when the current verb was forwarded. In `Tick`, the position
  goes stale when either holds:
  - BootMS − max(`lastCarAt`, `verbAt`) > 1 s;
  - the last location showed speed > 0 or `shift_state` `D` or `R`, and BootMS − max(`lastLocAt`, `verbAt`) > 1 s.

  Silence before a press never counts: this car sends nothing between holds. A stale position stays stale until a
  newer location frame arrives.
- Task 6's tests stay unchanged and passing (they seed a position and press from 10 m).
- **At each verb**, after the D2 checks:
  - no position, or a stale one, refuses with `invalid_vehicle_location`;
  - else, if `DistanceM` from the car to the phone (`Lat`/`Lon`), or from the car to `goalOf(m)`, is < 3.0, the verb
    is refused with `location_params_invalid`.
- **`device_location`** is never checked.

- [ ] **Step 1: Write the failing tests**

`d3_test.go` places the car at 52.52, 13.405 and builds positions by bearing and distance with a helper.
- `TestFloorPhoneWithin3m`, for each of `cmd_find_me`, `cmd_forward` and `cmd_reverse`:
  - 2.99 m is refused with `location_params_invalid`;
  - 3.01 m is forwarded.
- `TestFloorGoalWithin3m`: `cmd_find_me` with `pin_drop`, the phone at 20 m and the goal at 2 m, is refused.
- `TestFloorUsesPhoneWhenNoGoal`: `cmd_forward`.
- `TestNoCarPositionRefused`: `invalid_vehicle_location`.
- `TestStaleWhileActiveNoFrames`:
  - while verbActive, no car frame for 1001 ms makes it stale;
  - after the abort, a re-press is refused with `invalid_vehicle_location`;
  - a new location frame makes the next press go through.
- `TestStaleAfterMotionNoLocation`: a location with speed 2, then heartbeat echoes every 100 ms but no location for
  1001 ms, makes it stale.
- `TestNotStaleOutsideRun`: with no verb active, 60 s with no car frame keeps the position valid.
- `TestSilenceBeforePressDoesNotCount`:
  - 60 s with no car frame, then a forwarded verb: the position is not stale within 1 s of the verb;
  - a release, then a re-press 500 ms later, is forwarded.
- `TestRepressAfterCarRefusal`: a verb, then the car's `result: false` 50 ms later, then the phone's abort, then a
  re-press 2 s later with no location frame in between. The re-press is forwarded.
- `TestStandingInDriveDuringRunGoesStale`: this pins call 2's remaining case. A hold begins, a location with gear D
  and speed 0 arrives after the press, then echoes for 1.5 s. The position goes stale, and a re-press after the
  release is refused.
- `TestDeviceLocationWithin3mForwarded`.
- `TestHaversine`: (0,0) to (0,1) is 111194.9 m ± 0.5. The same point gives 0.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/summon/ -run 'Test(Floor|NoCar|Stale|NotStale|Silence|RepressAfter|Standing|DeviceLocation|Haversine)' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement the position tracking, staleness and floor in `engine.go`, and `goalOf` in `geo.go`**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./internal/summon/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/summon/engine.go internal/summon/geo.go internal/summon/d3_test.go
git commit -m "feat(summon): the 3 m floor at each press, with stale-position refusals"
```

---

### Task 8: The inner SUMMON capability

**Files:**
- Create: `internal/summon/frames.go`, `internal/summon/capability.go`, `internal/summon/capability_test.go`

**Interfaces:**
- Consumes: `capability.Capability/Window/Handler/Spec`; `testkit.FakeWindow` (`IssueTick`, `SetTickAge`,
  `FailTrySend`, `Sent`, `CloseReason`); `carlink.CarFrame`; `Engine` (Tasks 6 and 7); `Parse` (Task 3).
- Produces:

```go
package summon

const (
	TypeLive byte = 0x14
	TypeVerb byte = 0x15
	TypeHB   byte = 0x16
	TypeMsg  byte = 0x17
	TypeCar  byte = 0x91

	LoopEvery   = 10 * time.Millisecond
	MaxProgress = 250 * time.Millisecond
	CarQueue    = 1024
	MaxCarJSON  = 16383
)

func InnerSpec() capability.Spec // exactly the inner Spec in Global Constraints

type CapConfig struct {
	Car     Car
	Marker  Marker
	Clock   clock.Clock
	Now     func() time.Time
	Logf    func(format string, args ...any)
	Healthy func() bool      // the link is owned and healthy; checked by PreArm
	SetLink func(bool)       // writes a LINK record (the helper's relay listener)
	Fatal   func(err error)  // passed to each Engine
}

func NewCapability(cfg CapConfig) *Capability // implements capability.Capability
func (c *Capability) CarFrame(f carlink.CarFrame)         // non-blocking; for carlink.Handlers
func (c *Capability) Signaling(body map[string]any)       // type 9 ends the current session
func (c *Capability) End(reason string)                   // closes the current window, if any (NameLost, unhealthy)
func (c *Capability) Reply() int32                        // 1 while a session is up, else 0
func (c *Capability) Loop(ctx context.Context)            // the D2 loop: stamps progress, then Tick
func (c *Capability) Progress() time.Duration             // time since the last stamp
```

**Rules:**
- `PreArm` refuses unless `Healthy()`.
- **`Open`** builds one `Engine` per window.
  - Under the capability's lock it installs that engine as current, so `Reply` returns 1, before `Start` emits H1. The
    car answers H1 at once with its style, status and location, and those must find the engine.
  - A failed `Start` uninstalls it and returns an error, so the core closes the connection silently.
- **Each car frame is bound to the engine current when it was enqueued** (read under the same lock). The forwarder
  hands it to that engine, whose input is ignored once stopped. Frames enqueued with no engine installed are dropped.
- **`Frame` decodes:**
  - LIVE must be exactly 8 bytes;
  - VERB and HB must be at least 12;
  - each LIVE, VERB and HB passes `w.TickAge(id)`'s `(age, ok)` to the engine, which decides freshness;
  - JSON goes through `Parse` with the frame's `Kind`: `ErrFatal` returns an error (BYE "error"), and `ErrDrop` is
    logged by type and skipped.
- `Teardown` calls `Stop`.
- **Car frames.** `CarFrame` enqueues into a channel with capacity `CarQueue`. If the queue is full, it sets an
  overflow flag, and the forwarder ends the current session.
  - One forwarder goroutine drains the queue in order into the current engine.
  - The engine's `Phone.Car` is `w.TrySend(TypeCar, ch‖data)`. A `false` ends the session.
  - JSON over `MaxCarJSON` is dropped and logged by `msg_type` and size.
  - **The caller is logged (design §5.5).** For each session, the capability logs the `Sender` of the first car frame
    with its `UID`, then any later frame whose `Sender` differs. It accepts frames from every caller for now:
    narrowing to QtCarServer's connection waits for check 4.
- `Signaling` with an integer `type` equal to 9 closes the current window with reason `"close"`.
- The engine's `Phone.SetLink` calls `CapConfig.SetLink`. The engine computes the flag (Task 6).
- **`Loop`** runs every `LoopEvery`. It stores `Clock.BootMS()` in an atomic before it calls the current engine's
  `Tick`, which takes the lock.

- [ ] **Step 1: Write the failing tests**

`capability_test.go` uses `testkit.FakeWindow`, a recording fake `Car` and `clock.NewFake()`. Like Task 6, each session
is seeded with a car location, and verbs come from 10 m.
- `TestInnerSpec`: `InnerSpec()` equals the exact Spec.
- `TestH1PushReachesWindow`, run with `-count=50`. The fake `Car.Connect` synchronously calls `Capability.CarFrame`
  with the style, status and location. All three reach the window, in order.
- `TestEndClosesCurrentWindow`: `End("error")` closes the window, and the fake car records the H4 disconnect.
- `TestRealClockTrip`. `Loop` runs on the real clock (`clock.Boot()`). After the last fresh LIVE, the injected abort
  reaches the fake car between 300 and 350 ms later.
- `TestPreArmRefusesWhenUnhealthy`.
- `TestOpenEmitsH1Once`: Marker.Set, then Connect, then nothing more on a second `Frame`.
- `TestTickAgePassedThrough`, using `FakeWindow.SetTickAge`:
  - a VERB whose tick is unknown is refused, age 150 ms is forwarded, and age 151 ms is refused;
  - a LIVE or HB whose tick is unknown, or 151 ms old, stamps no liveness: the trip still fires 301 ms after the last
    fresh frame;
  - such an HB is not forwarded.
- `TestFrameDecodeErrors`:
  - a 7-byte LIVE returns an error;
  - an 11-byte VERB returns an error;
  - an invalid-JSON MSG returns nil and sends nothing.
- `TestTeardownRunsH4`.
- `TestCarChannelEncoding`: `dcTcp` becomes 0‖data and `dcUdp` becomes 1‖data. `"x"` is dropped.
- `TestOversizeCarFrameDropped`: 16384 bytes of JSON send nothing. The log line contains the `msg_type` and the size,
  and no field values.
- `TestCarFramesWhilePhoneStalled`. `FailTrySend(true)`, then 100 `CarFrame` calls from the test goroutine:
  - each call returns within 1 ms;
  - the window closes with reason "error";
  - the fake car records the H4 disconnect.
- `TestFramesBetweenSessionsDropped`, deterministic:
  - queue a `cmd_result` after `Teardown`, then call `Open` at once. The frame never reaches the second window;
  - the second session refuses a stale verb with `timestamp` = `Now`, not the old highest value.
- `TestQueueOverflowEndsSession`. It wraps `*testkit.FakeWindow` in a test type whose `TrySend` waits on a gate
  channel. Push `CarQueue`+2 frames:
  - each `CarFrame` returns within 1 ms;
  - after the gate opens, the window closes with "error", and the fake car records the H4 disconnect.
- `TestType9ClosesSession`.
- `TestLinkWired`: three LIVE frames with ages of 110 ms call `CapConfig.SetLink(true)`.
- `TestLogsCarFrameCaller`:
  - the first car frame of a session logs its sender and uid once;
  - a frame from a second sender logs that sender;
  - the log never contains the frame's data.
- `TestLoopStampsBeforeLock`. A `Car.Send` that blocks forever is hit by a trip, so the engine holds its lock.
  `Progress()` passes 250 ms within 400 ms.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/summon/ -run 'Test(Inner|PreArm|Open|H1Push|End|RealClock|TickAge|Frame|Teardown|Car|Oversize|Frames|Queue|Type9|LinkWired|Logs|Loop)' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement `frames.go` and `capability.go`**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./internal/summon/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/summon/frames.go internal/summon/capability.go internal/summon/capability_test.go
git commit -m "feat(summon): the inner SUMMON capability: frames, ordered car frames, caller logging and the D2 loop"
```

---

### Task 9: Peer credentials, relay records and the helper's relay listener

**Files:**
- Create:
  - `internal/peercred/peercred_linux.go`, `peercred_darwin.go`, `peercred_other.go`, `peercred_test.go`, copied from
    `internal/camhelper/sys_linux.go` and `sys_darwin.go`, plus a fallback for other systems that returns an error;
  - `internal/summon/relay/record.go`, `listener.go`, `record_test.go`, `listener_test.go`.

**Interfaces:**
- Produces:

```go
package peercred
func PeerUID(c net.Conn) (int, error)

package relay

const (
	RecInner  byte = 0x01
	RecLink   byte = 0x02
	RecOwned  byte = 0x03
	MaxInner       = 16384
	FirstWait      = 10 * time.Second
)

type Writer struct{ /* serializes records on one conn */ }
func NewWriter(w io.Writer) *Writer
func (w *Writer) Inner(p []byte) error // splits p into records of at most MaxInner
func (w *Writer) Link(degraded bool) error
func (w *Writer) Owned() error

type Reader struct{ /* ... */ }
func NewReader(r io.Reader, expectOwned bool) *Reader // agw side: expectOwned = true
func (r *Reader) Next() (typ byte, payload []byte, err error)

type ListenConfig struct {
	Path      string
	AllowUID  int
	PeerUID   func(net.Conn) (int, error) // default peercred.PeerUID
	Ready     func() bool                 // owned and healthy: required before OWNED
	FirstWait time.Duration
	Logf      func(format string, args ...any)
}

func Listen(cfg ListenConfig) (*Listener, error) // creates Path, 0660, as the current uid
func (l *Listener) Accept() (net.Conn, error)    // net.Listener
func (l *Listener) Close() error
func (l *Listener) Addr() net.Addr
func (l *Listener) SetLink(degraded bool)        // a LINK record on the active connection, if any
```

**Listener rules:**
- Its own accept goroutine handles one relay connection at a time.
  - A peer uid other than `AllowUID` is closed with no record and logged.
  - The next connection is not accepted until the active one has been closed by the core. The core closes it after
    `Teardown`, so after H4. Until then it waits in the kernel backlog.
- **Before OWNED:**
  - if `Ready()` is false, the connection is closed with no record;
  - otherwise the listener writes OWNED, reads the first INNER record (or closes after `FirstWait`), and only then
    returns the connection from `Accept`.
- **The returned `net.Conn`:**
  - `Read` yields INNER payloads as one byte stream, starting with the buffered first one. Any other record type from
    agw closes it.
  - `Write` sends INNER records.
  - The deadline methods delegate to the unix connection.
- **The socket:** `os.Remove(Path)` before `net.Listen("unix")`, then mode 0660. The directory's setgid bit gives it
  group 4950 on the car.

- [ ] **Step 1: Write the failing tests**

- `record_test.go`:
  - `TestRecordRoundTrip` (all three types);
  - `TestInnerSplitsAtMax` (40000 bytes become 3 records: 16384, 16384, 7232);
  - `TestReaderRejects`, each closing with an error: a zero-length INNER; length 16385; type 0x04; a LINK payload of
    2; OWNED twice; INNER before OWNED when `expectOwned`.
- `listener_test.go` (a unix socket under `os.MkdirTemp("", "rl")`):
  - `TestSocketMode0660`.
  - `TestPeerUIDRefused`: `PeerUID` returns `AllowUID+1`; the client reads EOF with no record.
  - `TestNotReadyNoOwned`.
  - `TestOwnedThenHandoffOnFirstInner`: `Accept` blocks until the first INNER arrives, and the conn's `Read` returns
    that payload.
  - `TestFirstWaitCloses`: `FirstWait` 100 ms, no INNER, so it closes.
  - `TestOneAtATime`: a second client gets OWNED only after the first returned conn is closed.
  - `TestSetLinkWritesRecord`.
- `peercred_test.go`: `TestPeerUIDSelf`, `PeerUID` over a socketpair equals `os.Getuid()`.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/peercred/ ./internal/summon/relay/ -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./internal/peercred/ ./internal/summon/relay/ && GOOS=linux GOARCH=amd64 go vet ./internal/peercred/ ./internal/summon/relay/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/peercred internal/summon/relay
git commit -m "feat(summon/relay): relay records, peer credentials and the helper's one-at-a-time relay listener"
```

---

### Task 10: The helper process (`serve`, `keygen`, `pin`)

**Files:**
- Create:
  - `internal/summon/logq/logq.go`, `logq_test.go`;
  - `internal/summon/helper/helper.go`, `marker.go`, `dumpable_linux.go`, `dumpable_other.go`, `helper_test.go`;
  - `cmd/summonhelper/main.go`, `cmd/summonhelper/main_test.go`.

**Interfaces:**
- Consumes:
  - `server.New`, `server.Config`, `(*Server).Serve`, `Revoke`, `Shutdown`;
  - `keys.Load`, `keys.Generate`, `keys.PinString`, `(*keys.Allowlist).Load/Watch`;
  - `authority.Oracle`, `authority.NewDBus`, `authority.DefaultAuthdSHA256`;
  - `carlink.Link`, `carlink.Bus`;
  - `summon.NewCapability`;
  - `relay.Listen`;
  - `supervise.StartLiveness`.
- Produces:

```go
package helper

type Config struct {
	Bus           carlink.Bus
	Oracle        authority.Oracle
	Key           *ecdsa.PrivateKey
	VIN           string
	AllowPath     string // /var/lib/airgapp-gw/allow
	Socket        string // /run/airgapp-summon/relay.sock
	AllowUID      int    // 4950
	PeerUID       func(net.Conn) (int, error)
	MarkerPath    string // /run/airgapp-summon/session
	LogPath       string // /var/lib/airgapp-summon/state/log
	Clock         clock.Clock
	Now           func() time.Time
	Logf          func(format string, args ...any)
	Exit          func(code int)    // carlink.WithExit
	Liveness      bool              // start supervise.StartLiveness; false only in tests
	OnListening   func()            // test hook
}

func Serve(ctx context.Context, cfg Config) error
func FileMarker(path string) summon.Marker // creates or removes path; Clear on a missing file succeeds
// Health is the liveness and Ready predicate: the link is healthy and the D2 loop stamped within MaxProgress.
func Health(link *carlink.Link, c *summon.Capability) func() bool
// ValidVIN accepts exactly 17 characters of [A-HJ-NPR-Z0-9].
func ValidVIN(v string) bool

package logq

// Logger never blocks: a queue of n lines drained by one goroutine into w; when full, lines are dropped and counted.
func New(w io.Writer, n int) *Logger
func (l *Logger) Logf(format string, args ...any)
func (l *Logger) Dropped() uint64
```

**`Serve` order:**
1. Disable dumpability on Linux: `prctl(PR_SET_DUMPABLE, 0)`.
2. `Link.Acquire()`. `ErrNameTaken` returns an error, and the process exits 1.
3. Build the capability, with `SetLink` bound late to the listener created in step 10. `Fatal` logs (non-blocking)
   and calls `Exit(3)`. The parent's takeover then sends the abort.
4. `Bus.Serve` with handlers wired to the capability (`CarFrame`, `Signaling`, `Reply`).
5. `Link.OnUnhealthy(func() { cap.End("error") })`. NameLost or a failed emit ends the open session at once. Its H4
   then fails the confirmation, so the marker stays for the takeover.
6. Warm the oracle: `AuthOwnerIsAuthd` with a 3 s context. A failure is logged and is not fatal; sign-ins fail closed.
7. Allowlist `Load`.
8. `server.New(Config{Key, VIN, Allow, Oracle, Clock, Caps: [cap], LogPath, MaxLogBytes: 1 << 20})`, then
   `go Watch(ctx, path, srv.Revoke)`.
9. Seed the progress stamp, then `go cap.Loop(ctx)`.
10. `relay.Listen`, with `Ready = Health(link, cap)`.
11. Liveness: `supervise.StartLiveness(Health(link, cap), onFail)`, where `onFail` is `srv.Shutdown("supervisor-lost")`
    followed by `Exit(1)`.
12. `srv.Serve(listener)`.

On ctx end: `srv.Shutdown("sigterm")`, which runs H4 through `Teardown`; then `Link.Release()`.

**`cmd/summonhelper`:**
- `serve`:
  - flags `-socket`, `-allow`, `-key`, `-log`, `-marker`, `-authd-sha256` (default `authority.DefaultAuthdSHA256`)
    and `-allow-uid` (default 4950);
  - the VIN from env `SUMMONHELPER_VIN`, which must pass `ValidVIN`. A refusal is logged without the value;
  - it refuses to run unless `supervise.LivenessEnv` is set;
  - every log line goes through one `logq.Logger` (1024 lines) on stderr.
- `keygen -key`.
- `pin [-key] [-vin | -vin-file /var/etc/vin]`: prints `keys.PinString` and nothing else.
- Defaults: the key is `/var/lib/airgapp-summon/state/key.pem`; the log is `/var/lib/airgapp-summon/state/log`.

- [ ] **Step 1: Write the failing tests**

`helper_test.go` runs `Serve` in-process with `carlink.FakeBus` plus `mockcar` (`FakePort`), and
`authority.Fake{Role: 2, Owner: true}`. `AllowUID` is `os.Getuid()`, the socket is under `os.MkdirTemp("", "sh")`,
and the phone is a Go client that plays agw's side of the relay socket. It uses `relay.NewWriter` and `NewReader`
(with `expectOwned` true) on the unix socket, pumped into a `net.Pipe`, and runs `client.Handshake` (Task 2) over the
pipe's other end.
- `TestServeRefusesWhenNameTaken`: `FakeBus.TakeName` first gives an error, and no socket is created.
- `TestOracleWarmedBeforeListening`: the fake oracle's call count is at least 1 when `OnListening` fires.
- `TestSignInAndSessionUp`: the mock car receives `data_channels_ready`, then the phone receives CAR frames for the
  style, status and location.
- `TestCoreClockStartsAtFirstInner`: connect, read OWNED, wait 2.5 s, then start the inner hello. Sign-in succeeds.
- `TestRearmWaitsForTeardown`:
  - session A arms and sends a verb (verbActive), then its relay conn closes;
  - connection B dials at once;
  - the mock car's received order is abort, disconnect, then B's `data_channels_ready`;
  - B's OWNED arrives after A's disconnect.
- `TestAllowlistRevocationEndsSession`:
  - rewriting the allowlist file without the phone key gives the inner BYE `revoked` within 1.5 s;
  - the mock car gets abort (verbActive), then disconnect, exactly once;
  - the marker file is gone.
- `TestNameLostStopsServing`: `FakeBus.LoseName` while verbActive gives all of these:
  - the window closes;
  - the marker file still exists;
  - the next relay connection gets no OWNED.
- `TestHealth`: false when `Progress()` exceeds 250 ms, after an emit overrun, and after NameLost. True otherwise.
- `logq_test.go`:
  - `TestLogfNeverBlocks`: with a writer that blocks forever, 10000 `Logf` calls return within 50 ms, and
    `Dropped()` counts the excess;
  - `TestLogqWritesInOrder`.
- `cmd/summonhelper/main_test.go`:
  - `TestKeygenAndPin`: with `-vin 5YJ3E1EA7KF000000`, the pin matches `^airgapp-gw:1:[0-9a-f]{130}:5YJ3E1EA7KF000000$`,
    and key.pem has mode 0400;
  - `TestServeRefusesWithoutLiveness`;
  - `TestServeRefusesBadVIN`: empty, 16 characters, and a value containing `I` are each refused, and the log does not
    contain the value.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/summon/logq/ ./internal/summon/helper/ ./cmd/summonhelper/ -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement `logq`, `helper.go`, `marker.go`, the dumpable files and `cmd/summonhelper/main.go` (`serve`, `keygen`, `pin`)**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./internal/summon/... ./cmd/summonhelper/ && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/summonhelper`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/summon/logq internal/summon/helper cmd/summonhelper
git commit -m "feat(summonhelper): serve the inner SUMMON core on the relay socket, with sign-in, revocation and the session marker"
```

---

### Task 11: The supervisor, its takeover, and the finish hook

**Files:**
- Modify `internal/supervise/supervise.go` and `supervise_test.go`:
  - add an exported `Options.OnFirstBeat func()`. `Run` calls it once, on its own goroutine, after the child's first
    liveness byte, never on the liveness reader;
  - make `Run` call `finish` before any log line that follows a kill;
  - rewrite the package comment, which still names `agw summon-finish`, for the summonhelper takeover and finish.
- Create: `internal/summon/helper/takeover.go`, `takeover_test.go`.
- Modify: `cmd/summonhelper/main.go` (`run`, `finish`), `cmd/summonhelper/main_test.go`.

**Interfaces:**
- Consumes: `supervise.Run`, `supervise.Options`, `supervise.Exit`; `carlink.DialBus/SystemBus`, `Link`, `Bus`
  (Task 4); `bustest` (Task 4); `mockcar.DBusPort` (Task 5).
- Produces:

```go
package helper

const (
	TakeoverWait = time.Second
	FinishRetry  = 20 * time.Millisecond
	FinishFor    = time.Second
	StartupGrace = 5 * time.Second
)

// Supervise starts serveArgv as the child, queues for the name after its first beat, and on the child's death takes
// the name and sends abort + disconnect if the marker exists.
func Supervise(ctx context.Context, bus carlink.Bus, serveArgv []string, markerPath string,
	logf func(format string, args ...any)) error

// Finish is the runit finish hook. With a marker it acquires the name (DO_NOT_QUEUE, retried every FinishRetry for
// FinishFor), sends abort + disconnect, releases the name and removes the marker. Without a marker it does nothing.
func Finish(bus carlink.Bus, markerPath string, logf func(format string, args ...any)) error

// MarkerPresent is the fail-safe marker check: true when Lstat succeeds, or fails with anything but ENOENT.
func MarkerPresent(path string) bool
```

**Rules:**
- **The marker check is fail-safe everywhere** (`run`, the takeover, `Finish`). The marker counts as present when
  `Lstat` succeeds, and also when `Lstat` fails with anything other than ENOENT.
- **`run`, before starting the child:** if the marker is present, run `Finish` with its retries. If `Finish` fails,
  exit non-zero without starting the child: a stale session is never left unaborted while a new one serves.
- **The parent** serves the two methods with `Reply` 0. It passes the child `StartupGrace` 5 s. On `OnFirstBeat` it
  calls `bus.RequestName(WebrtcName, true)`.
  - The expected reply is InQueue (`owned == false`, no error).
  - Any other result gets the child SIGKILLed and `run` exits non-zero: an error, or getting the name (then the child
    does not own it; release it first). No session ever runs without a queued parent.
- **Ownership is state, not an event.** From the queue request on, `OnNameAcquired` records that the parent holds the
  name. The child's bus connection can drop before the child dies, so the name can arrive first.
- **After the child dies** (killed for silence, killed by SIGTERM, or exited), on timers of its own
  (`context.WithoutCancel`), so a SIGTERM cannot cut it short:
  1. If the name is not held yet, wait up to `TakeoverWait` for it. If it never arrives, keep the marker and exit
     non-zero, so runit's `finish` retries.
  2. If the marker is present: `Link.Send(AbortJSON)`, then always `Link.Disconnect()` (confirmed). Remove the
     marker only if both succeeded; otherwise keep it and exit non-zero.
  3. Release the name. `run` exits non-zero, so runsv runs `finish` and restarts the service.
- **No log line before the emits.** `Supervise` and `Finish` send the abort and disconnect first, then log.
- **`run`** reads `SUMMONHELPER_VIN` and passes the environment through. Its child argv is `<self> serve <flags>`.
- **`finish`** runs `Finish` on `SystemBus`. It exits 0 when there is no marker or both emits succeeded, and 1
  otherwise, keeping the marker.

- [ ] **Step 1: Write the failing tests**

`takeover_test.go` uses bustest. The mock car is `mockcar.DBusPort`. The test binary is re-exec'd in two modes:
- `SUMMONHELPER_TEST_CHILD`: owns the name on the private bus, writes the marker, emits H1 (so the mock car counts
  as connected) and writes liveness. Its liveness `onFail` (EPIPE) exits. Then it idles.
- `SUMMONHELPER_TEST_PARENT`: runs `Supervise` over the child mode, so a test can stop or kill "the parent" without
  stopping itself.

The tests:
- `TestTakeoverAfterChildSIGKILL`: the marker exists, and the child gets SIGKILL. Then:
  - the mock car receives exactly abort, then disconnect, within 400 ms of the kill;
  - the marker is gone, and the name has no owner afterwards;
  - `Supervise` returns `*supervise.Exit`.

  It runs again with a `logf` that blocks forever, and gets the same result.
- `TestTakeoverAfterChildSIGSTOP`: the same outcome, through the 100 ms liveness timeout, within 500 ms.
- `TestTakeoverChildBusDropsFirst`: the child's bus connection closes while the child still runs. Abort and
  disconnect arrive within 400 ms.
- `TestTakeoverOnSIGTERM`: SIGTERM to the parent mid-session still gives abort and disconnect.
- `TestTakeoverNoMarkerSendsNothing`.
- `TestTakeoverWithoutNameKeepsMarker`: another connection holds the name when the child dies. Nothing is emitted,
  the marker is kept, `Supervise` returns an error, and `Finish` succeeds once the name is free.
- `TestTakeoverEmitFailureKeepsMarker`: a confirmation that fails keeps the marker.
- `TestParentQueuesAfterFirstBeat`: the parent's RequestName reply is InQueue.
- `TestQueueFailureKillsChild`: a RequestName error gets the child SIGKILLed.
- `TestRunFinishesStaleMarkerFirst`: with a marker present when `run` starts, the mock car receives abort, then
  disconnect, then the child's `data_channels_ready`.
- `TestFinish`:
  - a marker and a free name give abort and disconnect, and the marker is removed;
  - no marker emits nothing;
  - an unreadable marker counts as present;
  - a name held by another connection for 2 s gives an error, and the marker is kept.
- `TestSupervisorSIGSTOPChildKeepsServing`: with the parent process stopped, the child keeps answering car calls,
  and no abort is sent.
- `TestSupervisorKilledThenFinish`: SIGKILL the parent process. Then `Finish` sends abort and disconnect.
  - On Linux, assert that the child dies (Pdeathsig).
  - On darwin, assert that the healthy child exits by itself through EPIPE within 150 ms.
- `TestWedgedEngineIsKilledAndAborted`, the deadman chain in real time:
  - the child runs the real `helper.Serve` on bustest under `Supervise`, with liveness on;
  - the test acts as the phone, as in Task 10, and holds a verb;
  - on SIGUSR1, the child's test-only `Car` wrapper makes the next `Send` block forever, without calling `Link`, so the
    engine lock stays held;
  - the mock car must receive abort, then disconnect, within 600 ms of the signal.

`internal/supervise/supervise_test.go`:
- `TestOnFirstBeatCalledOnce`.
- `TestSlowOnFirstBeatKeepsChild`: an `OnFirstBeat` that takes 200 ms does not get a healthy child killed.
- `TestFinishRunsBeforeLogs`: with a `Logf` that blocks forever, `finish` still runs after a kill.

`cmd/summonhelper/main_test.go`: `TestRunRequiresVIN`.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/summon/helper/ ./internal/supervise/ ./cmd/summonhelper/ -run 'Test(Takeover|Parent|Queue|RunFinishes|Finish|Supervisor|Wedged|OnFirstBeat|SlowOnFirstBeat|FinishRunsBeforeLogs|RunRequiresVIN)' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement `OnFirstBeat`, `takeover.go`, and the `run` and `finish` commands**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./internal/supervise/ ./internal/summon/helper/ ./cmd/summonhelper/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/supervise internal/summon/helper cmd/summonhelper
git commit -m "feat(summonhelper): supervisor takeover of the bus name and the runit finish hook"
```

---

### Task 12: agw's SUMMON relay, `probe-route` and `probe-summon`

**Files:**
- Create: `capability/summonrelay/relay.go`, `capability/summonrelay/relay_test.go`
- Modify: `cmd/agw/main.go` (a capTable row; a hook; two commands), `cmd/agw/main_test.go`

**Interfaces:**
- Consumes: `relay.Reader/Writer`, `relay.RecInner/RecLink/RecOwned` (Task 9); `peercred.PeerUID` (Task 9);
  `guards.RouteOff` (Task 1); `capability.Host.SetSummonLinkDegraded`.
- Produces:

```go
package summonrelay

const (
	TypeInner     byte = 0x10 // phone → agw
	TypeInnerOut  byte = 0x90 // agw → phone
	DefaultSocket      = "/run/airgapp-summon/relay.sock"
	HelperUID          = 1337
	OwnedWait          = 500 * time.Millisecond
	QueueRecords       = 64
	WriteTimeout       = time.Second
)

type Config struct {
	Socket     string
	HelperUID  int
	PeerUID    func(net.Conn) (int, error)
	RouteCheck func(ctx context.Context) error // default guards.RouteOff(ctx, guards.DefaultTailscaleSock)
	SocketUID  func(path string) (int, error)  // default: Lstat's Stat_t.Uid
}

func New() capability.Capability
func NewWithConfig(cfg Config) capability.Capability

// in cmd/agw/main.go
var newSummonRelay = summonrelay.New // tests swap it
```

**Rules:**
- **PreArm:**
  - `RouteCheck` fails closed with a generic error;
  - `Lstat(Socket)` must be a socket (not a symlink) owned by `HelperUID`.
- **Open:**
  - dial unix within ctx;
  - `PeerUID` must equal `HelperUID`, else close and return an error;
  - the first record, within `OwnedWait`, must be OWNED, else an error;
  - then start a writer goroutine (a bounded queue of `QueueRecords` and `WriteTimeout` per write) and a reader
    goroutine.
- **Frame:** a 0x10 body is queued as INNER records. If the queue is full, Frame returns an error.
- **Reader:**
  - INNER goes out as `w.TrySend(0x90, payload)`, and a `false` gives `w.Close("drop")`;
  - LINK becomes `w.Host().SetSummonLinkDegraded(b)`;
  - EOF or an error gives `w.Close("close")`.
- **Closing:** the socket is closed on `w.Done()` and in `Teardown`. Teardown also calls
  `SetSummonLinkDegraded(false)`.
- **agw:**
  - The capTable row is `{"SUMMON", false, func() capability.Capability { return newSummonRelay() }}`.
  - `agw probe-route [-tailscale-sock]` prints `route gate: ok` (exit 0) or `route gate: refused` (exit 1). It never
    echoes the prefs body.
  - `agw probe-summon [-socket]` dials, checks the server uid, reads OWNED within 1 s and closes. It prints
    `summon relay: ok` (exit 0) or `summon relay: refused: <reason>` (exit 1). The expected uid is a package var,
    `probeSummonUID = summonrelay.HelperUID`, which tests set to `os.Getuid()`.
  - serve's listen log line names the served capabilities (for example `capabilities CAMERA,DASHCAM,SUMMON`).
  - Update the existing `TestSelectCaps` expectations for the new row.

- [ ] **Step 1: Write the failing tests**

`relay_test.go` uses `testkit.FakeWindow` and `FakeHost`, with a fake helper on a unix socket under
`os.MkdirTemp("", "sr")`, and `HelperUID` = `os.Getuid()`.
- `TestRelaySpec`.
- `TestPreArmRouteRefused`.
- `TestPreArmSocketMissing`.
- `TestPreArmSocketWrongOwner`: `SocketUID` returns uid+1.
- `TestPreArmSymlinkRefused`.
- `TestOpenWrongServerUID`.
- `TestOpenNoOwned`: the fake helper sends nothing, and Open fails after about 500 ms.
- `TestOpenGarbageFirstRecord`.
- `TestRelayBothWays`: 40000 bytes each way arrive intact. Phone-side frames are at most 16384 bytes.
- `TestLinkSetsHost`: LINK 1, then 0, toggles `FakeHost.SummonLinkDegraded()`.
- `TestWriteQueueFullClosesWindow`: the fake helper stops reading, and the window closes within `WriteTimeout` + 1 s.
- `TestHelperEOFClosesWindow`.
- `TestWindowDoneClosesSocket`.

`cmd/agw/main_test.go`:
- `TestServeRegistersSummonRelay`: `serve -bench -auth-socket` with `startSignIn(fakeAuthd)`, never `-fake-role`,
  which serves only bench rows. The listen log line names SUMMON.
- `TestSelectCaps`, updated: `CAMERA,DASHCAM,SUMMON,REAL` and `ECHO,CAMERA,DASHCAM,SUMMON,REAL`.
- `TestProbeRoute`: an httptest prefs server gives exit 0 for `[]` and exit 1 for `["192.168.90.0/24"]`, with no body
  in the output.
- `TestProbeSummon`: with `probeSummonUID` set to `os.Getuid()`, against a `relay.Listen` with `AllowUID`
  `os.Getuid()`, it prints `summon relay: ok`.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./capability/summonrelay/ ./cmd/agw/ -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement `relay.go` and the agw wiring**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./capability/summonrelay/ ./cmd/agw/ && GOOS=linux GOARCH=amd64 go vet ./capability/summonrelay/ ./cmd/agw/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add capability/summonrelay cmd/agw
git commit -m "feat(agw): the SUMMON relay (cap 0x01), probe-route and probe-summon"
```

---

### Task 13: The bench, end to end (phone → Pi → agw relay → helper → mock car)

**Files:**
- Create: `cmd/agw/summon_e2e_test.go`

**Interfaces:**
- Consumes:
  - from the cmd/agw tests: `setupPhone`, `startServe`, `startSignIn`, `fakeAuthd`, `row` and `waitLogCount`, plus
    `dialouttest.Start`;
  - `helper.Serve` (Task 10);
  - `mockcar` (Task 5); `bustest` (Task 4);
  - `client.Dial`, `client.InnerConn`, `client.Handshake` (Task 2);
  - `summon.Type*` (Task 8);
  - `newSummonRelay` (Task 12).

**The rig** (`startSummonRig(t, opts)`):
- `dialouttest.Start`, plus `startSignIn` with a `fakeAuthd` row giving the phone's key role 2.
- `serve -bench -dial <pi car addr> -auth-socket <sock>`, with `newSummonRelay` swapped to
  `NewWithConfig(summonrelay.Config{Socket: <short dir>/relay.sock, HelperUID: os.Getuid(), RouteCheck: func(context.Context) error { return nil }})`.
  The default `SocketUID` already sees the test user.
- The helper runs in-process (`helper.Serve`) with `FakeBus` plus `mockcar` (`FakePort`, real time),
  `authority.Fake{Role: 2, Owner: true}`, `AllowUID` `os.Getuid()` and its own key. agw and the helper share one
  allowlist file.
- The phone:
  1. `client.Dial(pi.PhoneAddr(), proto.CapSummon, …)` with agw's pin;
  2. `InnerConn(outer, 0x10, 0x90)`;
  3. `Handshake(…, proto.CapSummon, …)` with the helper's pin;
  4. a 20 Hz LIVE sender while "held". VERB, HB and MSG are built from Appendix A's shapes.
- A `delayProxy` sits between the phone and the Pi's phone address. It can delay both directions by D, delay only
  Pi→phone by D, or hold every byte and flush on demand.
- Between sessions the rig's own `waitIdle` polls `pi.Idle() == 3`, and hellos are spaced 250 ms apart: each core
  admits 5 hellos per second.

- [ ] **Step 1: Write the failing tests**

- `TestSummonHappyPath`:
  - the mock car's received sequence is `data_channels_ready`, the verb (canonical), heartbeats, then on release the
    abort;
  - the phone receives the style, status, location, the verb's `cmd_result` and the heartbeat echoes as CAR frames;
  - `mockcar.Violations()` is empty.
- `TestSummonLostFramesTrip`: holding with a verb active, the phone stops sending LIVE and HB. The mock car receives
  exactly one injected abort within 600 ms of the last LIVE sent, and its result never reaches the phone.
- `TestSummonStalledLinkTrips`: `delayProxy` holds. An abort reaches the car within 600 ms of the last LIVE that got
  through, and no heartbeat is forwarded after it.
- `TestSummonLateLinkTrips`, the case freshness exists for:
  - hold with a verb active, then delay only Pi→phone by 2 s;
  - the phone keeps sending LIVE and HB at their usual rate, but echoes old tick ids, so they arrive on time and
    stale;
  - exactly one injected abort reaches the car within 700 ms of the delay starting, and no heartbeat is forwarded
    after it;
  - a later re-press is refused with `action_invalid`.
- `TestSummonDelayedLinkRefusesQueuedVerb`: with a 2 s delay, release and re-press. The re-press's verb reaches the
  helper stale and is refused (`action_invalid`), and the car never receives a second verb.
- `TestSummonStallThenFlushNothingResumes`: hold for 2 s, then flush. No verb or heartbeat in the burst is forwarded,
  and the car's last message is the injected abort.
- `TestSummonVerbWhileActiveRefused`.
- `TestSummonFloor`: `cmd_find_me`, `cmd_forward` and `cmd_reverse` from 2 m are each refused with
  `location_params_invalid`. From 5 m each is forwarded.
- `TestSummonStalePosition`: `mockcar.FreezeAfter(0)` during a hold, then release and re-press. The re-press is
  refused with `invalid_vehicle_location`.
- `TestSummonDeviceLocationPassesThrough`: a `device_location` 1 m from the car reaches the mock car.
- `TestSummonSchema`:
  - a valid `info` is forwarded;
  - an `info` with `txid` `"x"` is not forwarded, and the session continues;
  - a VERB with `summon_type` `"x"` ends the inner session (BYE `error`), and the mock car records the disconnect.
- `TestSummonRevocation`: rewriting the allowlist without the phone key ends both sessions. The car gets abort and
  disconnect exactly once.
- `TestSummonRealBus`: the happy path with `bustest`, `carlink.DialBus` and `mockcar.DBusPort`.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./cmd/agw/ -run TestSummon -count=1`
Expected: FAIL (the rig does not exist).

- [ ] **Step 3: Implement the rig and the tests in `cmd/agw/summon_e2e_test.go`**

Production code changes only if a test exposes a bug. Fix it in the owning package, with a regression test there.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=3 ./cmd/agw/ -run TestSummon`
Expected: PASS, three times.

- [ ] **Step 5: Commit**

```bash
git add cmd/agw/summon_e2e_test.go
git commit -m "test(agw): the Summon bench end to end: deadman, floor, schema, revocation and a real bus"
```

---

### Task 14: `summonhelper probe`, the no-motion car check

**Files:**
- Create: `internal/summon/probe/probe.go`, `internal/summon/probe/probe_test.go`
- Modify: `cmd/summonhelper/main.go` (`probe`)

**Interfaces:**
- Consumes: `carlink.Link`, `carlink.DBusBus.CallerInfo/SubscribeKeysUpdated` (Task 4); `authority.Oracle`; `keys`.
- Produces:

```go
package probe

type Config struct {
	Bus            carlink.Bus
	Duration       time.Duration // -duration, default 120 s
	HBEvery        time.Duration // 100 ms
	KeepaliveEvery time.Duration // 15 s
	Now            func() time.Time
	CallerInfo     func(ctx context.Context, sender string) (uid, pid uint32, err error)
	KeysUpdated    func(f func()) error            // nil: skipped
	Roles          func(ctx context.Context) ([]string, error) // "entry 1: role 2"; nil: skipped
	MarkerPath     string                                      // the helper's session marker
	Out            io.Writer
}

type TypeStats struct{ Count int; First, MaxGap time.Duration }

type Report struct {
	HeartbeatsSent, Echoes int
	ByType                 map[string]TypeStats // msg_type → stats; never a field value
	Callers                []string             // "uid=… pid=… sender=…"
	KeysUpdated            int
	Roles                  []string
}

func Run(ctx context.Context, cfg Config) (Report, error)
```

**Rules:**
1. If `helper.MarkerPresent(MarkerPath)` (Task 11), refuse and tell the owner to run `summonhelper finish` first.
2. Acquire the name with DO_NOT_QUEUE. If it is taken, return an error saying to stop summonhelper and webrtc-comms
   first.
3. `Serve` with a reply of 1.
4. Set the marker (`helper.FileMarker`), then `Connect` (H1).
5. Every `HBEvery`, send `{"car_timestamp":<the last echo's car_timestamp; omitted while 0>,"created:timestamp":<ms>,
   "msg_type":"autopark:heartbeat_app_webrtc","timestamp":<ms>}`.
6. Send `{"msg_type":"control:keepalive"}` at start and every `KeepaliveEvery`.
7. Collect stats until `Duration` ends. On every exit after `Connect` (the duration's end, ctx cancel, or an error):
   - stop the heartbeat and keepalive senders first;
   - then `Disconnect` (confirmed) and `Release`;
   - clear the marker only if the disconnect was confirmed.
8. Print the report: per `msg_type`, the count, the first arrival after H1 and the largest gap; echoes/sent; the
   callers; the KeysUpdated count; the roles.

The `probe` command maps SIGINT, SIGTERM and SIGHUP to ctx cancel.

It never prints coordinates, keys or the VIN. `-keyinfo` fills `Roles` through `authority.NewDBus` for each allowlist
key, printing only the entry's index and role.

- [ ] **Step 1: Write the failing tests**

`probe_test.go` (FakeBus with `mockcar` `FakePort`, real time, a 1.2 s duration):
- `TestProbeCountsEchoes`: about 12 heartbeats sent, and echoes ≥ sent − 1.
- `TestProbeSendsOnlyAllowedMessages`: every `PeerCommsToCarserverData` the FakeBus records has `msg_type`
  `autopark:heartbeat_app_webrtc` or `control:keepalive`. The `LocalToRemoteSignaling` bodies are exactly H1, then
  the disconnect.
- `TestProbeCarTimestampEcho`: the first heartbeat has no `car_timestamp`, and later ones carry the mock's.
- `TestProbeReportHasNoValues`: the report text contains none of the mock's latitude, longitude or VIN strings.
- `TestProbeFreezeVisible`: `mockcar.FreezeAfter(5)` shows in the report as the echo count stopping, with the largest
  gap ≥ the remaining time.
- `TestProbeRefusesWhenNameTaken`.
- `TestProbeRefusesWhileMarkerPresent`: nothing is emitted.
- `TestProbeDisconnectsOnCancel`: cancel mid-run.
  - The signaling bodies are H1, then the disconnect.
  - Nothing is sent after the disconnect, the marker is cleared, and `mockcar.Violations()` is empty.
- `TestProbeKeepsMarkerWhenDisconnectFails`: after `LoseName`, the confirmation fails and the marker stays.
- `TestProbeHasNoVerbCode`: the probe package's non-test `.go` files contain none of `cmd_find_me`, `cmd_forward`,
  `cmd_reverse`, `cmd_abort` or `device_location`.
- `TestProbeRealBus` (bustest + `DBusPort`): echoes are counted, and `Callers` has one entry with uid =
  `os.Getuid()` and the car connection's sender. The pid is not asserted: the darwin bus cannot report it.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/summon/probe/ -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement `probe.go` and the `probe` command**

Flags: `-duration`, `-keyinfo`, `-allow`, `-authd-sha256`.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./internal/summon/probe/ ./cmd/summonhelper/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/summon/probe cmd/summonhelper
git commit -m "feat(summonhelper): the no-motion probe: heartbeats and keepalives only, with echo, caller and key reports"
```

---

### Task 15: The installer's `SUMMON=1` path

**Files:**
- Modify: `deploy/install-car.sh`, `cmd/camhelper/install_test.go`, `Makefile`, `deploy/README.md`

**Interfaces:**
- Consumes:
  - the `summonhelper` commands `run`, `finish`, `keygen` and `pin` (Tasks 10 and 11);
  - `agw probe-route` and `agw probe-summon` (Task 12);
  - the existing installer functions `exec_mount`, `write_if_changed`, `link_into`, `usable_svdir`, `log_run`,
    `gen_helper_run`.
- Produces:
  - `install-car.sh [--after-boot] [agw] [camhelper] [summonhelper]`, where the third path (default `./summonhelper`) is
    used only with `SUMMON=1`;
  - `make car` also builds `out/summonhelper`.

**Rules:**
- **Remove the old path:**
  - agw's run script is always plain `serve`;
  - delete `agw_has_summon`, `summon_mode`'s `supervise` grep, `summon_binary_ok`, `resolve_summon`,
    `SUMMON_RUN_DIR=/run/airgapp-gw`, the marker prep, and the `summon-finish` finish script;
  - delete install_test.go's `fakeAgw` summon variant.
- **Values tests can override,** baked into the generated scripts the way `IPT` is:
  - `GDBUS` and `TIMEOUT` (`AGW_TEST_GDBUS`, `AGW_TEST_TIMEOUT`);
  - `SV_BIN`, the `sv` command (`AGW_TEST_SV`). Never reuse `SV`, which is the service directory (:558);
  - `KILL_BIN` (`AGW_TEST_KILL`);
  - `WEBRTC_SV` (`/etc/sv/webrtc-comms`), `WEBRTC_PIDFILE` (`/run/service/webrtc-comms/webrtc-comms.pid`) and
    `PROC` (`/proc`);
  - `SUMMON_RUN_DIR` (`AGW_TEST_SUMMON_RUN_DIR`, default `/run/airgapp-summon`);
  - `VIN_FILE` (`AGW_TEST_VIN_FILE`, default `/var/etc/vin`);
  - `SUMMON_UID` and `GW_GID` (`AGW_TEST_SUMMON_UID` and `AGW_TEST_GW_GID`, defaults 1337 and 4950).
- **New hooks, all placed before the root check at :450:**
  - `AGW_SUMMON_SCRIPTS_ONLY` prints the helper's run and finish scripts;
  - `AGW_STOP_WEBRTC_ONLY` runs only the stop sequence;
  - `AGW_TEST_SUMMON_RUN` is the installed helper run script, for `--after-boot` detection;
  - `AGW_SUMMON_REMOVE_ONLY` runs only the removal, with `DIR`, `SV` and `SVDIR` overridable;
  - `AGW_SUMMON_PREFLIGHT_ONLY` runs only the preflight.
- **Preflight:** with `SUMMON=1`, or when a helper is being removed, require `gdbus` and `timeout`. This goes after
  SUMMON is resolved at :463, so a `SUMMON=0` install on a car without a helper never needs them.
- **Typed yes.** Each new car-state change gets an `ask`, on `--after-boot` too:
  - the summonhelper copy and keygen;
  - the stop sequence;
  - the `SUMMON=0` removal.
- **Files:**
  - the binary is `$DIR/summonhelper` (root 0755, in the exec-mounted `$DIR`);
  - `/var/lib/airgapp-summon` is root 0755;
  - `/var/lib/airgapp-summon/state` is 1337:1337 0700;
  - the key is created with `chpst -u :1337:1337 $DIR/summonhelper keygen -key <state>/key.pem` if absent;
  - the service dir is `$DIR/sv/summonhelper`, linked as `airgapp-summonhelper`;
  - the log goes to `$DIR/svlog/summonhelper`.
- **Run script (generated; root), in this order:**
  0. `exec 2>&1`.
  1. Refuse a symlink or a non-directory at `$SUMMON_RUN_DIR`. Then `mkdir -p`, `chown $SUMMON_UID:$GW_GID` and
     `chmod 2750`.
  2. `rm -f $SUMMON_RUN_DIR/relay.sock`.
  3. Read the VIN from `$VIN_FILE`, stripping whitespace. Refuse it if empty.
  4. Check the name is free:

     ```sh
     _o=$($TIMEOUT 5 $GDBUS call --system --dest org.freedesktop.DBus --object-path /org/freedesktop/DBus --method org.freedesktop.DBus.NameHasOwner com.tesla.Webrtc 2>/dev/null) && [ "$_o" = '(false,)' ]
     ```

     Anything else logs the raw output, does `sleep 10` and exits 1.
  5. `SUMMONHELPER_VIN="$VIN" exec chpst -u :$SUMMON_UID:$SUMMON_UID "$DIR/summonhelper" run${AUTHD_SHA256:+ -authd-sha256 $AUTHD_SHA256}`.

     The form matches `gen_helper_run`:306. When `AUTHD_SHA256` is unset, the compiled-in pin applies. An explicit
     empty `-authd-sha256` would make `authority.NewDBus` fail and the helper never start. On `--after-boot` the pin
     comes from camhelper's installed run script, as today.

  Every refusal follows agw's pattern: echo the reason, `sleep 10`, `exit 1`.
- **Finish script:** `exec 2>&1`, then `exec chpst -u :1337:1337 "$DIR/summonhelper" finish`.
- **The stop sequence** runs at install and on `--after-boot`, after S0, before the helper service starts:
  1. If our helper's runsv is running, `$SV_BIN -w 15 down $SV/summonhelper`. Its takeover and finish then run, and it
     lets go of the name.
  2. `$SV_BIN down $WEBRTC_SV || echo "note: webrtc-comms is not supervised now"`. Outside the infotainment and sentry
     run modes it has no runsv.
  3. If `$WEBRTC_PIDFILE` holds a pid whose `$PROC/<pid>/comm` is `minijail-init`, `$KILL_BIN -KILL` it. Then remove
     the pidfile. This matches `/etc/sandbox/sandbox.bash`'s `KillOrphanedMinijailProcessComm`.
  4. Poll the name check above every 1 s, up to 10 times.
  5. After S3 has linked the helper, `$SV_BIN up $SV/summonhelper`. S4 restarts only what changed.

  If step 4 fails:
  - on a full install, `die`, before anything for Summon is linked;
  - on `--after-boot`, never `die` before S3. agw and camhelper start as today, and the helper is neither linked nor
    started. The Summon failure is reported at the end, with a non-zero exit, so a Summon problem never keeps the
    camera down.
- **`--after-boot`:**
  - With `SUMMON` unset, it is inferred from the installed helper run script (`AGW_TEST_SUMMON_RUN` in tests).
  - `SUMMON=0` runs the removal below.
  - `SUMMON=1` with no installed helper dies with "install with SUMMON=1 first".
  - An installed agw run script that still contains ` supervise -- serve ` is regenerated as plain serve (migration).
  - The order is: S0 exec mounts, then the stop sequence, then S3 and S4.
- **Checks after the last restart** (after S4 on a full install; after the wait loop or S4 on `--after-boot`), SUMMON=1
  only:
  - retry `chpst -u :4950:4950 "$DIR/agw" probe-summon` every 1 s, up to 15 s, until it prints ok. A stale socket and
    the first stamp of the D2 loop both need the retry;
  - the same for `chpst -u :4950:4950 "$DIR/agw" probe-route`.

  Either failing makes the installer `die` with its output (on `--after-boot`, a report at the end and a non-zero
  exit). The installer reads the VIN from `$VIN_FILE` with the run script's exact whitespace strip. Beside agw's pin,
  it prints the output of `chpst -u :1337:1337 "$DIR/summonhelper" pin -vin "$VIN"`, the helper's pin.
- **Every two-service list** gains summonhelper when `SUMMON=1`:
  - `attached()`, the `link_into` loop, the unsupervised-process check, the wait loop, the S4 restart and change
    detection;
  - S2's directory and permission lines (:607-610), the svlogd `config` loop (:617-623), the printout (:597) and the
    DONE text (:686).
- **The `SUMMON=0` removal**, on a car that has `$SV/summonhelper`, in this order:
  1. `ask`.
  2. Remove the runsvdir link and `$SV/summonhelper/run` first, so a later `--after-boot` cannot re-enable the helper.
  3. `$SV_BIN -w 15 down $SV/summonhelper || true`.
  4. Poll until the name is free (10 × 1 s, as above). Stock calls `registerService` once and never retries, so
     webrtc-comms must not start while our helper still holds the name.
  5. `$SV_BIN up $WEBRTC_SV || echo "note: ..."`.
  6. `rm -rf $SV/summonhelper`.

  Otherwise `SUMMON=0` adds nothing.
- **Nothing opens an inbound port, and nothing uses `mount --bind` other than through `exec_mount`.** The existing
  scans at install_test.go:219-231 and :282-297 keep passing.
- `deploy/README.md`: rewrite the SUMMON section (154-177) and the `--after-boot` text (179-187) to describe the
  above, and add summonhelper to the layout table.
- `Makefile` `car`: add `... -o out/summonhelper ./cmd/summonhelper`.

- [ ] **Step 1: Write the failing tests**

Replace `TestInstallScriptSummonMode` and the old SUMMON assertions at install_test.go:106-178 and :403-415. Use the
existing `script` helper and the hooks above.
- The fakes are POSIX sh. `fakegdbus` pops one answer per call from a file and logs argv. The `sv` and `kill` fakes log
  argv and can be told to exit 1.
- Tests that reach an `ask` pass "yes" on stdin.
- Each refusal case asserts its own log line, so no case can pass on an earlier refusal.

The tests:
- `TestAgwRunIsPlainServe`, with both `SUMMON=0` and `SUMMON=1`.
- `TestSummonRunScriptOrder`. In this order: `exec 2>&1` first, then the dir prep, `rm -f`, the VIN, the name check,
  then `exec chpst -u :1337:1337`. Also the exact gdbus command, the `'(false,)'` comparison, and `SUMMONHELPER_VIN=`.
  The pin cases:
  - with `AUTHD_SHA256` unset there is no `-authd-sha256`;
  - a valid value appears verbatim;
  - `--after-boot` carries camhelper's installed pin.
- `TestSummonRunScriptRefusals`. The generated script runs under host `sh`, with `AGW_TEST_SUMMON_RUN_DIR`,
  `AGW_TEST_VIN_FILE` and `AGW_TEST_SUMMON_UID`/`AGW_TEST_GW_GID` set to the test user, the fakes, and `sleep` stubbed.
  Each of these exits 1 with its own reason: a symlinked dir, an empty VIN, and gdbus answering `(true,)`, an error or
  garbage.
- `TestSummonFinishScript`: `exec 2>&1` comes first.
- `TestStopWebrtcComms`:
  - with the helper up, `sv -w 15 down $SV/summonhelper` comes before the first gdbus call;
  - `$SV_BIN down /etc/sv/webrtc-comms` is called, and an `sv` that exits 1 is tolerated;
  - a pidfile whose comm is `minijail-init` gets `kill -KILL <pid>`, and a comm `bash` gets no kill;
  - the pidfile is removed;
  - gdbus answering `(true,)`, `(true,)`, `(false,)` succeeds on the third poll, and 10 × `(true,)` fails;
  - `sv up $SV/summonhelper` comes after the name is free.
- `TestSvBinIsNotSv`: a text scan. The stop sequence calls `$SV_BIN`, and `SV_BIN` is never reassigned after its
  definition.
- `TestAfterBootSummonModes`:
  - SUMMON unset with a helper installed brings the helper up;
  - `SUMMON=0` runs the removal;
  - `SUMMON=1` with no helper dies with "install with SUMMON=1 first";
  - an old ` supervise -- serve ` agw run script is regenerated plain.
- `TestAfterBootStopFailureStillStartsServices`: 10 × `(true,)` on `--after-boot` still reaches S3 for agw and
  camhelper, then exits non-zero with the Summon report.
- `TestSummonZeroRemovesHelper`: the removal order above, with the fakes.
- `TestSummonPreflightOnlyWithSummon`: with `SUMMON=0` and no helper installed, a missing gdbus does not fail.
- `TestSummonChecksRetry`: with a stale socket file present, a `probe-summon` fake that fails twice, then prints ok,
  passes. 15 failures die.
- `TestSummonAsks`: each new step asks. Without stdin, it dies with "no answer".
- The existing tests at :219-231 (no INPUT rule or `--dport`) and :282-297 (only `exec_mount` binds) still pass.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./cmd/camhelper/ -run 'Test(Agw|Summon|Stop|SvBin|AfterBoot|Install)' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement the script changes, the Makefile line and the README section**

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test -race -count=1 ./cmd/camhelper/ && make car && ls -l out/`
Expected: PASS. `out/` holds agw, camhelper and summonhelper.

- [ ] **Step 5: Commit**

```bash
git add deploy/install-car.sh deploy/README.md cmd/camhelper/install_test.go Makefile
git commit -m "feat(deploy): SUMMON=1 installs summonhelper and stops stock webrtc-comms; agw always serves plain"
```

- [ ] **Step 6: Hand the installer change to the camera session for review**

The controller sends the camera session the `git diff` of this commit (`deploy/`, `cmd/camhelper/install_test.go`,
`Makefile`) with a short note, and asks for a review before anything reaches `feat/camera-capabilities`. It records
the reply in the ledger.

---

### Task 16: Docs, the no-motion checks, and the final build

**Files:**
- Create: `docs/SUMMON.md`
- Modify: `docs/CAPABILITY-API.md`, `README.md`

**Interfaces:**
- Consumes everything above.

`docs/SUMMON.md` covers:
- **The architecture:**
  - the four processes and their uids;
  - the outer and inner sessions;
  - the frame types and relay records, from Global Constraints;
  - what the helper may originate;
  - **the 3 m boundary, for the phone plan.** The helper uses haversine with R = 6371 km on the coordinates as
    received, and refuses when d < 3.0 m. A phone that pushes a Go to Target point out to exactly 3 m (MapKit's
    geodesic is ellipsoidal) sits on that boundary, so the phone plan must choose a margin.
- **The bench:** how to run `go test ./cmd/agw/ -run TestSummon` and the `bustest` packages.
- **The no-motion checks** (design §8 check 4; the owner at the car). These are the only car steps, and none sends a
  verb. Exact commands, the expected output, and the Appendix A §9 row each one settles:
  1. Check that `summonhelper` is down, and stop stock webrtc-comms the way the installer does.
  2. Under `setsid`, so a dropped SSH session cannot cut it off, run
     `chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper probe -duration 120s -keyinfo`.
     - A probe cut short sends its disconnect on SIGINT, SIGTERM or SIGHUP.
     - If it dies anyway (SIGKILL, or exit 3 after an emit overrun), its marker stays. Run
       `chpst -u :1337:1337 /var/lib/airgapp-gw/summonhelper finish` before anything else.

     It settles:
     - whether QtCarServer accepts the helper (car frames arrive after H1);
     - the caller of the car frames (`Callers`);
     - the stream freeze (expect about 1200 echoes; Appendix A §9 row 4);
     - KeyInfo as uid 1337 (the roles);
     - KeysUpdated delivery (design §6.1). authd emits KeysUpdated only when it starts or a key changes, so the owner
       restarts authd or changes a key during the probe.
  3. The connect pre-prime, read over SSH, a few values only (Appendix A §9 row 5): `GUI_selfParkState` and `GUI_selfParkActivation`
     during the probe.
  4. Runtime config reads (Appendix A §9 row 15) and the keepalive effect (row 16), listed with the commands from the camera runbook's
     conventions.
  5. The gear probe (Appendix A §9 row 8): P to D to P with the brake held and an occupant in the seat. No verb is sent.
  6. The VIN the run script read (design §8 check 4). As root, compare the running `serve`'s `SUMMONHELPER_VIN`, from
     `/proc/<serve pid>/environ`, with the VIN in the helper pin the installer printed. Print only `match` or
     `mismatch`, never the VIN.
  7. The finish hook covers a killed supervisor, on Linux, with no session open:
     - `kill -STOP` the `serve` pid, then `kill -KILL` the `run` pid;
     - within 1 s, `serve` is gone (Pdeathsig), and `com.tesla.Webrtc` has no owner or belongs to the restarted helper;
     - the svlog shows `finish` ran and found no marker.
- **Reminders:**
  - be in position before waking the car (it re-sleeps after about 5 min);
  - never stream bulk data over SSH;
  - R22.
- **What check 4 cannot settle yet:** the round trip with the camera running needs the phone link (the phone plan;
  call 10).

`docs/CAPABILITY-API.md`: the SUMMON relay row, plus an "inner SUMMON capability" section with the inner Spec.

`README.md`: Summon status and its next step.

- [ ] **Step 1: Write the docs**

- [ ] **Step 2: Run every check**

```bash
gofmt -l .
go vet ./... && GOOS=linux GOARCH=amd64 go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
go test -race -count=3 ./...
git grep -n -E 'capability/summon([^a-z]|$)|summon-finish|agw_has_summon|cmdSupervise|TypeHold|TypeLoc' -- ':!docs/superpowers'
make car && shasum -a 256 out/agw out/camhelper out/summonhelper
```

Expected:
- gofmt prints nothing, and every check passes;
- the retired-names grep prints nothing;
- the three hashes are printed. Put them in the report.

- [ ] **Step 3: Commit**

```bash
git add docs/SUMMON.md docs/CAPABILITY-API.md README.md
git commit -m "docs(summon): the car-side architecture, the bench and the owner's no-motion checks"
```
