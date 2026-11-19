# Privacy in CampusPulse

Counting people with Wi-Fi can turn into tracking people with Wi-Fi. CampusPulse is built so the first never becomes the second. All of the rules below are enforced in the Go backend, before data reaches any API, WebSocket message or log line.

The prototype uses only synthetic data. No real GW network logs or timetables are involved.

## The rules

| Rule | Where it is enforced | Tested by |
|---|---|---|
| Raw device IDs are replaced as the first processing step | `pipeline.PrivacyStep`, inside the observation worker pool | `TestPrivacyStepRemovesRawID` |
| Hashes are keyed and rotate daily | `privacy.Hasher` (HMAC-SHA256, key derived per calendar day) | `TestHasherRotatesDaily` |
| No count under 10 is published, directly or indirectly | `privacy.Suppress`, applied by `occupancy.Publish`, which also hides device counts, ranges and ratios for suppressed rooms | `TestSuppressSmallGroups`, `TestPublisherNeverExposesSmallCounts`, `TestEngineEndToEnd` |
| Small random noise on published counts | `privacy.Publisher` (Gaussian, σ = 1 by default) | `TestAddNoiseNeverNegative` |
| Only group counts leave the backend | API and WebSocket only carry room, floor, building and campus totals | `TestEngineEndToEnd` |
| Movement shown in 3D is coarse | `engine.CoarsenFlows` rounds flows to multiples of 5 and drops smaller ones | `TestCoarsenFlows` |
| Ground truth never leaves the backend | `simulator.Truth` is read only by `internal/accuracy`, which reports error percentages | code structure (see below) |

## How device hashing works

```
day_key     = HMAC-SHA256(secret, "campuspulse/device-key/" + date)
device_hash = first 16 bytes of HMAC-SHA256(day_key, raw_device_id)
```

- The same phone gets the same hash all day, which is what de-duplication needs.
- Tomorrow it gets an unrelated hash, so nobody can follow a device across days.
- Without the secret, a hash can't be linked back to a MAC address, even by trying every possible MAC.
- If `CAMPUSPULSE_HASH_SECRET` isn't set, a random secret is generated at start-up. Hashes then don't even survive a restart.

The raw ID exists only inside the simulator and the first pipeline step. `PrivacyStep` overwrites the field. `ProcessBatch` then writes the scrubbed observation back into the producer's slice, so the raw string isn't left sitting in the batch either. `WiFiObservation.RawDeviceID` is tagged `json:"-"`, so it can't be serialised by accident.

## Small-group suppression

A published count goes through these steps:

1. Add noise to the raw estimate.
2. If the result is below 10, publish `{"suppressed": true, "display": "<10"}` with a value of 0.
3. For a suppressed room, also hide everything the count could be worked back out from: the low/high range, the device count (devices ÷ 1.65 ≈ people) and the occupancy ratio (ratio × capacity ≈ people). All are published as zero or `<10`.

An early version hid the headcount but still published the device count and a rounded ratio. Bell 203 showed `<10` people next to 15 devices, which works out to about 9 people. `TestPublishSuppressesSmallRooms` and `TestEngineEndToEnd` now check that a suppressed room leaks nothing through either field.

The four-colour status level (low, moderate, high, very high) is still shown for suppressed rooms. "This 6-seat carrel room is busy" is the whole point of the product, and it isn't a count. That's a deliberate trade-off between privacy and usefulness. A stricter deployment could hide the level for rooms under 10 seats.

Floor, building and campus totals are added up from raw room estimates **before** suppression and then published as a whole. Three small study rooms with 4, 5 and 6 people therefore give a floor total of 15, which can be shown. The rooms themselves still show `<10`.

## Ground truth stays inside

The simulator knows exactly how many people are in every room. That's what lets CampusPulse measure its own accuracy. The separation is enforced by structure:

- `simulator.Truth` is reachable only through `Simulator.Truth()`. The engine reaches it through its `SimView` interface and passes it only to `accuracy.Evaluator`.
- The evaluator returns `accuracy.Report`, which contains error percentages and sample counts and no headcounts.
- HTTP handlers receive `campus.ReadRepository`. That interface has no method that returns raw estimates or ground truth, so a handler couldn't leak them even by mistake.
- `store.People()` (raw estimates, used by the forecaster) isn't part of any interface the API layer gets.

## What a real deployment would add

- Store the hash secret in a secrets manager and rotate it.
- Keep only aggregates. CampusPulse never stores observations; they're dropped once aggregated.
- Publish a data-protection notice and get sign-off from the university's privacy office.
- Consider a formal privacy model (for example, differential privacy with a stated budget) rather than fixed noise.
