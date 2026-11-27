# Summon review fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** summonhelper vouches for a Summon session only while that session's process is alive and its phone is present.
- The live file is held under an flock.
- No stale live file survives any end path.
- With the phone's opt-in, a presence lease ends the session 10 s after the last fresh tick echo.

**Architecture:**
- **The live file.** It becomes a per-session hold. A locked temp file is renamed into place, and the fd is kept until the session ends. The off path only unlinks it, if it is still ours, and closes it.
- **Stale-file removals.** These are added to every place a helper's end is handled: Serve, the takeover, finish, the runit run script, and the installer's Summon removal.
- **The presence lease.** It lives in `internal/summon`'s capability. A new inner frame 0x18 and any fresh tick echo stamp the tick's issue time, and the D2 loop closes the window `lease` after 10 s without one. The phone opts in through the authenticated inner ARM params.

**Tech Stack:** Go 1.26.2 (go.mod), POSIX sh (deploy/install-car.sh), `syscall.Flock`.

**Spec:** `docs/superpowers/specs/2026-11-26-summon-review-fixes-design.md` (approved by the owner on 2026-11-26). The review behind it is
tesla-dump `docs/summon-live-file-review-2026-11-26.md`, findings F5r, L1-L5, live-2, T1, T2, T8 and T12. Line numbers below are at
efd7ff1.

## Global Constraints

### The live file

- **Path:** `proto.SummonLivePath` = `/run/airgapp-summon/live` (camhelper/proto). It no longer follows `-marker`.
- **Writer:** temp file `.live.` + random suffix in the same directory, opened `O_WRONLY|O_CREATE|O_EXCL|O_NOFOLLOW` (os.OpenFile adds
  `O_CLOEXEC`), mode `0600`. Then `flock(LOCK_EX)`, write `"<pid>\n"`, rename to `live`, and keep the `*os.File` for the session.
- **Off:** unlink `live` only if `os.SameFile(Lstat(path), f.Stat())`, then close `f`. The off path opens nothing.
- **Order:** live off, then Stop (H4), exactly as `session.Teardown` does today (capability.go:407-410).
- **camhelper's reader (efd7ff1, not changed here):** the directory and the file must be owned by 1337 with `perm&022 == 0`. The
  session counts as live only when `flock(LOCK_SH|LOCK_NB)` fails with `EWOULDBLOCK`.
- **Stale-file removal** is unconditional in each of these, and never changes the result of Finish or the takeover:
  - Serve, before Acquire;
  - `takeover.run`, on every branch after the abort;
  - `Finish` and `cmdFinish`, before the no-marker return;
  - the runit run script, beside `relay.sock` and after the directory checks;
  - `summon_remove`, after `helper_gone`.

### The presence lease

- **Frame:** `TypePresence = 0x18`, body exactly 8 bytes (a tick id). Inner Spec `In` adds `0x18: 8`. Nothing else in the Spec
  changes (LeaseMS 20000, TickHz 20).
- **Timing:** `PresenceFresh = 3 * time.Second`: the largest tick age that stamps. `PresenceAfter = 10 * time.Second`: the session
  closes when more than 10000 ms have passed since the stamp. At exactly 10000 ms it stays open; at 10001 ms it closes.
- **Stamp:** `issued = now - ceil(age to ms)`, stored as `max(stored, issued)`. It is recorded for PRESENCE, LIVE, VERB and HB when
  `TickAge` is ok and `0 <= age <= 3 s`, whether or not presence is enforced.
- **Opt-in:** enforced only when the inner ARM params are a JSON object whose `presence` is the number `1`.
  - nil, empty, `null`, `{}`, or an object without `presence`: not enforced. Unknown keys are ignored.
  - Any other `presence` value, a non-object, or malformed JSON: Open refuses and logs exactly
    `summon: the session did not start: bad params`. It never logs the params or the JSON error text.
- **Close reason:** the literal `"lease"`. The normal teardown follows: uninstall (live off), `eng.Stop` (H4), BYE `lease`. It is
  logged once, as `summon: no fresh presence for <ms> ms: ending the session (lease)`.

### Unchanged

