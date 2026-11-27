# Shared Car Gateway Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One authenticated car-side gateway that the phone reaches through the Pi. It does the shared login (allowlist, signed challenge, HPKE Auth arm, KeyRole) and hosts pluggable capabilities. The camera session builds CAMERA/TALK/DASHCAM on top of it first; SUMMON follows in its own plan.

**Architecture:** A static Go binary (`agw`) runs on the car's MCU as an unprivileged uid, bound to the car's Wi-Fi address only. Phone → Pi (tailnet TCP) → car (Wi-Fi TCP); the Pi only forwards bytes. Each window is one TCP connection with its own HPKE context. Capabilities implement a small Go interface and register in `main`. The phone side is a Swift module in the airgapp app (CryptoKit HPKE), with a thin JS API.

**Tech Stack:** Go (car binary: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`), `github.com/cloudflare/circl/hpke`, `github.com/godbus/dbus/v5`, `golang.org/x/sys/unix`; Swift/CryptoKit (iOS 17+, macOS 14+ for tests); Expo module (TypeScript); the Pi's Go service (`airgapp/rpi`).

**Spec:** `~/Work/tesla-dump/2026.8.6/re/bridge/PLAN-B2-summon-productionization-2026-11-19.md` (v1.3.7): §4.1–4.4, §4.7, §4.9, §9 R3/R7/R10. Read §4.9 in full before Task 6.

**Repos:**
- `GW` = `~/Work/airgapp/gateway` (new; `git init` in Task 1; Go module `airgapp/gateway`).
- `RPI` = `~/Work/airgapp/rpi` (existing git repo; work on branch `feat/gateway-forward`).
- `MOB` = `~/Work/airgapp/mobile` (existing git repo; work on branch `feat/gateway-client`).

## Global Constraints

- HPKE: mode **Auth** (0x02), KEM **DHKEM(P-256, HKDF-SHA256)** (0x0010), KDF **HKDF-SHA256** (0x0001), AEAD **AES-256-GCM** (0x0002). Go's stdlib `crypto/hpke` is Base-only: use circl `Receiver.SetupAuth`.
- Challenge: N = 128-bit CSPRNG; expiry ≤ **5 s** on **CLOCK_BOOTTIME**; N single-use; signed **ECDSA P-256 / SHA-256** by the gateway's long-term key.
- Arm accepted only if `com.tesla.Auth.KeyRole(pkS, 0, false)` ∈ {**2 OWNER, 3 DRIVER**}, the owner of `com.tesla.Auth` has the uid of user `authd` (403 on this car; look it up by name in `/etc/passwd`), and every authority reply arrives within **500 ms**. Fail closed on any error. *(Amended 2026-11-20: the role now comes from `KeyInfo`, see the amendment below.)*
- `pkS` is exactly **65 bytes**, uncompressed, on-curve, and present in the owner allowlist **before any D-Bus call**.
- Pre-auth: ≤ **2** connections, each with a **2 s** deadline for the whole pre-auth phase; hello rate ≤ **5/s**; every inbound frame ≤ **1 KiB** before arm.
- After arm: inbound control ≤ 1 KiB; TALK PCM ≤ **16 KiB**; outbound H264 access units ≤ **1 MiB**; DASHCAM chunks ≤ **64 KiB**.
- At most **one window per capability**; a new connection can never touch an open window.
- CAMERA and DASHCAM: phone heartbeat every **5 s**, lease **20 s**; expiry → teardown.
- Bind the car's Wi-Fi-side address only. Refuse `0.0.0.0`, `::`, `192.168.90.0/24`, `100.64.0.0/10` and loopback unless `-bench` is set.
- The gateway itself never writes car settings (DataValues), never sends anything to `192.168.90.0/24` (R22), and never calls `Auth.Decrypt`, `RouteMessageBytes`, `com.tesla.Debug` or `CommandRouter.*`.
- Long-term key file mode **0400**, owned by the gateway uid, generated on the car; never in a package payload.
- Session log: key fingerprint, capability, detail, start, end, end reason. **Never media or PCM.**
- No secrets in logs, stdout or test output (keys, VIN, tailnet names).

## Amendment 2026-11-20: the role comes from KeyInfo

**Car finding.** uid 4950 had no passwd entry, so dbus-daemon refused its connection outright. The camera installer's per-boot `/etc/passwd` overlay gives it one (not this repo). With the overlay, `com.tesla.Auth.KeyRole` is still denied: the bus policy grants it to `command-router` and root only. `com.tesla.Auth.KeyInfo` is open to any connected uid. Read as 4950 it listed the phone key (Id `<key id>`, role 2, form factor 6, seconds left 0), and `GetNameOwner` / `GetConnectionUnixUser` answered (uid 403).

**Change.** `agw` never calls `KeyRole`. It calls `KeyInfo()` (no arguments, reply `a(ayayuut)`: Id, PublicKey, Role, FormFactor, SecondsLeft). The RE of authd sha256 `33d62d9e818afcb9b4a4af8cec6ecda2ff1801c5a667bd527e39a2b4f6f477e9` (`tesla-dump/2026.8.6/re/bridge/RE-gw-bus-access-2026-11-20.json`) found that a listed key's Role is what `KeyRole(pk, 0, false)` returns: same list, same accessor. A key that is not listed is denied.

**Status: interim, step 1 of the RE's design A.** This gives the Wi-Fi-facing agw a default-context bus connection. On this car that connection may call any member of `com.tesla.DisplayManager` (including `SetPower` and `BacklightControl`) and `com.tesla.Videod` (RE car check 6, read 2026-11-20), so a compromised agw could blank the centre display. Don't run the bus-connected agw in a car that is driven. Step 2 moves these calls into the root helper (a camhelper auth op that reuses this package) and removes the overlay, so agw has no bus access at all, enforced by dbus-daemon itself.

**Checks** (`internal/authority`; any error denies, all inside `Check`'s 500 ms):
1. `GetNameOwner(com.tesla.Auth)` gives a unique name. Its `GetConnectionUnixUser` must be authd's uid from `/etc/passwd`. `KeyInfo` is sent to that unique name at `/Auth`, never to `com.tesla.Auth`, so a name handed over after the check is never asked.
2. The reply is exactly one `a(ayayuut)`, checked by type assertion (godbus `Store` silently turns an `int32` into a `uint32`).
3. `pkS` is 65 bytes starting `0x04`. Exactly one row has that PublicKey (none or two or more: deny). Its Id is `SHA-1(pkS)[:4]`, SecondsLeft is 0 and Role ≤ 11.
4. Never authorize on the 4-byte Id or on `GetPubKey`.
5. Role 2 or 3, as before (`authority.Check` and the `Oracle` interface are unchanged).

**authd pin.** The SHA-256 of `/usr/bin/authd` must equal the hash above. It is computed once, on the first check. A mismatch fails every check with "authd changed … KeyInfo must be re-verified". `serve` and `probe-auth` take `-authd-sha256` for a re-verified build; the pin cannot be turned off.

**Lazy oracle.** `NewDBus` connects nothing. The first check hashes authd, reads `/etc/passwd` and dials the bus. A failure fails that check closed and is retried at most once a second; a dropped connection is dialled again. `serve` no longer exits (and runit no longer restart-loops it) when the bus is unavailable.

**After an OTA.** If every arm fails with "authd changed", redo the RE for the new authd before passing its hash with `-authd-sha256`: KeyInfo and `KeyRole(pk, 0, false)` still read the same list and role; the reply is still `a(ayayuut)` with the same role values; nothing sets SecondsLeft; the bus policy still lets 4950 call KeyInfo (and only `command-router` and root call KeyRole); authd still runs as user `authd`. Re-run RE car checks 1 and 4-6 (hashes, KeyInfo shape, the Auth and Webrtc policy, the default-context grants). Then run `probe-auth` as 4950 and expect `owner=ok role=2 fp=<fp> id=<key id> ff=6 secs=0` (fp is the allowlist fingerprint, id is authd's key Id). On the car the flag only takes effect once it is in the runit run script, which the camera installer generates.

**Not covered here.** Re-checking open windows when keys change (`KeysUpdated` or a timer); today only the allowlist revokes at once. Moving the bus calls into a root helper so that agw has no bus access at all. *(Both done in Step 2, below.)*

## Amendment 2026-11-20 (Step 2): agw signs in through camhelper

**Change.** agw has no system bus access at all (design A, the owner's choice). camhelper (root) asks authd with the checks above and answers agw on `/run/airgapp-camhelper/auth.sock` (`camhelper/authproto`, `internal/authsrv`; `docs/CAMERA-HELPER.md`, "Sign-in socket"). The passwd overlay is gone (5ee219f). This amendment is agw's side.

- **Oracle.** `authority.NewHelper` replaces `NewDBus` in agw. One persistent request connection; the helper's end must be uid 0 (SO_PEERCRED); one request at a time, each within 400 ms from the call, the wait for the connection included (or the caller's earlier deadline); strict parsing. A key answer must carry kid = SHA-1(pk)[:4], SecondsLeft 0 and role ≤ 11. Any I/O error, timeout or protocol violation denies and closes the connection; it is dialled again at most once a second, and only by a request with at least 50 ms left (one already out of time touches nothing). `Check` asks the helper once, `key`, which runs the owner check first; roles 2/3 are unchanged. Sign-ins and re-checks have a request connection each, so a re-check never waits behind a sign-in. With two sign-ins at once (the pre-auth limit) each helper answer must come within about 200 ms; a re-check has the helper's full 400 ms.
- **Error classes.** `internal/authority` exports `ErrAuthOwner`, `ErrKeyUnknown`, `ErrKeyAmbiguous`, `ErrShape`, `ErrBus` and `ErrAuthdChanged`, wrapped at every error site. The helper's codes map to them; any other code, and no usable answer, is `ErrHelper`. Every one denies.
- **Re-checks.** Every open window is checked again with `Check`: on each `keys_changed` from the helper's watch, every 30 s, and every 10 s while the watch is down or its connection is lost. `keys_changed` passes start at least 1 s apart: the first runs at once, one held back runs at the next tick and covers every change before it starts, so a burst of notices costs the helper's rate limit (shared with sign-ins) one request per open key a second. A failed or unreachable check revokes the window: BYE `revoked` and `Teardown("revoked")`, as for an allowlist revocation; the session log's reason says which trigger and why.
- **Bus self-test.** `serve` (not `-bench`) opens a private bus connection with a 2 s handshake and closes it (`authority.ProbeSystemBus`: reachable, refused, or no verdict). Only a definite refusal passes, and only two probes in a row: no socket (ENOENT, ECONNREFUSED, EACCES), every mechanism `REJECTED`, or a disconnect right after `BEGIN`, which is how dbus-daemon refuses a uid it cannot resolve. That disconnect is also what dbus-daemon does to the oldest connection that has not finished Hello when more than `max_incomplete_connections` are incomplete (64 by default; the car's dbus-daemon 1.14.10 config sets no `<limit>`), so a single refusal is not a verdict; a refusal of the uid repeats every time. A probe without a verdict (a timeout, EAGAIN, a close before `BEGIN`, an error reply to Hello) is never a pass, since a busy bus may still let the uid in, and it ends a run of refusals; 4 probes at most. If the bus takes the connection or no two probes in a row refuse, agw logs `BUS SELF-TEST FAILED`, waits 60 s and fails, so runit retries slowly. dbus-daemon caches uid lookups: after the overlay test uid 4950 can reach the bus until the car reboots or the bus reloads its configuration.
- **Flags.** `serve` and `probe-auth` drop `-authd-sha256` and take `-auth-socket`. The authd pin and the post-OTA steps above now apply to camhelper's `-authd-sha256`. `probe-auth` asks the helper only and prints the same lines; run it as 4950 after installing.

**Covered since the merge into feat/camera-capabilities.** The helper's `classify()` maps the sentinels with `errors.Is`, so agw sees `key_unknown` and the other classes instead of `auth_failed`; `helper_e2e_test.go` now requires `key_unknown` for an unlisted key.

## Wire protocol (shared by all tasks)

**Framing.** Every message is `u32 big-endian length ‖ body`. The reader checks the length against the current limit **before** allocating.

**hello** (phone → car, plaintext, 71 bytes): `"AGW1"` ‖ `ver` (`0x01` or `0x02`, below) ‖ `cap` (u8) ‖ `pkS` (65).
Capability ids: `0x01` SUMMON, `0x02` CAMERA, `0x03` DASHCAM, `0x7F` ECHO (test only). Any other `ver` is refused.

**challenge, version 1** (the answer to `ver=0x01`; car → phone, plaintext, 185 bytes): `N` (16) ‖ `pkE` (65) ‖ `expiry_ms` (u64 BE, BOOTTIME ms) ‖ `binding` (32) ‖ `sig` (64, raw r‖s).
- `binding = SHA-256("airgapp-gw/1 car" ‖ VIN)`.
- `sig` signs `SHA-256("airgapp-gw/1 challenge" ‖ hello ‖ challenge-without-sig)`.
- The phone verifies `sig` with the gateway key it pinned (the QR's `airgapp-gw:1:` pin).

**challenge, version 2** (amendment 2026-11-26, the answer to `ver=0x02`; 250 bytes): version 1's body, then `pkG`, then the signature:
`N` (16) ‖ `pkE` (65) ‖ `expiry_ms` (8) ‖ `binding` (32) ‖ `pkG` (65) ‖ `sig` (64, raw r‖s). The body is 186 bytes.
- `pkG` is the uncompressed P-256 public key of the key that signs: agw's long-term key, or for the inner SUMMON
  session summonhelper's. It is inside the signed body, so it cannot be swapped.
- `sig` signs `SHA-256("airgapp-gw/2 challenge" ‖ hello ‖ challenge-without-sig)` (a domain of its own: a v2
  signature never verifies as v1, nor the other way round).
- The phone checks `pkG` is on the curve and verifies `sig` with it. With a pinned key it also requires `pkG` to equal
  the pin; without one it may trust `pkG` on first connect and pin it. `binding` is checked as in version 1.
- Code: `proto.ParseChallengeV2`, `SignChallengeV2`, `VerifyChallengeV2`; test vectors `testdata/vectors-v2.json`
  (version 1's stay in `testdata/vectors.json`, unchanged). A `ver=0x01` hello gets the version 1 challenge byte for
  byte as before, so existing phone builds keep working.

**Who gets a challenge** (amendment 2026-11-26, no allowlist): the car asks authd about `pkS` at the hello and sends
a challenge only when it is OWNER or DRIVER; any other key gets the silent close and no byte. authd is asked again after
the ARM (the gate, since only the ARM proves possession of `pkS`).

**HPKE.** `transcript = hello ‖ challenge`: the exact challenge bytes sent, signature included (185 bytes for version 1,
250 for version 2).
- `info = "airgapp-gw/1" ‖ 0x00 ‖ SHA-256(transcript)` (45 bytes).
- First phone → car frame: `enc` (65) ‖ `ciphertext`. Later phone → car frames are `ciphertext` only, opened in order by the one receiver context. `aad` is empty.
- Car → phone: `key = Export("airgapp-gw/1 s2c key", 32)`, `base = Export("airgapp-gw/1 s2c nonce", 12)`; nonce = `base` XOR (u64 BE counter, in the last 8 bytes); AES-256-GCM, empty aad; the counter starts at 0 and the phone requires strict order.

**Plaintext messages** are `type` (u8) ‖ `body`.

| Dir | Type | Name | Body |
|---|---|---|---|
| in | 0x01 | ARM | JSON `{"n": "<32 hex>", "cap": <id>, "exp": <expiry_ms>, "params": {…}}` |
| in | 0x02 | HEARTBEAT | empty |
| in | 0x03 | CLOSE | empty |
| in | 0x10–0x1F | SUMMON | reserved (SUMMON plan) |
| in | 0x20–0x3F | CAMERA / TALK | camera session |
| in | 0x40–0x4F | DASHCAM | camera session |
| out | 0x80 | READY | JSON `{"lease_ms": <int>}` |
| out | 0x81 | BYE | UTF-8 reason, ≤ 64 bytes, then the car closes |
| out | 0x82 | TICK | reserved (SUMMON plan) |
| out | 0xA0–0xBF | CAMERA / TALK | camera session |
| out | 0xC0–0xCF | DASHCAM | camera session |

Before the HPKE context exists (bad hello, not allowlisted, bad open, failed authority check) the car closes with **no** message, so it gives no oracle.

## Review Focus

1. **Phone drops mid-window and reconnects at once** (app backgrounded, Wi-Fi blip): the old window must finish teardown and free its capability slot within 100 ms, and the new hello must then succeed — never a stuck "window open". Test in Task 6.
2. **The car dozes and resumes** between challenge and ARM: expiry runs on BOOTTIME (includes suspend), so a resumed ARM is refused. Go's monotonic clock excludes suspend, so the clock must be injected and read from `CLOCK_BOOTTIME` on Linux. Test in Task 6.
3. **A slow or stalled sender before auth** (3 bytes, then nothing): the 2 s deadline covers partial frames, and a third pre-auth connection is refused while two stall. Test in Task 6.
4. **A hostile length prefix** (`0xFFFFFFFF`): closed before any allocation. Test in Task 2.
5. **Revocation while a window is open:** removing a key from the allowlist file closes that key's windows within 1 s. Test in Task 6.

---

### Task 1: Repo, framing and the bench clock

**Files:**
- Create: `GW/go.mod`, `GW/.gitignore`, `GW/Makefile`, `GW/README.md`
- Create: `GW/internal/wire/frame.go`, `GW/internal/wire/frame_test.go`
- Create: `GW/internal/clock/clock.go`, `GW/internal/clock/boottime_linux.go`, `GW/internal/clock/boottime_other.go`, `GW/internal/clock/clock_test.go`

**Interfaces:**
- Produces: `wire.ReadFrame(r io.Reader, max int) ([]byte, error)`, `wire.WriteFrame(w io.Writer, body []byte) error`, `wire.ErrTooLarge`.
- Produces: `clock.Clock` interface `{ BootMS() uint64 }`; `clock.Boot()` (real: `CLOCK_BOOTTIME` on Linux, monotonic fallback elsewhere); `clock.Fake` with `Advance(ms uint64)`.

- [ ] **Step 1:** `git init` in `GW`; `go mod init airgapp/gateway`; Makefile targets `test` (`go test ./...`) and `car` (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o out/agw ./cmd/agw`).
- [ ] **Step 2: Failing tests** `TestFrameRoundTrip` (bodies of 0, 1, 1024 bytes), `TestFrameTooLarge` (length 1025 with max 1024 → `ErrTooLarge`), `TestFrameHostileLength` (prefix `ff ff ff ff`, max 1024 → `ErrTooLarge` and the reader consumed only 4 bytes), `TestFakeClockAdvance`.
- [ ] **Step 3:** Run `go test ./internal/...` → FAIL (undefined).
- [ ] **Step 4:** Implement both packages. `ReadFrame` uses `io.ReadFull` for the 4-byte prefix, checks `max`, then `io.ReadFull` for the body.
- [ ] **Step 5:** `go test ./internal/...` → PASS.
- [ ] **Step 6:** Commit `feat: framing and boot clock`.

