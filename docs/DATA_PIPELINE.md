# CampusPulse data pipeline

This document follows one tick of data from the simulator to the browser, then explains each model and how its settings were chosen.

## The stages

```
Simulator (internal/simulator)
  │  ObservationBatch (512 readings)   TelemetryBatch (per AP)   SimFrame (activity)
  ▼
Worker pool, N = CPU cores (internal/workerpool + internal/pipeline/ingest.go)
  │  each batch: validate → normalise → privacy hash → pre-aggregate
  ▼
Aggregator (internal/pipeline/pipeline.go)
  │  AggregatedTick: unique devices per room and per AP, weak-signal share
  ▼
Engine (internal/engine)
  ├─ fusion ──────► occupancy estimates ──► privacy publication ──► store + Updates bus
  ├─ forecaster ──► 15/30/60/120-min forecasts + changeover warnings ──► store + bus
  ├─ diagnostics ─► AP status (healthy, congested, dead zone, offline, degraded)
  ├─ alerts ──────► raise / de-duplicate / resolve ──► store + bus
  ├─ history ─────► ring-buffered time series for charts ──► store
  └─ evaluator ───► accuracy against simulator ground truth (internal only)
```

Every arrow is a typed Go channel. Each channel has exactly one sending goroutine, which closes it when its own input closes. Cancelling the root context stops the simulator. The simulator's output channels close, and closure cascades down the graph until every goroutine has exited. The tests check this with goroutine counts before and after.

### Backpressure

- The simulator blocks if the observation channel (buffer 64 batches) is full. The pool can't fall behind without slowing the source.
- The worker pool's output buffer equals its worker count. Busy workers stop reading input.
- Between engine stages, `offer` keeps only the latest value in a 1-slot channel. If the forecaster is still busy when a new tick arrives, it skips straight to the newest one. A stale forecast is worth nothing.
- The `events.Bus` never blocks a publisher. A slow subscriber (a browser tab on bad Wi-Fi) has updates dropped for it alone, and the drops are counted.

## Stage details

### 1. Simulator

The simulator produces three feeds per tick. See `docs/ARCHITECTURE.md` for how it runs concurrently. What it models:

| Effect | How it's simulated | Why it matters |
|---|---|---|
| Devices aren't people | 6% of students carry no device, 25% one, 50% two, 19% three. About 8% of devices are idle in any tick | The calibration factor is needed |
| Signal leakage | 15% of devices associate with a neighbouring AP, 45% in a dead zone | Device counts per AP aren't counts per room |
| Location error | The controller's location service puts 15% of devices in the wrong room on the same AP | Room estimates carry real error |
| Timetable isn't attendance | Hidden turnout of 68–95% per class; 4% of classes cancelled | Fusion must trust Wi-Fi over the timetable |
| Changeover crowds | Lobby dwell time grows with how crowded the lobby already is | Lobby, lift and stair queues build up naturally |
| Rooms fill up | Each tick the free seats in a study space (minus people already there or on their way) are split exactly between the four student shards | No room is ever over capacity, even though shards choose independently |
| Students avoid bad Wi-Fi | Dead-zone rooms are considered a fifth as often, most students who pick one turn back, and those who stay leave after 10–20 minutes | The east-wing room stays free, as in the proposal's example |
| Dead zones | Three planted APs with -84 dBm coverage | Diagnostics must find them without being told |
| AP failures | Random outages (about 0.004 per AP-hour, recovering after 20–50 min), plus manual ones from the controls | Tests failure detection |

### 2. Ingest: validate, normalise, hash

Three `fn.Step` functions are composed with `fn.Chain`:

| Step | Rejects | Notes |
|---|---|---|
| `ValidateStep` | unknown AP, signal outside -110 to -10 dBm, missing device ID or timestamp | Rejections are counted, not fatal |
| `NormalizeStep` | none | Rounds signal to whole dBm; replaces an unknown room with one the AP serves |
| `PrivacyStep` | none | Replaces the raw device ID with a daily HMAC hash and wipes the original |

Each worker then groups its batch by room and AP, so the single aggregator only merges a few small maps.

### 3. Aggregation

The aggregator collects every partial for a tick. Each batch carries the tick's batch total, so arrival order doesn't matter. It de-duplicates device hashes per room and per AP and computes each AP's mean client signal and the share of clients below -75 dBm. If a batch is lost (a worker panicked), that tick is dropped when a later tick completes, and counted in `dropped_ticks`.

### 4. Occupancy fusion (`internal/occupancy`)

```
wifi_people = devices / 1.65
expected    = expected attendance of any class running now (timetable)

class running, AP up :  0.75 × wifi_people + 0.25 × expected
no class, AP up      :  wifi_people
class running, AP down: expected            (confidence 0.35)
no class, AP down    :  wifi_people from neighbouring APs (confidence 0.25)

smoothed = 0.85 × new + 0.15 × previous   (reset whenever a class starts or ends)
```

Every estimate comes with a range (±12% to ±50% depending on the situation) and a confidence from 0.25 to 0.9. Confidence drops when Wi-Fi and the timetable disagree, when counts are small, or when the AP is down.

The calibration factor of 1.65 observed devices per person comes from the device mix above (1.82 devices per student, 92% active). In a real deployment you would calibrate it against a few manual headcounts.

### 5. Forecasting (`internal/forecast`)

The rule-based model works on one room and one horizon at a time. Floor, building and campus forecasts add up their rooms.