D2 (freshness 150 ms, trip 300 ms), D3 (floors, staleness, the carry rule), H1, H4's order, the takeover's abort and disconnect,
the verbs, and the car messages. The engine (`engine.go`) is not edited.

### Host safety

- No test touches a real `/run` path. Every rig injects `LivePath` into a temp dir, and `helper.Config` refuses an empty one.
- No test signals a pid it did not spawn. New signals go only through the existing `signalPID` (`TestNoUnguardedKill` enforces this).
- Nothing contacts the car or the Pi.

### Commits

End every commit message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never push.

## Review Focus

1. **A slow but present phone.** An opted-in phone whose echoes arrive 2.9 s old, once a second, stays up indefinitely. Test in
   Task 4: `TestPresenceKeepsIdlePhone`.
2. **A reconnect after a lease end.** The new session gets a fresh seed, a fresh live file and a fresh lock, so it is not closed at
   once. Tests: Task 4 `TestPresenceReconnectSeedsFresh`, Task 1 `TestLiveFile/release keeps another hold's file`.
3. **An opted-out phone sending PRESENCE** (a phone build ahead of its opt-in) is accepted, stamped and never enforced. It never
   gets BYE `grammar` or `lease`. Test: Task 4 `TestPresenceNotEnforcedAcceptsPresence`.
4. **A helper SIGKILLed mid-session, then supervised again.** The takeover removes the old live file before the name is released,
   and the next session creates a new, locked one. Test: Task 2, in `TestTakeoverAfterChildSIGKILL`, plus the lock probe in
   `TestWedgedEngineIsKilledAndAborted`.
5. **An unlink that fails** (an unwritable directory). The lock is still released, so the leftover file proves nothing. It is
   logged as such, and Finish and the takeover still succeed. Tests: Task 1 `TestLiveFile/release when unlink fails`, Task 2
   `TestFinishNeverFailsOnLiveRemoval`.

## Rulings (decided here; reviewers check against these)

- **R1.** `CapConfig.Live` becomes `func() (off func())`. It is called once per session after a successful Start, and returns that
  session's off, or nil when no file is held. The fd is therefore per session, and with `SameFile` a late off can never unlink the
  next session's file (L4). `session.live bool` becomes `liveOff func()` plus `liveEnded bool`, guarded by `liveMu`.
- **R2.** `helper.Config.LivePath` is required (host safety). `cmd/summonhelper` supplies the default through a hook variable,
  `liveFile = proto.SummonLivePath`.
- **R3.** Serve's removal before Acquire is unconditional, as the contract with the camera session says. A stray second serve now
  unlinks a live session's file. That fails closed (no Summon camera) and is accepted.
- **R4.** In `takeover.run` and `Finish`, the removal runs before any log line and before the name's Release. Its outcome is logged
  after Release. Its error is never folded into the function's result.
- **R5.** L2 is fixed by making the off path open nothing and by `setLive` recovering a panic from `Live` or its off. The order is
  not changed.
- **R6.** VERB and HB stamp before `parse`: a fresh echo proves presence even when the JSON is then dropped.
- **R7.** The presence check runs in `Capability.tick` after `s.eng.Tick()`. It loads the stamp before reading the clock. A clock
  that ran backwards (`now < stamp`) counts as expired. The close and its log line run once, guarded by `CompareAndSwap`.
- **R8.** The review's test 4 ("D2/D3 tables unchanged") is done by re-running the capability-level D2 tests on an opted-in
  session. engine_test and d3_test never reach the capability.
- **R9.** At a presence expiry with a verb that was held, the car sees D2's one injected abort, then H4's disconnect, and no second
  abort. The test asserts exactly that.
- **R10.** The Summon bench (`cmd/agw/summon_e2e_test.go`) gains only `LivePath`. Under the opt-in it needs no PRESENCE.
- **R11.** The run script also removes leftover `.live.*` temp files.
- **R12.** Only `summon_remove` gains an installer-side removal, as the contract says. The other hand-backs rely on the takeover.
- **R13.** Historical plans get an amendment note, not a rewrite: the 2026-11-21 car-side plan's Global Constraints pin the inner
  Spec.

---

### Task 1: The held live file

