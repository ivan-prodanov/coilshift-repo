# Car Dials Out Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** agw connects out to the Pi, and the Pi pairs each car connection with a phone connection, so the gateway
works behind the car's Wi-Fi NAT.

**Architecture:**
- A new gateway package, `internal/dialout`, is a `net.Listener` that keeps idle outbound connections to the Pi. Its
  `Accept` returns one only after the pre-pairing handshake. `agw serve -dial` hands it to the unchanged server core.
- In the rpi repo, `GatewayForward` stops dialling the car. Instead it pools the car's connections (accepted on the
  hotspot address, port 17101) and pairs each phone connection with one; the existing pipe stays.
- A lab command and an env-gated test then run both real implementations together on loopback.

**Tech Stack:** Go (gateway module `airgapp/gateway`, go 1.26.2; rpi module
`github.com/iulianfsdro/rpi-network-filter`, go 1.23); standard library only, with no new dependencies.

**Spec:** `~/Work/airgapp/gateway/docs/superpowers/specs/2026-11-21-car-dials-out-design.md` (feat/summon
f6cd986).

## Global Constraints

**Where the work lands.** Nothing is pushed, and nothing contacts the car or the Pi.

| Repo | Worktree | New branch | Cut from |
|---|---|---|---|
| gateway | `~/Work/airgapp/gateway-dialout` | `feat/agw-dial-out` | `feat/camera-capabilities` d6f1d72 |
| rpi | `~/Work/airgapp/rpi-pairing` | `feat/gateway-pairing` | `feat/gateway-forward` be3120a |

Leave the main rpi checkout's untracked files alone.

**Protocol constants, identical in both repos:**
- The hello is the 8 bytes `AGWDIAL1`, sent car to Pi right after connecting. The Pi reads it within 3 s.
- The control bytes are `K` (keepalive, either way), `G` (Pi to car) and `A` (car to Pi). Before pairing, only the
  hello, `K`, `G` and `A` are allowed; anything else closes the connection.
- While idle, each side sends `K` every 10 s, and each closes after 25 s with nothing heard.

