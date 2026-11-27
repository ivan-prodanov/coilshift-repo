# Camera root helper (`camhelper`)

The gateway (`agw`, uid 4950) never touches the car. Everything CAMERA, TALK and DASHCAM need from the car goes
through `camhelper`, a small root process with a compile-time allowlist (PLAN-B2 §4.9 "Privilege separation and
the helper allowlist"). Code: `cmd/camhelper` (command), `internal/camhelper` (server), `camhelper/proto` (wire
protocol), `camhelper/client` (the gateway side), `internal/annexb` (H.264 access-unit splitter). The phone-facing
protocol is `docs/CAMERA-PROTOCOL.md`.

## Layout and identities

| What | Where | Owner / mode |
|---|---|---|
| binary | `/var/lib/airgapp-camhelper/camhelper` | root:root 0755 (outside the gateway dir) |
| socket dir | `/run/airgapp-camhelper/` | root:4950 0750 (created by the helper at start) |
| socket | `/run/airgapp-camhelper/camhelper.sock` | root:4950 0660; only peer uid 4950 is served (SO_PEERCRED) |
| sign-in socket | `/run/airgapp-camhelper/auth.sock` | root:4950 0660; only peer uid 4950 is served (see "Sign-in socket") |
| state dir | `/run/airgapp-camhelper-state/` | root:root 0711: markers (0600), `lock`, `children.json` |
| cut dir | `/run/airgapp-camhelper-state/cut/` | 4951:4951 0700 (ffmpeg output) |
| log | `/var/lib/airgapp-camhelper/svlog/` | root 0750: svlogd, 3 files of 1 MB |
| runit service | `/var/lib/airgapp-gw/sv/camhelper/{run,finish,log/run}` | root 0755, written by `deploy/install-car.sh` |

The helper runs as root. Its children (captures, talk writer, ffmpeg) run as uid 4951, gid 4951, supplementary
groups audio 29 and pipewire 1021 (`SysProcAttr.Credential`), each in its own process group (`Setpgid`) with
`Pdeathsig` SIGKILL. The car tools (`set-local-dv`, `get-local-dv`, `audioctl`, `pw-link`) run as root, also in
their own process group with `Pdeathsig`, a 10 s timeout that kills the group, and `WaitDelay` 1 s, so a crashed
helper leaves no write in flight. Flags: `camhelper run [-state] [-sock] [-gw-uid 4950] [-gw-gid 4950]
[-child-uid 4951] [-child-gid 4951] [-child-groups 29,1021] [-writer pw-cat|aplay] [-teslacam /mnt/usb/TeslaCam]
[-auth-sock /run/airgapp-camhelper/auth.sock] [-authd-sha256 <64 hex>]`. `-auth-sock` must sit in `-sock`'s
directory; `-authd-sha256` (default: the verified 2026.8.6 authd) is checked before anything is created.
`-state` and the socket's directory must be `/run/airgapp-<name>`: the helper creates, chowns and chmods them, so
it refuses anything else and never touches a system directory.

## Lifecycle

- **Start** (`camhelper run`, root only): fixes the dirs above (refusing a symlink or non-directory), takes the
  exclusive state lock (`flock`; a second helper or sweep exits: never two), runs the **sweep**, then listens.
- **Sweep** (at start, and `camhelper sweep` = the runit `finish` hook): clears exactly what the markers of a previous
  run record, in the §4.9 order: requestAudio 0, sessionActive 0, selectable 0, `pws-media-gain -200` only if the gain
  marker exists, then SIGKILL to the process group of each recorded child whose `/proc/<pid>/cmdline` still equals
  the recorded argv (a recycled pid is left alone), then the cut files. Nothing unmarked is ever written. When any
  marker exists it first waits 1 s (a write still in flight from the dead helper lands before ours). Each DataValue
  0 is confirmed by a `get-local-dv` readback; an unconfirmed one keeps its marker (logged `LATCH NOT CLEARED`) and
  the next helper retries it.
- **SIGTERM/SIGINT**: every session is closed and released (below), a final sweep runs, exit 0.
- **Supervision**: runit, both services (`agw`, `camhelper`) from `deploy/install-car.sh`. The firmware's runsvdir
  watches `/service`, which resolves onto the read-only rootfs, so the expected outcome is **our own runsvdir**
  (`setsid runsvdir -P /var/lib/airgapp-gw/svdir`); after every car boot run `install-car.sh --after-boot`, which
  starts it again and re-adds the iptables rule. See `deploy/README.md`.