### Task 2: hello, challenge, signatures, transcript and golden vectors

**Files:**
- Create: `GW/internal/proto/hello.go`, `GW/internal/proto/challenge.go`, `GW/internal/proto/info.go`, `GW/internal/proto/proto_test.go`
- Create: `GW/testdata/vectors.json` (generated by `TestWriteGoldenVectors` when `-update` is set; committed)

**Interfaces:**
- Produces: `proto.Hello{Cap byte; PkS [65]byte}`, `proto.ParseHello(b []byte) (Hello, error)`, `(Hello).Bytes() []byte`.
- Produces: `proto.Challenge{N [16]byte; PkE [65]byte; ExpiryMS uint64; Binding [32]byte; Sig [64]byte}`, `proto.SignChallenge(k *ecdsa.PrivateKey, hello []byte, c *Challenge) error`, `proto.VerifyChallenge(pub *ecdsa.PublicKey, hello []byte, c Challenge) bool`, `(Challenge).Bytes() []byte`, `proto.ParseChallenge(b []byte) (Challenge, error)`.
- Produces: `proto.Binding(vin string) [32]byte`, `proto.Info(hello, challenge []byte) []byte`, constants `proto.CapSummon=0x01, CapCamera=0x02, CapDashcam=0x03, CapEcho=0x7F`, and the type constants from the wire table (`TypeArm`, `TypeHeartbeat`, `TypeClose`, `TypeReady`, `TypeBye`).

