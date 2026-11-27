# Capability API

A capability is a Go type implementing `capability.Capability` (package
`airgapp/gateway/capability`, a public leaf package that imports nothing
internal). The core owns the transport, HPKE, authorization, heartbeat and
lease; a capability owns only its frames.

## Interface

- `Spec()`: `Name`, `ID`, `In`/`Out` (allowed type -> max body bytes),
  `LeaseMS` (0 = none), `TickHz` (0 = none; only the inner SUMMON
  capability, which summonhelper serves, uses 20), `Bulk` (outbound types
  on the bulk lane).
- `PreArm(ctx context.Context, pkS []byte) error`: extra gates before ARM is
  accepted (CAMERA returns nil).
- `Open(ctx context.Context, w Window, params json.RawMessage) (Handler, error)`:
  params is the ARM `params` object and may be empty/nil.
- `Handler.Frame(t, body)` and `Handler.Teardown(reason)`.
- `Window`: `Send`, `TrySend`, `QueuedBytes`, `SetDetail`, `LogEvent`,
  `Close`, `Done`, `KeyFingerprint`, `PublicKey`, `TickAge`, `Host`.
- `Window.PublicKey()` returns a copy of the authenticated phone key `pkS`
  (65 bytes, uncompressed P-256): the key the hello named, the ARM proved and
  the authority (authd: OWNER or DRIVER) accepted. Use it to re-check the key's role
  (for example on `KeysUpdated`); never log it (log `KeyFingerprint()`).

## Budgets and rules for PreArm, Open and Teardown

- The core calls `PreArm` with a context bounded by **1 s** and `Open` with
  one bounded by **2 s** (both are also cancelled by shutdown). Honour them,
  including in any D-Bus or root-helper call made from there.
- A `PreArm`/`Open` error, a panic in either, or an overrun (still running
  when the budget ends, or returning after the context expired) closes the
  connection with no message, like any other pre-auth failure, and frees the
  slot. A `Handler` that `Open` returns after its budget gets
  `Teardown("open overran")`, after its window is closed (`Done()` closed).
  That late `Teardown` may still be running while a new window of the same
  capability opens, so keep state per `Handler`, never in capability-wide
  globals.
- **Never block in `Open`.** If the capability needs time to become ready
  (a camera stream starting, a helper call), return the `Handler` at once and
  report readiness later with the capability's own STATUS frame.
- READY is always the first frame on the wire; the window's writer starts
  only after `Open` returns. Until then `Send` never blocks: if the frame's
  lane is full it returns an error (the frame is not queued).
- `Teardown` must finish in well under **0.5 s**: on SIGTERM, `Shutdown`
  waits only 1 s for every window. Every call to the root helper needs a
  timeout.
- Goroutines a capability starts must recover their own panics. The core
  recovers panics in `PreArm`, `Open`, `Frame` and `Teardown` only; a panic
  in any other goroutine kills the whole `agw` process.

## IDs and type ranges

Capability ids: SUMMON `0x01`, CAMERA `0x02`, DASHCAM `0x03`, VCMD `0x04`, ECHO `0x7F`.

- VCMD `0x04` (agw, `capability/vcmd`; direct mode Phase 2, 2026-11-27): Tesla's own signed vehicle-command
  messages (`universalmessage.RoutableMessage`, what the phone would send over BLE) to and from the car's
  command-router, through camhelper's relay on `/run/airgapp-camhelper/cmd.sock` (`internal/cmdrelay`).
  `Spec{Name: "VCMD", ID: 0x04, In: {0x50: 8192}, Out: {0xD0: 8192, 0xD1: 64, 0xD2: 0}, LeaseMS: 20000}`.
  - `0x50` MSG (phone to car): one RoutableMessage. `0xD0` MSG (car to phone): one reply. `0xD1` ERR: a reason, then 0x00
    and the message's uuid: `refused`, `rate`, `monitor`, `busy` (not injected: safe to resend) or `unknown` (handed
    to the router with no answer: it may have run, so wait for its reply rather than resending). `0xD2` READY: the relay's bus monitor is up (sent once).
  - The relay injects through `com.tesla.CommandRouter.RouteFromAuthd` from a child running as the authd user (it
    redials a dropped bus; a dead child is restarted) and reads the replies with a root D-Bus monitor on
    `com.tesla.Auth.RouteMessageBytes`, believing only the router's current owner (`NameOwnerChanged`). Nothing reaches Tesla's
    servers. Routing addresses are rewritten per session (a phone only ever receives replies to its own requests; at
    most 8 per session, the oldest forgotten first);
    only the VEHICLE_SECURITY and INFOTAINMENT domains are accepted; 20 messages/s (burst 40). The car checks every
    message's signature and the key's role itself.
  - Open: the relay's socket server must be uid 0. A relay that ends closes the window (`error` on the wire).