**Files:**
- Modify: `internal/summon/helper/live.go` (rewrite), `internal/summon/helper/helper.go:57-114,169-190`,
  `internal/summon/capability.go:41-45,141-170,287-303`, `cmd/summonhelper/main.go:66-84,293-330`
- Test: `internal/summon/helper/live_test.go` (rewrite), `internal/summon/capability_test.go:446-485`,
  `internal/summon/helper/helper_test.go:179-225,867-890,964-983,994-1009`, `internal/summon/helper/takeover_test.go:244-253`,
  `cmd/agw/summon_e2e_test.go:784-796`, `cmd/summonhelper/main_test.go:97-120`

**Interfaces:**
- Consumes: `proto.SummonLivePath` (camhelper/proto/proto.go:40).
- Produces:
  - `type liveHold struct { f *os.File; path string }` (unexported)
  - `func holdLive(path string) (*liveHold, error)`
  - `func (h *liveHold) release() error`
  - `func RemoveLive(path string) (removed bool, err error)` (exported)
  - `CapConfig.Live func() (off func())`
  - `helper.Config.LivePath` is now required.
  - `cmd/summonhelper` gets the hook `var liveFile = proto.SummonLivePath`.
  - `LiveName`, `livePath` and `setLiveFile` are deleted.

- [ ] **Step 1: Write the failing tests**

  `live_test.go` `TestLiveFile` has these subtests. Each uses its own `t.TempDir()`.
  - `held`: `h, err := holdLive(p)`.
    - The file reads `fmt.Sprintf("%d\n", os.Getpid())`.
    - It is a regular file with `Mode().Perm() == 0o600`.
    - A probe reports locked: a second open `O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK`, then
      `syscall.Flock(fd, LOCK_SH|LOCK_NB)`, fails with `EWOULDBLOCK`.
    - No directory entry starts with `.live.`.
  - `release removes and unlocks`: `h.release()` is nil, and `Lstat(p)` is `fs.ErrNotExist`.
  - `release keeps another hold's file` (L4):
    - `a := holdLive(p)`, then `b := holdLive(p)`.
    - `a.release()` is nil. `p` still exists and the probe still reports locked.
    - `b.release()` removes `p`.
  - `release when unlink fails`:
    - Under root, `t.Skip("root ignores directory permissions")`.
    - `chmod 0555` the directory: `release()` returns non-nil.
    - A fresh open with `Flock(LOCK_EX|LOCK_NB)` succeeds.
    - Restore `0755` in `t.Cleanup`.
  - `a FIFO at the path never blocks release`: `holdLive`, `os.Remove(p)`, `syscall.Mkfifo(p, 0o600)`. `release()` returns within
    1 s, nil, and the FIFO stays (it is not ours).
  - `a symlink at the path is replaced`: `os.Symlink(target, p)` with `target` absent. After `holdLive(p)`, `Lstat(p)` is a regular
    file and `target` does not exist.
  - `RemoveLive`: an absent path gives `(false, nil)`; a present one gives `(true, nil)` and the path is gone.

  `capability_test.go` `TestLiveFollowsTheSession`, rewritten for R1:
  - `Live` records `on` and returns a func that records `off`.
  - A clean close: `[on off]`. Wait with `s.waitTorn(t)`, never with a bare `<-s.torn` (T12).
  - An unclean end, with the fake car's disconnect failing (add `fakeCar.setDisconnectErr`, mirroring `setSendErr`,
    engine_test.go:102): `[on off]` (T2).
  - A failed Start (`setConnectErr`): no calls.
  - While a second real session is current, `r.c.setLive(first, true)` makes no call.

  New capability tests:
  - `TestLiveOffPanicDoesNotSkipStop`: the off panics. `r.calls()` still contains `car.disconnect` then `marker.clear`, and
    `Fatal` is not called.
  - `TestLiveOnPanicIsContained`: `Live` itself panics. The session still runs, and its teardown makes no off call and still runs H4.

  Helper and command tests:
  - helper_test `newRig`: `LivePath: filepath.Join(dir, "live")`.
  - `TestSignInAndSessionUp`: after `p.waitCar(typeLocation)`, the file holds the pid and the probe reports locked.
  - `TestShutdownRunsH4ThenReleases`: the file is gone after `r.finished`.
  - `TestNameLostStopsServing`: the file is gone after `p.waitEnd`, while the marker stays.
  - `TestServeRefusesBadConfig`: an empty `LivePath` is refused, and the error contains `paths are required`.
  - takeover_test `testServe` and the summon e2e rig: `LivePath: filepath.Join(dir, "live")`.
  - main_test, a new `TestServeLivePathDefault`: the recorded `helper.Config.LivePath == proto.SummonLivePath`.

