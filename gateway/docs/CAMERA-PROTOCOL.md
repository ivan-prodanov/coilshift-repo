# CAMERA / TALK / DASHCAM wire protocol (gateway capabilities)

Owner: the camera session. Spec source: `PLAN-B2 §4.9` (v1.3.7+) in
`~/Work/tesla-dump/2026.8.6/re/bridge/PLAN-B2-summon-productionization-2026-11-19.md`.
Core transport, auth, lease and lanes: `docs/CAPABILITY-API.md`. Car facts verified on the car:
`~/Work/tesla-dump/2026.38.6/re/bridge/camera/TALK_RESULTS.md`. The car side (root helper):
`docs/CAMERA-HELPER.md`.

All JSON is UTF-8, compact. Integers are big-endian. Lease 20000 ms for both capabilities (phone HEARTBEAT every 5 s).
Inbound JSON bodies are strict: an unknown field, a wrong type or trailing data is a protocol error (teardown,
BYE `error`).

## CAMERA (capability id 0x02), TALK inside it

ARM `params`: `{"view": "<view>", "mode": "full"|"keyframe", "talk": true|false}`. All optional: the defaults are
`front`, `full`, `false`. An unknown view or mode refuses the ARM (connection closed, no message).

Views: `front`, `rear`, `left_repeater`, `right_repeater`, `left_pillar`, `right_pillar`, `grid`, `interior`, `fascia`,
`summon`.