- SUMMON `0x01` (agw): the Summon relay (`capability/summonrelay`),
  `Spec{Name: "SUMMON", ID: 0x01, In: {0x10: 16384}, Out: {0x90: 16384}, LeaseMS: 20000, TickHz: 0}`.
  - `0x10` INNER (phone to agw) and `0x90` INNER (agw to phone), both on the
    priority lane, carry chunks of the inner session's byte stream (the
    phone's session with summonhelper, "Inner SUMMON capability" below).
    agw never parses them.
  - PreArm: the R1 route gate (tailscaled must advertise no route but the
    exit-node pair `0.0.0.0/0` and `::/0`), and the relay socket
    `/run/airgapp-summon/relay.sock` must be a socket owned by uid 1337.
  - Open: the socket's server must be uid 1337, and its first record, within
    500 ms, OWNED (the helper owns `com.tesla.Webrtc` and is healthy).
  - Frame queues each 0x10 body for one writer goroutine (64 bodies, a 1 s
    deadline per write). A full queue is an error, so the window ends.
    After a failed write to the helper socket, 0x10 bodies are discarded
    until the window closes "drop" (≤ 250 ms), so the helper's last records
    reach the phone first.
  - The helper's LINK records set `Host().SetSummonLinkDegraded`.

  The architecture and the owner's car checks: `docs/SUMMON.md`.

| Dir | Range | Owner |
|---|---|---|
| in | `0x01` ARM, `0x02` HEARTBEAT, `0x03` CLOSE | core |
| in | `0x10-0x1F` | SUMMON |
| in | `0x20-0x3F` | CAMERA / TALK |
| in | `0x40-0x4F` | DASHCAM |
| in | `0x50-0x5F` | VCMD |
| in | `0x7E` | ECHO |
| out | `0x80` READY, `0x81` BYE, `0x82` TICK | core (TICK only when `Spec.TickHz > 0`: the inner SUMMON session) |
| out | `0x90-0x9F` | SUMMON |
| out | `0xA0-0xBF` | CAMERA / TALK |
| out | `0xC0-0xCF` | DASHCAM |
| out | `0xD0-0xDF` | VCMD |
| out | `0xFE` | ECHO |

## What the core enforces

- Grammar: any inbound type outside `Spec.In` (other than ARM/HEARTBEAT/CLOSE
  where permitted by state) tears the session down. Bodies above the `In` limit
  tear down too; outbound types must be in `Out` and within size: `Send`
  returns an error (and `TrySend` false) for any other type or an oversize
  body, without tearing the window down (it is a capability bug).
- Lease: with `LeaseMS > 0` the session is torn down if no accepted inbound
  frame arrives within the lease. Any accepted frame renews it; HEARTBEAT is
  just the idle keep-alive. The phone sends HEARTBEAT every 5 s (lease 20 s =
  three missed beats). The lease counts arrivals: a frame renews it however
  long a relay held it back. A capability that must know the phone is still
  there bounds that itself, as the inner SUMMON capability's presence lease
  does ("Inner SUMMON capability" below); it closes with BYE `lease` too.
- At most one window per capability exists at a time.
- HEARTBEAT and CLOSE are handled by the core and never reach `Handler.Frame`.
- `Handler.Frame` is called from a single goroutine, in order. A non-nil
  return tears down with BYE reason `"error"`; the error text is logged on
  the car, never sent. Do not block in it indefinitely.
- `Teardown` runs exactly once on every path (CLOSE, lease expiry, handler
  error, revocation, connection loss, SIGTERM), after reads stop and before BYE.
  Release hardware/state there; it must be quick.