- [ ] **Step 1: Failing tests:** `TestHelloRejects` (wrong magic, ver ≠ 1, length ≠ 71, unknown cap, `pkS` not on curve, compressed point → error); `TestChallengeSignVerify` (verifies; flipping any byte of hello or challenge fails); `TestInfoLength` (45 bytes, prefix `"airgapp-gw/1\x00"`); `TestGoldenVectors` (recomputes `vectors.json`: fixed keys, hello, challenge-without-sig, the signed digest, binding for a dummy VIN `"TESTVIN0000000000"`, info; equal byte for byte).
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement. Signatures are raw 64-byte r‖s, each half left-padded to 32 bytes. `ParseHello` validates the point with `ecdh.P256().NewPublicKey`.
- [ ] **Step 4:** `go test ./internal/proto -run . -update` once to write `vectors.json`, then `go test ./internal/proto` → PASS.
- [ ] **Step 5:** Commit `feat: hello/challenge, signatures, golden vectors`.

### Task 3: HPKE session crypto

**Files:**
- Create: `GW/internal/session/crypto.go`, `GW/internal/session/crypto_test.go`
- Create: `GW/testdata/rfc9180_a33.json` (the RFC 9180 A.3.3 vector: DHKEM(P-256), HKDF-SHA256, AES-128-GCM, Auth mode)