Markers live in the root-only state dir: `ra` (requestAudio was set 1), `sa` (sessionActive was set 1), `sel`
(selectable was set non-zero), `gain` (the gain was changed), plus `children.json` (pid + argv of every running
child). A marker is created **before** the change; if it cannot be created the operation is refused (`internal`),
never done unmarked. It is removed only **after** the 0 write succeeded **and** a `get-local-dv` readback shows 0
(for the gain: `audioctl` exited 0) and nobody owns it again. Release decisions come from the in-memory state;
markers only drive the crash sweep and the retries: every second the tick queues a clear for each marker that no
session owns (backoff 1, 2, 4, 8, 16, 30 s, logged `LATCH NOT CLEARED` until it succeeds).

## Car I/O

Every DataValue write, gain change and readback runs on one ordered car-queue goroutine, in the order the state
changed; the arbiter lock is never held while a car tool runs (they can take seconds under load). Requests that
change car state answer after their writes ran. The talk PCM path has its own lock, so a slow car call never
stalls talk audio. The tick waits for its own writes before the next tick, so slow tools slow the loop down
instead of piling writes up.

## Sessions and ownership

One unix connection is one session. Each CAMERA or DASHCAM window opens its own session (in its own goroutine,
never in `Open`); closing it (window teardown, agw crash, helper connection loss) makes the helper release
everything that session acquired. State is per session, so the late `Teardown` of an abandoned (overrun) window
can never release a newer window's hold (`TestLateTeardownOfAbandonedWindowSparesTheNewOne`):

- `WEBCAM_webcamSessionActive`: refcount over holding sessions. 1 is written by the first holder; 0 only when the
  last holder releases.
- talk (`WEBCAM_webcamSessionRequestAudio`, the writer, the gain): one owning session at a time (`busy` otherwise).
- `GUI_selectableCameraRequest`: one owning session at a time, parked only, except in SUMMON mode (below).
- SUMMON mode: at most one session, granted only while summonhelper holds the lock on its live file, which it does
  while the engine of a Summon session that opted into its presence lease is up (the `summon` op, below).
- captures: **one child per port, shared** by every session that subscribed to it (their access units are fanned
  out), so overlapping windows never double the gst children; the last subscriber stops it. Per session at most
  8 subscriptions; at most 10 children in all (captures, the talk writer, a cut).
- cut files: per session.

### Release order (PLAN §4.9)

On disconnect, SIGTERM, a gate, `release`/`talk_disarm`/`sel 0`:

1. `WEBCAM_webcamSessionRequestAudio` 0 (if the session owns talk);
2. `WEBCAM_webcamSessionActive` 0 only if no holder is left; if others still hold and talk ended, an edge
   (0, 0.4 s, 1) un-routes the exterior stream instead;
3. `GUI_selectableCameraRequest` 0 (if the session owns it);
4. `pws-media-gain -200` only if the gain marker exists (we changed it; a Boombox is never muted);
5. children stopped by their own process group: SIGTERM, 1 s, SIGKILL (never by name: the car's dashcam and Sentry
   recorders are `gst-launch-1.0` too);
6. the session's cut files removed.

The helper does this itself after the session's request worker has stopped, so no request can re-acquire anything
afterwards. The gateway never waits for it (its `Teardown` only closes the connection, bounded at 300 ms).

## The 1 Hz loop

Every second the helper retries the clear of any marker no session owns (above). While any session holds or
owns something, it also:

1. reads `VAPI_shiftState`, `GUI_sentryModeState`, `GUI_keepClimateOn`, `GUI_mobileCameraAccessEnabled`
   (`get-local-dv CarServer …`, e.g. `(true, 'P')`);