- BYE reasons on the wire are only `close`, `lease`, `grammar`, `oversize`,
  `decrypt`, `revoked`, `sigterm`, `error`, `drop` (connection lost). A
  capability may pass any text to `Window.Close`: a fixed reason is sent
  verbatim, anything else goes on the wire as `error`. The original text
  reaches `Handler.Teardown`, the car's stderr log and the session log only.
  A window revoked by the authority re-check gets BYE `revoked` and
  `Teardown("revoked")`; why (the
  trigger and the error) is only in the car's stderr log and the session log.

## Sending and lanes

- Outbound types listed in `Spec.Bulk` go on the bulk lane (cap 4 MiB). Every
  other type, and BYE, uses the priority lane and is written before any queued
  bulk frame.
- `Send` blocks while the frame's lane is full. `TrySend` returns false instead.
- `TrySend` works on either lane (false: that lane is full): use it wherever
  ending the session, or dropping the frame, beats blocking. Both SUMMON
  capabilities use it on the priority lane and end the session when it
  returns false, so a frame is never dropped silently (plan call 6): agw's
  relay for 0x90, the helper's inner capability for 0x91. `QueuedBytes` is the
  bulk lane's depth: dropping to the next IDR under pressure is the
  capability's job, using its own watermarks on `QueuedBytes()`.
- After Close (or any teardown), `Send` returns `capability.ErrClosed` and
  `TrySend` returns false; nothing is sent. Goroutines must exit on `Done()`
  or on these results.
- `Send`/`TrySend` take ownership of `body`: do not modify it after the call.
  The body given to `Handler.Frame` is valid only until `Frame` returns; copy
  it to keep it.
- Capabilities send from their own goroutines; `Window` is safe for that. Stop
  them on `Done()`.
- `LogEvent(event)` appends `{"t": <ms since window start>, "e": "<event>"}`
  to the window's session-log record (an `"events"` array). At most 64 events
  per window, each at most 128 bytes (longer text is truncated, extra events
  are dropped). Short facts only, never media or PCM. CAMERA logs each
  selectable-camera hold with it (PLAN section 4.9: camera, start, end).
- Capabilities never write car DataValues. The camera root helper does (PLAN
  section 4.9).

## Freshness ticks (`Spec.TickHz`)