| Situation at the target time T | Target value |
|---|---|
| The class running now is still running at T | current estimate (90% weight) |
| A different class is scheduled at T | expected attendance × learned attendance ratio |
| Nothing scheduled at T | learned time-of-day pattern for the room |
| Room is a lobby | plus a surge for classes ending near T (people leaving) and starting within 20 min (people waiting) |

`forecast = w × current + (1 − w) × target`, where `w = exp(−horizon / 45 min)`. The weight `w` is 0 if the current crowd is explained by a class that ends before T. Each forecast lists the factors behind it in plain language, such as "Introduction to International Affairs lets out at 2:50 PM (~280 people)".

What the model learns while it runs:

- **Attendance ratio**: observed ÷ timetabled attendance for classes past their first 10 minutes. It settles around 0.8–0.95.
- **Room scale**: how busy each room runs compared with a generic prior for its type.
- **Time-of-day pattern**: per room, in half-hour buckets, kept across simulated days. That's why forecasts improve on day 2.

A naive "nothing changes" persistence model is registered too, so the evaluator can show what the rule-based model adds.

**Changeover warnings** list every class ending in the next 3 hours with 80 or more people. Each warning gives the crowded window (8–20 minutes, longer for bigger classes), the affected lobbies, and "arrive before / after" times. Head counts in warnings are rounded to the nearest 10.

### 6. Wi-Fi diagnostics (`internal/diagnostics`)

Rules are function values in a registry. Each sees a 3-sample rolling summary for one AP:

| Rule | Fires when | Diagnosis |
|---|---|---|
| `ap_silent` | no telemetry for ≥ 3 simulated minutes | AP failure (critical) |
| `high_utilisation` | utilisation ≥ 75% **and** (latency ≥ 35 ms **or** loss ≥ 1.5%) | congestion (critical at ≥ 92%) |
| `weak_coverage` | ≥ 50% of devices below -75 dBm **and** utilisation < 60% **and** at least 4 devices | dead zone |
| `poor_performance` | latency ≥ 60 ms or loss ≥ 3% without high load or weak signal | degraded (interference) |

The utilisation conditions keep congestion and dead zones mutually exclusive. A busy AP with some weak clients is congestion. A quiet AP whose clients can barely hear it is a dead zone.

Alerts need 2 consecutive clean batches before they resolve, so they don't flap around a threshold.

## Measured accuracy

The evaluator compares raw (pre-noise) estimates with the simulator's true headcounts. It uses the weighted absolute percentage error (total absolute error ÷ total true people) and only scores ticks with at least 100 people on campus.

Validation run: seed 42, which was **not** used for tuning. About two simulated days, 2 simulated minutes per tick, after the seat-budget and dead-zone-avoidance changes to the simulator.

| Target from the proposal | Measured | Met |
|---|---|---|
| Occupancy error < 15% | 13.3% room level, 5.8% building level | yes |
| 60-minute forecast error < 20% | building level: 19.6% cumulative over two days (about 23–25% on day 1 alone) | from day 2 |
| | room level: 39.2% (naive baseline 36.8%) | no |
| Planted dead zones found ≥ 90% | 3 of 3 | yes |
| Congestion vs dead zone ≥ 85% | 98.6% of 3,661 AP diagnoses | yes |
| Failed AP detected < 5 min | 2 min mean, 2m01s worst, 3 outages | yes |
| ~50,000 readings/s | about 4.2 million/s on an Apple M2 (`BenchmarkPipelineThroughput`) | yes |

An earlier long run on an Apple M2 (about 13 simulated days, 7.7 million readings, before the two simulator changes above) gave similar figures: 14.7% room-level occupancy error, 17.9% building-level 60-minute forecast error, 98.2% classification accuracy, 3 of 3 dead zones, and 20 of 20 AP failures detected in about 2 minutes. It ran with zero rejected readings, zero dropped ticks and a flat 3.3 MB heap.

These come from a synthetic campus, so they show the method works on data with known answers. They aren't a claim about real GW Wi-Fi.

### Limits worth knowing

- **Room-level forecasts are weak.** An hour ahead, a single study room's occupancy depends on individual choices the model can't see. The naive "no change" baseline does as well or slightly better, so the rule-based model adds nothing at this level. At building level the errors cancel, and the model clearly beats the baseline. The student UI shows forecasts at building and floor level, and uses room-level forecasts only to rank rooms.
- **Day-1 forecasts lack history.** With no learned pattern yet, the first simulated day's 60-minute building error is about 23–25%.
- **The tick is 2 simulated minutes.** Very short events, such as lobby dwell under 2 minutes, are only partly visible.

## How the settings were chosen

`internal/engine/tune_test.go` is an opt-in harness. It records two simulated days on seed 7 and replays them through a grid of settings:

```
TUNE=1 go test -run TestTune -v ./internal/engine/
TUNE=1 DAY2=1 go test -run TestTune -v ./internal/engine/
```

Findings that shaped the defaults:

- **Smoothing.** EWMA weight 0.6 gave 13.9% room error, 0.85 gave about 12.3%, and 0.4 gave 17%. Each tick covers 2 simulated minutes, so heavy smoothing lags behind real movement.
- **Wi-Fi weight during class.** Anything from 0.6 to 0.9 lands within 0.2 percentage points. 0.75 was kept.
- **Calibration.** 1.65 beat 1.60 and 1.70, which matches the device mix.
- **Forecast.** The first version under-predicted study spaces and lobbies. Learning a per-room scale and modelling students waiting before class cut day-1 building error from 28.3% to 23.5%. Shaping free-space demand with timetable activity made it worse (30%): students leave before their last evening class ends, so the timetable overstates evening activity. It was reverted.