2. never fights `checkWebcamAllowed`: the first non-P gear (R/N/D) ends talk and the selectable hold, and
   sessionActive is not re-asserted on it; the second consecutive one ends every hold (never re-asserted); three
   unknown gear readings end talk and the selectable; Sentry `Off`
   without Dog mode, or camera access not `true`, seen twice, ends everything. Sessions get `talk_end`/`sel_end`/
   `hold_end` events with the gate code, and holds are refused while a gate fails (the refusal carries the status).
   **SUMMON mode** is the exception, as Tesla's webcam handler skips `checkWebcamAllowed` for a summon session:
   that session's own hold is not ended by the gear, Sentry or camera-access counters, its selectable is not ended
   by a non-P gear, and sessionActive is re-asserted in any gear while it holds. Talk keeps its strict gate (and
   never runs for the SUMMON session), and every other session's hold and selectable end as usual. The counters
   keep counting. The SUMMON session's selectable must be Tesla's camera for the gear: one that still does not
   match on two consecutive ticks is ended (`sel_end`, `select_refused`). A **watcher** checks the live file every
   200 ms, independently of this loop's car reads: once it is gone (or cannot be checked) the mode ends with
   `summon_end` (`live`) and the session's selectable released (`sel_end`); the tick checks it too, and the normal
   gates apply from the next tick (two non-P readings end the holds at once). A mode granted while the tick was
   reading the car is judged at the next check, not this one;
