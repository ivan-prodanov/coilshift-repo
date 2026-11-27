# Summon behaviour spec: copy the Tesla app (Come to Me, Go to Target, dead_man arrows)

**Appendix A to `2026-11-20-summon-design.md`.** Extracted read-only on 2026-11-20 (workflow wf_add6fec7-a3b:
three reports, then a synthesis). Evidence and the raw reports:
`~/Work/tesla-firmware/work/app-re/derived/summon-ui/behaviour-spec-2026-11-20/`.

**Verification status.** The app-protocol and firmware-side reports were adversarially verified. The app-ui-status
report was not (its verdicts are null in `workflow-result.json`). So the UI and status rows, mainly §2.3, §4.1-§4.4,
§4.6, §5, §6 and §7, are unverified unless marked "(re-checked)". A read-only verification pass over them runs
before the screen is built (design §6.4).

**Where the design overrides this appendix** (the design wins):
- **§2.6 (D2), design §5.2.** Every motion-relevant phone frame carries a freshness tick:
  - a native `LIVE` frame on every tick while a control is physically held, which is liveness only;
  - `VERB` and `HB` frames, forwarded only if their tick is no more than 150 ms old.

  Liveness is fresh `LIVE`, `HB` or forwarded `VERB` frames only; the verb's `cmd_result` no longer counts.
  verbActive is cleared only by an abort or session end. A stale verb, or a verb while one is active, is refused.
  After a trip the helper forwards no heartbeat until the next verb, so Tesla's own 2 s / 10 s path shows. The
  "[AIRGAPP implementation]" note in §2.6 is replaced.
- **§2.2-§2.3, design §5.2.** The motion controls are native views that reproduce Tesla's Pressability release
  rules: lift, cancel, sliding out of the frame expanded by 20/20/20/30 pt, one responder, and a slide back in is a
  new press. Every release sends `cmd_abort` immediately, from native code. Heartbeats and verbs go out only while
  the press is held, so the "quick tap leaks heartbeats into Ready" quirk cannot occur.
- **§3.4 (D3), design §5.3.**
  - At each press the floor applies to every verb's phone position and goal, the arrows included, with the goal
    derived as the car derives it.
  - The car position goes stale on a silent stream.
  - All motion controls are disabled on the phone within 3 m.
  - `device_location` follows Tesla at all times (owner, 2026-11-21).
- **§1.2 H2.** Non-conforming non-verb messages are dropped and logged, while a non-conforming verb or framing ends
  the session.
- **§1.2 H3.** Car-call order is captured on godbus's read goroutine.
- **§4.3, §7.** The secure-connection alert has a concrete trigger (design §6.4).
- **§6 and §9 #10, the camera strip.** It is answered by the firmware. For `webcam:signal` with context `summon`,
  QtCarServer skips its webcam checks and runs `startWebcam(summon=true)`: selectable Grid (0xb), reason Webcam,
  `WEBCAM_webcamSessionActive` = 1, and `summon-webcam` capturing 9895 at 456×342, 10 fps, about 495 kbps
  (QtCarServer.c:1376760-1376866, 1378449-1378535). airgapp copies it: Grid while SUMMON is open (owner, 2026-11-21; design §6.5).
- **§1.1 keepalive.** Tesla parity is supported by the car's cloud-side `ControlCommandHandler` keepalive branch
  (QtCarServer.c:1174135-1174300). Its effect on this car is still measured in design §8.
- **Research note (correction).** Every research run kept stock `webrtc-comms` as the owner of `com.tesla.Webrtc`;
  the laptop was the phone's WebRTC peer. A non-stock owner has never been tried (design §8, check 4).

This spec is for the airgapp phone screen and the on-car SUMMON helper (uid webrtc, owns com.tesla.Webrtc). The car is fw 2026.8.6, EU, autopark_style "dead_man".

It merges three reports and their verifiers: app-protocol, app-ui-status and firmware-side. It also merges the prior recon in ~/Work/tesla-firmware/work/app-re/derived/summon-ui/summon-screen-recon-2026-11-20.json and summon-gtt-recon-2026-11-20.json. Verifier corrections are applied and refuted claims are dropped. Items the synthesizer re-read in primary sources are marked "(re-checked)". Everything is read-only; nothing was run on the car.

## 0. Rules, sources, conventions

**The copy rule.** The owner's directive (memory summon-copy-tesla.md) is "copy from tesla app, every decision". Every behaviour below is the Android 4.58.0-4392 app. iOS 4.57.5 matches wherever it was checked. Only three deviations are allowed, each stated in one line:
- **D1 [STANDBY-ADAPT].** This car always reports "standby". The fix is a phone-side status normalizer (§4.5).
- **D2 [OWNER-DEADMAN].** A 300 ms deadman on signal loss, enforced in the helper (§2.6).
- **D3 [OWNER-3M].** A goal is never within 3 m of the car's GPS (§3.4).

**Other tags.**
- [AIRGAPP-EQUIV] replaces only Tesla's cloud or WebRTC transport, never a behaviour.
- [INFERRED] means reasoned from code, not observed.
- [UNVERIFIED] means not proven.

**Dropped from the old Pi-gateway plan.** The owner rejected these as invented policy:
- one press per window;
- a cool-down of 35 s or more;
- a locked mode toggle;
- a goal frozen at press;
- no device_location before the verb;
- a 30 m accuracy cap;
- a 15 s no-start rule;
- CARLOC carrying lat/lon/heading only;
- one heartbeat per HOLD;
- warning texts driven by the gateway.

Release now means pause, and pressing again resumes (§8.3).