- [ ] **Step 2: Run them to see them fail**

  Run: `go test -race -count=1 ./internal/summon/... ./cmd/summonhelper/`. Expected: build errors (`holdLive`, `RemoveLive` and
  `liveFile` undefined; `Live` has the wrong type).

- [ ] **Step 3: Implement**
  - **live.go.**
    - `holdLive` tries random `.live.` names up to 10 times on `fs.ErrExist`.
    - Its blocking `Flock(LOCK_EX)` retries on `EINTR`.
    - A deferred cleanup keyed on success closes `f` and removes the temp file on any failure or panic.
    - `release` uses `os.SameFile`. It closes `f` unconditionally and returns the unlink error (ENOENT counts as nil).
  - **capability.go.**
    - `setLive(s, on bool)` keeps its signature.
      - On, when `c.current() == s && !s.liveEnded && s.liveOff == nil`: `s.liveOff = f()`.
      - Off: `s.liveEnded = true`, then take, nil and call `s.liveOff`.
    - Both calls run under `liveMu`, inside a recover that logs `summon: the live signal failed: <panic>` and never re-panics.
    - Update the doc comments at 41-45 and 141-157.
  - **helper.go.**
    - `check()` requires `LivePath` alongside the others: "the allowlist, socket, marker and live paths are required".
    - `Live: func() func()` calls `holdLive(cfg.LivePath)`.
      - On error it logs `summonhelper: live file not created: <err> (no Summon camera for this session)` and returns nil.
      - Otherwise it returns a func whose `release` error logs
        `summonhelper: live file not removed: <err> (its lock is released, so it proves nothing)`.
    - Replace step 3's `setLiveFile(live, false)` (158-162) with `RemoveLive(cfg.LivePath)`, still after Acquire. Task 2 moves
      it before Acquire.
  - **main.go.** Add the hook `liveFile = proto.SummonLivePath` and set `LivePath: liveFile` in cmdServe.

- [ ] **Step 4: Run the tests**

  Run: `go test -race -count=1 ./internal/summon/... ./cmd/summonhelper/ ./cmd/agw/ ./internal/camhelper/`. Expected: PASS.

- [ ] **Step 5: Commit**

  `fix(summonhelper): hold an flock on the live file for the session (review L1-L5)`

### Task 2: Stale-file removals in the helper

**Files:**
- Modify: `internal/summon/helper/helper.go:116-162`, `internal/summon/helper/takeover.go:3-7,29-99,145-234`,
  `cmd/summonhelper/main.go:5,231-275`
- Test: `internal/summon/helper/helper_test.go`, `internal/summon/helper/takeover_test.go` (newTK, the Supervise calls at 184, 534
  and 1246, the 10 Finish calls), `cmd/summonhelper/main_test.go:489-523,584-592,614-703`

**Interfaces:**
- Consumes: `RemoveLive(path string) (bool, error)` (Task 1).
- Produces:
  - `func Supervise(ctx context.Context, bus carlink.Bus, serveArgv []string, markerPath, livePath string, logf func(format string, args ...any)) error`
  - `func Finish(bus carlink.Bus, markerPath, livePath string, logf func(format string, args ...any)) error`
  - `takeover.livePath string`