3. re-asserts what is held, in order: requestAudio 1 (talk), sessionActive 1 (holders), the selectable value,
   `pws-media-gain 0` while push-to-talk is boosted (owner choice 2026-11-20; Tesla's own max is -6);
4. runs the **route watchdog** while talk is armed: parses `pw-link -l` (with `PIPEWIRE_RUNTIME_DIR=/var/run/pipewire
   PIPEWIRE_CONFIG_NAME=client.conf`); the route is up when `TDM-playback:playback_9` and `_10` are fed by
   `webcam:monitor_*`. Missing for 2 checks after a 3 s grace: the edge is re-fired (backoff 3, 5, 10, 20, 30 s).
   `route` events tell the session;
5. restarts a dead talk writer, with backoff; after 5 failed restarts talk ends (`talk_end` `spawn_failed`);
6. sends a `status` event to every session that holds or owns something.

## Wire protocol (`camhelper/proto`)

Frames both ways: `u32 N` (big-endian) then N bytes = type byte + body. Client frames ≤ 64 KiB, helper frames ≤ 1 MiB
+ 13. Requests are strict JSON (unknown fields close the session).

| Dir | Type | Body |
|---|---|---|
| → | 0x01 REQ | `{"id", "op", …}` (below) |
| → | 0x02 PCM | raw s16le mono 48 kHz, even, ≤ 16 KiB, to this session's talk writer (dropped otherwise) |
| ← | 0x81 RESP | `{"id", "ok", "err", "status", "cut", "size"}`, one per REQ (except a successful `dash_read`) |
| ← | 0x82 EVENT | `{"ev": "status"\|"cap_end"\|"hold_end"\|"talk_end"\|"sel_end"\|"summon_end"\|"route", "port", "reason", "status"}` |
| ← | 0x83 AU | `u16 port, u8 flags (bit0 IDR, bit1 SPS/PPS), u8 0, u64 capture µs` + Annex-B AU (the VIDEO header's size, so the gateway forwards it in place) |
| ← | 0x84 DATA | `u32 id, u64 total` + bytes: the `dash_read` reply |
| ← | 0x85 LIST | `u32 id` + JSON page `{"dirs": [{type, folder, has_event, event (bytes), files: [{name, size}]}], "recent": [...]}` (then RESP) |

| Op | Args | Effect | Errors |
|---|---|---|---|
| `hold` | | refcounted sessionActive; returns the status; in SUMMON mode the gates are waived for this session (the live file checked again) | `gate_gear`, `gate_sentry`, `gate_camera_access` (with status), `internal` (marker) |
| `release` | | drops this session's hold (and its talk) | |
| `talk_arm` | | requires a hold; strict gates; refused for the session in SUMMON mode; `exterior_busy` if anything but `webcam:monitor` feeds TDM 9/10; requestAudio 1 → sessionActive 0 → 0.4 s → 1 → the writer attached at once | `not_held`, `busy`, `summon`, gates, `exterior_busy`, `spawn_failed` |
| `talk_disarm` | `no_edge` | ends talk (release order 1, 2, 4, 5); with `no_edge` no sessionActive 0 → 1 edge while the hold stays (for a caller that releases the hold right after) | |
| `gain` | `boost` | true: marker, 0 dB; false: -20 dB (only if we ever changed it) | `not_armed` |
| `sel` | `sel` 1/3/9/11/0 | 1 interior, 3 front (the windshield ultrawide); parked only, one owner; in SUMMON mode (the live file checked again) only Tesla's camera for the gear, in any gear: 11 Grid in P or N, 9 Backup in R, 3 in D, and any of the three while the gear cannot be read (1 stays parked-only); 0 releases (and stops this session's 9895 capture) | `select_refused`, `busy`, `bad_request` |
| `summon` | `on` | on: SUMMON mode for this session, only while summonhelper holds the lock on `/run/airgapp-summon/live`. Contract: for each Summon session that opted into the presence lease (its inner ARM params `{"presence":1}`; ruling F1), once its start succeeded, summonhelper creates it as a temp file in that directory (`O_EXCL\|O_NOFOLLOW`, 0600), takes `flock(LOCK_EX)`, writes its pid, renames it into place and keeps the fd until the session ends (unlink, only while the name is still that file, then close), and it removes a file left behind wherever a summonhelper's end is handled (`docs/SUMMON.md`, "The live file"); camhelper opens it `O_NOFOLLOW\|O_NONBLOCK`, requires a regular file and a real directory both owned by the summon uid (`-summon-uid`, 1337) and not group- or other-writable, and counts it live only when `flock(LOCK_SH\|LOCK_NB)` fails with `EWOULDBLOCK`. The kernel drops the lock when summonhelper dies, so a stale file proves nothing, and agw cannot open the 0600 file to hold a lock itself. (summonhelper's session marker is no use here: it stays after an unclean end, for the takeover's abort.) On also ends this session's talk. Off: the normal gates again, this session's selectable released | `summon_unverified`, `busy` |
| `cap_start` / `cap_stop` | `port` | subscribe to the shared capture of 9892, 9893, 9896, 9897, 9898, 9900, 9901; 9895 only during this session's selectable hold; ≤ 8 per session (`MaxCapturesPerSession`), ≤ 10 children in all (`MaxChildren`) | `port_not_allowed`, `busy`, `spawn_failed` |
| `status` | | fresh gate readings + this session's state | |
| `dash_list` | | TeslaCam inventory (LIST pages) | `io` |
| `dash_read` | `path` or `cut`, `offset`, `length` ≤ 65516 | DATA | `forbidden_path`, `not_found`, `io`, `bad_request` |
| `dash_cut` | `path`, `ss` ≥ 0, 0 < `t` ≤ 120 | ffmpeg window into the cut dir (reused per path/ss/t; ≤ 4 per session; 128 MiB for all, enforced with `-fs`) | `cut_failed`, `busy` (budget or free space), `forbidden_path`, `not_found`, `bad_request` |

## Sign-in socket (`camhelper/authproto`)

uid 4950 has no account in the firmware's `/etc/passwd`, so dbus-daemon refuses it, and that is kept on purpose: on
this car any connected uid may call `com.tesla.DisplayManager` (SetPower, BacklightControl) and `com.tesla.Videod`
(RE car check 6, 2026-11-20). The helper answers the gateway's sign-in questions instead, as root, from authd's
`KeyInfo` (the gateway core's `authority.NewDBus` oracle: the unique owner of `com.tesla.Auth` must run as authd's
uid, `/usr/bin/authd` must match the pin, exactly one exact 65-byte row, Id = SHA-1(pk)[:4], SecondsLeft 0,
role ≤ 11). Code: `internal/authsrv` (server and the key-change watch), `camhelper/authproto` (wire).

The same framing as `camhelper/proto` (`u32 N`, type byte, body), on its own socket. At most 8 connections. A
connection is a request connection (any number of `owner`/`key`, answered in order) or a watch connection (its first
request is `watch`; any later frame ends it).

| Dir | Type | Body (limit) |
|---|---|---|
| → | 0x01 REQ | `{"v": 1, "id", "op": "owner"\|"key"\|"watch", "pk"}` (≤ 1 KiB; exactly these lowercase keys, each once, none null) |
| ← | 0x81 RESP | `{"id", "ok", "err", "msg", "role", "ff", "secs", "kid"}` (≤ 512 B), one per REQ, in order |
| ← | 0x82 EVENT | `{"ev": "keys_changed"\|"watch_up"\|"watch_down", "reason"}` (≤ 256 B), watch connections only |

- `owner`: ok when `com.tesla.Auth`'s unique owner runs as authd's uid. Otherwise `auth_owner` (`msg` says whose).
- `key` (`pk`: exactly 130 lowercase hex digits, `04`‖X‖Y; only with `key`): the owner check, then pk's KeyInfo row:
  `role`, `ff` (form factor, 6 = iOS), `secs` (SecondsLeft), `kid` (authd's 4-byte Id, hex). The helper passes the
  role through; the gateway re-checks kid = SHA-1(pk)[:4] and accepts only role 2 or 3.
- `watch`: ok, then at once the subscription state (`watch_up` or `watch_down`) and `keys_changed` `resubscribed`
  (anything may have changed before the watcher attached; it is registered before its RESP is written, so no event
  falls in between), then `keys_changed` with reason `keys_updated` (authd's KeysUpdated), `owner_changed`
  (NameOwnerChanged for `com.tesla.Auth`) or `resubscribed` (the subscription came back after `watch_down`; changes
  may have been missed). All three mean: re-check every open window. A watcher that does not read is closed when 16
  events are queued.
- Errors (every one denies): `bad_request` (then the helper closes: malformed or non-strict JSON, `v` ≠ 1, unknown op,
  a bad or misplaced `pk`, `watch` after a request), `rate_limited` (shared bucket of 20, refilled 10/s),
  `timeout` (400 ms per request from receipt; an answer after it is a timeout, even one that also failed), then the
  authority error's class (`internal/authority`'s sentinels, matched with `errors.Is`): `authd_changed`, `auth_owner`,
  `key_ambiguous`, `key_unknown`, `shape`, `bus`; `auth_failed` for an authority error that wraps none (an oracle row
  for another key). `internal` is reserved (a helper shutting down or broken; this one sends none yet). `msg` has the
  error's text.
- An unknown frame type or an oversized frame closes the connection without a reply.

The watch is a private system bus connection with two match rules, both pinned to their sender:
`type='signal',sender='com.tesla.Auth',path='/Auth',interface='com.tesla.Auth',member='KeysUpdated'` and
`type='signal',sender='org.freedesktop.DBus',interface='org.freedesktop.DBus',member='NameOwnerChanged',
arg0='com.tesla.Auth'`. A match rule only filters broadcasts: any client may send a signal straight to the watch's
unique name. So the watch also learns `com.tesla.Auth`'s unique owner (GetNameOwner right after the rules, then each
NameOwnerChanged from the bus driver, whose sender no client can take) and counts KeysUpdated only from that owner.
AddMatch and GetNameOwner are bounded (2 s), the connection is checked every 5 s, and it reconnects with a 1 s → 10 s
backoff that resets only after a subscription held for 30 s (a flapping bus backs off). Tested against a real
dbus-daemon (`internal/authsrv/watch_test.go`): a KeysUpdated broadcast by another client, one sent straight to the
watch, and a NameOwnerChanged sent straight to it are all ignored, while authd's own is delivered.

The gateway's side (`authority.NewHelper` in agw, which has no bus access): two persistent request connections
(sign-ins; re-checks of open windows, so one never waits behind the other), one watch connection, a short-lived
request connection for `probe-auth`, the helper's uid checked to be 0, at most 400 ms per request. Each check is one
`key` request (it runs the owner check first). Open windows are re-checked at once on `keys_changed` (passes at least
1 s apart), every 30 s, and every 10 s while the watch is down or lost; a failed or unreachable check revokes the
window. Before serving, agw's bus self-test refuses to start unless the bus definitely refuses uid 4950 (`README.md`,
`deploy/README.md`).