With `TickHz > 0` the core sends TICK (`0x82`) on the priority lane at that
rate, from a real ticker, starting after READY. The body is exactly 16 bytes:
an 8-byte random id (crypto/rand) followed by the issue time in BOOTTIME ms
(u64 big-endian, from the gateway's clock). If the priority lane is full the
tick is skipped, never blocked on. Ticks stop at teardown: none is queued
after `Close`, ticks still queued at `Close` are dropped, and none follows
BYE.

The phone echoes a tick id in its own frames: in the inner SUMMON session,
LIVE, VERB, HB and PRESENCE carry the newest one. `Window.TickAge(id)`
returns how long ago that tick was issued, measured on the gateway's
BOOTTIME clock (never a phone or wall clock). `ok` is true only for one of
the last 64 ticks issued **in this window**: an unknown id, an older one, or
a tick from another window is `!ok`. The inner SUMMON capability calls it on
every LIVE, VERB, HB and PRESENCE. For D2 it treats `!ok` or an age above
F = 150 ms as not fresh (LIVE, VERB and HB only; `docs/SUMMON.md`). For the
presence lease an echo counts when it is `ok` and at most 3 s old (below).

`TickHz` is 0 for every capability agw serves, its SUMMON relay included.
Only the inner SUMMON capability, which summonhelper's core serves, uses 20.

## Inner SUMMON capability (served by summonhelper, not agw)

`summonhelper serve` (uid 1337) runs the same server core on
`/run/airgapp-summon/relay.sock`, with its own key and pin, and serves one
capability, the SUMMON engine (`internal/summon`, `InnerSpec`). The phone
reaches it through agw's SUMMON relay: the inner session's bytes travel in
the relay's 0x10 and 0x90 frames.

```go
capability.Spec{
    Name: "SUMMON", ID: 0x01, LeaseMS: 20000, TickHz: 20,
    In:  map[byte]int{0x14: 8, 0x15: 1036, 0x16: 524, 0x17: 2048, 0x18: 8}, // LIVE, VERB, HB, MSG, PRESENCE
    Out: map[byte]int{0x91: 16384},                                          // CAR
}
```

| Dir | Type | Body |
|---|---|---|
| in | `0x14` LIVE | tick id (8 bytes) |
| in | `0x15` VERB | tick id (8) ‖ press id (u32 BE) ‖ Tesla's `cmd_find_me`, `cmd_forward` or `cmd_reverse` |
| in | `0x16` HB | tick id (8) ‖ press id (u32 BE) ‖ Tesla's `heartbeat_app_webrtc` |
| in | `0x17` MSG | Tesla's `cmd_abort`, `device_location`, `info` or `control:keepalive` |
| in | `0x18` PRESENCE | tick id (8 bytes), about once a second, held or not |
| out | `0x91` CAR | channel (1 byte: 0 `dcTcp`, 1 `dcUdp`) ‖ the car's JSON, verbatim |

- The inner types differ from the relay's (0x10, 0x90), so a frame sent to
  the wrong session is refused as grammar instead of being misread.
- ARM params: `{"presence":1}`, the JSON number 1 (`1.0` and `1e0` count
  too), opts the session into the presence lease. No params, `null`, `{}` or
  an object without `presence` leave it off, and other keys are ignored. Any
  other `presence` value, a value that is not an object, or malformed JSON
  refuses the ARM: the connection closes with no message, and the helper
  logs `summon: the session did not start: bad params`, never the params.
  agw's relay ignores its own ARM params.
- The presence lease: in a session that opted in, the helper closes the
  window with BYE `lease` once more than 10 s have passed since the issue
  time of the newest tick echoed at most 3 s old (in a PRESENCE, LIVE, VERB
  or HB), or since Open while there is none. PRESENCE reaches neither the
  engine nor the car. A session that did not opt in keeps only the 20 s
  lease; its PRESENCE frames are still accepted, and renew that lease like
  any frame.
- An older helper's Spec has no 0x18, so it answers PRESENCE with BYE
  `grammar`: the helper ships before any phone that sends it.
- The helper parses press ids and logs them; no rule applies to them.
- Car JSON over 16383 bytes is dropped and logged by type and size. A CAR
  frame the priority lane cannot take ends the session (H4): a car frame is
  never dropped silently.
- Freshness, the deadman (D2), the 3 m floor (D3), the message shapes and
  the refusals: `docs/SUMMON.md`.

## Host (cross-capability state)

`Window.Host()` returns the gateway-wide `capability.Host`, the same state
from every window:

- `SummonOpen()`: true while a SUMMON (`ID 0x01`) window holds its slot (from
  the ARM's slot reservation until teardown frees it).
- `SummonLinkDegraded()`: SUMMON's report that its link is degraded. The
  relay sets it from the helper's LINK records: 3 LIVE tick ages in a row
  over 100 ms set it, and the end of a hold or 2 s of ages at or under 75 ms
  clear it.
- `SetSummonLinkDegraded(v)`: honoured only when called through the SUMMON
  window that holds the slot; from any other window (or a SUMMON window that
  has freed its slot) it does nothing. The flag resets to false when the
  SUMMON window frees its slot.

Rules for the other capabilities:

- CAMERA: while `SummonLinkDegraded()`, drop to keyframe-only (send IDRs
  only).
- CAMERA: while `SummonOpen()`, no TALK and no selectable camera.
- Poll these from your own goroutine or per frame; they are cheap (a mutex)
  and there is no change notification.

## Spec limits (checked by `server.New`)

`server.New` panics (a programming error) if any `In` limit exceeds 16384,
any `Out` limit exceeds 1 MiB (`1 << 20`), `In` contains a core type
(`0x01`-`0x03`), `Out` contains `0x80`, `0x81` or `0x82`, a `Bulk` type is
not in `Out`, `TickHz` is outside 0..50, or two capabilities share an ID.

## ARM params budget

Every inbound frame before arm is at most 1024 bytes, including the first
one: `enc` (65) + AES-GCM tag (16) + type (1) + the ARM JSON. The ARM wrapper
(`n`, `cap`, `exp`) takes about 90 bytes, so keep `params` under about
**850 bytes** of JSON. A larger ARM is closed without a message.

## Sign-in

The handshake (hello, challenge, HPKE ARM) is the wire protocol in
`docs/superpowers/plans/2026-11-20-shared-gateway-core.md`, "Wire protocol". A
version 2 hello (`ver=0x02`) gets a version 2 challenge that also carries the
signing gateway's long-term public key (signed), so a phone can trust the car's
identity on first connect; version 1 is unchanged. A sign-in is granted when
the ARM proves possession of the hello's key and authd lists that key as OWNER
or DRIVER; there is no key list of the gateway's own. authd is asked twice: at
the hello, as a filter, so a key it does not list gets no challenge (which
carries the VIN binding and, for v2, the gateway's key), only the silent close;
and after the ARM, as the gate. Whether the capability's slot is busy is told
only after the gate.

## Reconnecting

A hello sent right after a drop can reach the car while the old window is
still tearing down (it frees its slot within about 100 ms). That sign-in
finds the slot busy after its ARM and authority check, and is closed with no
message, like every pre-auth refusal.
Clients retry with backoff, for example after 250 ms, 500 ms, then 1 s.

## Example Spec (CAMERA, numbers agreed with the camera session)

```go
capability.Spec{
    Name: "CAMERA", ID: 0x02, LeaseMS: 20000,
    In: map[byte]int{
        tSelect: 64, tMode: 16, tTalkStart: 64, tTalkPCM: 16384, tTalkStop: 16,
    }, // placeholders: pick values in 0x20-0x3F
    Out: map[byte]int{
        tH264: 1 << 20, tStreamConfig: 1024, tStatus: 1024, tTalkState: 256,
    }, // placeholders: pick values in 0xA0-0xBF
    Bulk: map[byte]bool{tH264: true},
}
```

DASHCAM (`ID 0x03`, Lease 20000): In LIST <= 256, GET <= 512; Out LIST_RESULT
<= 64 KiB, CHUNK <= 64 KiB (bulk), ERROR <= 256; types in `0x40-0x4F` /
`0xC0-0xCF`.

## Registering

`cmd/agw/main.go` hosts capabilities from the `capTable` slice. Adding one is a
one-line change:

```go
var capTable = []struct{ name string; benchOnly bool; new func() capability.Capability }{
    {"ECHO", true, echo.New},        // test-only: registered only with -bench
    {"CAMERA", false, camera.New},   // example: a car build registers it always
    // the SUMMON relay to summonhelper's socket; agw never drives the car
    {"SUMMON", false, func() capability.Capability { return newSummonRelay() }},
}
```

`newSummonRelay` is `summonrelay.New`, a variable so that tests can swap it.

`benchOnly` entries are never registered without `-bench`; ECHO must stay one.
Real capabilities are never served with a fake authority: with `-fake-role`,
`selectCaps` registers only `benchOnly` entries (a test pins this).

## Testing

Unit: `testkit.NewFakeWindow()` implements `Window`. It records sends
(`Sent(t)`) and events (`Events()`), supports `FailTrySend(true)`,
`SetQueuedBytes(n)`, `Detail()`, `CloseReason()`, and a once-closed `Done()`.
Ticks: `IssueTick()` returns a fresh id that `TickAge` reports as ok with age
0, and `SetTickAge(id, age, ok)` sets what `TickAge` returns for any id
(unknown ids are `!ok`); no TICK is recorded in `Sent`. Host: `Host()` is a
`*testkit.FakeHost` (`SetSummonOpen`, and it honours `SetSummonLinkDegraded`
from anyone); `SetHost(h)` shares one `NewFakeHost()` between windows.
`SetPublicKey(pk)` sets what `PublicKey()` returns (nil until set). After Close it refuses sends like the real window
(`ErrClosed` / false, not recorded). It happens to copy bodies; do not rely on it. It is concurrency-safe and does
not enforce Spec sizes. See `capability/echo/echo_test.go`.

```go
w := testkit.NewFakeWindow()
h, _ := myCap.Open(context.Background(), w, nil)
h.Frame(tSelect, []byte("front"))
got := w.Sent(tStatus)
```

End to end: drive a real server with the bench client (`client.Dial`) against
the ECHO capability or yours (`cmd/agw/main_test.go`). For a capability with
`TickHz > 0`, run a `Recv` loop: it takes TICKs in (never returns them) and
`Session.NewestTick()` exposes the newest, and `client.TickMaxAge` is the
freshness bound for echoing it. SUMMON's bench, the phone through agw's relay
to summonhelper and a mock QtCarServer, is `cmd/agw/summon_e2e_test.go`
(`go test ./cmd/agw/ -run TestSummon`; see `docs/SUMMON.md`).