- [ ] **Step 1: Write the failing tests**
  - **`TestServeRemovesStaleLiveFile`.** Write `"123\n"` (no lock) at `LivePath`.
    - With `r.bus.TakeName`, Serve returns `ErrNameTaken`, and the file is gone anyway.
    - With the name free, the file is gone before `OnListening`.
    - The log has `summonhelper: removed a stale live file (an earlier session did not end cleanly)` exactly once.
    - `summonhelper: live file <path>` is logged once at start.
  - **takeover_test.** Add `k.live = filepath.Join(k.dir, "live")` and pass it everywhere.
    - Pre-create `k.live`, then assert it is gone in each of these:
      - `TestTakeoverAfterChildSIGKILL` (both logf variants), before Supervise returns;
      - `TestTakeoverNoMarkerSendsNothing`;
      - `TestTakeoverWithoutNameKeepsMarker`;
      - `TestQueueFailureKillsChild` ("an error");
      - `TestParentConnectionLossKillsChild`;
      - `TestFinish` ("no marker", and "a marker and a free name");
      - `TestSupervisorKilledThenFinish`.
    - In `TestWedgedEngineIsKilledAndAborted`: the real Serve child's `k.live` is locked during `holdVerb` (the cross-process probe
      gets `EWOULDBLOCK`) and gone after the takeover.
    - The blocking-logf variants still pass, which shows no log line comes before Release.
  - **`TestFinishNeverFailsOnLiveRemoval`.** Non-root (`t.Skip` under root). A live file sits in a 0555 directory with no marker.
    `Finish` returns nil and logs the removal error.
  - **main_test.**
    - `TestFinishCommand` points `liveFile` at `t.TempDir()` (restored in `t.Cleanup`) for the whole test, the runsv subtest
      included. With no marker, the live file is removed and `buses == 0`.
    - `TestRunForwardsServeFlags`: the stub receives `livePath == proto.SummonLivePath`.

- [ ] **Step 2: Run them to see them fail**

  Run: `go test -race -count=1 ./internal/summon/helper/ ./cmd/summonhelper/`. Expected: FAIL (signatures, then the files still
  present).

- [ ] **Step 3: Implement**
  - **Serve.** After `setBusLogf` (150) and before `NewLink` (153):
    - log the path;
    - call `RemoveLive(cfg.LivePath)` and log the two lines above;
    - delete the old removal at 158-162;
    - renumber the doc steps at 116-137.
  - **`takeover.run`.** Call `RemoveLive(t.livePath)` between the switch's end (175) and `t.link.Release()` (176). Keep the result
    and log it after Release as `summonhelper: takeover removed the live file`, or with its error. Do not touch `abortAndDisconnect`.
  - **`Finish`.** Remove first, before the marker check. In the marker path the order is abort, disconnect, marker clear, Release,
    then the log. In the no-marker path, log after the decision. Never return the removal's error.
  - **`cmdFinish`.** Call `helper.RemoveLive(liveFile)` before `marker.Present` (266) and log it. Pass `liveFile` to `helper.Finish`.
  - **`cmdRun`.** Pass `liveFile` to `superviseHelper`.
  - Update the doc comments named in the Files list.

- [ ] **Step 4: Run the tests**

  Run: `go test -race -count=1 ./internal/summon/... ./internal/supervise/ ./cmd/summonhelper/`. Expected: PASS, including
  `TestNoUnguardedKill`.

- [ ] **Step 5: Commit**

  `fix(summonhelper): remove a stale live file wherever a helper's end is handled (review L1, live-2)`

### Task 3: Stale-file removals in the installer

**Files:**
- Modify: `deploy/install-car.sh:251-255,827-861,1105-1146,1212,1267`
- Test: `cmd/camhelper/install_test.go:689-856,1176-1279`

**Interfaces:**
- Consumes: none. The installer only removes the files.
- Produces: the generated run script line `rm -f "<run>/live" "<run>"/.live.*`; summon_remove's `rm -f "$SUMMON_RUN_DIR/live"`.

- [ ] **Step 1: Write the failing tests**
  - **`TestSummonRunScriptOrder`.** Insert `rm -f "/run/airgapp-summon/live" "/run/airgapp-summon"/.live.*` into `inOrder`, after
    the relay.sock line (717) and before the VIN line (718). Add `rm -f "/t/r/live" "/t/r"/.live.*` to the override list (752).
  - **`TestSummonRunScriptRefusals`.** Pre-create `runDir/live` and `runDir/.live.x1` in the empty-VIN case and the name-taken case.
    Both are gone after the refusal.
  - **`TestSummonZeroRemovesHelper`.** `live` is gone after a completed removal, and kept in "a summonhelper process lingers".
  - **`TestSummonHooksNeedRunDirOverride`.** `AGW_SUMMON_REMOVE_ONLY=1` and `AGW_EXIT_PATH_ONLY=1` without
    `AGW_TEST_SUMMON_RUN_DIR` are refused by `hook_needs`.
  - `TestSummonMainFlowOrder` is unchanged and must still pass.