Neither side logs a full key: only `kid` or the key fingerprint (SHA-256(pk)[:8]). Nothing the gateway sends
reaches the bus: `KeyInfo` takes no arguments, and pk is only compared with authd's rows.

dbus-daemon caches a uid's lookup for its lifetime: a uid that once resolved (for example during the 2026-11-20
overlay test) keeps connecting until the car reboots or the bus reloads its configuration.

## Children (every argv fixed, no shell)

Per CAMERA session: while parked (gear P) with the sessionActive hold, **six warm captures**, the five direct views
(9892, 9893, 9896, 9897, 9900) and the front bumper fisheye 9901 (the `fascia` view), whatever view is shown, so a
switch is answered at once from the capability's per-port GOP cache (`docs/CAMERA-PROTOCOL.md`, catch-up); **plus
9895** during the session's selectable hold, which CAMERA keeps at 3 (front, the windshield ultrawide) by default
while parked and held, or 1 while interior is selected, starting the capture 1 s after each selectable write (the
APE keeps sending the previous camera that long); **plus the talk writer** while talk is armed. At most 7 captures,
under the per-session limit of 8. While driving (the MCU runs the UI and navigation; the cost of always-on captures
is unmeasured), while a SUMMON window is open, and while parked without a hold, only the viewed stream(s) are
captured (front = 9898 while driving or during SUMMON, when 9895 carries the summon camera): the unwatched captures stop on
the status event that reports the gear change (or the 150 ms SUMMON poll), and warm up again once parked and held.
Two overlapping CAMERA windows cannot both hold the selectable: the second one's front waits (`busy`, retried every
5 s) until the first releases it. Captures are shared per port between sessions, so an
overlapping window adds subscriptions, not children. The multicast is local to the
car's Ethernet, so a warm capture costs only gst depay/parse CPU. Every one of them is
stopped by the session's release (`TestTeardownStopsEveryWarmCapture`). A DASHCAM session has no capture, only
ffmpeg cuts.