**Interfaces:**
- Consumes: `proto.Info`.
- Produces: `session.Suite` (the Global Constraints suite); `session.OpenArm(skE *ecdh.PrivateKey, pkS []byte, info, firstFrame []byte) (*Inbound, []byte, error)` → the receiver context plus the first plaintext; `(*Inbound).Open(ct []byte) ([]byte, error)` (strict order); `session.NewOutbound(in *Inbound) (*Outbound, error)`; `(*Outbound).Seal(pt []byte) []byte`. Test helper sender: `session.TestSender(skS *ecdh.PrivateKey, pkE []byte, info []byte) (enc []byte, s *TestSealer, err error)` with `Seal(pt) []byte` and `OpenS2C(ct) ([]byte, error)` (used by later tests and by the Go bench client in Task 7).

- [ ] **Step 1: Failing tests:** `TestRFC9180AuthKAT` (circl with the A.3.3 suite reproduces the vector's `enc`, first ciphertexts and exporter values); `TestArmRoundTrip` (AES-256 suite; the sender seals 3 messages; `OpenArm` + `Open` return them); `TestOutOfOrderFails` (opening message 3 before 2 fails); `TestWrongSenderFails` (sealed by another key with the right `pkS` claimed → error); `TestS2CRoundTrip` (outbound seal → `OpenS2C` in order; a replayed or reordered frame fails).
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement with circl `hpke.Suite.NewReceiver(skE, info).SetupAuth(enc, pkS)`. The outbound key and base nonce come from `Export` with the labels in the wire section.
- [ ] **Step 4:** Run → PASS.
- [ ] **Step 5:** Commit `feat: HPKE Auth session crypto`.

### Task 4: Long-term key, allowlist and authority checks

**Files:**
- Create: `GW/internal/keys/longterm.go`, `GW/internal/keys/allowlist.go`, `GW/internal/keys/keys_test.go`
- Create: `GW/internal/authority/authority.go`, `GW/internal/authority/dbus_linux.go`, `GW/internal/authority/fake.go`, `GW/internal/authority/authority_test.go`

**Interfaces:**
- Produces: `keys.Generate(path string) error` (writes PKCS#8 PEM, mode 0400; refuses to overwrite); `keys.Load(path string) (*ecdsa.PrivateKey, error)` (refuses a mode other than 0400); `keys.PinString(pub *ecdsa.PublicKey, vin string) string` = `"airgapp-gw:1:" + hex(65-byte pub) + ":" + vin`; `keys.Fingerprint(pk []byte) string` = first 8 bytes of SHA-256, hex.
- Produces: `keys.Allowlist` with `Load(path string) error`, `Contains(pk []byte) bool`, `Watch(ctx, path, onRemove func(pk []byte))` (polls mtime every 500 ms; reloads; calls `onRemove` for each removed key). File format: one hex 65-byte key per line, `#` comments; a malformed line fails the whole load and keeps the previous set.
- Produces: `authority.Oracle` interface `{ KeyRole(ctx context.Context, pk []byte) (int32, error); AuthOwnerIsAuthd(ctx context.Context) (bool, error) }`; `authority.NewDBus() (Oracle, error)` (Linux only; dest `com.tesla.Auth`, path `/Auth`, method `com.tesla.Auth.KeyRole`, args `([]byte pk, uint32 0, false)`; owner via `org.freedesktop.DBus.GetNameOwner` + `GetConnectionUnixUser`, compared with the uid of `authd` parsed from `/etc/passwd`); `authority.Fake{Role int32; Owner bool; Delay time.Duration; Err error}`; `authority.Check(ctx, o Oracle, pk []byte) error` (500 ms timeout; role must be 2 or 3). *(Superseded 2026-11-20: `NewDBus(authdSHA256)` calls `KeyInfo`, never `KeyRole`; see the amendment.)*

- [ ] **Step 1: Failing tests:** `TestKeyFileMode` (Load rejects 0644); `TestGenerateNoOverwrite`; `TestPinString`; `TestAllowlistParse` (comments, blank lines, uppercase hex OK; a 64-byte key, an off-curve key or a compressed key fails the load and keeps the previous set); `TestAllowlistWatchRemove` (removing a line calls `onRemove` within 1 s); `TestCheckRoles` (roles 2 and 3 pass; 0, 1, 4, 8, 11 fail); `TestCheckOwnerFalse`; `TestCheckTimeout` (Delay 600 ms → error); `TestCheckError`.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement. `dbus_linux.go` has build tag `linux`; on other OSes `NewDBus` returns an error.
- [ ] **Step 4:** Run → PASS. `GOOS=linux go vet ./internal/authority` is clean.
- [ ] **Step 5:** Commit `feat: long-term key, allowlist, authority oracle`.

### Task 5: Capability API and the ECHO capability

**Files:**
- Create: `GW/capability/capability.go`, `GW/capability/echo/echo.go`, `GW/capability/echo/echo_test.go`
- Create: `GW/docs/CAPABILITY-API.md` (the camera session's guide: the interface, type ranges, limits, lease, teardown guarantees, how to register, how to test with the bench client)

**Interfaces:**
- Produces (exact, other sessions build against this):
```go
package capability

type Spec struct {
    Name    string       // "CAMERA", "DASHCAM", "SUMMON", "ECHO"
    ID      byte         // the hello cap byte
    In      map[byte]int // allowed inbound types (besides ARM/HEARTBEAT/CLOSE) → max body bytes
    Out     map[byte]int // allowed outbound types (besides READY/BYE) → max body bytes
    LeaseMS int          // 0 = no lease; CAMERA and DASHCAM use 20000
    Bulk    map[byte]bool // outbound types that go on the bulk lane (H264 AUs, DASHCAM chunks);
                          // every other outbound type, and BYE, uses the priority lane and is
                          // written before any queued bulk frame
}

type Window interface {
    Send(t byte, body []byte) error          // blocks while that lane is full
    TrySend(t byte, body []byte) bool        // false if that lane is full (media drops)
    QueuedBytes() int                        // bulk-lane depth in bytes; a lock-free atomic read; bulk cap 4 MiB
    SetDetail(detail string)                 // session-log detail, e.g. "front,rear"; no media
    Close(reason string)                     // starts teardown; idempotent
    Done() <-chan struct{}
    KeyFingerprint() string
}

type Handler interface {
    Frame(t byte, body []byte) error // non-nil → teardown with that error as the reason
    Teardown(reason string)          // called exactly once, after reads stop and before BYE
}

type Capability interface {
    Spec() Spec
    PreArm(pkS []byte) error // extra gates; SUMMON adds R1/proximity; CAMERA returns nil
    Open(w Window, params json.RawMessage) (Handler, error)
}
```
- Produces: `echo.New() capability.Capability` (ID `0x7F`; inbound `0x7E` ≤ 1024 → echoes it back as outbound `0xFE`; LeaseMS 20000).
- Produces: `capability/testkit` (file `GW/capability/testkit/fakewindow.go` + test): `testkit.NewFakeWindow() *FakeWindow` implementing `capability.Window`; `Sent(t byte) [][]byte` (sends recorded per type, in order); `FailTrySend(bool)`; `SetQueuedBytes(n int)`; `Close` is idempotent and closes `Done()`; `CloseReason() string`. Used by the camera session to unit-test CAMERA/DASHCAM without the server, and by the ECHO test.

- [ ] **Step 1: Failing test** `TestEchoSpec` (ID 0x7F, In {0x7E: 1024}, Out {0xFE: 1024}, LeaseMS 20000) and `TestEchoFrame` (with a fake `Window`, a `0x7E` frame produces one `0xFE` send with the same body).
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** Run → PASS.
- [ ] **Step 5:** Write `CAPABILITY-API.md`. It must state: type ranges per capability; the core enforces the grammar (any type outside `Spec.In` tears down), the size limits and the lease; `Teardown` runs exactly once on every path (CLOSE, lease, error, revocation, connection loss, SIGTERM); capabilities never write DataValues directly (the camera session's root helper does, §4.9).
- [ ] **Step 6:** Commit `feat: capability API and ECHO`.

### Task 6: Connection state machine and window manager

**Files:**
- Create: `GW/internal/server/server.go`, `GW/internal/server/conn.go`, `GW/internal/server/window.go`, `GW/internal/server/sessionlog.go`, `GW/internal/server/server_test.go`

**Interfaces:**
- Consumes: Tasks 1–5.
- Produces: `server.Config{Key *ecdsa.PrivateKey; VIN string; Allow *keys.Allowlist; Oracle authority.Oracle; Clock clock.Clock; Caps []capability.Capability; LogPath string; MaxLogBytes int64}`; `server.New(cfg Config) *Server`; `(*Server).Serve(ln net.Listener) error`; `(*Server).Shutdown(reason string)` (tears down every window with that reason); `(*Server).Revoke(pk []byte)`.

**Behaviour (spec §4.3–4.4, §4.7, §4.9):**
1. Accept. More than 2 connections in pre-auth → close the new one at once. Hello rate > 5/s (token bucket) → close.
2. Whole pre-auth deadline 2 s, partial frames included. Read hello (≤ 1 KiB). Unknown cap or no registered capability → close. Capability window already open → close (never touch it). `pkS` not allowlisted → close (no D-Bus call yet).
3. Fresh `skE`, N; send the signed challenge (expiry = now + 5000 ms BOOTTIME).
4. Read the first frame (≤ 1 KiB): `OpenArm`; plaintext type must be ARM; JSON `n` equals N, `cap` equals the hello's cap, `exp` equals the challenge's; now ≤ expiry; N not used before (remember N for 10 s).
5. `authority.Check`; capability `PreArm`; reserve the capability slot (atomically; lost race → close); `Open`. Any failure → close without a message.
6. Send READY `{"lease_ms": LeaseMS}`. From here, inbound frames are size-checked against the largest `Spec.In` value + 1 + 16 (and ≤ 1 KiB for core types), opened in order, then dispatched: HEARTBEAT renews the lease; CLOSE → teardown "close"; a type in `Spec.In` within its limit → `Handler.Frame`; anything else → teardown "grammar".
7. Any accepted frame renews the lease. Lease expiry → teardown "lease".
7a. Outbound is two-lane (one writer goroutine per window): priority lane (READY, BYE and every type not in `Spec.Bulk`, cap 64 KiB) is always drained before the bulk lane (types in `Spec.Bulk`, cap 4 MiB). `QueuedBytes` is an atomic counter of the bulk lane. Test `TestPriorityLaneOvertakesBulk`: queue 3 MiB of bulk, then one priority frame; the client receives the priority frame before the remaining bulk frames.
8. Teardown (idempotent, ordered): stop reading → `Handler.Teardown(reason)` → send BYE(reason) best-effort with a 200 ms write deadline → close the socket → free the slot → write the session-log entry.
9. Session log: a JSON line per window `{start, end, fp, cap, detail, reason}` (wall clock for the log only), appended to `LogPath`; at `MaxLogBytes` rotate to `.1`.

- [ ] **Step 1: Failing tests** (in-process listener on `127.0.0.1:0`, `clock.Fake`, `authority.Fake`, ECHO, the Task 3 test sender):
  - `TestHappyPathEcho` (READY, echo round trip, CLOSE → BYE "close", log line written with cap ECHO and reason close);
  - `TestNotAllowlisted` (closed with no bytes, and the Fake oracle saw no call);
  - `TestRoleRejected` (role 8 → closed, no message);
  - `TestBadSignatureNeverSent` (the phone side checks the challenge signature with the pinned key; a gateway with another key fails verification);
  - `TestArmMismatch` (wrong N, wrong cap, wrong exp → closed);
  - `TestArmReplayNewConnection` (an ARM sealed for connection 1 replayed on connection 2 → closed);
  - `TestChallengeExpiredAfterSuspend` (Fake clock advanced 6000 ms before the ARM → closed) — Review Focus 2;
  - `TestPreAuthSlowloris` (two connections send 3 bytes and stall; a third is closed at once; both stalled ones are closed at 2 s) — Review Focus 3;
  - `TestHelloRate` (6 hellos within 1 s → the 6th is closed);
  - `TestSecondWindowRefused` (an ECHO window open; a second ECHO hello is closed; the first keeps working);
  - `TestReconnectAfterDrop` (kill the client socket mid-window; within 100 ms a new hello for ECHO reaches READY) — Review Focus 1;
  - `TestGrammarTeardown` (an inbound type not in `Spec.In` → BYE "grammar");
  - `TestOversizeAfterArm` (a 1025-byte ECHO body → teardown);
  - `TestLeaseExpiry` (no frames for 20 s on the Fake clock → BYE "lease"; Teardown called once);
  - `TestRevocation` (remove the key from the allowlist file → BYE "revoked" within 1 s) — Review Focus 5;
  - `TestShutdown` (`Shutdown("sigterm")` → every window gets Teardown once and BYE "sigterm").
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** `go test -race ./internal/server` → PASS.
- [ ] **Step 5:** Commit `feat: gateway state machine and window manager`.

### Task 7: The `agw` binary, bench client and car install script

**Files:**
- Create: `GW/cmd/agw/main.go`, `GW/cmd/agw/main_test.go`
- Create: `GW/client/client.go` (Go bench client: the phone protocol, used by tests and by the camera session from the laptop), `GW/client/client_test.go`
- Create: `GW/deploy/install-car.sh`, `GW/deploy/README.md`

**Interfaces:**
- Produces: CLI `agw serve -listen <ip:port> -key <path> -allow <path> -vin-file /var/etc/vin -log <path> [-bench]`; `agw keygen -key <path>`; `agw pin -key <path> -vin-file <path>` (prints the pin string for a QR); `agw allow add|rm -allow <path> <hex>`. Default port **17100**. SIGTERM → `Shutdown("sigterm")`, exit 0. `serve` sets `RLIMIT_CORE` to 0 before loading the key (spec §4.2: no core dumps).
- Produces: `client.Dial(ctx, addr string, cap byte, params any, pinned *ecdsa.PublicKey, vin string, device *ecdh.PrivateKey) (*client.Session, error)`; `(*Session).Send(t byte, body []byte) error`; `Recv() (t byte, body []byte, err error)`; `Close() error`. The client sends HEARTBEAT every 5 s when the READY lease is non-zero.

- [ ] **Step 1: Failing tests:** `TestListenRefusesForbidden` (`0.0.0.0:17100`, `192.168.90.100:17100`, `100.101.1.1:17100`, `127.0.0.1:17100` without `-bench` → error naming the rule); `TestServeEchoEndToEnd` (run `serve -bench` on loopback in-process with `authority.Fake` injected through an unexported test hook; the client completes ECHO and CLOSE); `TestPinPrintsNoPrivate`.
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** `go test ./...` → PASS; `make car` builds `out/agw` (static: `file out/agw` says "statically linked").
- [ ] **Step 5:** Write `install-car.sh` (run by the owner as root over SSH; not run by this plan): copy `agw` to `/var/lib/airgapp-gw/agw`; create `/var/lib/airgapp-gw/{key.pem,allow,log}` owned by uid:gid **4950:4950** (no passwd entry needed for the files; *superseded 2026-11-20: the bus needs the camera installer's passwd overlay, see the amendment*); `keygen` once; add the per-boot rule `iptables -I INPUT -i wlan0 -s <pi-wifi-ip> -d <car-wifi-ip> -p tcp --dport 17100 -j ACCEPT`; start with `chpst -u :4950:4950 /var/lib/airgapp-gw/agw serve -listen <car-wifi-ip>:17100 …`. The script prints what it will do and asks for `yes` before each write. It ends with a clearly marked, empty section `# ==== CAMERA ROOT HELPER (owned by the camera session) ====` with placeholders for the helper binary, its runit service, its socket path and its uid. `README.md` documents the Phase 1a probe below and the R22 rule.
- [ ] **Step 6: Car check (owner present, read-only, no motion):** with the binary copied but not serving, the owner runs `chpst -u :4950:4950 /var/lib/airgapp-gw/agw probe-auth` — a subcommand that only calls `AuthOwnerIsAuthd` and `KeyRole` for the allowlisted phone key, and prints `owner=ok role=2` or the D-Bus error. This answers PLAN Phase 1a for this uid: if the system bus denies uid 4950, CAMERA cannot arm, and the fix goes back to design. Add `probe-auth` in this task with a test using `authority.Fake`. *(Superseded 2026-11-20: the bus denied uid 4950 on the car; probe-auth now uses `KeyInfo`, see the amendment.)*
- [ ] **Step 7:** Commit `feat: agw binary, bench client, car install script`.

### Task 8: CryptoKit interop (Swift ↔ Go)

**Files:**
- Create: `GW/interop/swift/GatewayCrypto.swift` (the phone's crypto: verify challenge, `HPKE.Sender` Auth with `.P256_SHA256_AES_GCM_256`, s2c export open; **this exact file is copied into the app in Task 10**)
- Create: `GW/interop/swift/main.swift` (macOS CLI: `seal <vectors.json>` writes `GW/testdata/cryptokit_arm.json`; `opens2c <file>` checks Go-sealed s2c frames)
- Create: `GW/internal/session/interop_test.go`

**Interfaces:**
- Produces (Swift, used by Task 10): `enum GatewayCrypto { static func verifyChallenge(pinned: P256.Signing.PublicKey, hello: Data, challenge: Data) -> Bool; static func info(hello: Data, challenge: Data) -> Data; static func makeSender(device: P256.KeyAgreement.PrivateKey, pkE: P256.KeyAgreement.PublicKey, info: Data) throws -> (enc: Data, sender: HPKE.Sender); static func s2cKeys(sender: HPKE.Sender) throws -> (key: SymmetricKey, base: Data); static func openS2C(key: SymmetricKey, base: Data, counter: UInt64, ct: Data) throws -> Data }`.

- [ ] **Step 1: Failing Go test** `TestCryptoKitArm` (reads `cryptokit_arm.json`: hello, challenge, enc‖ciphertexts sealed by CryptoKit with the Task 2 fixed keys; `OpenArm` + `Open` recover the plaintexts) and `TestGoS2CForSwift` (writes `testdata/go_s2c.json` for the Swift side).
- [ ] **Step 2:** Run → FAIL (file missing).
- [ ] **Step 3:** Implement the Swift file and CLI; run `swiftc -O interop/swift/*.swift -o out/interop && out/interop seal testdata/vectors.json && out/interop opens2c testdata/go_s2c.json` → prints `ok`.
- [ ] **Step 4:** `go test ./internal/session -run CryptoKit` → PASS. The Swift verifier also rejects `vectors.json` with one signature byte flipped.
- [ ] **Step 5:** Commit `test: CryptoKit <-> circl interop`.

### Task 9: Pi forwarder (RPI repo)

**Files:**
- Create: `RPI/internal/services/gwforward.go`, `RPI/internal/services/gwforward_test.go`
- Modify: `RPI/internal/services/services.go` (construct and start it), `RPI/internal/services/firewall.go` (next to the tailscale0 rules at ~488-489: `iifname "tailscale0" tcp dport 17100 accept`), the config/settings where the Pi stores ports (add `gw_car_addr`, default empty = disabled, and `gw_car_mac`), and the dnsmasq config writer: when `gw_car_mac` and `gw_car_addr` are both set, emit `dhcp-host=<mac>,<ip>` so the car's Wi-Fi address (and the car's iptables rule from Task 7) stays valid; test `TestStaticLeaseForCar`

**Interfaces:**
- Produces: `NewGatewayForward(listen, target string, maxConns int) *GatewayForward`; `Start() error`; `Stop()`. Listens on the Pi's tailnet address `:17100` only; copies bytes both ways to `target` (`<car-wifi-ip>:17100`); at most **6** concurrent connections; no parsing, no logging of payloads, no TLS. Disabled while `gw_car_addr` is empty.

- [ ] **Step 1: Failing tests:** `TestForwardCopiesBothWays` (echo server as target); `TestForwardMaxConns` (7th connection closed at once); `TestForwardHalfClose` (client CloseWrite propagates; the target's reply still arrives); `TestForwardTargetDown` (client closed within 3 s, no goroutine leak); `TestFirewallHasGatewayRule` (the generated nftables text contains the tailscale0 17100 rule only when enabled).
- [ ] **Step 2:** Run (`go test ./internal/services -run 'Forward|GatewayRule'`) → FAIL. **Step 3:** Implement. **Step 4:** Run → PASS, and `go test ./...` in RPI still passes.
- [ ] **Step 5:** Commit on `feat/gateway-forward`: `feat: dumb TCP forwarder for the car gateway`.

### Task 10: Phone gateway client (MOB repo)

**Files:**
- Create: `MOB/modules/expo-gateway-client/` (`expo-module.config.json`, `ios/GatewayClient.podspec`, `ios/GatewayCrypto.swift` (copied verbatim from Task 8), `ios/GatewaySession.swift`, `ios/GatewayClientModule.swift`, `index.ts`)
- Create: `MOB/src/services/gateway/client.ts`, `MOB/src/services/gateway/client.test.ts`, `MOB/src/services/gateway/pin.ts`, `MOB/src/services/gateway/pin.test.ts`

**Interfaces:**
- Consumes: the Task 8 Swift API; the device key from the same Keychain item `KeychainKey` uses (service `airgapp.passiveentry`, account `deviceKeyHex`; 32-byte scalar hex → `P256.KeyAgreement.PrivateKey(rawRepresentation:)`).
- Produces (Swift, for native media consumers such as a VideoToolbox decoder): `public final class GatewaySession { init(host: String, port: UInt16, cap: UInt8, params: Data, pinned: P256.Signing.PublicKey, vin: String); var onFrame: ((UInt8, Data) -> Void)?; var onReady: ((Int) -> Void)?; var onClose: ((String) -> Void)?; func setNativeHandler(types: Set<UInt8>, queue: DispatchQueue, _ h: @escaping (UInt8, Data) -> Void); func start(); func send(type: UInt8, body: Data); func close() }`. Types claimed by a native handler are delivered in order on that serial queue and are **never** forwarded to JS (media must not cross the RN bridge); a session is reachable from other native modules through `GatewayRegistry.session(id:)`. It uses `NWConnection`, enforces the same frame limits inbound (car → phone: ≤ 1 MiB + overhead), sends HEARTBEAT every 5 s when the lease is non-zero, and closes on any decrypt or order failure.
- Produces (JS): `openGateway({host, port, cap, params, pin}): Promise<string /* sessionId */>`; `sendGateway(id, type, bodyB64)`; `closeGateway(id)`; events `gatewayReady {id, leaseMs}`, `gatewayFrame {id, type, bodyB64}`, `gatewayClose {id, reason}`. `pin.ts`: `parsePin(s: string): {pubHex: string, vin: string}` for `"airgapp-gw:1:<130 hex>:<17-char VIN>"`; anything else throws.

- [ ] **Step 1: Failing tests:** `pin.test.ts` (valid pin parses; wrong prefix, version, hex length or VIN length throws); `client.test.ts` (with the native module mocked: `openGateway` passes the parsed pin through, events route by session id, `closeGateway` is idempotent).
- [ ] **Step 2:** Run `pnpm jest src/services/gateway` → FAIL. **Step 3:** Implement. **Step 4:** Run → PASS; `pnpm tsc --noEmit` clean.
- [ ] **Step 5:** iOS build: `npx expo run:ios` builds with the module linked (iOS 17 deployment target confirmed in the Podfile).
- [ ] **Step 6: Bench end-to-end:** on the laptop, `agw serve -bench -listen 127.0.0.1:17100` with an allowlist holding the simulator app's device public key and a Fake oracle build flag (`-fake-role 2`, accepted only with `-bench`). A debug screen (dev builds only) opens ECHO and shows the round trip. Expected: READY, echo, CLOSE → BYE "close".
- [ ] **Step 7:** Commit on `feat/gateway-client`: `feat: gateway client module`.

### Task 11: Handoff to the camera session

- [ ] **Step 1:** Send the camera session (`Remote webcams via WebRTC`) the paths of `GW/docs/CAPABILITY-API.md`, the type ranges, the bench client and the Swift `GatewaySession` API, and the Task 7 car check result.
- [ ] **Step 2:** Record in `GW/README.md`: what is done, what still needs the car (install, `probe-auth`), and that SUMMON is the next plan (ticks, deadman, supervisor finish hook, R1/proximity `PreArm`, QtCarServer D-Bus).
