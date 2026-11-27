# The car dials out to the Pi: design (short)

Date: 2026-11-21. Status: waiting for the owner's go-ahead. Nothing is built yet.

## Why

The camera session proved on the car on 2026-11-21 that the car's Wi-Fi module NATs the MCU and drops every
inbound packet. The Pi therefore cannot open any connection to agw. The car can connect out: it reached
192.168.4.1:53. The owner chose to have the car dial out to the Pi ("Car dials out to Pi", in the camera session).

**Approaches considered** (in the camera session):
- **(a) agw dials out to the Pi**, which pairs those connections with phone connections. Chosen. The car accepts no
  inbound connection at all, and the car-to-Pi leg stays on local Wi-Fi.
- **(b) Pi to car over the tailnet.** Not chosen: it puts agw on the car's tailnet interface, and its speed depends on
  Tailscale finding a direct path. Summon's deadman needs a round trip under about 150 ms.

## What changes

| Part | Change | Owner of the work |
|---|---|---|
| agw (gateway repo) | `serve -dial <pi-ip>:17101`: keeps a few idle outbound connections to the Pi, and hands each to the server core when the Pi pairs it with a phone. Exactly one of `-dial` and `-listen`; `-listen` stays for the bench and tests | this session |
| Pi (rpi repo, netfilterd `gwforward`) | Accepts car connections on its hotspot address, port 17101, from `gw_car_addr` only, and keeps them idle in a pool. Pairs one with each phone connection on tailnet port 17100, then pipes bytes exactly as today (idle timeout, car-close limit). nft: `iifname <LAN> ip saddr <gw_car_addr> ip daddr <LAN gateway> tcp dport 17101 accept` | this session |
| Car install (`install-car.sh`) | Egress lock: uid 4950 may only connect `-o wlan0 -d PI_IP -p tcp --dport 17101`, then DROP. The ip6 DROP stays and the INPUT rule goes away. agw's run line uses `-dial` | camera session |
| Phone | Nothing: it still connects to the Pi's tailnet address on 17100 | none |

The server core, sign-in, windows, CAMERA, DASHCAM and Summon are unchanged. The 2 s pre-auth clock starts when a
connection is paired, not when it is dialled.

## The protocol on a car connection, before pairing

- **Hello.** The car sends 8 bytes, `AGWDIAL1`, right after connecting. The Pi reads them within 3 s, or closes the
  connection.
- **Idle.** Each side sends `K` every 10 s, and each closes the connection after 25 s with nothing heard. This also
  keeps the module's NAT entry alive.
- **Allowed bytes.** Before pairing the only bytes are the hello, `K`, `G` (Pi to car) and `A` (car to Pi). Anything
  else closes the connection.
- **Pairing, on the Pi.**
  1. A phone connects to 17100.
  2. Under the pool lock, the Pi takes one idle car connection out of the pool, so no second phone can get it.
  3. It stops its own `K` writer, then sends `G`.
  4. It reads one byte at a time, discarding `K`, until `A`.
  5. If no `A` arrives within 2 s, it closes that car connection and tries the next. If no idle car connection is
     available within 3 s, it closes the phone.
  6. After `A`, it pipes bytes as it does today.
- **Pairing, on the car.** The car reads one byte at a time while idle, so it never consumes a phone byte. On `G` it
  stops its `K` writer, writes `A`, and hands the connection to the server core. It never sends a `K` after `A`.
  Because TCP keeps order, everything after `G` from the Pi is phone data, and everything after `A` from the car is
  gateway data.
- **Pool sizes and rates.**
  - The car keeps 3 idle connections and redials at once when one is handed out. It makes at most 4 dials a second,
    with backoff from 1 s to 10 s on failures.
  - The Pi keeps at most 4 idle car connections and closes any beyond that.

## Security

- The car accepts no inbound connection at all.
- **A hostile Pi** stays a byte pipe without keys. It can withhold or misdirect pairings, which ends in a stop or a
  refused sign-in, never a session. The phone verifies the gateway's pinned key, and the gateway verifies the phone's
  key. It can burn pre-auth slots, which the server already limits (2 at once, rate-limited hellos), and the car's
  dial rate cap bounds the churn.
- **Another process on the car, or a hotspot client spoofing the car's address,** that reaches port 17101 can at most
  take pairings (denial of service). It cannot complete a gateway handshake without the gateway's key.
- Nothing is sent to 192.168.90.0/24. The egress lock allows uid 4950 exactly one destination.

## Tests

- **agw:** the dialing listener against a fake Pi, then end to end through the real server core. Cases:
  - the hello, keepalives, and the 25 s timeout;
  - `G` and `A`, no `K` after `A`, and phone bytes never swallowed;
  - redial, backoff and the rate cap;
  - a Pi that sends `G` over and over;
  - shutdown.
- **Pi:** the pool, pairing, the existing pipe, and Stop with pooled and pairing connections. Cases:
  - the hello timeout, the cap, keepalives and the timeout;
  - pairing used once, a dead car connection skipped, the 3 s wait, `K` discarded, and no `K` after `G`.
- **Both ends together:** one end-to-end test with the real agw dialer and the real Pi pool on loopback, and a phone
  client signing in.

## Where it lands

- agw: a new branch from feat/camera-capabilities (f706e65), handed to the camera session to merge, as with the
  sign-in client.
- Pi: a new branch from rpi feat/gateway-forward (be3120a).
- Nothing is pushed, and there is no car contact; the camera session runs the install and the car test.