- **Capture** (per port): `/usr/bin/gst-launch-1.0 -q udpsrc multicast-group=224.0.0.155 multicast-iface=eth0
  port=<p> caps=application/x-rtp,media=video,encoding-name=H264,payload=96,clock-rate=90000 ! rtpjitterbuffer
  latency=150 ! rtph264depay ! h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au !
  fdsink fd=1`, with the car's `/etc/gst-registry.vars` values in its environment (prebuilt registry, never
  rebuilt). Receive only (an IGMP join, as the car's own recorders do); nothing is ever sent to 192.168.90.0/24.
  The helper splits the byte stream into access units (`internal/annexb`: AUD, SEI/SPS/PPS after a VCL,
  `first_mb_in_slice == 0`; cached SPS/PPS inserted before an IDR that lacks them; AUs over 1 MiB - 12 dropped). An
  AU is emitted when the next one starts: about one frame of latency. Per session at most 4 MiB of AUs are queued;
  above that it drops to the port's next IDR.
- **Talk writer** (talk armed): `-writer pw-cat` (default) `/usr/bin/pw-cat --playback --target webcam --format s16
  --rate 48000 --channels 1 -`, or `-writer aplay` `/usr/bin/aplay -D webcam -t raw -f S16_LE -r 48000 -c 1 -`, with
  the PipeWire client environment. Attached right after the edge (an idle route is reaped about 4 s after arming).
  **Silence fill is the capability's job**: CAMERA paces queued PCM, or silence, to the helper in real time
  (20 ms chunks, about 100 ms ahead) for as long as talk is armed; the helper is a bounded pipe (64 chunks; overflow
  dropped).
- **Cut**: `/usr/bin/ffmpeg -nostdin -hide_banner -loglevel error -y -protocol_whitelist file -ss S -t T -f mov -i
  file:<resolved clip> -c copy -movflags +faststart -fs <budget left> -f mp4 file:<cut dir>/<random>.mp4` (30 s
  bound). The forced demuxer and the file-only protocol whitelist keep a crafted clip from opening anything else.
  Before it starts, the cut dir must have at least the remaining budget free (statfs), else `busy`; a cut that
  reaches the `-fs` limit is truncated and dropped (`cut_failed`).

## TeslaCam access

Listing mirrors the laptop viewer's `INVENTORY_CMD`: `SentryClips/*/` and `SavedClips/*/` (sorted, hidden names
skipped) with `event.json` (≤ 64 KiB) and `*.mp4`, then `RecentClips/*.mp4`. Reads and cuts accept only a clean
absolute path under the root whose realpath (symlinks resolved) is still under the root's realpath, opened
`O_NOFOLLOW` and required to be a regular file. Listed entries obey the same realpath rule (the USB drive is FAT, so
there are no symlinks in practice). No drive mounted: an empty listing.

## SUMMON (CAMERA side)