- [ ] **Step 2: Run them to see them fail**

  Run: `go test -race -count=1 ./cmd/camhelper/ -run 'Summon'`. Expected: FAIL.

- [ ] **Step 3: Implement**
  - **The run script.** Put the new `rm` right after the relay.sock line (1122). That is after the symlink and non-directory
    refusal and the mkdir/chown/chmod (1112-1121), and before the VIN and name-owner refusals.
  - **`summon_remove`.** Put the new `rm` after the `helper_gone` guard's `fi` (853), before the name_owned/hand_back branch.
  - **`hook_needs`.** Add `AGW_TEST_SUMMON_RUN_DIR` at 1212 and 1267.
  - **Comments.** Update 251-255, 1108-1110 and 1139-1142 ("does nothing otherwise"), and 827-839. Keep "up to about a minute".

- [ ] **Step 4: Run the tests**

  Run: `go test -race -count=1 ./cmd/camhelper/`. Expected: PASS.

- [ ] **Step 5: Commit**

  `fix(deploy): the run script and SUMMON=0 remove a stale Summon live file (review L1)`

### Task 4: The presence lease

**Files:**
- Modify: `internal/summon/frames.go:10-71`, `internal/summon/capability.go:18-28,101-137,253-274,287-303,346-388`
- Test: `internal/summon/capability_test.go` (TestInnerSpec 333-349, capRig.open 183-215, TestFrameDecodeErrors 714-724, and new
  tests after 810)

**Interfaces:**
- Consumes: `capability.Window.TickAge`, `Window.Close`, `clock.Clock.BootMS`.
- Produces:
  - `const TypePresence byte = 0x18`
  - `const PresenceFresh = 3 * time.Second`
  - `const PresenceAfter = 10 * time.Second`
  - `func decodeTick(t byte, body []byte) ([tickLen]byte, error)`, which replaces `decodeLive`
  - `func parseParams(p json.RawMessage) (presence bool, err error)`
  - session fields `presence bool`, `echoAt atomic.Uint64` and `presenceGone atomic.Bool`
  - `func (s *session) stamp(age time.Duration, ok bool)`
  - `func (s *session) presenceCheck()`
  - Do not name anything `fresh`: `engine.go:377` already has `fresh()`.