**Pi timings:** wait up to 2 s for `A`, then close that car connection and try the next. Wait up to 3 s for an idle
car connection, then close the phone. At most 4 idle car connections. After `A`, the existing pipe applies (idle
120 s, 5 s after the car's EOF).

**Car side:**
- Keeps 3 idle connections and redials at once when one is handed out.
- Makes at most 4 dials in any 1 s window (a sliding window, like `server.takeHelloToken`).
- One supervisor goroutine makes the dials, one at a time. Consecutive failed dials back off, doubling from 1 s to
  10 s, and a successful dial resets that. A connection that ends before pairing is replaced after 1 s.
- Reads one byte at a time before pairing, and never sends a `K` after `A`.

**agw flags:** `serve -dial <ip>[:port]`, default port `17101`. Exactly one of `-dial` and `-listen`; `-listen` stays.
`-dial` follows `-listen`'s address rules:
- with `-bench`, loopback only;
- otherwise it refuses unspecified, loopback, the vehicle LAN 192.168.90.0/24 (R22) and the tailnet range
  100.64.0.0/10.

**Pi config:** car connections are accepted on `cfg.LANGateway:17101`, and only from `gw_car_addr`. The nft rule is
`iifname "<cfg.LANInterface>" ip saddr <gw_car_addr> ip daddr <cfg.LANGateway> tcp dport 17101 accept`, present
exactly when the existing tailnet 17100 rule is.

**Logging:** the Pi logs no payload bytes, keys or phone addresses (`gwforward.go`'s existing rule), and agw logs no
key or VIN.

**Checks:**
- gateway: `gofmt -l .` empty; `go vet ./...` for darwin and for `GOOS=linux GOARCH=amd64`;
  `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`; `go test -race -count=3 ./...`.
- rpi: `gofmt -l .` empty; `go vet ./...`; `GOOS=linux GOARCH=arm64 go build ./...`;
  `go test -race -count=3 ./internal/services/...`.

**Out of scope:** the car's egress lock and the agw run line, which are done in d6f1d72; the phone; and deploying to
the Pi or the car (the camera session's runbook).

## Review Focus

1. **The Pi restarts or the hotspot blips** while the car holds idle connections. The car redials (with backoff) and
   is pairable again within about 2 s of the Pi's return. Test: Task 1 `TestPiRestart`.
2. **A car connection dies silently** (the MCU dozes, or the module's NAT entry expires). Both sides drop it after
   25 s of silence, and a phone arriving meanwhile skips it within 2 s. Tests: Task 1 `TestSilenceClosesAndRedials`,
   Task 3 `TestPairingSkipsDeadCar`.
3. **No car is connected** (the car is asleep). The phone is closed within 3 s and never hangs. Test: Task 3
   `TestPhoneClosedWithoutCar`.
4. **Several windows open at once** (Summon plus camera). Each phone gets its own car connection, none gets two, and
   the car refills. Tests: Task 3 `TestEachCarPairedOnce`, Task 1 `TestRefillAfterHandout`.
5. **A keepalive crosses the pairing.** It never reaches the gateway or the phone. Tests: Task 1
   `TestNoKeepaliveAfterAck`, Task 3 `TestNoKeepaliveAfterGo`.

---

### Task 1: `internal/dialout`, the dialing listener (gateway)

**Files:**
- Create: `internal/dialout/dialout.go`, `internal/dialout/dialout_test.go`
- Create: `internal/dialout/dialouttest/pi.go`: a minimal, correct Pi side for higher-level tests

**Interfaces:**
- Produces, in package `dialout`:
  ```go
  const Hello = "AGWDIAL1"
  const (KeepaliveByte byte = 'K'; GoByte byte = 'G'; AckByte byte = 'A')
  type Config struct {
      Addr string                       // the Pi's car port, literal ip:port
      Idle int                          // default 3
      DialsPerSecond int                // default 4
      BackoffMin, BackoffMax time.Duration // defaults 1 s, 10 s
      KeepaliveEvery, Silence time.Duration // defaults 10 s, 25 s
      DialTimeout time.Duration         // default 3 s
      Dial func(ctx context.Context, network, addr string) (net.Conn, error) // nil: net.Dialer
      Log *log.Logger                   // nil: log.Default()
  }
  func Listen(cfg Config) (*Listener, error) // errors only on an unparsable Addr; starts dialing at once
  func (l *Listener) Accept() (net.Conn, error) // a paired connection; net.ErrClosed after Close
  func (l *Listener) Close() error              // idempotent; closes idle and handshaking connections
  func (l *Listener) Addr() net.Addr            // the Pi's address
  ```
- Produces, in package `dialouttest`: `Start(t testing.TB) *Pi`, `(*Pi).CarAddr() string`,
  `(*Pi).PhoneAddr() string` (pairs each phone with one idle car connection, per the spec), `(*Pi).Idle() int`,
  `(*Pi).Close()`.

- [ ] **Step 1: Write the failing tests** in `dialout_test.go`. Each test is a raw `net.Listener` on 127.0.0.1:0
  playing the Pi, with short timings set in `Config`.
  - `TestDefaults`: `Listen(Config{Addr: "127.0.0.1:1"})` fills Idle 3, DialsPerSecond 4, BackoffMin 1 s, BackoffMax
    10 s, KeepaliveEvery 10 s, Silence 25 s, DialTimeout 3 s.
  - `TestKeepsIdleConnectionsWithHello`: within 1 s the raw Pi has exactly 3 connections, and each one's first 8 bytes
    equal `"AGWDIAL1"`.
  - `TestPairingHandsOverExactlyThePhoneBytes`:
    - The Pi writes `K` then `G`, reads up to `A` (only `K` before it), then writes `"phone-bytes"`.
    - `Accept()` returns a connection whose first read is exactly `"phone-bytes"`.
    - That connection's write of `"gw"` is the next thing the Pi reads after `A`.
  - `TestNoKeepaliveAfterAck`: KeepaliveEvery 1 ms, 200 iterations. The Pi sends `G` after a random 0-5 ms; after
    reading `A`, it reads for 20 ms and gets 0 bytes.
  - `TestSilenceClosesAndRedials`: Silence 100 ms and the Pi never sends `K`. The Pi sees EOF on a connection within
    80-300 ms of its hello, and then a new connection arrives.
  - `TestPeerKeepalivesHoldTheConnection`: Silence 100 ms, and the Pi sends `K` every 30 ms. The connection is still
    open after 500 ms.
  - `TestUnexpectedByteCloses`: the Pi sends `X`. EOF follows, `Accept` returns nothing within 200 ms, and a
    replacement connection arrives.
  - `TestDialRateCapAndBackoff`:
    - With a `Dial` hook that always fails (BackoffMin 50 ms, BackoffMax 200 ms), the gaps between dials run
      ≥ 50, ≥ 100, ≥ 200 and then stay at ≥ 200 ms.
    - With a Pi that sends `G` at once on every connection, at most 9 dials are made in 2 s (the sliding window).
  - `TestRefillAfterHandout`: after one pairing, a 4th connection reaches the Pi within 100 ms.
  - `TestCloseUnblocksAcceptAndClosesConnections`: a blocked `Accept` returns `net.ErrClosed` within 100 ms, and the
    Pi sees EOF on every idle connection.
  - `TestPiRestart`: BackoffMin 50 ms, BackoffMax 200 ms. Close the raw Pi's listener and connections, then listen
    again on the same port 150 ms later. 3 idle connections return within 1 s.

- [ ] **Step 2: Run the tests and see them fail.** Run: `go test ./internal/dialout/`. Expected: FAIL, undefined:
  `Listen`.

- [ ] **Step 3: Implement `dialout.go`.**
  - Each connection: dial, write `Hello`, then loop reading one byte with a deadline of last-heard plus `Silence`.
    - `K` refreshes last-heard.
    - `G`: under the connection's write mutex, mark it paired, stop its keepalive ticker and write `A`. Then clear
      the deadlines and deliver it to `Accept` through a channel with capacity `Idle`; after Close, close it instead.
    - Any other byte, or an error, closes the connection.
  - A ticker writes `K` under the same mutex while the connection is unpaired.
  - One supervisor goroutine keeps `Idle` connections dialing or idle, and dials one at a time.
    - A handout triggers a redial at once.
    - A connection that closed before pairing is replaced after `BackoffMin`.
    - Every dial passes a sliding window of `DialsPerSecond` per second.
    - Consecutive failed dials wait `BackoffMin`, doubling to `BackoffMax`; a successful dial resets the delay.

- [ ] **Step 4: Implement `dialouttest/pi.go`** to the spec's Pi rules (hello within 3 s, the pool, `G`/`A`, then
  piping), with its timings exposed as fields.

- [ ] **Step 5: Run the tests and see them pass.** Run: `go test -race -count=3 ./internal/dialout/...`.
  Expected: PASS.

- [ ] **Step 6: Commit.** `git commit -m "feat(dialout): a listener that dials the Pi and hands over paired connections"`

### Task 2: `agw serve -dial` (gateway)

**Files:**
- Modify: `cmd/agw/main.go`: `cmdServe` flags, `checkDial`, listener choice, log line
- Modify: `cmd/agw/main_test.go`
- Modify: `README.md`: the bench line also shows `-dial`

**Interfaces:**
- Consumes `dialout.Listen` from Task 1, and `dialouttest.Start` for tests.
- Produces `checkDial(addr string, bench bool) (string, error)` and `const defaultDialPort = "17101"`. `serve -dial`
  passes the listener to `srv.Serve`, and `onListen` receives `Listener.Addr()`. It logs
  `agw: dialing the Pi at %s (%d capabilities)`.

- [ ] **Step 1: Write the failing tests.**
  - `TestServeNeedsExactlyOneOfListenAndDial`: both flags, or neither, exits with code 2 and stderr containing
    `exactly one of -listen and -dial`.
  - `TestCheckDial`, a table:

    | Input | Result |
    |---|---|
    | `192.168.4.1` | `192.168.4.1:17101` |
    | `192.168.4.1:9` | kept as is |
    | `192.168.90.10:17101` | error containing `R22` |
    | `100.100.1.1` | error containing `tailnet` |
    | `0.0.0.0` | error |
    | `127.0.0.1`, no bench | error |
    | `127.0.0.1:5`, bench | `127.0.0.1:5` |
    | `pi.local` | error containing `literal IP` |

  - `TestServeDialEndToEnd`:
    - Run `dialouttest.Start(t)`, then `serve -bench -fake-role 2 -dial <pi.CarAddr()> …` in a goroutine.
    - Wait until `pi.Idle() == 3`.
    - `client.Dial(pi.PhoneAddr(), proto.CapEcho, …)` echoes `"ping"`. A second session after closing the first also
      echoes.
    - `pi.Idle()` is back at 3 within 1 s.

- [ ] **Step 2: Run them and see them fail.** Run: `go test -run 'Dial' ./cmd/agw/`. Expected: FAIL.

- [ ] **Step 3: Implement.**
  - Write `checkDial` with `checkListen`'s rules: the default port 17101, and `literal IP` in its error.
  - In `cmdServe`, choose `dialout.Listen(dialout.Config{Addr: addr, Log: log.Default()})` or `net.Listen`.
  - The bus self-test still runs whenever `-bench` is absent.

- [ ] **Step 4: Run the gateway checks** (Global Constraints). Expected: all pass.

- [ ] **Step 5: Commit.** `git commit -m "feat(agw): serve -dial connects out to the Pi"`

### Task 3: the car pool and pairing (rpi)

**Files:**
- Create: `internal/services/gwpool.go` (protocol constants, `carPool`, `carConn`), `internal/services/gwpool_test.go`
- Modify: `internal/services/gwforward.go` (`GatewayForward`: constructor, `Start`, `Stop`, phone accept)
- Modify: `internal/services/gwforward_test.go` (helpers and the pipe tests)

**Interfaces:**
- Produces:
  ```go
  const (gatewayCarPort = 17101; carHello = "AGWDIAL1"; carKeepalive byte = 'K'; carGo byte = 'G'; carAck byte = 'A')
  // defaults, each overridable on GatewayForward for tests:
  // carHelloWait 3 s, carKeepaliveEvery 10 s, carSilence 25 s, carAckWait 2 s, phoneCarWait 3 s, carPoolMax 4
  func NewGatewayForward(listen, carListen, carAddr string, maxConns int) *GatewayForward // carAddr: the only source IP accepted on carListen
  func (f *GatewayForward) CarAddr() string // the bound car listen address
  func (f *GatewayForward) IdleCars() int
  ```
- Task 4 consumes `NewGatewayForward`, and Task 5 consumes `NewGatewayForward`, `Addr` and `CarAddr`.

- [ ] **Step 1: Write the failing tests.** Fake cars are TCP clients in the test; timings are shortened to about
  100 ms.
  - `TestCarHelloRequired`: `"WRONGHEL"` is closed. Silence is closed after the hello wait. The correct hello gives
    `IdleCars() == 1`.
  - `TestCarFromOtherAddressRefused`: with carAddr `127.0.0.2`, a car from 127.0.0.1 is closed, and
    `IdleCars() == 0`.
  - `TestCarPoolCap`: of 5 cars, 4 are held and the 5th sees EOF.
  - `TestCarKeepaliveAndSilence`: the car receives `K`s every interval. A silent car is closed after the silence
    limit; a car sending `K` stays.
  - `TestCarUnexpectedByteBeforePairing`: `X` closes it and removes it from the pool.
  - `TestPairingSendsGAndPipesAfterA`:
    - The car reads `G` (only `K` before it), then answers `K` and `A`.
    - Phone `"hi"` reaches the car exactly, and car `"yo"` reaches the phone exactly.
  - `TestNoKeepaliveAfterGo`: keepalive every 1 ms, 100 iterations. The car never receives a byte between `G` and
    the phone's first byte.
  - `TestPairingSkipsDeadCar`: car1 never sends `A` and is closed after the ack wait; the phone is piped to car2.
  - `TestPhoneClosedWithoutCar`: with no cars, the phone is closed between the car wait and the car wait plus 150 ms.
  - `TestPhoneWaitsForArrivingCar`: a phone first, then a car 50 ms later, are paired.
  - `TestEachCarPairedOnce`: 3 cars and 3 simultaneous phones form 3 pairs. Each car gets exactly one `G`, and a
    phone's bytes reach exactly one car.
  - Rework the existing tests onto pooled fake cars: `TestForwardCopiesBothWays`, `TestForwardMaxConns`,
    `TestForwardHalfClose`, `TestForwardIdleTimeout`, `TestForwardIdleDefault`, `TestForwardCarEOFLinger`,
    `TestForwardStopMidAccept`.
  - `TestForwardTargetDown` becomes `TestPhoneClosedWithoutCar`.
  - `TestForwardStopMidDial` becomes `TestForwardStopMidPairing`: Stop returns promptly and every connection,
    pooled or pairing, is closed.

- [ ] **Step 2: Run them and see them fail.** Run: `go test ./internal/services/ -run 'Car|Pairing|Phone|Forward'`.
  Expected: FAIL.

- [ ] **Step 3: Implement `gwpool.go`.**
  - Car accept loop: close a connection whose remote IP is not carAddr; read the hello within the hello wait; close
    it if the pool is full.
  - One reader per car reads one byte at a time with a deadline of last-heard plus the silence limit:
    - while idle, `K` refreshes and anything else closes;
    - while pairing, `K` is discarded, `A` signals and the reader exits (so the pipe owns reads), and anything else
      fails.
  - A ticker writes `K` under the connection's write mutex while it is idle.
  - `take`: removes one idle car under the pool lock. Pairing: stop the ticker, write `G`, and wait up to the ack wait
    for `A`.

- [ ] **Step 4: Change `GatewayForward`.**
  - `Start` listens on both addresses.
  - The phone handler takes a paired car, trying the next on failure and waiting for an arrival until the car wait;
    otherwise it closes the phone.
  - It then runs the existing pipe with `up` set to the car connection.
  - `Stop` also closes the pool and any pairing connections.

- [ ] **Step 5: Run the tests and see them pass.** Run: `go test -race -count=3 ./internal/services/`. Expected: PASS.

- [ ] **Step 6: Commit.** `git commit -m "feat(gateway): pool the car's outbound connections and pair each phone with one"`

### Task 4: service, config and firewall wiring (rpi)

**Files:**
- Modify: `internal/services/gwforward.go`:
  - `NewGatewayForwardService(db *sql.DB, cfg config.Config)`;
  - a `carListenHost` hook (default `net.JoinHostPort(cfg.LANGateway, "17101")`);
  - `Reconfigure` keyed on listen, carListen and carAddr.
- Modify: `internal/services/services.go:69` (pass `cfg`)
- Modify: `internal/services/firewall.go:487-491` (the car rule)
- Modify: `internal/services/gwforward_test.go`

**Interfaces:**
- Consumes `NewGatewayForward` from Task 3.
- Produces a log line: `[GATEWAY] phones on %s; the car dials %s from %s`.

- [ ] **Step 1: Write the failing tests.**
  - `TestFirewallHasGatewayRule`, with cfg LANInterface `wlan0`, LANGateway `192.168.4.1` and gw_car_addr
    `192.168.4.20`:
    - the output contains `iifname "wlan0" ip saddr 192.168.4.20 ip daddr 192.168.4.1 tcp dport 17101 accept` and the
      existing 17100 rule;
    - neither rule appears while disabled or without a tailnet.
  - `TestGatewayServiceFollowsSettingsAndTailnet`, with both hooks at `127.0.0.1:0`:
    - the forwarder runs only with an address and a tailnet;
    - changing `gw_car_addr` restarts it with the new address;
    - clearing `gw_car_addr` stops it.

- [ ] **Step 2: Run them and see them fail.** Run: `go test ./internal/services/ -run 'Firewall|GatewayService'`.
  Expected: FAIL.

- [ ] **Step 3: Implement the wiring** as in Files.

- [ ] **Step 4: Run the rpi checks** (Global Constraints). Expected: all pass.

- [ ] **Step 5: Commit.** `git commit -m "feat(gateway): the Pi takes the car's connections on its hotspot address"`

### Task 5: both real ends together on loopback

**Files:**
- Create in rpi: `cmd/gwforward-lab/main.go`. Flags `-phone`, `-car` and `-car-addr`; it starts
  `services.NewGatewayForward`, prints `phone=<addr> car=<addr>`, and runs until SIGINT or SIGTERM.
- Create in gateway: `cmd/agw/dial_e2e_test.go`. `TestDialThroughRealPi` is skipped unless `AGW_E2E_PI_PHONE` and
  `AGW_E2E_PI_CAR` are set.

**Interfaces:** consumes rpi `NewGatewayForward`, `Addr` and `CarAddr`, and gateway `serve -dial` and `client.Dial`.

- [ ] **Step 1: Write `TestDialThroughRealPi`.**
  - Start `serve -bench -fake-role 2 -dial $AGW_E2E_PI_CAR` and wait for the `dialing the Pi` log line.
  - Run three `client.Dial($AGW_E2E_PI_PHONE, CapEcho)` sessions in a row; each echoes `"ping"` within 2 s.

- [ ] **Step 2: Write the lab command and commit it** in rpi:
  `git commit -m "feat(gateway): gwforward-lab runs the pairing forwarder on a laptop"`

- [ ] **Step 3: Run both together.**
  ```bash
  cd ~/Work/airgapp/rpi-pairing && go run ./cmd/gwforward-lab -phone 127.0.0.1:27100 -car 127.0.0.1:27101 -car-addr 127.0.0.1
  ```
  ```bash
  cd ~/Work/airgapp/gateway-dialout && AGW_E2E_PI_PHONE=127.0.0.1:27100 AGW_E2E_PI_CAR=127.0.0.1:27101 go test -race -run TestDialThroughRealPi -v ./cmd/agw/
  ```
  Expected: PASS, with three echo round trips. Without the variables the test reports SKIP.

- [ ] **Step 4: Commit** in gateway: `git commit -m "test(agw): dial-out end to end through the real Pi forwarder"`

- [ ] **Step 5: Hand back.**
  - Re-run both repos' checks.
  - Send the camera session both branch heads: it merges `feat/agw-dial-out`, rebuilds and hashes. The rpi branch
    is deployed by its runbook's Pi step.
  - Record both heads in memory (`car-wifi-nat.md`).