CAMERA polls `Window.Host()` every 150 ms. With `SummonLinkDegraded` every stream is keyframe-only. With
`SummonOpen` it copies Tesla's summon webcam (design §6.5; `WebcamCommandHandler::checkWebcamAllowed` for a summon
session, 2026.38.6): `talk_disarm` (with the edge: the hold stays, so the exterior stream is un-routed; the car is
still parked when the window opens), the hold kept, and `summon on`, asked every tick until granted (only once talk is
off). The helper
grants it only while summonhelper holds the lock on its live file (above), which it does only while the engine of a
Summon session that opted into summonhelper's presence lease is up. agw cannot open such a session alone (it has no
phone's private key), but it can stretch one a phone opened by holding the phone's frames back, by at most 10 s past
the phone's last fresh tick echo (before any echo, 10 s plus up to 2 s past its ARM). A session that did not opt in
can be stretched by about 20 s per held frame, so it makes no live file and never gets SUMMON mode (ruling F1;
`docs/SUMMON.md`, "The live file" and "The presence lease"): the Summon camera needs a phone build that opts in.
Once granted, the selectable is the summon camera
for the gear, whatever view is shown: 11 Grid in P, N
or an unknown gear, 3 (the front fisheye) in D, 9 Backup in R. Tesla falls back to 3 when its cellular data limit
is reached; airgapp has no such limit. Each change is released to 0 first, then written; 9895 is captured again 1 s
later. CAMERA keeps capturing only the watched views (front on 9898, the `summon` view on 9895) and logs
`capture_dead_during_summon port=… view=…` for a watched capture that stalls. When SUMMON closes: `sel 0`, then
`summon off`, then the parked default (3) and talk again (talk only once `summon off` was confirmed). A refused summon
camera (the gear changed under the request) is asked again after one tick. A `summon_end` from the helper (the car's Summon session
ended) makes CAMERA ask again. **Car check still open (design §8, supervised test 5):** whether the firmware's own
`checkWebcamAllowed` leaves the held session alone once the car leaves P. Our helper re-asserts sessionActive and
the selectable every second in SUMMON mode. See `docs/CAMERA-PROTOCOL.md`.

## What the helper never does

No generic DataValue setter (only the nine writes in `car.go`), no motion DV (`CAPI_*`, gear, rail, brake, creep,
Summon, self-park), no shell, no `sv`, no videod D-Bus, no kill by name, nothing sent to the vehicle LAN. Its only
bus calls are the sign-in ones (GetNameOwner, GetConnectionUnixUser, `com.tesla.Auth.KeyInfo` with no arguments,
two AddMatch rules); nothing the gateway sends is passed to the bus. Tests pin
each of these (`internal/camhelper/argv_test.go`).

## The car visit must verify

Read-only, owner present (`deploy/probe_media.sh`, run as root):

1. **Writer argv** (probe step 3): `pw-cat stdin: rc=0` keeps the default; otherwise `aplay stdin: rc=0` means
   reinstall with `HELPER_WRITER=aplay`; neither means talk is blocked until fixed.
2. **uid 4951 multicast receive** (probe step 1): every `port …` line must show `helper>0` (re-run with
   `--with-stream` if root sees nothing). `BLOCKED_FOR_NONROOT` means captures must change (firewall or permission).
3. **uid 4951 PipeWire access** (probe step 2): `helper>0` nodes.

Also check, read-only:

4. uid 4951 can read the clips (ffmpeg cuts run as 4951):
   `chpst -u :4951:4951:29:1021 head -c 8 /mnt/usb/TeslaCam/RecentClips/<any>.mp4 | od -c`. If refused, cuts fail
   (`cut_failed`; the app falls back to whole-file GETs): add the clips' group with `-child-groups`.
5. `df /run` (cut files live in tmpfs, at most 128 MiB).
6. The install script always uses our own runsvdir (`/var/lib/airgapp-gw/svdir`); it never links into another one.

First silent session after the install (attenuated, like TALK_RESULTS S2): STATUS gates and VIDEO flowing on
9895 (front: selectable 3 held, `front hold` in the session log at the close) and 9901 (fascia); an interior hold
diverts and restores the dashcam wide slot (session log event present); the talk route with a
-60 dBFS tone; `kill -9` of agw → the helper releases (`get-local-dv CarServer WEBCAM_webcamSessionActive` back to
false); `kill -9` of camhelper → the runit finish sweep clears the markers.

Also unverified: the exact `get-local-dv` text of the three camera DataValues after a 0 write (the readback
accepts `(true, 'false')`, `(true, '0')`, `(true, 0)`, `None` or an invalid DV as cleared, anything else keeps
the marker), so check `get-local-dv CarServer WEBCAM_webcamSessionActive` and `GUI_selectableCameraRequest` once
at the visit. Unverified on the car (desk-tested only): raw-stdin `pw-cat`, uid 4951 capture/PipeWire/clip access, the
`pw-link -l` feeder parse on the live graph, the AU latency and IDR sizes per port, the MCU CPU load of six or
seven warm gst captures while parked (and agw's GOP caches, at most 7 × 1.5 MiB), `audiod` resetting the boosted gain (the
helper re-asserts -6 every second anyway), and the runsvdir location.