**Refuted claims. Do not build these.**
- **A 10 s command-ack timer ("Request timed out").** It is dead code. The commandTimer entry is a closure that only *returns* `send(COMMAND_FAILED{COMMAND_TIMED_OUT},{delay:CommandTimeoutThresholdMs,id:'commandTimer'})`. xstate's toActionObject wraps any function as {type, exec}, and exec's return value is discarded.
  - Re-checked: H L5798406-5798442 (fn#125162) and L5774745 (fn#124657 +0x13a-0x151).
  - iOS has the same: fn#119033.
  - So 'Request timed out' (enA.tsv:312) is unreachable.
- **'Communicating' on the first connect.** On the first connect, CONNECTION_SUCCESSFUL's choose runs only its first branch (FromUnconnectedStates), so 'Connecting' stays. Evidence: S/machine_config.json root `on.CONNECTION_SUCCESSFUL`; fn#124688 +0x318..0x345.
- **A device_location on entering WaitingForInitialStatus.** The actions sit under the key 'enter', which xstate never reads. Evidence: fn#125161 +0x831; StateNode reads only entry/onEntry (fn#124997 +0x52f/+0x53e); iOS fn#119032.
- **A device_location after every car status.** It is sent only for smart 'unavailable' + 'no_phone_location' (fn#125352 +0x199..0x229).
- **"PARKER cars show 'Summon is not ready'".** It is the reverse: PARKER shows 'Close doors and complete charging' and all others show 'Summon is not ready' (D/sm.dis:4927-4975, re-checked).
- **The caution banner shows once per install.** Both flags are on the appSettings persist blacklist, so it shows once per app launch (H L3057709, L3057725, re-checked).
- **The car self-resumes from UserPaused on a heartbeat.** Only Pause (0x31) resumes on a heartbeat, and 0x31 is unreachable on this car.
- **cmd_abort tears the session down.** abort() never ends the session.
- **A hello retunes the car's deadman.** It cannot: the handler is a no-op slot (§8.5).
- **The app-ui-status normalizer's rule "reason '' → 'ready' while pressed".** In summonInProgress that status yields STATUS_ELIGIBLE, which moves to ready and runs stopSummonHeartbeat (machine_config summonInProgress.on). The heartbeat would stop mid-run. §4.5 replaces that rule.
- **"Car STATUS exists only inside a window" and the gateway-made warning or CARLOC adaptations.** These are obsolete: the helper session lasts as long as the screen, and the helper forwards every car frame verbatim.

**Source abbreviations.**
- **H** = ~/Work/tesla-summon/work/bundle.hasm. This is the Hermes disassembly of the Android 4.58 bundle ~/Work/tesla-firmware/work/app-re/index.android.bundle. Cited as fn#N +0xOFF or line L.
- **D** = ~/Work/tesla-firmware/work/app-re/derived/summon-ui/ (sm.dis, ui_a.dis, map.dis, enA.tsv, reasonmaps.txt, enums_wire.txt).
- **S** = ~/Work/tesla-firmware/work/app-re/derived/summon-ui/behaviour-spec-2026-11-20/evidence/out/ (machine_config.json, provider.dis, negotiator.dis, rtt.dis, entry_home.dis, entry_controls.dis, reqloc.dis, reqprecise.dis).
- **Q** = ~/Work/tesla-dump/2026.8.6/re/ghidra/decomp/QtCarServer.c
- **G** = ~/Work/tesla-dump/2026.8.6/re/ghidra/decomp/libQtCarGUI.so.1.0.0.c
- **V** = ~/Work/tesla-dump/2026.8.6/re/ghidra/decomp/libQtCarVAPI.so.1.0.0.c
- **W** = ~/Work/tesla-dump/2026.8.6/re/ghidra/decomp/webrtc-comms.c
- **ELF** = ~/Work/tesla-dump/2026.8.6/usr/tesla/UI/bin/QtCarServer (virtual addresses).

**iOS 4.57.5 parity.** These functions are identical where checked:
- executeSummonCommand fn#119225 (1732 B);
- sendDeviceLocationUpdate fn#119353 (1284 B);
- the dead 'enter' fn#119032;
- the dead timer fn#119033;
- 600 ms fn#119396;
- 500 ms fn#119292;
- 2000/10000/100 fn#118438-#118440;
- the entry paths fn#97553-#97556 and #95566-#95570.

The status/enable selectors and the camera were read from Android only [UNVERIFIED on iOS].

## 1. Session lifecycle

### 1.1 Phone (copy)

1. **Mount** (VehicleSummonScreenV2 effects run once, with empty deps).
   - connectIfNeeded, analytics, and an AppState 'change' listener plus a BackHandler listener.
   - If the channels are already open, send CONNECTION_SUCCESSFUL. Otherwise call reconnect(true) and send CONNECTION_REQUESTED. The machine goes to `connecting`: text 'Connecting' (enA:298), both busy flags set, and a 15-pt spinner.
   - Evidence: fn#124448, fn#124452 +0x5..0xa4, fn#125336.
   - Location starts too (§5.2).
2. **Session up.** Send CONNECTION_SUCCESSFUL (guard sameVehicleAndNotStateReady). The machine goes to connected.waitingForInitialStatus with [SetConnectionState, SetWaitingForInitialStatusFlag, StartMonitorDeviceLocation]. StartMonitorDeviceLocation is a no-op emitter.
   - The text stays 'Connecting'.
   - **The phone sends nothing at this edge.**
3. **The car pushes, on connect** (§1.2 H1): autopark:style, autopark:status, and one vehicle_data:location immediately.
   - For **every** autopark:style the phone does two things:
     - It sends RECEIVED_SUMMON_STYLE; isDeadman = (autopark_style === 'dead_man').
     - It **always** replies with one autopark:info on the same channel: `{txid:'<none>', x_tesla_user_agent:UA, model, name:deviceName, tag:UA, public_key_hash:public_key_id}`.
   - Evidence: fn#125476, fn#125475.
   - This is the only info sender. The 'sendInfoRequest' listener has no emitter (H L5813856).
4. **First status, or quirk: the first vitals or changed fix.** With the flag still set, checkEligibility returns NOT_CONNECTED. checkSummonEligibility has no case for it and falls through to STATUS_ELIGIBLE, so the machine goes to `ready` (fn#125257, fn#125305 +0x2c3).
   - Ready's entry runs checkSummonEligibility again with the flag cleared (machine_config ready.entry). It then settles to ready or notReady by the normalized status (§4.5).
   - A null stored status yields STATUS_UNKNOWN; an undefined one is treated as eligible (D/sm.dis #125252 +0x1ef-0x215, re-checked).
5. **While mounted.**
   - device_location for each changed fix, in every state (§3.1).
   - Statuses and vitals are received.
   - **No heartbeat and no command until a press is acknowledged.**
6. **Keepalive.** Tesla sends a CONTROL 'keepalive' at mount and then every 15000 ms. It goes on the cloud signaling socket, not the data channel (fn#125434 +0x37, fn#125435). On the car, 'control' sets GUI_remoteAccPowerRequest=true (Q:1165399-1165409).
   - [AIRGAPP-EQUIV] Send `{msg_type:'control:keepalive'}` over the helper session at session-up and then every 15000 ms. That Tesla's relay forwards it to the car is [INFERRED].
7. **Exit, blur, background, back:** see §7.
8. **Connection loss.** DISCONNECTED moves the machine to unconnected.notConnected, which plays mediumHaptic('not connected') if it was executing (D/sm.dis:7292-7311).
   - Reconnect 1000 ms later while mounted and active (scheduleDelayedReconnect fn#98482 +0xc7).
   - After a heartbeat-critical timeout (10000 ms) or an RTT above 10 s: DISCONNECTED, then disconnect, then **500 ms** later reconnect(true) + CONNECTION_REQUESTED (fn#125421 +0x8c, fn#125452).
   - **None of these paths send cmd_abort.**
   - The connect attempt timer is 20000 ms (fn#98467 +0x101).
   - The retry budget useRef(3)/6 is [UNVERIFIED].
   - There is no initial-status timer: INITIAL_STATUS_TIMED_OUT is only declared in the enum (fn#67643).

### 1.2 Helper (copy what stock webrtc-comms does toward QtCarServer)

**D-Bus contract.**
- The helper owns com.tesla.Webrtc at /Webrtc (W:106588/106599).
- QtCarServer connects to two signals: `LocalToRemoteSignaling(QVariantMap)` and `PeerCommsToCarserverData(QVariantMap)` (Q:1152824-1152826, re-checked).
- QtCarServer calls two methods:
  - `CarserverToPeerCommsData(QVariantMap)`, async, reply watched (Q:1152930-1152946);
  - `RemoteToLocalSignaling(QVariantMap)` (Q:1156506-1156740).

**H1. Session up.** Once the phone session is authenticated, emit LocalToRemoteSignaling `{data_channels_ready:true}` **once**.
- Stock does the same: W:109947. The car only checks that the key is present; the value does not matter (Q:1160537-1160586, re-checked).
- On the false→true edge, webRTCConnectedImpl does the following (Q:1163641-1163667, 1163403-1163616, 1166063-1166259):
  - sets CAPI_remoteWebRTCConnected and CAPI_remoteAutopilotConnected to true;
  - stops the 60 s timer;
  - runs remoteRefreshState(true): CAPI_remoteHeartbeat DV = 0, status cache cleared, then sends **autopark:style + autopark:status**;
  - runs homelinkStateChanged;
  - starts vehicle_data:location sampling, with an immediate first frame.
- **Side effect: the car pre-primes.** SelfparkStateIdle is wired to CAPI_remoteAutopilotConnected through dvConnect (G:4932588-4932590). Its remoteSocketConnectionChanged sets GUI_selfParkActivation=4 (StandbyMode) and calls initiateSelfpark when all of these hold (G:4943549-4943625, re-checked):
  - hasSmartSummon;
  - no factory summon;
  - !autoSummonOk;
  - GTW_chargeState==0;
  - the display-state check passes;
  - GUI_selfParkInhibited==0;
  - GUI_selfParkStandbyMode is set.
- The activation enum (ELF 0x18674e0, decoded): 0 None, 1 Keyfob, 2 RemoteAutopark, 3 AutoSummon, 4 StandbyMode, 5 Factory.
- So opening the session wakes the buses, looks for the key and turns the drive rail on. This is the '' → detect_key_fob → turn_on_drive_rail cycle seen with no command sent (ON-CAR-STATE.md:160-166). Linking the two is [INFERRED].
- Prep timeouts (G:3018790-3018802, 4941305-4941314, 4941345-4941349):
  - selfpark/pre_primed_timeout_ms = 35000;
  - primed_timeout_ms = 20000, which leads to NotStartedTimeout;
  - NotStartedTimeout returns to Idle after 1000 ms.

**H2. Phone → car.** Each authenticated frame becomes PeerCommsToCarserverData `{uid:<non-empty>, data:<the phone's JSON text, verbatim>}`. QtCarServer (Q:1159722-1160080, 1165286-1165325):
- drops the frame if uid or data is missing;
- parses the JSON;
- treats any parsed frame as an assertion of "connected" (webRTCConnected(true));
- unwraps a top-level "_request" key;
- requires msg_type to split into exactly two parts on ':', and lower-cases both.

[AIRGAPP hardening, behaviour-neutral] Allowlist exactly what Tesla's app sends: autopark: cmd_find_me, cmd_forward, cmd_reverse, cmd_abort, device_location, info, heartbeat_app_webrtc, plus control:keepalive. Drop everything else.

**H3. Car → phone.**
- Implement CarserverToPeerCommsData and **return the D-Bus reply immediately**, before touching the phone link.
- Forward `{channel, data}` to the phone, preserving the channel and the order within each channel:
  - 'dcTcp' carries status, style, cmd_result, error and location (pref 0);
  - 'dcUdp' carries heartbeat_car_webrtc (pref 5), heartbeat_car, viz and occupancy (pref 2).
- Car frames carry uid 'test_phone' (Q:1155195-1155518, re-checked).

**H4. Session end** (phone closes, auth or lease lost, or the helper stops). In this order:
1. If a verb is active (§2.6), send cmd_abort.
2. Emit LocalToRemoteSignaling `{ice_state_change:true, msg_type:'autopark:set_webrtc', set_webrtc_value:false}` **exactly once**. The car checks only that ice_state_change is present and that the value is false. It then calls webRTCConnected(false) synchronously, as a direct call, not queued (Q:1160467-1160534, 1152253-1152285).
3. Drop all phone frames until the next session-up, because any parsed frame re-asserts "connected".

On the car (Q:1153086-1153185, 1154017-1154196; G:4938350-4938383; Q:1156744-1156844):
- RemoteSummon::disconnected writes CAPI_remoteAutopark=3 **only if** inAbortableState and a verb was accepted on this connection.
- It invalidates the phone GPS and accuracy, the device name, the key SHA1 and the goal.
- SelfparkManager moves Primed..Active, RemotePreAbort or UserPaused to **RemoteAbort, with no pause**.
- Outbound frames stop.
- 60000 ms later the car sends RemoteToLocalSignaling `{type:9, uid:'test_phone', session_id:''}`. The helper replies and closes any phone session.

**H5. Do not copy webrtc-comms' ICE mirroring.** Stock emits set_webrtc_value=(state==2) on every ICE transition, including connected→completed (W:106628-106874, 106780). That would abort a run mid-way. The helper emits connect and disconnect once per session.

**H6. A finish hook is mandatory.** QtCarServer does not watch the owner of com.tesla.Webrtc: none of its 10 QDBusServiceWatcher sites covers it, and failed calls are only logged (Q:1153698-1153967). Without H4, the car stops only through its 10 s stop timer, then UserPaused, then 35 s more: about 45.3 s in all (§8.5).

**H7. What the helper may originate.** It never synthesizes heartbeats or motion verbs. It may originate only:
- cmd_abort (D2 and H4);
- the disconnect map;
- refusal cmd_results (§3.4).

It does not forward the cmd_result of an abort it originated itself.

## 2. Controls (isDeadman = true)

### 2.1 Wire messages, phone → car

Every message is serialized as `{msg_type, ...payload, 'created:timestamp': Date.now()}` (fn#100074). Commands and info go on TCP; heartbeats go on UDP. The car does not care which channel an inbound frame used.

```
autopark:cmd_find_me  {latitude, longitude, accuracy(fix|-1), goal_latitude, goal_longitude, client_version:2, summon_type:'find_me'|'pin_drop'}   fn#125465
autopark:cmd_forward  {latitude, longitude, client_version:2}      fn#125466
autopark:cmd_reverse  {latitude, longitude, client_version:2}      fn#125467
autopark:cmd_abort    {}   (payload-less)                          fn#125464
autopark:heartbeat_app_webrtc {timestamp:Date.now(), car_timestamp:<last car hb car_timestamp; omitted while 0/null>}   fn#125481
autopark:device_location {latitude, longitude, accuracy, altitude, altitude_accuracy, goal_latitude, goal_longitude, summon_type}   fn#125482 (§3.1)
autopark:info {txid:'<none>', x_tesla_user_agent, model, name, tag, public_key_hash}   fn#125475
```

- SummonApiVersion = 2 (fn#67643 +0xaf).
- The app never sends heartbeat_app, cmd_pause, cmd_resume, or hello on the data channel.

### 2.2 Press / hold / release / re-press

Every press or release is SET_BUTTON_PRESS_STATE{button, pressed}. The handler is top level and acts only when the pressed state changes. It runs executeSummonCommand, which emits **one** message (fn#125348, fn#125354).

Preconditions for a press (fn#125354):
- If there is no phone fix or no vehicle location, the press is a silent NO_OPS: nothing is sent and nothing is shown.
- If there is a fix whose lat or lon is null, the result is a local COMMAND_FAILED{DEVICE_LOCATION_UNKNOWN}.

After the press, the machine goes from ready or summonInProgress to **waitingForCommandResponse** (WFCR).

| Control | Press-in | While held | Release (dead_man) | Re-press |
|---|---|---|---|---|
| **Come to Me** (mode FIND_ME, or GTT with no pin) | One cmd_find_me. Goal = the same phone fix, 'find_me'. No device_location, no haptic. | heartbeat_app_webrtc every 100 ms on UDP, **only after the car's cmd_result**. device_location for each changed fix, goal = the new fix. Nothing is ever re-sent. | One cmd_abort + stopSummonHeartbeat. Then WFCR until the abort's cmd_result, then ready. | Identical fresh cmd_find_me with the current fix. Within 35 s the car **resumes** (§8.3). There is no resume message. |
| **Go to Target** (mode GTT, pin set) | One cmd_find_me. Goal = findMeTarget, 'pin_drop'. lat/lon/accuracy are still the phone fix. Then the press-capture effect re-sets the target from the projected map centre and sends a device_location('pin_drop'). The order "cmd first" is [INFERRED]. | Same as Come to Me, but device_location carries goal = pin, 'pin_drop'. The crosshair is hidden and a pin Marker is drawn. | cmd_abort + stop heartbeat. The map recentres on the pin. 600 ms later editing resumes and a device_location('pin_drop') follows [INFERRED]. | Fresh cmd_find_me 'pin_drop' with the current pin. |
| **Up / Down arrow** (FORWARD 0 / REVERSE 1) | One cmd_forward / cmd_reverse. isExecutingForward/Reverse = connected. The opposite arrow and the hold button are disabled. | Heartbeats as above. device_location for each fix still carries **the current mode's** goal and summon_type. | cmd_abort + stop heartbeat. On this ASS car the arrows **pause** too (§8.3). | Allowed only when autopark_state ∈ {ready, forward_ready} (up) or {ready, reverse_ready} (down), and activeCommandType == null, i.e. after the abort's cmd_result. |

Evidence: fn#124428/#124429 (FindMe press-in/out); fn#124437/#124438 and fn#124440/#124441 (arrows); fn#125354 at +0x7b..0x12b (release), +0x2d8..0x423 (find_me / pin_drop), +0x451..0x559 (fwd/rev); fn#125521 (press capture); fn#125523 +0x9d (600 ms).

**Quirks to keep. All are harmless on this car.**
- **Quick tap.** If the finger lifts before the press's cmd_result, the abort is sent. The late success still moves the machine to summonInProgress and **starts** the heartbeat. The abort's cmd_result then returns to ready **without stopping** the heartbeat, so beats can leak into Ready [INFERRED]. Evidence: WFCR has no COMMAND_EXECUTED handler, fn#125161 +0x854..0xa43; summonInProgress COMMAND_SUCCESSFUL else-branch, +0xb7d..0xbab.
  - On the car, a tap released before Primed is a terminal abort, and heartbeats in Idle do nothing.
- **Presses made in WFCR still send their message**, but the COMMAND_EXECUTED event is dropped.
- **Back or background sends cmd_abort twice**: once on focus loss, once on termination [INFERRED].
- **The critical and RTT paths do not clear the 100 ms send interval.** Those beats go nowhere because the session is closed.
- **On a non-deadman car**, executeSummonCommand returns COMMAND_EXECUTED{ABORT} even when it sends nothing. N/A here.

### 2.3 Enable rules and the look (copy)

**Hold button** (isSmartSummonButtonEnabled, fn#124581). Enabled only when all of these hold:
- the machine is in {ready, WFCR, summonInProgress};
- smart_summon_state ∉ {unavailable, unsupported, complete} (null passes);
- no arrow is pressed;
- autopark_state !== 'standby'.

The trailing deadman branch only logs. Rendering:
- status = (enabled && !findMePressed) ? NONE : **DISABLED**, so it is DISABLED while held (fn#124426).
- The button has press handlers **only if** isSmartSummonEnabled (supportsSmartSummon===true, fn#124594). airgapp sets supportsSmartSummon and supportsActuallySmartSummon to true for this car (DAS_lastSeenSummonVersionIdentifier 'ActuallySmartSummon', ON-CAR-STATE.md:97-101).
- With supportsActuallySmartSummon true, the 'Warning … does not currently support Actually Smart Summon' alert never shows, and checkSummonDistance is skipped (fn#125318 +0x62).

**Arrows** (isForward/ReverseButtonEnabled fn#124579/#124580; validClassicSummonDirections fn#125264). Enabled when all of these hold:
- the machine is in {ready, WFCR, summonInProgress};
- the other arrow is not pressed;
- the direction is allowed by autopark_state: ready → both, forward_ready → up, reverse_ready → down, anything else → none;
- and (this arrow is pressed, or !isDeadman, or activeCommandType==null).

**Never shown on this car:**
- the Stop button, which appears only when !isDeadman (ui_a.dis:2301-2314);
- the proximity-sensor abort, which is installed only when !isDeadman (fn#124460).

**The mode toggle has no disabled prop.** It stays live during a hold (fn#124422).

**Look** (tesla-ux recon; DARK theme):

Hold button:
- full band width (screen − 40), height 55, radius 5, border 2;
- label BodyLabel 14/20, Medium;
- idle and enabled: transparent, border rgba(255,255,255,0.1), label #F1F1F1;
- disabled: label rgba(255,255,255,0.1);
- held: FILLED+DISABLED, #1E2E5B with label rgba(255,255,255,0.1) [code-derived; device look UNVERIFIED];
- activeOpacity 1, no press animation, no haptic;
- label 'Come to Me' (enA:6476) or 'Go to Target' (6491).

Arrows:
- two flex-1 buttons, 55 pt tall, 15 pt gap;
- icons arrow_up / arrow_down at 36 pt: #F1F1F1 when enabled, #969696 when disabled;
- a held arrow is FILLED #3368FF.

Implementation note [INFERRED]: Tesla's TouchableOpacity keeps the responder when `disabled` flips mid-press, so onPressOut still fires. airgapp must restyle only and must never cancel the touch.

### 2.4 Heartbeat (copy)

**Starting.** The heartbeat starts on:
- WFCR plus COMMAND_SUCCESSFUL (CommandNotStop), which moves to summonInProgress and runs StartSummonHeartBeat (fn#125161 +0x870..0x8bd); or
- RECEIVED_IN_PROGRESS_STATUS while executing (+0xe54..0xea1).

Each start restarts three timers (fn#125441, fn#125414; defaults fn#124567-#124569):
- the 100 ms send interval (the first beat goes 100 ms after start);
- the warning timer, **2000 ms**;
- the critical timer, **10000 ms**.

**Car echo.** For autopark:heartbeat_car_webrtc on UDP, the phone sends RECEIVED_HEART_BEAT with timestamp = car_timestamp. It counts as fresh when car_timestamp ≥ the last one. When fresh (fn#125471, fn#125356, fn#125442):
- if heartbeatActive, the warning and critical timers restart;
- otherwise they stop;
- the text 'Reducing speed due to poor connectivity' is cleared;
- activateKeepAwake runs.

summonInProgress entry and exit also activate and deactivate keep-awake.

**Warning** at 2000 ms of car-heartbeat silence:
- HEARTBEAT_WARNING sets classicSummonStatusText = 'Reducing speed due to poor connectivity' (enA:406);
- **one** extra beat is sent;
- nothing is aborted (fn#125418, fn#125357).

**Critical** at 10000 ms: DISCONNECTED{'Heartbeat Critical Timeout'}, then disconnect, then reconnect after 500 ms, with **no cmd_abort** (fn#125420-#125422).

**Stopping.** stopSummonHeartbeat runs on:
- release;
- SendAbort;
- SendAbortIfInActiveSummonState;
- showErrorPopup;
- summonInProgress STATUS_ELIGIBLE;
- notReady entry.

**Tuning.** Only a cloud hello can change 100/2000/10000 (fn#125483). The airgapp phone keeps the defaults.

[AIRGAPP-EQUIV] Because the helper forwards heartbeat_car_webrtc, this logic is copied verbatim; no adaptation is needed.

### 2.5 Results, errors, timers

**autopark:cmd_result** (fn#125479; fn#125273):
- If result===true **or** reason==='already_set', the phone sends COMMAND_SUCCESSFUL{commandType from cmd_type}.
- Otherwise it sends COMMAND_FAILED{SummonErrorTypeForErrorString(reason)}.

In **WFCR** (stale results are dropped):
- a non-stop success moves to summonInProgress and starts the heartbeat;
- a stop success moves to ready and clears the active command;
- a failure moves to ready and runs setError, showErrorPopup (non-stop only), checkSummonEligibility and clear.

In **summonInProgress** there is no stale guard:
- a non-stop success only sets the timestamp;
- a stop success moves to ready **without** stopping the heartbeat.

**autopark:error** (fn#125480):
- null is ignored and DUPLICATE_COMMAND is swallowed;
- in WFCR: setError + eligibility;
- in summonInProgress: move to WFCR and run showErrorPopup;
- in Ready or NotReady: ignored.

The car never sends autopark:error (§8.6).

**showErrorPopup** (D/sm.dis:7419-7525) runs sendAbort + stopSummonHeartbeat, then shows Alert 'Summon Failed' (341) / ErrorAlertString / 'Close' (325).

**Timers that do NOT exist.** Do not add them:
- a command-ack timer (§0);
- an initial-status timer;
- a command re-send;
- a device_location interval.

[AIRGAPP requirement] Because the ack timer is dead, **every** phone command must get a cmd_result: the car's own, forwarded, or the helper's refusal (§3.4). Otherwise the faithful app waits in WFCR forever. The helper's own cmd_result must use a timestamp ≥ the last cmd_result timestamp, taken from the car epoch clock.

### 2.6 D2: helper deadman (owner rule, exact)

**verbActive** is set when the helper forwards cmd_forward, cmd_reverse or cmd_find_me. It is cleared when any of these happens:
- the helper forwards or injects cmd_abort;
- QtCarServer answers that verb with result:false and reason ≠ 'already_set';
- the session ends.

**Liveness instants** are:
- the moment the verb is forwarded;
- the moment that verb's cmd_result is forwarded to the phone;
- every authenticated heartbeat_app_webrtc.

**Trip.** If verbActive and now − (latest instant) > **300 ms**, the helper injects `{msg_type:'autopark:cmd_abort'}`. These are the same bytes as Tesla's release.
- The car then pauses: UserPaused if it was in Primed..Active, otherwise a terminal abort.
- The helper swallows that abort's cmd_result and **keeps the session**.
- The phone learns nothing. If the finger is still down, the UI keeps 'Release to stop'. Release followed by a re-press resumes.

**Budget.** Tesla's first beat leaves the phone 100 ms after it receives cmd_result. So the deadman holds only if phone-link RTT + 100 ms < 300 ms, i.e. **RTT < 200 ms**. After that, it tolerates two lost or late beats in a row (§9).

[AIRGAPP implementation] Generate the 100 ms beats from a timer that cannot stall, i.e. a native one. A JS stall longer than about 200 ms would trip D2.

**Transport loss.** If no authenticated frame arrives within the session lease, run H4: abort, then the disconnect map, which is a terminal RemoteAbort. This is the equivalent of stock ICE loss.

Tesla itself has no 300 ms rule. Its app warns at 2000 ms and disconnects at 10000 ms; the car's stop timer is 10000 ms.

## 3. Come to Me follow and Go to Target pin

### 3.1 device_location (copy)

**Content** (fn#125482):
- lat/lon = the fix, or 91/181 if there is none;
- accuracy, altitude and altitude_accuracy = the fix values, or −1 (fn#66922);
- goal: in FIND_ME mode, the fix passed in; in GO_TO_TARGET, the pin, or the fix if there is no pin;
- summon_type: 'find_me' or 'pin_drop';
- a GCJ overlay only for coordinateType GCJ (China; N/A here);
- sent only if the channel is open; otherwise dropped (the cloud-socket fallback is N/A).

**Triggers.** Nothing else sends it. It is not tied to the button and continues between holds.
- (a) **Every changed phone fix, in every machine state.** The newest fix in each batch becomes DEVICE_LOCATION_UPDATE. The guard DeviceMoved passes on the first fix, a null fix, or a strict change of lat/lon/heading. There is **no** distance, accuracy or age filter (fn#124456, fn#125291, fn#125246, fn#125317).
- (b) The stored lastKnownDeviceLocation. It is dropped if older than 60000 ms (fn#125412 +0x2f) and is normally a duplicate.
- (c) Each mode toggle, immediately (fn#125329).
- (d) Each SET_SMART_SUMMON_TARGET: a GTT pan ending, the press capture, GTT mode entry, the 600 ms unlock, and null at exit (fn#125324).
- (e) A status with smart 'unavailable' and 'no_phone_location' while a fix exists (fn#125352).

If a target or mode change comes before any fix, the sentinels are sent [INFERRED].

### 3.2 Come to Me (copy)

- **The goal is the phone, and it follows the phone.** Every new fix re-sends the goal, at about 1 Hz (§5.2). Tesla sends cmd_find_me only once per press.
- **There is no phone-side leash or 300 m abort on ASS cars**: checkSummonDistance returns early.
- **Camera framing:**
  - it fits phone and car on entry;
  - it refits when either one leaves the view;
  - it refits 3 s after the user pans away (THREE_SECONDS, map.dis:2415/2420, :2829);
  - while held, it fits car, goal, phone and path (map.dis:3675/3702).

### 3.3 Go to Target (copy)

**Crosshair.** The crosshair is fixed and the map pans under it. There is no long-press and no tap-to-drop (map.dis:642-686; grep finds no onLongPress).
- pinMarker box 40×40 with IconName.target at 28 pt;
- white on the hybrid map, #F3F3F3 on the standard map;
- left = SCREEN_WIDTH/2 − 20;
- top = insets.top + (mapHeight − insets.top)/2 − 20.

Tesla reads the region centre, so the drawn crosshair may sit insets.top/2 (about 30 pt) below the real target [INFERRED; §9].

**Setting the target.** Each region-change-complete calls SET_SMART_SUMMON_TARGET with the projected centre. That sends a **device_location('pin_drop') before any press**.
- It happens only while the GoToTarget hook is active: no button pressed and allowSettingTarget is true.
- Android debounces this by 250 ms (map.dis:4038-4113); iOS calls it directly.

**Clamp.** If distance(car, centre) > radius, the point moves to **radius − 1 m** on the same bearing from the car (map.dis:3198-3255).
- radius = leash ?? 65. After §4.5 this gives a **64 m** cap on this car.
- While a drag is outside the circle, map touches are disabled. When the gesture completes, the map animates back ('move back to circle').
- The clamp re-runs whenever the car moves or the radius changes.

**Entering GTT:**
- if a target already exists, the map moves to it;
- coming from the initial or not-ready hook with no target, the target becomes the phone position projected into the circle;
- otherwise the target becomes the projected map centre.

**Press.** The target is re-captured. The crosshair is replaced by a pin Marker {identifier 'pin', anchor (0.5,0.5), zIndex 100, opacity 1, tracksViewChanges false} (map.dis:1095-1147).

**Release.** The map moves to the pin. **600 ms** later allowSettingTarget returns and the crosshair comes back. In Come to Me, or with no pin, it returns immediately (fn#125523).

**Pin editing** needs all of these:
- the machine is in the connected group;
- smart ∉ {unsupported, complete};
- no arrow is pressed;
- the car location is known;
- radius > 0;
- and, for the pin to be available, !findMePressed.

The target survives mode switches and is cleared at exit.

**Mode toggle** (ui_a.dis:1128-1395):
- a vertical ToggleSelector: account (Come to Me) on top, target (GTT) below;
- icons at 24 pt, rotated 270°;
- active pill #0f52ba, wrapper border #212121, radius 5, height 40, 100 ms slide;
- onChange: mediumHaptic + SET_USE_FIND_ME, then a device_location immediately, and the mode is persisted (default FIND_ME);
- **it works during a hold**, so summon_type can flip mid-run.

### 3.4 D3: 3 m goal floor (owner rule)

**On the phone:**
- In Come to Me, the hold button is disabled, with no text, while d(phone fix, car) < 3 m.
- In GTT, a projected target closer than 3 m to the car is pushed out to 3 m on the same bearing. This mirrors Tesla's outer clamp; Tesla has no minimum.

**In the helper,** for each **cmd_find_me**:
- goal = (goal_latitude, goal_longitude) if both are present, else (latitude, longitude). This is the car's own rule (Q:1158757-1158916).
- car = the latest valid vehicle_data:location the helper relayed.
- If the haversine distance (R = 6371 km) is under 3.0 m, do not forward. Reply cmd_result `{cmd_type:'autopark:cmd_find_me', result:false, reason:'location_params_invalid', timestamp}`. The app shows 'Summon Failed' / 'Invalid location' (enA:352) / 'Close' and sends its own cmd_abort.
- If the car location is unknown, reply with reason 'invalid_vehicle_location'. The app then shows 'Something went wrong', the same as the car's own refusal.

**Scope [INFERRED].** The floor applies to cmd_find_me at press and re-press only. That is where the rule came from (the gateway's verb-time CheckGoal) and where the start-time self-abort lives. Mid-run device_location updates and the arrows follow Tesla.

### 3.5 Firmware goal handling (what retargeting does)

**For every device_location** (Q:1159236-1159403, FUN_00a636e0 Q:1158730-1159229):
- DAS_lastKnownPhoneGpsLocation and DAS_lastKnownPhoneGpsAccuracy are written.
- The goal comes from goal_* only if **both** keys are present; otherwise it is the phone position.
- The orientation comes from goal_final_orientation only when both goal keys are present; otherwise it is 0.0. Tesla never sends it.
- summon_type is matched case-insensitively. Containing 'find_me' gives FindMe(1); any other string gives PinDrop(0). Absent, it is FindMe if goal == phone, else PinDrop.
- number_steps ≥ 0 is stored. Tesla never sends it.
- There is **no** range, leash, accuracy, rate or state check, no timer kick and no reply. A message without lat/lon writes (0,0).

**Inside every verb**, the same parse runs **before** the gates (Q:1163884). So:
- a refused verb still rewrites the goal and the type;
- cmd_forward and cmd_reverse set goal = phone and type FindMe.

**Delivery to the APE** [INFERRED path] (V:642383-642410, 645491-645571):
- SummonGoalMessage 0x3dc: 8 B, every 1000 ms, and on change;
- the phone GPS through PhoneLocationStatusMessage;
- in DASDevControlMessage: GUI_selfParkRequest, the FindMe bit, and a 2-bit heartbeat = (CAPI_remoteHeartbeat + GUI_factorySummonHeartbeat) & 3.

The MCU-side RemoteRequest data publication has a 10 ms trailing debounce and a 1000 ms period (Q:935265-935279).

Whether the APE re-plans toward a moving goal is off-dump [UNVERIFIED].

## 4. Status → UI

### 4.1 Priority (getSummonStatusText fn#124616; D/sm.dis:10598-10676, re-checked)

The status line shows the first non-empty of:
1. **smartSummonCompletedText**: 'Summon complete' (6477) or the abort-reason text. It is kept only while executing.
2. **classicSummonStatusText** if non-empty. Otherwise, if autopark_state==='standby', generateErrorStatusFromContext (fn#124613).
3. **smartSummonWaitReason**. It is set only while a button is held and smart ∈ {paused, active, preactive}.
4. If isActive && (isExecutingForward || isExecutingReverse): the classic usage text, which is 'Release to stop' (299) on dead_man.
5. If !findMeButtonShown: ''.
6. **getSmartSummonStatusText**:
   - 'Release to stop' while held. In 'available' this also needs findMeButtonShown && isDeadman; in preactive/active/paused any press counts.
   - complete gives 'Summon complete'.
   - unavailable gives the unavailable text, the standby text, or '' during a classic move.
   - initializing, abort and unsupported give ''.
7. If still '', or equal to 'Ready to summon' (292, only ever compared): the idle hint, **only after the caution sequence**. On deadman cars it is 'Press and hold controls to move vehicle.' (6474) (ui_a.dis:1839-1880).

isNothing treats null, undefined, whitespace-only strings, 0 and [] as empty.

### 4.2 Texts (enA.tsv line numbers)

**classicSummonStatusText by state:**
- connecting: 'Connecting' (298);
- a repeated CONNECTION_SUCCESSFUL: 'Communicating' (297);
- ready: '';
- summonInProgress (D/sm.dis:7034-7110, re-checked): 'Preparing' (377) for autopark 'preparing'; 'Issuing HomeLink command' (4337) for 'opening_gate'; otherwise '';
- heartbeat warning: 'Reducing speed due to poor connectivity' (406).

**notReady / notConnected** use getStatusFromError (fn#125284, re-checked):
- For E = STATUS_STANDBY:
  - StandbyMessageString[reason] if the reason is mapped;
  - else, if the reason is empty and smart is 'unavailable', the unavailable text;
  - else PARKER ? 'Close doors and complete charging' (388) : **'Summon is not ready'** (364). summonMode is never set in 4.58, so this is always 'Summon is not ready'.
- For any other E: ErrorStatusStrings[E], else 'Cannot connect to vehicle' (317).

**Standby reasons:**
- connecting_to_bluetooth_device, waking_up_buses, detect_key_fob, turn_on_drive_rail, waiting_for_command and primed → 'Summon is getting ready' (360);
- plugged_in → 'Unplug vehicle' (389);
- door_open → 'Close doors' (385);
- keyfob_not_present → 'Move key fob closer' (320);
- not_started_timeout → 'Summon cannot be started at this time' (327);
- panic → 'Take control immediately' (374);
- disabled → 'Enable summon by tapping Self-Driving > Summon on your vehicle's touchscreen' (324);
- crash_state and unknown_error → 'Summon is not ready'.

**Error statuses:**
- STATUS_ABORTING → 'Stopping' (391);
- STATUS_UNAVAILABLE → 'Summon is unavailable' (366);
- DEVICE_LOCATION_UNKNOWN → 'Waiting for improved phone location accuracy' (322);
- TOO_FAR_FROM_VEHICLE → 'Move closer' (365);
- VEHICLE_LOCATION_UNKNOWN → 'Cannot find vehicle' (357);
- LOCATION_SERVICES_DENIED → 'Enable Location Services' (362);
- STATUS_IN_PROGRESS → 'Command in progress' (318);
- CONNECTION_CLOSED → 'Connection lost' (359);
- NOT_CONNECTED → 'Cannot connect to vehicle' (397);
- STATUS_PANIC → no text, so the line under the overlay reads 'Cannot connect to vehicle' [INFERRED quirk].

The full wire → text tables are in D/reasonmaps.txt and joined_maps.txt (wait, unavailable, abort and standby reasons).

### 4.3 Alerts (copy)

**Alert texts.** Alerts are 'Summon Failed' + ErrorAlertString + 'Close'. An unknown error gives 'Something went wrong' (347). Car refusal reasons map as follows:
- unavailable → 'Summon is unavailable' (346);
- mobile_autopark_disabled → (324);
- plugged_in → 'Disconnect charge cable' (333);
- door_open → 'Close all doors and trunks' (315);
- brake_pedal_pressed → 'Take foot off brake' (306);
- too_far_from_vehicle → 'Move closer';
- smart_summon_not_supported → 'Smart Summon not available' (383);
- location_accuracy_too_low → 'Phone location accuracy too low' (368);
- location_params_invalid → 'Invalid location' (352);
- already_set → treated as success;
- dog_mode, autosteer_not_enabled, invalid_vehicle_location and action_invalid → 'Something went wrong'.

**Connection alert** (fn#124466). If the secure connection fails: 'Summon Failed' / 'Failed to establish secure connection for Summon. Please make sure your Phone Key is paired and try again later.' (350) / Close, which calls goBack. [AIRGAPP-EQUIV] Show it when the helper refuses authentication.

### 4.4 Spinner, haptics, panic (copy)

**Spinner.** A 15-pt BusyIcon (right margin 5) shows only when the raw text !== '' **and** a busy flag is set.
- connecting sets both busy flags;
- ready clears them;
- notReady sets busy = (E==STATUS_ABORTING);
- smart 'initializing' sets smart busy; 'unavailable' clears it.

Evidence: ui_a.dis:1891-1920; D/sm.dis:7780-7800.

**Haptics** (debounce 500 ms):
- none on press or release;
- mediumHaptic on a mode toggle;
- a debounced medium haptic on each new wait reason ≠ none, or on a transition to 'aborting' while executing;
- mediumHaptic('not connected') when entering notConnected while executing;
- for panic, Vibration 1000 ms every 1750 ms (fn#124468 +0x50).

**Panic overlay** (autopark 'panic' only):
- card at top SCREEN_HEIGHT/3, width SCREEN_WIDTH−32, padding 10, radius 10, background #222324;
- alert icon at 28 pt + 'Take control immediately'.

### 4.5 D1: minimal adaptations for the always-"standby" car

**What this car sends.** This car always reports autopark_state 'standby' and smart_summon_state 'initializing'. This is the ASS short-circuit (Q:1168311-1168328). GUI_smartSummonState read Unknown in all 356,197 watcher samples. Only autopark_state_reason changes, and leash_proximity is 0.0.

**What a faithful copy would do here:**
- sit in notReady forever with every control disabled;
- abort any run on its first status;
- never draw the leash circle or allow a pin;
- never show the panic overlay.

**The fix.** A phone-side normalizer N sits in front of Tesla's **unchanged** onSummonVehicleStatus → RECEIVED_VEHICLE_STATUS:

```
running = isExecutingSummon(ctx) && machine in connected.summonInProgress   // i.e. a verb was acked and the finger is down
N(raw):
 a) if leash_proximity_meters <= 0: leash_proximity_meters = null       // Tesla's '?? 65' -> 65 m circle, 64 m clamp
 b) if raw.autopark_state=='standby' && raw.smart_summon_state=='initializing' && reason in PREP
       PREP = {'', connecting_to_bluetooth_device, waking_up_buses, detect_key_fob, turn_on_drive_rail, waiting_for_command, primed}
    then smart='available'; autopark = !running ? 'ready'
                                     : reason!='' ? 'preparing'
                                     : (held button==REVERSE ? 'active_reverse' : 'active_forward')     (reason kept raw)
 c) if raw standby && reason=='panic': autopark='panic'                   // DIPanic arrives as standby+'panic'
 d) everything else passes through unchanged (Tesla's own texts, notReady, abort-if-active)
Re-run N on every raw autopark:status, and once more on the last raw status (same timestamp; isFreshMessage uses >=, fn#125239) when `running` goes true->false.
```

**Basis for (b).** This is what the same firmware reports when the short-circuit does not apply. SummonState::current calls autoparkStateString(**false**) (ELF 0x959455 `xorl %edx,%edx`, re-checked). The mapping (Q:1168275-1168436, re-checked):
- Idle 2, PostActiveSuccess 0x2d and UserPaused 0x34 give 'ready' when not inhibited;
- prep states 0x1f-0x23 give 'preparing', or **'ready' under StandbyMode activation 4** (connect pre-prime, §1.2);
- 0x25-0x27 with shift R/D give 'active_reverse' / 'active_forward', otherwise 'preparing';
- 0x28-0x2a give 'opening_gate';
- 0x2b, 0x2e and 0x33 give 'aborting';
- 0x2f gives 'panic';
- 0x31 gives 'paused';
- 0x32 gives 'resuming';
- anything else gives 'standby'.

**Why `running` and the re-run matter.**
- '' must never become 'ready' while running. summonInProgress would take STATUS_ELIGIBLE → ready + stopSummonHeartbeat, stopping the heartbeat, and D2 would trip within 300 ms.
- Statuses that arrive in WFCR are only stored, so marking them not-running is harmless. If the verb is refused, COMMAND_FAILED's eligibility check then sees 'ready' and does not restart heartbeats.
- Without the re-run at press end, Ready's entry check would find a stored 'preparing' or 'active_*' with exec=false. That gives VEHICLE_STATUS_INELIGIBLE{STATUS_IN_PROGRESS}, which sends an extra cmd_abort and keeps the arrows disabled.

**Not adapted. Copy as-is:**
- heartbeat warning and critical (the helper forwards the echoes);
- the command timer (Tesla has none);
- vehicle_data:location, forwarded verbatim with shift, speed and blinker;
- wait, abort and complete texts, which simply never appear;
- the panic quirk text.

No 'Summon complete', 'Stopping' or wait text is synthesized.

### 4.6 What the copy shows on this car

- **Opening the screen.** 'Connecting' with the spinner. The car pushes standby/initializing, with reason '' or the StandbyMode prep reasons. N turns this into ready/available, so the screen is Ready with the hold button and both arrows enabled. After the caution sequence (first visit per app launch) the idle hint shows.
- **Press.**
  - If the car is already primed, no status arrives and the screen shows 'Release to stop'.
  - From Idle, the RemoteAutopark prep reasons arrive. Running, they become 'Preparing'; when the reason returns to '' they become active_*, which shows 'Release to stop'.
  - The header shows 'D · <speed> KM/H · <dist>'.
- **Release.** cmd_abort, then the abort's cmd_result, then Ready. N re-runs and gives 'ready', so the idle hint returns. A stock car would show 'Stopping' while RemotePreAbort lasts (25 ms); this car sends no status, so that is not reproduced.
- **Car finishes or aborts on its own.** This car sends no status, so 'Release to stop' stays while the button is held. The header shows 'Parked' once shift P arrives. This is a residual difference [§9].
- **An inhibitor appears.** Tesla's text shows (e.g. 'Close doors'), the controls are disabled, and cmd_abort is sent if active.
- **The prep times out.** 'Summon cannot be started at this time' with controls disabled for about 1000 ms, then ready again.

## 5. Location permission and accuracy

### 5.1 Entry gates (copy the Home-row path; airgapp's entry is the Home 'Summon' row)

**Home-row path.**
1. requestLocationPermission:
   - title 'Enable Location Services' (321);
   - message and rationale 'To use Summon, please allow Location Permissions' (6568);
   - sleepSeconds 0, onlyIfAsked false;
   - on failure: Alert with title and message, buttons 'OK' (160, no-op) and 'Open Settings' (5272, Linking.openSettings), cancelable:false.
   - Evidence: S/entry_home.dis:1-83, S/reqloc.dis:160-300.
2. Then requestPreciseLocation('SummonPreciseLocationPurposeKey'):
   - iOS needs the result 'full'. If the user declines, **nothing happens**: no alert, no navigation.
   - The iOS<14 error counts as success; Android always returns true (S/reqprecise.dis:28-56).
   - Purpose text: 'Summon uses your location to determine your proximity to your vehicle' (4664).
   - [AIRGAPP build requirement] This needs Info.plist NSLocationTemporaryUsageDescriptionDictionary.SummonPreciseLocationPurposeKey, which means a native build.

**Controls-tile path** (only if added): foreground permission only. Refusal shows 'Permission Required' (5214) / (6568) / Cancel (158) + Settings (7450), cancelable false.

### 5.2 On screen

**Mount:** configure LocationUpdateConfigActive (fn#125240 +0x1ed):
- distanceFilter 0;
- desiredAccuracy: iOS 'bestForNavigation', Android 'highAccuracy';
- interval / fastestInterval / maxWaitTime 1000;
- activityType 'other', headingFilter 1, headingOrientation 'portrait';
- pausesLocationUpdatesAutomatically false;
- allowsBackgroundLocationUpdates false, showsBackgroundLocationIndicator false.

**Unmount:** Standby config {distanceFilter 0, interval/fastestInterval/maxWaitTime 1000}. Both configs are applied in the same mount flush; that Active wins is [INFERRED].

**Fixes.** Subscribe. If there is no fix yet, call getLatestLocation({timeout:**5000**}) (fn#124455 +0x83).

### 5.3 Accuracy and age (copy; owner: no added gate, and the gateway's 30 m cap is dropped)

**The phone adds no accuracy gate.** SUMMON_MIN_LOCATION_ACCURACY = 5 is declared but never read, on Android and iOS. Accuracy is sent raw, or −1. Screen fixes have no age limit; stored fixes older than 60000 ms are not forwarded.

**The rules that do apply:**
- No fix → DEVICE_LOCATION_UNKNOWN → notReady 'Waiting for improved phone location accuracy' with controls disabled. A press with no fix is a silent NO_OPS.
- Phone more than **300 m** from the car → 'Move closer' with controls disabled (fn#125254 +0x77).
- Car side: find_me with accuracy > **200 m** → location_accuracy_too_low; phone > **300 m** → too_far_from_vehicle (Q:928867-928871, 932571-932573).
- Losing location permission mid-screen changes no state: DEVICE_LOCATION_PERMISSION is never dispatched in 4.58.
- EU BLE check (requireEuBLECheck, a cloud setting): Bluetooth denied → 'Enable Bluetooth' (304); disabled → 'Allow Bluetooth Access' (305). Tesla has these two swapped. The value for this car is [UNVERIFIED, §9].

## 6. Screen extras (tesla-ux recon values; colours are Tesla's own "look A")

**Layout.** No title and no sheet.
- The map runs full-bleed down to a solid #222222 band (a LinearGradient of [primaryBackgroundLine ×2]).
- Band padding: horizontal 20, bottom 40 on notched phones, 20 otherwise.
- Band contents, top to bottom:
  - status row: minHeight 40, margins 10/10;
  - hold button: 55 pt, marginBottom 15;
  - arrow row: 2 × 55 pt with a 15 pt gap.
- The caution text is absolute inside the band (bottom 60, padding 20/10).
- Until the car location is valid, a 30-pt BusyIcon replaces the map.

**Map.**
- Apple on iOS, Google on Android. mapType HYBRID by default; the globe toggles STANDARD, and the choice is persisted.
- pitch, rotate, compass and indoors are off; showsUserLocation shows the native blue dot.
- minCenterCoordinateDistance 250; Android zoom 8–18; initialRegion = car ± 0.002°.
- fit edgePadding {top 100, left 50, right 50, bottom 50 + strip height}; map debounce 100 ms.
- **Car marker:**
  - images vehicle_mapmarker_online.png (red arrow) and _stale.png (grey), 282 px source shown at 62×62 pt;
  - anchor at the centre; rotateZ(heading.toFixed(2)+'deg');
  - zIndex 1, no animation.
- **Leash circle:**
  - centred on the car; radius = leash (normalized to 65);
  - fill rgba(62, 106, 225, 0.1), stroke rgba(53, 103, 233, 0.7), width 2;
  - hidden while a classic move executes (map.dis:1031-1036, 1147-1177).
- **Path line** (smart_summon_viz):
  - width 5, colour rgba(62,106,225,α) with α = clamp(dist-to-end / max(min(total/2, 35 m), 1), 0, 1);
  - probably never drawn on this car [INFERRED].

**Header** (top = statusBarHeight + 8, height 40, horizontal padding 20).
- Back button: 40×40, IconName.back at 36 pt, white; calls goBack.
- While executing, a summon badge: 20 pt, white at 0.7 opacity, on #0f52ba, radius 5, padding 2.
- DriveStatusText = [gear, speed, phone↔car distance] joined by ' · '. The separator is SpecialCharacters.dotSeparator = '·' (H L2817311, re-checked).
  - Gear: 'Parked' (376) unless R/N/D; a missing shift counts as P.
  - Speed: hidden in P. Shown through convertMilesToGuiDistanceUnitString as '{{speed}} KM/H' or '{{speed}} MPH'. The car's speed is in mph [INFERRED from values of 1.000 and multiples of 0.621].
- Poor-connectivity icon: shown when RTT ≥ **500 ms**, sampled every 2000 ms (rtt.dis:178-179). [AIRGAPP-EQUIV] Use the phone↔helper link RTT.
- **Blinkers:** two 28-pt icons on #00d474 tiles, the left one rotated 180°. They flash every **650 ms**, only while **find-me** is held and blinker_state ∈ {left, right, both}.

**Right column** (right 20, top statusBarHeight + 8, width 40):
- the mode toggle (40×80, marginBottom 50), only when Smart Summon is supported;
- the globe;
- the sentry button 40×40 (sentry_on/off), only when a frame has arrived **and** the car is in Park.

**Camera strip.**
- Tesla starts one live stream automatically on the first connect (webcam:signal ctx 'summon'). There is no camera picker, and summonSentryCamEnabled is hard-coded true. [AIRGAPP-EQUIV] Use airgapp's camera capability; which camera to show is in §9.
- Visibility: shown when there is a stream, it is connected and a first frame has arrived, unless (loading && P) or (user hid it && P). **Outside P it is always visible.**
- Animation and size:
  - fade 200 ms;
  - width 0.95×SCREEN_WIDTH, height 0.95×SCREEN_WIDTH×3/4×0.75, radius 10;
  - bottom = band + 5 on iOS, + 10 on Android;
  - a 24-pt spinner while loading.
- Stats every 1 s: no new frame for 1 s shows the spinner; 5 s hides the strip; RTT ≥ 1000 ms shows the spinner. A failed or disconnected stream retries immediately (negotiator.dis:471-517, 857-1160).
- The sentry button toggles a persisted hide preference (default false). The stream keeps running.

**First-use caution.** While shownSummonWarning is false, the sequence runs after 10 ms:
1. controls go to opacity 0 (0 ms);
2. the caution text fades in over 400 ms;
3. it holds for 3000 ms;
4. it fades out over 400 ms;
5. the controls fade back in over 400 ms.

Details:
- SET_SUMMON_WARNING_SHOWN is dispatched at the start and …_ANIMATION_COMPLETE at the end. The idle hint appears only after complete.
- There is no acknowledgement button.
- Text: 'Warning: Monitor vehicle's surroundings for people, animals, or unpredictable movement.' (6475).
- **Once per app launch**, because the flags are blacklisted from persistence.
- Quirk: leaving mid-animation leaves complete false, so there is no idle hint until the next launch [INFERRED].

## 7. Entry and exit (copy)

**Entry.** conditionallyNavigateToSummon (S/entry_home.dis:146-190):
- If the phone key is not paired or not authorized: Alert 'Pair phone key' (569) / 'Please set up Phone Key in order to use Summon' (373), default OK.
- Otherwise navigate **immediately** to vehicle_summon_screen_v2, with no confirmation or disclaimer.
- [AIRGAPP-EQUIV] Use the flag for "phone not provisioned for the helper".

**Exit (unmount, e.g. the back button).**
1. TERMINATION_REQUESTED{exit_summon_screen}: cmd_abort + stop heartbeat **only** in WFCR or summonInProgress.
2. SET_SMART_SUMMON_TARGET null, which sends one more device_location.
3. Remove the listeners; unsubscribe location and apply the Standby config.
4. WebRtcContext.disconnect(), which waits if still connecting. The helper then runs H4.

The focus cleanup (SCREEN_OUT_OF_FOCUS) also fires; that it runs first is [INFERRED]. Evidence: fn#124451, fn#124471/#124472, fn#125308.

**Blur** (another screen is pushed). SCREEN_OUT_OF_FOCUS:
- WFCR → ready + sendAbort + reset + clear;
- summonInProgress → ready + sendAbort + reset;
- any other state → sendAbort + reset, only if a button is pressed.

The session stays open.

**AppState 'inactive' or 'background'** (e.g. the iOS Control Center or the app switcher):
- TERMINATION_REQUESTED{app_background}: abort if active, then goBack, which runs the unmount path.
- The provider also closes the connection.
- Returning to active only resets the RTT. There is no auto-reconnect, because prewarming is compiled off (fn#124449, fn#98485, fn#98456 +0x2e3).

**Not applicable here:** Android back, location-restricted drivers, vehicle switch, and the proximity sensor (dead_man).

## 8. Firmware facts the helper relies on (2026.8.6)

### 8.1 Connect and disconnect

See H1–H6 in §1.2.

### 8.2 Commands

**Verbs.** command() runs these steps (Q:1163695-1164170, 1172160-1172540, 1164208-1164527):
1. Requires latitude+longitude; otherwise 'location_params_invalid'.
2. Runs FUN_00a636e0.
3. Runs autoparkRequest, which can refuse with:
   - unavailable (RemoteAutoparkDisabled, !hasSelfPark, !GUI_selfParkAllowed);
   - mobile_autopark_disabled;
   - plugged_in / autosteer_not_enabled / dog_mode / door_open / brake_pedal_pressed (GUI_selfParkInhibited 3/10/9/7/8);
   - invalid_vehicle_location;
   - too_far_from_vehicle (> 300.0 m);
   - already_set (the same verb is still latched);
   - smart_summon_not_supported, or location_accuracy_too_low (> 200.0 m, find_me only);
   - action_invalid.
4. Sets CAPI_remoteAutopark = 1, 2 or 6.
5. Sends cmd_result {cmd_type = **the request's msg_type**, result, reason '', timestamp} on dcTcp. Come to Me and GTT both answer as 'autopark:cmd_find_me'.
6. On success sets RemoteSummon[0x18]=1.
7. Kicks the pause and stop timers **even when the verb was refused**.

**Latches.** Verb values 1/2/4/5/6 reset after 20000 ms. Abort (3) resets after 5000 ms, or on DI COMPLETE/ABORTED, or on PostActive* (Q:1168218-1168265; config Q:928857-928866, re-checked).

**cmd_abort** (Q:1161934-1162283):
- Refused with 'unavailable' if RemoteAutoparkDisabled or !hasSelfPark; 'already_set' if CAPI is already 3.
- Otherwise sets CAPI=3, sends cmd_result {cmd_type:'autopark:cmd_abort'}, and stops the pause and stop timers.
- It **never ends the session**.

### 8.3 Pause and resume window

**Process.** SelfparkManager runs in the QtCar UI process (libQtCarGUI). CAPI changes cross a process boundary to reach it; the latency is unmeasured [INFERRED].

**Release while connected.** On CAPI 3 with GUI_selfParkActive set (G:4937523-4937561, 4924150-4924185, 4922480-4922505, 4941859-4941890):
- If canPauseOnRemoteAbort holds, i.e. state ∈ {Primed 0x23, StartedForward 0x25, StartedReverse 0x26, Active 0x27, 0x33, 0x34} and (hasActuallySmartSummon || smart==4) and connected: **RemotePreAbort for 25 ms → UserPaused** (if still connected), else RemoteAbort.
- On this car the classic arrows follow this too.

**Not pausable:**
- the prep states 0x1f-0x22, gate states 0x28-0x2a, Pause 0x31, Resume 0x32, or any disconnected state;
- these go to RemoteAbort → PostActiveFailure → Idle, after 10000 ms or 2500 ms after the drive rail goes off;
- **a tap released before Primed is terminal**, and the next verb starts a new run.

**GUI_selfParkRequest sent to the APE:**
- 0x33/0x34/0x31/0x28 → Pause 5;
- 0x32/0x29/0x2a → Resume 6;
- 0x2b/0x2e → Abort 3;
- 0x25-0x27 → None 0.

**UserPaused (0x34).** A 250 ms tick runs only when deferSummonChecksToAP is false (version ActuallySmartSummon=1; vapi/force_defer_summon_checks_to_ap defaults to false). UserPaused ends as follows (G:4921378-4921396, 4941378-4941512, 4928305-4928346; Q:1153528-1153535, 1153621-1153628):
- RemoteAbort when elapsed > **35000 ms** (it lands at 35.0–35.3 s), when DAS_setSpeed is invalid, or when |DAS_setSpeed| > 0 after 1000 ms;
- DI COMPLETE → PostActiveSuccess;
- DI ABORTED, SNA or invalid → PostActiveFailure, **immediately if DI is invalid on entry**;
- DI UNAVAILABLE or STANDBY → PostActiveFailure after 250 ms;
- shift not R/N/D for 250 ms → Idle;
- disconnect → RemoteAbort.

It is not affected by:
- cmd_abort (a no-op);
- the stop and pause timers (skipped in 0x34);
- heartbeats, which **never** resume it.

**Resume.** Any accepted verb in 0x34 goes to Resume 0x32 with request Resume 6 (G:4946821-4947151, 4922726-4922763, 4942132-4942185):
- ASS branch: 6 → SMART_SUMMON, 1 → FORWARD, 2 → REVERSE, 5 → UNKNOWN; 0, 3 and 4 are ignored.
- First check after 1000 ms, then DAS_accState: APC_FORWARD 6 → StartedForward, APC_BACKWARD 5 → StartedReverse. **The APE picks the direction.**
- Timeout 10000 ms → RemoteAbort.
- Started* → Active after 1000 ms.
- An abort during Resume is terminal.

**On this car UserPaused sends no status.** A stock car would report 'ready' or 'standby' there, never 'paused', so Tesla's app sends no abort on it.

### 8.4 Retargeting

See §3.5. Retargeting rewrites the goal with every device_location and every verb, with no checks. The APE behaviour is off-dump.

### 8.5 Heartbeat and heartbeat loss

**heartbeat_app_webrtc** (also heartbeat_app) (Q:1162289-1162742, 1152400-1152456, 1164536-1164978):
- CAPI_remoteHeartbeat = (n+1) & 3. The member counter is not reset on reconnect; the DV is reset to 0.
- Echo heartbeat_car_webrtc {timestamp: echoed, car_timestamp: car epoch ms} on dcUdp (pref 5). heartbeat_app gets heartbeat_car instead, pref 2.
- Kicks the pause timer (disabled, 0) and the **stop timer (10000 ms)**.
- The Pause(0x31) resume branch is dormant.
- There is no freshness check (max_heartbeat_pingpong_ms 0 is unused).

**Stop timer.**
- Re-armed by every heartbeat and every verb. Stopped by abort(), and by autoparkStateChanged in Idle, PostActive* and DIPanic (Q:1163168-1163198).
- When it fires: unless in a gate state, unless in 0x34, and if inAbortableState (always true here outside PostActive*), it writes CAPI=3. That leads to UserPaused, then 35 s, then RemoteAbort: **about 45.3 s after the last frame**.
- Gate states 0x28-0x2a have no MCU stop.
- device_location never kicks it (Q:1153332-1153690).

**hello cannot tune anything.** newAutoparkConfiguration reaches only a no-op slot, ignore_server_config defaults to 1, and the timeouts are copied once in the constructor. The effective values are pause 0 and stop 10000 ms (Q:1146525, 1151959-1152026, 1158669-1158691; Q:928744-928756, re-checked).

**APE-side reasons** are produced by the APE and their thresholds are off-dump: waiting_for_heartbeat, stopping_due_to_no_heartbeat, mobile_app_heartbeat_timeout, paused_for_user_timeout and too_many_pause_cycles. This car never reports them.

### 8.6 Motion signals on this car

- **autopark:status** is always standby/initializing. It is sent only when one of its 12 dvConnected fields changes, or on reconnect. Nothing is sent during motion, pause, resume or end: the reason is '' for 0x22-0x27, 0x2b, 0x2d, 0x2e and 0x31-0x34 (Q:1158704-1158716, 1163225-1163290, 1168623-1168836).
- **autopark:style** is 'dead_man'. It is sent on connect and when GUI_easySelfParkEnabled changes.
- **vehicle_data:location** is the only motion indicator. It is the same gear test the firmware uses for active_*.
  - Fields: lat, lon, heading, speed (0 if invalid; mph [INFERRED]), blinker_state off/left/right/both, shift_state Invalid/P/R/N/D/SNA, timestamp.
  - Sampled every 250 ms and sent only on change. Withheld while heading is invalid. The first frame after connect is always sent.
  - Evidence: Q:1165696-1166011, 1159416-1159584.
- **cmd_result** means "accepted", not "moving".
- **autopark:error** is never sent: the ASS last-error is ''.
- **smart_summon_viz / occupancy grid** are probably never sent [INFERRED].

### 8.7 Other inbound messages the car handles

(Q:1165250-1165490; 1165195-1165197.)
- control:keepalive → GUI_remoteAccPowerRequest=true.
- homelink:cmd_trigger.
- webrtc:* restarts the 60 s timer.
- autopark:set_webrtc writes only a flag that nothing reads.
- autopark:info: name → CAPI_summonDeviceName, public_key_hash → CAPI_summonDevicePublicKeySHA1, public_key → CAPI_summonDevicePublicKey, all published to the APE. A disconnect invalidates the name and SHA1. Binding does not gate motion (memory summon-security-model / productionization).
- Unknown autopark subtypes are logged.
- There is no cmd_pause or cmd_resume.

### 8.8 Defaults

QtCarServer (Q:928744-928872, re-checked):
- enable_webrtc 1, stub_ready_state 0, send_on_all 0;
- location_sample_ms 250;
- simulate_heartbeat_loss 0.

libQtCarGUI (G:3018786-3018882, re-checked):
- pause_timeout 10000, resume_timeout 10000, resume_first_check 1000;
- transition 1000, pre_abort 25, user_paused 35000;
- post_active 10000 and 2500;
- pre_primed 35000, primed 20000.

## 9. Open questions and unverified items

| # | Item | Why it matters | Settle by |
|---|---|---|---|
| 1 | Does the APE honour Pause 5 / Resume 6? Does a release hold the car in UserPaused, and does a re-press within 35 s resume smoothly? | Tesla's release = pause, press = resume | **Supervised motion test** with a read-only DV watcher on GUI_selfParkState: expect 0x33→0x34, then 0x32→0x25/26→0x27 |
| 2 | During UserPaused: DAS_setSpeed valid and 0? DI_autoparkState valid and not STANDBY/UNAVAILABLE? Gear stays R/N/D? | Any "no" ends the 35 s window early, immediately if DI is invalid | Same **supervised motion test**: watch DAS_setSpeed, DI_autoparkState and VAPI_shiftState for 35 s after release |
| 3 | APE behaviour: a moving Come to Me goal, PinDrop (never run), how far short it stops, heartbeat slow/stop thresholds | Copy fidelity; safety envelope | **Supervised motion tests** only (APE off-dump): static Come to Me first, then a walking goal, then a short GTT pin, at a geofence-allowed spot |
| 4 | Car→client stream froze after about 150 messages / 15–30 s in every research session; the MCU has no throttle. Hypothesis A: dbus-daemon pending-reply limit (max_replies_per_connection; built-in default 128 [UNVERIFIED]; the car has no system.conf) when CarserverToPeerCommsData calls go unanswered. Hypothesis B: the webrtc-comms or SCTP transport | Without echoes the faithful app shows 'Reducing speed…' at 2 s and disconnects at 10 s | **No-motion car check**: helper session with 10 Hz heartbeats for 120 s and no verb. Expect about 1,200 echoes and no 'webrtc handleCarserverToPeerCommsDataReply error on dbus' in the QtCarServer log. The 10 s stop timer then writes CAPI=3 in Idle, which is inert |
| 5 | Connect-time StandbyMode pre-prime: is GUI_selfParkStandbyMode on? What reasons and timeouts follow (35000/20000 → not_started_timeout → Idle)? | Normalizer inputs; opening the screen turns the drive rail on | **No-motion car check**: open an idle session for 90 s and read GUI_selfParkState and GUI_selfParkActivation |
| 6 | Phone-link RTT and jitter versus the D2 300 ms budget (RTT < 200 ms needed for the first beat) | Spurious pauses | **No-motion car check**: echo RTT over the real path (Wi-Fi, Tailscale) |
| 7 | JS heartbeat timer stalls longer than 200 ms | Spurious D2 trips | **Device check** (no car): log beat intervals during map pans |
| 8 | vehicle_data:location during motion: shift D/R and speed units | Header text, camera "outside P" rule, the only motion signal | **No-motion gear probe** (P→D→P with brake held, occupant in seat, no verb), then the first **supervised run** |
| 9 | Format of public_key_hash in info (Tesla's public_key_id: 8-hex id or 40-hex SHA-1) | Fidelity only; binding is inert for motion | **Decompile**: jadx com/teslamotors/plugins/ble/i.java:433 (origin of eVarD.f56917d) and the iOS equivalent |
| 10 | Which camera Tesla streams for ctx 'summon'; whether airgapp's camera keeps flowing outside P | Camera strip copy | **Decompile** the webrtc-comms webcam:signal path (W, around L111945) and the videod config, then a **supervised motion test** |
| 11 | Code-derived visuals: held button #1E2E5B FILLED+DISABLED; crosshair offset insets.top/2 (~30 pt); target icon 28 pt versus the 40 pt box | Pixel fidelity; GTT pin accuracy | **Device check** of the real Tesla app (screen recording; no car) |
| 12 | Does the RN press survive the button flipping to DISABLED mid-press? | Deadman release correctness | **Device check** in airgapp: hold 10 s, confirm no onPressOut |
| 13 | Fix cadence of a stationary iPhone (device_location is sent only on a strict lat/lon change) | Come to Me goal refresh | **Device check**: 60 s stationary at bestForNavigation |
| 14 | requireEuBLECheck and R79 6 m (GUI_applyEceR79, VAPI_R79BehaviorOverride, FEATURE_disableR79Restrictions; the disable-r79 patch is in the dumped libs but not in the coilshift ledger) | Bluetooth gate texts; car-side 6 m waits | **No-motion car check** (DV reads) |
| 15 | Runtime config: smart_summon_worst_accuracy 200, remote_request_radius_m 300, user_paused_timeout 35000, default_stop_timeout 10000 / pause 0, force_defer_summon_checks_to_ap false, stub_ready_state false; is the car fused? | Every timing above | **No-motion car check** (read settings.conf / ConfigValue) |
| 16 | Effect of control:keepalive from the helper session (GUI_remoteAccPowerRequest); does it extend the ~5 min doze? | Copying Tesla's 15 s keepalive | **No-motion car check** |
| 17 | Car-side completion and aborts are invisible on this car, so 'Release to stop' stays until release | Residual difference from stock | Observe in a **supervised motion test**. Inferring 'complete' from P-after-D/R would be a new deviation and is not in this spec |
| 18 | Scope of the 3 m floor (cmd_find_me only, at press and re-press) [INFERRED] | Near-arrival updates in Come to Me | Watch in the first **supervised Come to Me** run |
| 19 | Exact ICE-loss timing equivalent (libwebrtc consent freshness) | Transport-loss threshold | **Decompile** the libwebrtc constants in W, or keep the airgapp session lease (behaviour: terminal abort on loss) |
| 20 | iOS parity of the status/enable selectors and the camera | Fidelity | **Decompile** iOS 4.57.5 main.jsbundle (same pipeline) |

**Settled by this synthesis:**
- the car ignores hello;
- data_channels_ready connects;
- UserPaused never reads 'paused';
- the dotSeparator is '·';
- the caution shows once per launch;
- the command timer is dead;
- 'Communicating' appears only on a re-connect edge.
