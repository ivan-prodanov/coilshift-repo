# grok

Airgapped **Grok on the home-region assistant shard**. **Verified live on-car
2026-11-10** (an EU-homed car, Premium Connectivity
active, fw `develop-2026.8.6`).

> Not a `libQtCarGUI` byte patch like the other features here — this one is an
> **assistant-endpoint redirect + a DataValue hold** (car-assist config, no lib
> patching). Mechanism differs; folder layout is the same.

## Root cause

The car was switched to **US region** (`/var/etc/country=US`). car-assist chooses
its assistant endpoint in `/etc/car-assist.vars` purely from region — `is-emea-car`
/ `is-china-car` read `/var/etc/country`, and **NA is the default fall-through**
(there is no `is-na-car`) — so on US it dials `assistant-api.prd.na…`.

Transport was fine: QUIC + VIN mTLS to NA established (healthy `quic_tracing_metrics`,
RTT ~216 ms). But every Grok turn was closed by the server with:

```
Application error 0x193 (remote): connection closed by server      # 0x193 = 403 Forbidden
```

403 = the NA shard refusing to **entitle an EU-homed VIN**. It is **not** a token
problem: `MEDIA_authToken` and `GUI_assistantAuthToken` are both 10-byte stubs (no
JWT), Hermes is sealed so none can be minted — and the fix below still works.

## Fix (two levers)

1. Point car-assist at the **home shard** (euw1): `touch /tmp/car-assist/force-eu`
   (+ remove `force-na`) and `sv restart car-assist`.
2. Open the client auth gate: `MEDIA_needAuthToken=false`.

On euw1 the server returns `control_message_interaction_ack enable=true` +
`control_message_conversation_id` (real Grok conversation), mic audio streams, **zero
0x193**. Grok answers — airgapped, **no cloud token, Hermes closed**.

**Key finding:** airgapped Grok needs **no live mothership/Hermes OAuth token** on
the home shard. The token is only a client-side fail-closed gate
(`MEDIA_needAuthToken`); force it false and euw1 authorizes on **VIN mTLS + Premium
Connectivity** alone. Corrects the earlier `tesla-firmware/out/grok-*` reports (the
US failure was region-entitlement, not a stale token).

## Durable form: CoilShift's built-in

This repo no longer has a grok package. CoilShift ships its own signed built-in
`dev.coilshift.grok-region`, which does the same thing (force-<region> marker + held
`MEDIA_needAuthToken=false`), so that's the durable form. Our package was removed
2026-11-20. Don't run `grok-eu.sh` on top of the built-in; it's only for a car without it.

Prereq (unchanged): the shard must be reachable on your gateway — UDP/18113 to the
resolved `assistant-api.prd.euw1…` IPs.

## Files

- `grok-eu.sh` — standalone re-apply script (only for a car without the built-in
  `dev.coilshift.grok-region`; forces euw1, opens the gate, verifies)