- [ ] **Step 1: Write the failing tests**

  All tests use `newCapRig(t, clock.NewFake())`. Add `r.openWith(t, w, params)` and `r.openSeededWith(...)`, plus
  `s.presence(t, age, ok)`, which uses `tickWith` and then sends `TypePresence`.

  Spec and decode:
  - `TestInnerSpec`:
    - `In == {0x14: 8, 0x15: 1036, 0x16: 524, 0x17: 2048, 0x18: 8}`;
    - `TypePresence == 0x18`;
    - it differs from `0x10`, `0x90` and `0x14`-`0x17`.
  - `TestFrameDecodeErrors`: add rows for a 7-byte and a 9-byte PRESENCE. Each returns an error.

  The opt-in, `TestPresenceOptIn`, is a table:
  - nil, `{}`, `null`, and `{"other":1}` → not enforced;
  - `{"presence":1}` → enforced;
  - `{"presence":0}`, `{"presence":true}`, `{"presence":"1"}`, `{"presence":2}`, `[]`, `"x"`, and `{"presence":1` → refused.

  Each refusal logs `summon: the session did not start: bad params` exactly once, and `noSentinel` holds for a params value
  carrying the sentinel. "Enforced" means: with no echo, `r.step(10001)` closes with `lease`. "Not enforced" means it stays open
  after `r.step(60000)`.

  Keeping and ending a session:
  - `TestPresenceKeepsIdlePhone`, opted in:
    - for 600 fake seconds, every 1000 ms: `r.step(1000)` and `s.presence(t, 2900*time.Millisecond, true)`;
    - a keepalive MSG every 5 s.

    The window stays open, `r.car.sends()` is exactly the MSGs, `r.linkCalls()` is empty, and there is no `lease` line.
  - `TestPresenceExpiryTearsDown`, opted in:
    - one fresh PRESENCE (age 0) at t0;
    - `r.step(10000)` leaves it open, `r.step(1)` closes it, and `CloseReason() == "lease"`;
    - after 50 more `r.step(10)`, the line `summon: no fresh presence for` appears exactly once;
    - `Live`'s off ran, and `r.calls()` ends with `car.disconnect`, `marker.clear`, with no abort.

    Variant (R9): a verb was held with fresh LIVEs, then the LIVEs stop. The calls hold exactly one `car.send` of the abort (D2's,
    at 300 ms), then `car.disconnect`, and no second abort.

  Which echoes stamp:
  - `TestPresenceNeedsFreshTick`, opted in:
    - only PRESENCE with `ok=false`, PRESENCE aged 3001 ms, and MSGs are sent, from Open on: it closes at exactly 10001 ms after
      Open;
    - in a separate session, PRESENCE aged exactly 3000 ms at t0+5000 stamps t0+2000: open at t0+12000, closed at t0+12001.
  - `TestPresenceFromHold`, one subtest each for LIVE, HB and VERB, opted in:
    - that frame with age 0 at t0+9000 keeps the session open at t0+19000 and closed at t0+19001;
    - the HB case passes even though the engine drops the HB (no verb is active).

    Then re-run `TestTickAgePassedThrough`, `TestLinkWired`, `TestRealClockTrip` and `TestTeardownRunsH4` on a
    `{"presence":1}` session (R8), by parameterizing their open.

  Edge cases:
  - `TestPresenceClockBackwards`, opted in: with `stepClock` going backwards below the stamp, the next tick closes with `lease`.
  - `TestPresenceNotEnforcedAcceptsPresence`: an opted-out session takes fresh and stale PRESENCE frames with no error and no
    close, and makes no car send for them.
  - `TestPresenceReconnectSeedsFresh`: after a `lease` close, a new opted-in session opened at t is open at t+10000.

- [ ] **Step 2: Run them to see them fail**

  Run: `go test -race -count=1 ./internal/summon/ -run 'Presence|InnerSpec|FrameDecode'`. Expected: build errors, then FAIL.

- [ ] **Step 3: Implement**
  - **frames.go.**
    - Add the constants, plus `presenceAfterMS` as at engine.go:37-40.
    - Replace `decodeLive` with `decodeTick`, whose error names `frameName(t)`, and give `frameName` a `PRESENCE` case.
    - Update the doc at 10-12.
  - **capability.go.**
    - Add `InnerSpec` `In[TypePresence] = 8`.
    - In `Open`, parse the params before `c.mu.Lock()` (121). Set `s.presence` and seed `s.echoAt` with `c.cfg.Clock.BootMS()`
      before the install.
    - In `Frame`, call `s.stamp(age, ok)` right after each `TickAge` (357 and 364, VERB/HB before parse). Add a
      `case TypePresence:` that runs `decodeTick`, `TickAge` and `stamp`, makes no engine call, and returns nil.
    - In `tick()`, keep the progress stamp first, then `s.eng.Tick()`, then `s.presenceCheck()` (R7).
    - The close literal is `"lease"`.
    - Update the doc comments at 18, 101-109, 253, 267-268 and 346-349.

- [ ] **Step 4: Run the tests**

  Run: `go test -race -count=1 ./internal/summon/... ./cmd/agw/ ./capability/...`. Expected: PASS. The bench needs no change
  (R10).

- [ ] **Step 5: Commit**

  `feat(summon): an opt-in presence lease ends the session 10 s after the last fresh tick echo (review F5r)`

### Task 5: The held-back-frames attack, end to end

**Files:**
- Test: `internal/summon/helper/helper_test.go`. Use the helper rig: real Serve, the core and the mock car in one process, with
  `r.cfg.Clock = clock.NewFake()`. Never use the takeover rig.

