# Summon review fixes: the live-file lock and the presence lease (design)

Date: 2026-11-26. Branch `feat/summon-review-fixes`, cut from `feat/camera-capabilities` at efd7ff1.

**Status: approved.** The owner approved these fixes on 2026-11-26. They answered "Yes, build them" to the question "Should I build
the Summon-side fixes?", after the fixes were described as follows. The phone sends a once-a-second presence message that echoes the
helper's latest tick, and the helper ends the session 10 s after the last fresh one. The helper holds a lock on its live file that the
kernel releases when the helper dies, and the file is removed wherever a stop is handled.

Source: the review of 841d3df in tesla-dump `docs/summon-live-file-review-2026-11-26.md`, findings F5r, L1 (= live-1, live-2, T5,
F-livepid), L2-L5, T1, T2, T8 and T12. Every finding was confirmed by a second, adversarial reader.

## Why

camhelper's SUMMON mode copies Tesla's summon webcam. It waives the gear, Sentry, camera-access and service-mode gates, so the camera
can be held in any gear. camhelper grants it only while summonhelper vouches, through `/run/airgapp-summon/live`, that a phone is
running a Summon session. agw (uid 4950) relays the Summon bytes but must never be able to keep that vouching up on its own. The
review found two ways it can. Neither can move the car, because every verb still needs a tick echo no older than 150 ms, and agw can
neither read nor make one.

- **F5r (critical).** The inner session's only idle bound is the 20 s lease. Any frame that opens in sequence renews it, however old
  the frame is. An idle phone sends no tick-bearing frame: it sends a HEARTBEAT every 5 s, a keepalive every 15 s and device_location
  on each fix. So agw can hold back those frames during an idle visit and release one every <20 s after the phone has left. Each one
  buys about 20 s of session, live file and SUMMON mode, so a 2-minute idle visit is worth roughly 8-45 minutes.
- **L1 (important).** Every unclean end of `summonhelper serve` skips the session's uninstall: every runit stop (the supervisor
  SIGKILLs the child), Fatal (exit 4), the emit overrun (exit 3), and the liveness kill. Only the next serve removes the live file,
  and only after it wins the bus name. After a hand-back to stock webrtc-comms, which is also uid 1337, the file stays until reboot.
  The pid-and-uid check that 841d3df used then passes for any uid-1337 task that reuses the pid, thread ids included.

## Goals

- **G1.** The live file proves a running session process. It counts only while the process that wrote it holds an flock on it, and
  the kernel drops that lock when the process dies.
- **G2.** No stale live file survives a known end path: the takeover, finish, Serve's start, the runit run script, or the installer's
  Summon removal.
- **G3.** When presence is enforced, the session ends within 10 s after the phone's last fresh tick echo, whatever agw held back,
  and the live file goes with it.