| View | Source on the car (multicast 224.0.0.155) |
|---|---|
| front | the windshield ultrawide: selectable 9895 with `GUI_selectableCameraRequest=3` (FrontFisheye) while parked; 9898 (the dashcam front stream) while driving or while a SUMMON window is open |
| rear | 9900 |
| left_repeater / right_repeater | 9897 / 9896 |
| left_pillar / right_pillar | 9893 / 9892 |
| grid | phone-tiled 2×2 from front, rear and the two repeaters; the pillars are single views only, never grid tiles (owner, 2026-11-27). The APE's own Grid, sel 11, is the `summon` view's |
| interior | selectable 9895 with `GUI_selectableCameraRequest=1` (parked only) |
| fascia | 9901, the front bumper fisheye (parked only: it needs the car's webcam session, which driving drops; off during SUMMON) |
| summon | **the Summon strip's view: only while a SUMMON window is open.** Selectable 9895 with the car's own summon camera, which follows the gear as Tesla's summon webcam does: `GUI_selectableCameraRequest=11` (the APE's Grid) in P, N or an unknown gear, 3 (the windshield ultrawide) in D, 9 (Backup) in R. No `dewarp` (shown as the car sends it, as in Tesla's strip). Outside SUMMON it has no stream (STATUS `select_refused`) |

Verified on the car 2026-11-20: 9901 is the bumper fisheye (selectable 12 shows the same camera), selectable 3 on
9895 is the windshield ultrawide, and 9895 with selectable 0 carries no video.

**The selectable** (`GUI_selectableCameraRequest`, one value at a time) diverts the dashcam "wide" recording slot
while held. While parked (gear P) with the window holding the car's webcam session and no SUMMON window, the window
holds 3 (front) by default, whatever view is shown (front, the grid, every direct view, fascia), so switching back
to front is answered from the warm 9895 capture; it holds 1 instead while `interior` is selected. While a SUMMON
window is open it holds the summon camera instead (11 Grid, 3 or 9 by gear, see `summon`), whatever view is shown,
as Tesla's summon webcam does, in any gear; while Grid is held the dashcam's wide recording captures the grid,
exactly as with stock Summon. It is restored to 0 while driving (outside SUMMON), when SUMMON closes and on every
teardown path, and released before another value is written. Each hold is recorded in the session log
(`Window.LogEvent`: `<camera> hold start=<ms> end=<ms> (dashcam wide slot diverted)`, `<camera>` being the held
camera, `front` (3), `interior` (1), `grid` (11) or `backup` (9), whatever view was on screen; ms since the window
opened). The APE can keep sending the previous camera on 9895 for up to about 1 s after a change, so
after every selectable write the car captures 9895 again only 1 s after the write succeeded (the settle), and
nothing captured before the change is ever sent or cached: a stream on 9895 starts at the new capture's first IDR.

The car becomes ready asynchronously: READY only means the window is open. STREAMS, STATUS and TALK_STATE follow
as the car side comes up (a stream is "ready" when its port flows, normally well within 12 s).

### Inbound (phone → car)

| Type | Name | Max | Body |
|---|---|---|---|
| 0x20 | SELECT | 64 | JSON `{"view": "<view>", "focus": "<single view>"}` (`focus` only with `grid`, one of its four tiles: that tile runs full-rate; default `front`, kept across SELECTs) |
| 0x21 | MODE | 32 | JSON `{"mode": "full"\|"keyframe"}` for the single view / grid focus |
| 0x22 | TALK_START | 64 | JSON `{}` (or empty): a message begins (push-to-talk boost to 0 dB) |
| 0x23 | TALK_PCM | 16384 | raw PCM, s16le, mono, 48000 Hz, even length (odd length is a protocol error) |
| 0x24 | TALK_STOP | 16 | JSON `{}` (or empty): the message ends; gain back to -20 dB 1.5 s after the queued audio has played |

TALK frames are accepted only when ARM had `"talk": true`; otherwise the capability returns an error (teardown).
The car plays queued PCM in real time and fills with silence; it buffers at most 45 s (more → `talk_overflow`
in TALK_STATE and STATUS, the excess is dropped). Queued PCM plays only once the route is up (`ready`); until then
the writer gets silence and the audio waits in the queue. TALK_PCM without TALK_START plays at the native -20 dB.

### Outbound (car → phone)

| Type | Name | Lane | Max | Body |
|---|---|---|---|---|
| 0xA0 | VIDEO | bulk | 1 MiB | 12-byte header + one H.264 access unit, Annex-B (4-byte start codes); SPS+PPS are present in every IDR AU |
| 0xA1 | STREAMS | priority | 1024 | JSON `{"view": "<view>", "focus": "<grid focus, grid only>", "streams": [{"index": 0, "view": "front", "full": true, "dewarp": true}, …]}` sent on open and on every SELECT/MODE/source change (stream fields below) |
| 0xA2 | STATUS | priority | 1024 | JSON, see below; sent on change and every 5 s |
| 0xA3 | TALK_STATE | priority | 256 | JSON `{"state": "off"\|"arming"\|"ready"\|"playing"\|"error", "queued_ms": <int>, "reason": "<code or empty>"}`; only when ARM had talk |

STREAMS stream fields: `index` (the VIDEO header's stream index), `view` (the camera), `full` (full rate; false:
keyframe-only), `dewarp` (present, `true`, only on a stream from 9895 while selectable 3 is held: the windshield
ultrawide, i.e. front while parked, single or the grid's tile; absent everywhere else, never on 9898). The phone
undistorts a `dewarp` stream: radial lens correction with k1 = -0.25 (k2 = 0) in ffmpeg `lenscorrection` semantics,
`r_src = r_tgt * (1 + k1 * (r_tgt / r0)^2 + k2 * (r_tgt / r0)^4)`, centred on the image, r0 = half the image
diagonal; then it trims the bottom 3% of the picture.

VIDEO header (12 bytes):

| Offset | Size | Field |
|---|---|---|
| 0 | 1 | version = 1 |
| 1 | 1 | stream index (matches STREAMS) |
| 2 | 1 | flags: bit0 = IDR (keyframe), bit1 = SPS/PPS included, bit2 = CATCHUP (decode, do not display) |
| 3 | 1 | reserved = 0 |
| 4 | 8 | capture time, microseconds (car monotonic since the helper started; for jitter/latency display only) |

Every new stream starts at an IDR with SPS/PPS: after STREAMS, after a source change (front 9895 ↔ 9898), after
`keyframe` → `full`, and after any drop.

**Warm captures and catch-up.** While the car is parked (gear P) and the window holds the car's webcam session,
the car captures the five direct views (rear, repeaters, pillars), fascia (9901) and 9895 (front's selectable 3 by
default, interior's 1 while interior is selected; from the end of its settle) whatever is shown: at most 7. While
driving it captures only the viewed stream(s) (front = 9898), so a switch then waits for the next IDR; the
unwatched captures stop as soon as the car leaves P and warm up again once it is parked and held. For each
captured port the car keeps the current GOP: every AU since the last IDR, up to 1.5 MiB (a larger GOP is not cached until the next
IDR). When a stream starts on such a port (SELECT, entering the grid, a new grid focus, MODE `keyframe` → `full`),
the car sends STREAMS and then, at once, that port's cached GOP as VIDEO frames in order: every AU but the newest
has flags bit2 CATCHUP (decode it to rebuild the reference pictures, do not display it); the newest is displayed;
live AUs follow. A keyframe-only stream gets just the cached IDR (displayed). The burst uses the bulk lane and the
watermarks below: above the high watermark (or with no cache) there is no catch-up and the stream waits for its
next IDR, as does a stream whose burst was cut short. A phone that ignores bit2 just shows a quick fast-forward
of up to 0.75 s. Under bulk-lane pressure the car drops access units (watermarks: drop
everything above 1 MiB queued; below 256 KiB each stream resumes at its next IDR); a full lane (`TrySend` false)
drops to the next IDR too. In `keyframe` mode only IDRs are sent (about every 0.75 s). In grid the focused tile is
full-rate, the other tiles keyframe-only. The grid stream indices are fixed: 0 front, 1 rear, 2 left_repeater,
3 right_repeater (no pillars). A camera that is not available has no stream (STATUS `reason` says
why): `interior` without its selectable 1, front while parked without its selectable 3 (`"streams": []`; in the grid
the index 0 tile is left out, e.g. while another window holds the selectable or until the first write),
`fascia` while driving or during SUMMON, and `summon` outside SUMMON or until the summon camera is held (and 1 s
after each change of it).

STATUS JSON:
```json
{"gear": "P", "driving": false, "sentry": "Armed", "view": "front", "streams_ok": true,
 "route": "ok|down|off", "talk": "off|arming|ready|playing|error", "gain_db": -20, "reason": ""}
```
`gear` is `P`/`R`/`N`/`D` or `""` (unknown), `driving` is gear R/N/D. `streams_ok`: every current stream had an
access unit in the last 3 s. `route` is `off` unless talk is armed. `gain_db` is the exterior gain the car last set
for this talk session (-20 or 0), `null` while talk is off or the gain is untouched.

`reason` (one, by priority): `summon` (a SUMMON window is open), `helper_down`, `gate_gear`, `gate_sentry`, `gate_camera_access` (the car's
checkWebcamAllowed refused the hold; video may still flow, e.g. while driving), `select_refused`
(interior while not parked, or front/the grid while parked, when the selectable they need was refused or is held
by another window; fascia while driving; a refused default hold behind another view is not reported), `no_video`
(a selected port sent nothing for 12 s; the capture is restarted), `route_lost`, `talk_overflow`, then a talk-arm
refusal (`exterior_busy`: a Boombox or the megaphone owns the exterior speaker; `busy`; `spawn_failed`; a gate
code), or `""`.

**SUMMON.** The car checks every 150 ms whether a SUMMON window is open or reports a degraded link:

- link degraded: every stream is keyframe-only (STREAMS `full: false`); the phone's mode returns, with a catch-up,
  when it clears;
- SUMMON open (design §6.5, copying Tesla's summon webcam): talk is disarmed (TALK_STATE `error`, reason
  `summon`; queued PCM is dropped); the car's webcam session stays held, and the car asks its root helper for
  SUMMON mode, which is granted only while the car's own Summon session is live and waives the gear, Sentry and
  camera-access gates, as Tesla does for a summon session. From then on the selectable is the summon camera for
  the gear (11 Grid in P/N, 3 in D, 9 Backup in R), whatever view is shown, so the `summon` view streams it (STREAMS
  `view: "summon"`, index 0, no `dewarp`; after each gear change the stream resumes at a fresh IDR about 1 s
  later, at most about 1 s after the gear changed, with its own SPS/PPS, as the camera behind it changed).
  Interior and fascia have no stream; every other view the phone watches keeps streaming (grid tiles too), front
  from 9898 (no `dewarp`). A watched capture that stalls is restarted and logged in the session log as
  `capture_dead_during_summon port=<p> view=<v>`. STATUS reason `summon` throughout. When SUMMON closes the summon
  camera goes back to 0, then the normal rules resume (the parked default selectable, talk).

TALK_STATE `state`: `off` (no talk in ARM), `arming` (route not up yet; `reason` `route_lost` after it was up, or
`helper_down`), `ready`, `playing` (audio queued, a message open, or within the 1.5 s tail), `error` (`reason`: the
helper's refusal or gate; the car retries every 5 s). `queued_ms` updates at most twice a second.

## DASHCAM (capability id 0x03)

ARM `params`: `{}` (empty or absent). Read-only access to `/mnt/usb/TeslaCam` (path allowlist enforced after
symlink resolution, on the car by the root helper).

### Inbound

| Type | Name | Max | Body |
|---|---|---|---|
| 0x40 | LIST | 256 | JSON `{"id": <u32>}` |
| 0x41 | GET | 512 | JSON `{"id": <u32>, "path": "/mnt/usb/TeslaCam/…mp4", "offset": <int>, "length": <int>, "ss": <sec, optional>, "t": <sec, optional, ≤120>}` |
| 0x42 | CANCEL | 64 | JSON `{"id": <u32>}` |

GET serves `[offset, offset+length)` clipped to the file; `length` 0 means "to the end". `ss` and `t` come together
(`ss` ≥ 0, 0 < `t` ≤ 120; anything else is a protocol error). With `ss`/`t` the car first cuts that window with
`ffmpeg -c copy -movflags +faststart` into a temp file (bounded by the car's cut budget; ERROR `busy` when the car
lacks the room) and serves `offset`/`length` from the cut; the cut file is
reused for the same (path, ss, t) until the window closes (at most 4 cuts per window are kept). GETs are served
one at a time in order (up to 8 queued; more, or a duplicate id still queued or running, gets ERROR `busy`). CANCEL
drops a queued GET or stops a running one; chunks already in flight may still arrive.

### Outbound

| Type | Name | Lane | Max | Body |
|---|---|---|---|---|
| 0xC0 | LIST_RESULT | priority | 65536 | JSON `{"id": n, "part": i, "last": bool, "events": [...]}` (split across frames) |
| 0xC1 | CHUNK | bulk | 65536 | header `u32 id, u64 offset, u64 total` (20 bytes) + data (≤ 65516 bytes) |
| 0xC2 | ERROR | priority | 256 | JSON `{"id": n, "error": "<code>"}`: `not_found`, `forbidden_path`, `cut_failed`, `busy`, `io` |

A GET answers with CHUNKs in offset order, or one ERROR. There is always at least one CHUNK (an empty one at
`offset` when `offset` ≥ `total`), so `total` is always learned; the transfer is complete when
`offset + len(data)` reaches `min(offset+length, total)`. A LIST answers with parts `0..n` (only the last has
`"last": true`; concatenating their `events` arrays gives the whole list) or one ERROR (`io`, `busy`).

Event objects in LIST_RESULT are EXACTLY today's laptop viewer output (`re/bridge/camera/dashcam_viewer.py`
`build_events`, consumed by the app's `src/hooks/useDashcamClips.ts`), so the app's parsing stays unchanged. Saved
and Sentry events, in this key order:
`{"type": "SavedClips"|"SentryClips", "folder", "path", "reason", "city", "lat", "lon", "trigger_cam", "minutes":
{"<YYYY-MM-DD_HH-MM-SS>": {"<camera>": {"path", "size"}}}, "thumb", "timestamp" (only when event.json parsed),
"minute_list", "start", "trigger_cam_key", "trigger_minute", "trigger_offset"}`; the RecentClips event:
`{"type": "RecentClips", "folder", "path", "reason": "rolling buffer", "city", "lat", "lon", "trigger_cam",
"trigger_cam_key", "trigger_minute", "trigger_offset", "timestamp", "minutes", "minute_list" (newest first),
"start", "thumb"}`. Cameras are `front`, `back`, `left_repeater`, `right_repeater`, `left_pillar`, `right_pillar`.
Order: Sentry, Saved (each oldest `start` first), Recent. `reason`/`city`/`lat`/`lon`/`timestamp` keep event.json's
JSON types; `trigger_offset` is a float or null. `thumb` is null over the gateway (the phone fetches a short
`ss`/`t` window instead). The golden test (`capability/dashcam/testdata`) is produced by the Python itself.
An event that alone would exceed one LIST_RESULT frame (only a RecentClips buffer of more than about 90 minutes)
loses its oldest minutes until it fits.