**Interfaces:**
- Consumes: Tasks 1 and 4: `LivePath`, `TypePresence`, and `{"presence":1}`.
- Produces: test helpers only:
  - `a.signInWith(r, params any)`, a variant of trySignIn at 434-449;
  - an up pump with a `hold()` / `releaseOne()` mode at the INNER-record level, where one record is one sealed frame
    (internal/wire/frame.go:34-43);
  - `p.startPresence(every time.Duration)`, copied from `startLive` (598-619), which sends `TypePresence` with `NewestTick`.

- [ ] **Step 1: Write the tests**

  Real tickers read the fake clock, so after each `Advance` the tests wait up to 1 s with `eventually`.
  - **`TestSilentAgwEndsSessionAtLease`** (an opted-out phone, the control for today's bound).
    - After sign-in, the pump forwards nothing more, then the clock advances 20001 ms.
    - The phone's end error contains `BYE: lease`.
    - The mock car got a disconnect.
    - `LivePath` is gone.
  - **`TestHeldBackFramesCannotStretchSession`** (opted in).
    - The phone sends PRESENCE every fake second while all of it is forwarded, for 5 s.
    - Then the pump holds every record; the phone keeps sending for 10 more fake seconds, then stops.
    - The pump then releases one held record every 15 fake seconds.
    - The session must end with `lease` no later than 10001 ms after the last echo the helper received fresh.
    - Then the disconnect is seen, `LivePath` is gone, and the helper log has the presence line once.
  - **`TestHeldBackFramesStretchAnOptedOutSession`** (control, opted out). The same schedule: after 4 releases, 60 fake seconds,
    the session is still open and `LivePath` is still locked. This records the bound the opt-in removes.

- [ ] **Step 2: Run them**

  Run: `go test -race -count=1 ./internal/summon/helper/ -run 'Lease|HeldBack'`. Expected: PASS. To show the test has teeth,
  temporarily make `presenceCheck` return early: `TestHeldBackFramesCannotStretchSession` must then FAIL. Revert before committing,
  and say in the report that this was done.

- [ ] **Step 3: Commit**

  `test(summonhelper): agw holding back frames cannot stretch an opted-in session (review F5r)`

### Task 6: Docs

**Files:**
- Modify:
  - `docs/SUMMON.md`: 30-38 (the process table and the run script), 47-50 (Paths), 64-67 (agw can delay and close), 70 (the Spec),
    72-80 (the frame table), 146-149 (the H4 and lease bullet), 216-218 (log lines), 236-270 (the phone contract);
  - `docs/CAMERA-HELPER.md`: 77, 153 and 277-279;
  - `docs/CAPABILITY-API.md`: 96-98, 156-163 and 176-199;
  - `deploy/README.md`: 70-72, 226-229 and 333-345;
  - `docs/superpowers/specs/2026-11-20-summon-design.md`: 152-166, 222-223, 277-279, 355, 394-406 and 584-585;
  - `docs/superpowers/plans/2026-11-21-summon-car-side.md`: an amendment note after its Global Constraints (85-91).

**Interfaces:**
- Consumes: the final names and log lines from Tasks 1-5.

- [ ] **Step 1: Write**

  Every statement must match the code.
  - **The live file:** a flock is held for the session, and the five removal places are listed.
  - **The PRESENCE row:** `0x18`, a tick id of 8 bytes, phone → helper.
  - **The presence lease:**
    - 3 s freshness and 10 s expiry, stamped at the issue time;
    - the opt-in through the inner ARM params only;
    - BYE `lease` through the same H4.
  - **What agw can do:** it can delay and close frames. The bound is 10 s when presence is on, and about 20 s per held frame for an
    opted-out phone.
  - **The phone contract:**
    - PRESENCE on every 20th tick of an up connection, held or not;
    - `{"presence":1}` on the inner leg only;
    - a `lease` end is a non-revoked drop;
    - **the helper ships before the phone**, because an old helper answers 0x18 with BYE `grammar`.
  - **CAMERA-HELPER.md 277-279:** replace "agw alone can never lift the gates" with the bounds that are now true.

- [ ] **Step 2: Check**

  Run: `grep -n "can never lift\|empties it\|LiveName\|Dir(MarkerPath)" -r docs deploy internal cmd`. Expected: no stale hits.
  Run: `go test -race -count=1 ./...`. Expected: PASS (35 packages).

- [ ] **Step 3: Commit**

  `docs(summon): the live-file lock and the presence lease`