- **G4.** A phone that sits idle on the Summon screen is never dropped (the owner's copy-Tesla rule). D2, D3, the D3 carry rule, H1,
  H4's order and the takeover are unchanged.
- **G5.** The rollout does not break the installed phone. Presence is enforced only when the phone opts in through the
  authenticated inner ARM params `{"presence":1}`. An older phone keeps today's behaviour, the 20 s lease.

## Non-goals

- camhelper's half of the live-file contract. The camera session built it in eed5773 and efd7ff1.
- The phone's half of the presence lease. The phone session builds it against the contract below.
- Making presence mandatory. That is a later change, once the owner's phone build ships.
- Any change to motion: D2 freshness (150 ms) and trip (300 ms), the D3 floors and staleness, the verbs and the car messages.

## Approaches considered

For F5r:
- (a) A shorter lease: this only shortens the stretch each held frame buys. Rejected.
- (b) Checking the `created` timestamps in device_location and info against the car's clock: this depends on two clocks agreeing,
  and HEARTBEAT and keepalive carry no timestamp. Rejected.
- (c) Capping the time since the last tick echo, using only today's frames: an idle phone sends none, so this would drop it. Rejected.
- (d) **Chosen.** A presence frame that echoes the latest tick about once a second. Ticks are sealed inside the inner session, so agw
  cannot read one, and a frame it held back carries a tick that is already old.

For L1:
- (a) Writing `<pid> <starttime>` and having camhelper compare it with /proc.
- (b) **Chosen with the camera session.** The writer holds an flock for the session's life. The kernel enforces it, no /proc parsing
  is needed, and a failed unlink becomes harmless by construction (L3). The explicit removals stay, because a SIGKILLed child stuck
  in D state still holds its lock.

## Design

### The live file

camhelper's half is already in efd7ff1. Both halves ship in one install, because camhelper refuses SUMMON mode until summonhelper
holds the lock.

- **Path.** `proto.SummonLivePath` (`/run/airgapp-summon/live`, in camhelper/proto). summonhelper's default uses it in place of
  `Dir(marker)/live` (L5). Tests inject a temp path. Serve logs the path once.
- **On.** Create a temp file `.live.XXXXXX` in the same directory with `O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC` and mode 0600. Take
  `flock(LOCK_EX)` on it (blocking is fine), write `<pid>\n`, rename it to `live`, and keep the fd for the session. camhelper opens
  only `live`, by name, so the temp file is never seen.
- **Off.** Under `liveMu`, unlink `live` only if it still names this session's file: `Lstat(path)` must match `fstat(fd)` on device
  and inode (L4). Then close the fd. The off path opens nothing, so there is no blocking open (L2). `setLive` recovers its own panic.
  The order is unchanged: live off, then Stop (H4).
- **No truncate fallback.** If the unlink fails, the close still drops the lock, so the file reads as not live (L3). The failure is
  logged as what it is, distinctly for on and off.
- **Stale-file removals (G2).** These run in five places:
  - `takeover.run`, on every branch, after the abort and disconnect;
  - `Finish` and `cmdFinish`, before their no-marker return;
  - Serve, before Acquire;
  - the generated runit run script, beside `relay.sock` and before the name-owner refusal;
  - the installer's Summon removal, once the helper is gone.

  Serve logs when it actually removed a file, as evidence of an unclean end.
- **camhelper's check, for reference.** The directory and the file must be owned by 1337, with `perm & 022 == 0`. The file is opened
  `O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC`. The session counts as live only when `flock(LOCK_SH|LOCK_NB)` fails with EWOULDBLOCK.

### The presence lease

- **Frame.** `PRESENCE = 0x18`, with an 8-byte body: the latest TICK id the phone received, encoded the same way as LIVE's tick id.
  The inner Spec's `In` gains `0x18: 8`.
- **Fresh stamp.** A PRESENCE, LIVE, HB or VERB frame stamps presence when its tick is known and at most `PresenceFresh` = 3 s old.
  The stamp is `fresh = max(fresh, now - age)`, which is the issue time of that tick, so holding a frame back gains agw nothing.
- **Expiry.** On each D2 loop pass, if presence is enforced and `now - fresh > PresenceAfter` (10 s), the helper logs once and
  closes the window with the reason `lease`. The normal teardown then runs: Handler.Teardown, uninstall (live off), Engine.Stop
  (cmd_abort if a verb is active, the disconnect, the marker), BYE.
- **Seed.** Open sets `fresh = now`.
- **Nothing else.** PRESENCE makes no Engine call. lastLive, the link flag, the D2 trip, D3 staleness and the D3 carry rule are
  untouched.
- **Opt-in (G5).** Presence is enforced only when the authenticated inner ARM params carry `"presence":1`. Today's phone sends `{}`
  and keeps the 20 s lease.
- **Phone contract** (built by the phone session):
  - On every 20th tick of an up connection (about 1 Hz), held or not, the phone sends PRESENCE carrying the latest tick.
  - The inner leg's ARM params carry `{"presence":1}`. The outer leg's do not.
  - A `lease` end is handled like any other non-revoked drop.
- **Why 10 s.** It is ten presence periods, it rides out stalls of several seconds, and an idle phone is dropped only when every
  echo is more than 3 s late for 10 s. After the phone leaves, agw gets at most 10 s more, whatever it held back, instead of 20 s per
  held frame.

## Constraints

- Nothing in this work contacts the car or the Pi. The assistant never commands motion.
- Tests are host-safe. No test signals a pid it did not spawn, and none writes a real `/run` path.
- `docs/SUMMON.md`, `docs/CAMERA-HELPER.md` and the 2026-11-20 Summon design change with this work: the idle bound, the live file's
  meaning, the inner Spec. Old wording that claims "agw alone can never lift the gates" is corrected to what is now true.

## Left to the builder

Internal names, log wording, and test helper structure, within the plan's interfaces.
