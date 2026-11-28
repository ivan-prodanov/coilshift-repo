# close-on-lock

**Close the frunk and trunk when the car locks**, if they were left open.

- **Trunk open:** it closes. This needs a power liftgate; a car without one ignores it.
- **Frunk open:** the car gets the frunk's *open* command again. An aftermarket power frunk takes a
  second release as "close". A stock frunk is already open, so nothing happens.

It works for every way of locking: walking away, the touchscreen, the app, the key card, the phone
key. The one exception is a lock while the car is in R, N or D (drive-away lock): the car is moving
then, so nothing is touched.

**Status:** 🧪 built and tested off the car 2026-11-28 (lint, `csdk test`, `test/run.sh`); not yet
tried on the car. Read from the 2026.38.6 dump (Model 3/Y build). S/X are assumed to work the same.

## Install

Install **Close frunk & trunk on lock** in CoilShift. There are no settings. The status line under
it says `Watching for lock.` once the service runs.

## How it works

A CoilShift service, `bin/cs-closeonlock.sh`, reads `VAPI_isLocked` every 0.5 s. The car API
sends no lock event, so it polls. Only a change from unlocked to locked counts: starting up or
waking up already locked does nothing. On that change:

1. **Gear:** if `VAPI_shiftState` reads R, N or D, stop there. A gear that can't be read right
   after the driver has left still counts as parked.
2. **Door state:** `VAPI_doorState` is a `DoorType` bitmask, where 16 = `TrunkFront` and
   32 = `TrunkRear`. The service accepts a decimal or hex number or the names; the exact text
   form on the car isn't confirmed yet, and each lock logs it.
3. **Frunk open:** set `GUI_frontTrunkRequest=Pressed`, the touchscreen's own "open frunk". The
   frunk goes first because the touchscreen stops offering frunk control once the alarm arms
   (`VehicleUtils::shouldFrunkControlBeEnabled` checks `VAPI_alarmStatus`).
4. **Trunk open:** set `GUI_rearTrunkRequest=Close`. This is the touchscreen trunk popup's Close
   (`TrunkOpenPopupController::trunkCloseRequested`), a close-only bit
   (`VehicleControlMessage3::assembleMessage`, `UI_closeTrunkRequest`). It never opens a shut
   trunk. The other value, `Pressed`, is a toggle.
5. **Reset:** one second later, set both requests back to `None`, so the next lock is a fresh
   press.

The requests go to the touchscreen process (`com.tesla.CenterDisplay`, D-Bus
`com.tesla.LocalDataValue.DataSetValueRequest`, as `/usr/local/bin/set-local-dv` does). The
touchscreen owns the `GUI_` requests and passes them over shared memory to `QtCarVehicle`, which
puts them on the CAN bus (`libQtCarVAPI`), exactly like a tap. The SDK's `PUT /v1/dv` is not used:
it skips a value the car already shows and keeps a ledger entry, both wrong for a press.

### Firmware facts behind it (2026.38.6)

- **No factory power frunk:** `VehicleUtils::hasPowerFrunk()` is hard-coded `return 0`. The frunk
  CAN request is one release bit (`VehicleControlMessage::frunkLatchRequest` =
  `GUI_frontTrunkRequest == Pressed`), and Tesla's own `automation::...::FrunkAction` only opens.
  That is why closing a frunk needs an aftermarket kit that reacts to the release.
- **Tesla's Automations engine has no "vehicle locked" trigger**, so this can't be built from it.
- **Lock source:** `VAPI_lockRequestType` says how the car was locked (9 walk-away,
  4 drive-away, 12 touchscreen, 14 app, 16 key card, 18 phone key). It's unused here because
  every lock counts.

## To check on the car

1. **The car takes the requests while locked.** Lock with the trunk open, then with the frunk
   open, and watch for `sent GUI_rearTrunkRequest` / `sent GUI_frontTrunkRequest` in the package
   log (and the trunk moving). The body controller may refuse touchscreen requests once the alarm
   arms. If the frunk misses, that is the first suspect.
2. **The door-state text.** The first lock logs `doors=...`. If it is neither a number nor
   `DoorType` names, `open_in` needs that format.
3. **Walk-away lock with the trunk open.** Whether the car locks at all with a closure open is
   decided outside the dump (VCSEC).

## Test

```sh
close-on-lock/test/run.sh
```

It runs the real service against a fake `gdbus`, whose DataValues are files, under the system
bash. On macOS that is bash 3.2, like the car's. It covers 12 cases: startup while locked, each
closure, drive-away, an unreadable gear or door state, three door-state formats, a flicker in the
lock state and stopping on TERM.
